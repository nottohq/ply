package main

import (
	"fmt"
	"strings"
	"testing"
)

func culpritIDs(v any) []string {
	var out []string
	for _, c := range v.(map[string]any)["culprits"].([]any) {
		out = append(out, c.(string))
	}
	return out
}

// One bad change among many, from several agents merged together.
func TestCulpritFindsTheOneBadChange(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 6; i++ {
		e.write("main", fmt.Sprintf("m%d.txt", i), "ok\n")
	}
	e.must("main", "snap", "-m", "base")
	check := "! grep -rq BUG --include=*.txt ."
	e.must("main", "check", "--", check)
	for a := 0; a < 3; a++ {
		ws := fmt.Sprint("agent", a)
		e.must("main", "ws", "new", ws)
		for i := 0; i < 5; i++ {
			body := fmt.Sprintf("ok\nwork %d by %s\n", i, ws)
			if a == 1 && i == 3 {
				body += "BUG\n"
			}
			e.write(ws, fmt.Sprintf("%s-%d.txt", ws, i), body)
			e.must(ws, "snap", "-m", fmt.Sprintf("%s step %d", ws, i))
		}
	}
	for a := 0; a < 3; a++ {
		e.must("main", "merge", fmt.Sprint("agent", a))
	}
	res := e.js("main", "culprit", "--", check)
	ids := culpritIDs(res)
	if len(ids) != 1 || ids[0] != e.idOf("main", "agent1 step 3") {
		t.Fatalf("want exactly agent1 step 3, got %v", res)
	}
	m := res.(map[string]any)
	if m["among"].(float64) != 15 || m["runs"].(float64) > 12 {
		t.Fatalf("searched %v changes in %v runs", m["among"], m["runs"])
	}
	// The search must leave the workspace exactly as it was.
	if s := e.must("main", "snap"); !strings.Contains(s, "nothing changed") {
		t.Fatalf("culprit search disturbed the workspace: %s", s)
	}
	if e.read("main", "agent1-3.txt") == "<missing>" {
		t.Fatalf("culprit search without --hold removed the change")
	}
}

// Two changes that are each fine and fail only together: no linear history can show this.
func TestCulpritFindsAnInteraction(t *testing.T) {
	e := newEnv(t)
	e.write("main", "a.cfg", "mode=old\n")
	e.write("main", "b.cfg", "limit=10\n")
	e.must("main", "snap", "-m", "base")
	check := "! (grep -q mode=new a.cfg && grep -q limit=99 b.cfg)"
	e.must("main", "check", "--", check)
	for i := 0; i < 4; i++ {
		ws := fmt.Sprint("w", i)
		e.must("main", "ws", "new", ws)
		e.write(ws, fmt.Sprintf("noise%d.txt", i), "x\n")
		e.must(ws, "snap", "-m", fmt.Sprint("noise ", i))
	}
	e.write("w1", "a.cfg", "mode=new\n")
	e.must("w1", "snap", "-m", "switch to new mode")
	e.write("w3", "b.cfg", "limit=99\n")
	e.must("w3", "snap", "-m", "raise the limit")
	for i := 0; i < 4; i++ {
		e.must("w"+fmt.Sprint(i), "check", "--", check) // each agent's own work passes
		e.must("main", "merge", fmt.Sprint("w", i))
	}
	out := e.must("main", "culprit", "--hold", "--", check)
	if !strings.Contains(out, "switch to new mode") || !strings.Contains(out, "raise the limit") || !strings.Contains(out, "fail together") || strings.Contains(out, "noise") {
		t.Fatalf("want the two interacting changes and nothing else:\n%s", out)
	}
	e.must("main", "check", "--", check)
	if e.read("main", "noise2.txt") != "x\n" {
		t.Fatalf("holding back the culprits removed unrelated work")
	}
	// Bring one of the pair back: still passes, because each is fine alone.
	e.must("main", "redo", e.idOf("main", "raise the limit"))
	e.must("main", "check", "--", check)
	if e.read("main", "b.cfg") != "limit=99\n" || e.read("main", "a.cfg") != "mode=old\n" {
		t.Fatalf("redo of one of the pair gave a=%q b=%q", e.read("main", "a.cfg"), e.read("main", "b.cfg"))
	}
}

// merge --check: what breaks the command is held back, the rest lands.
func TestMergeWithCheckStaysGreen(t *testing.T) {
	e := newEnv(t)
	e.write("main", "calc.sh", "echo 4\n")
	e.must("main", "snap", "-m", "base")
	check := "test \"$(sh calc.sh)\" = 4"
	e.must("main", "ws", "new", "w")
	e.write("w", "README", "docs\n")
	e.must("w", "snap", "-m", "docs")
	e.write("w", "calc.sh", "echo 5\n")
	e.must("w", "snap", "-m", "break the calculator")
	e.write("w", "LICENSE", "MIT\n")
	e.must("w", "snap", "-m", "license")
	out := e.must("main", "merge", "w", "--check", check)
	if !strings.Contains(out, "break the calculator") || !strings.Contains(out, "held back") {
		t.Fatalf("merge --check should name and hold back the breaking change:\n%s", out)
	}
	if e.read("main", "calc.sh") != "echo 4\n" || e.read("main", "README") != "docs\n" || e.read("main", "LICENSE") != "MIT\n" {
		t.Fatalf("expected the good changes in and the bad one out: %v", e.tree("main"))
	}
	e.must("main", "check", "--", check)
	// The author still has their work, and can see it was held back upstream.
	e.must("w", "merge", "main")
	if log := e.must("w", "log"); !strings.Contains(log, "hold back: fails") {
		t.Fatalf("the hold-back should be visible to the author:\n%s", log)
	}
}

// No check was ever recorded: the search finds a passing state along the trail by itself.
func TestCulpritWithoutAKnownGoodState(t *testing.T) {
	e := newEnv(t)
	e.write("main", "f", "1\n")
	e.must("main", "snap", "-m", "base")
	for i := 2; i <= 9; i++ {
		body := e.read("main", "f") + fmt.Sprint(i) + "\n"
		if i == 6 {
			body += "BUG\n"
		}
		e.write("main", "f", body)
		e.must("main", "snap", "-m", fmt.Sprint("step ", i))
	}
	res := e.js("main", "culprit", "--", "! grep -q BUG f")
	if ids := culpritIDs(res); len(ids) != 1 || ids[0] != e.idOf("main", "step 6") {
		t.Fatalf("want step 6, got %v", res)
	}
}

// The thing that broke the build can be an undo.
func TestCulpritCanBeAnUndo(t *testing.T) {
	e := newEnv(t)
	e.write("main", "f", "a\n")
	e.must("main", "snap", "-m", "base")
	e.write("main", "f", "a\nneeded\n")
	e.must("main", "snap", "-m", "add the needed line")
	check := "grep -q needed f"
	e.must("main", "check", "--", check)
	e.write("main", "g", "other\n")
	e.must("main", "snap", "-m", "unrelated")
	e.must("main", "undo", e.idOf("main", "add the needed line"), "-m", "remove what looked unused")
	out := e.must("main", "culprit", "--hold", "--", check)
	if !strings.Contains(out, "remove what looked unused") || strings.Contains(out, "unrelated") {
		t.Fatalf("want the undo named as the culprit:\n%s", out)
	}
	e.must("main", "check", "--", check)
	if e.read("main", "g") != "other\n" {
		t.Fatalf("unrelated work was lost")
	}
}
