package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ===== exactness =====

func TestRoundTripExact(t *testing.T) {
	e := newEnv(t)
	files := map[string]string{
		"a.txt":            "one\ntwo\nthree\n",
		"no-newline.txt":   "one\ntwo",
		"empty.txt":        "",
		"only-newlines":    "\n\n\n",
		"crlf.txt":         "a\r\nb\r\n",
		"unicode.md":       "héllo\n日本語\n🙂\n",
		"deep/er/file.go":  "package x\n",
		"bin.dat":          "\x00\x01\x02\xff\xfe binary \n\x00",
		"invalid-utf8.txt": "ok\n\xc3\x28\n",
	}
	for n, b := range files {
		e.write("main", n, b)
	}
	os.Chmod(filepath.Join(e.root, "a.txt"), 0o755)
	e.must("main", "snap", "-m", "import")
	e.must("main", "ws", "new", "copy")
	if !reflect.DeepEqual(e.tree("main"), e.tree("copy")) {
		t.Fatalf("copy differs from main")
	}
	e.write("copy", "a.txt", "one\n2\nthree\nfour")
	e.write("copy", "no-newline.txt", "one\ntwo\n")
	e.write("copy", "empty.txt", "now has text\n")
	e.write("copy", "bin.dat", "\x00different\x00")
	e.write("copy", "invalid-utf8.txt", "now valid text\n")
	e.rm("copy", "crlf.txt")
	e.rm("copy", "deep/er/file.go")
	e.write("copy", "new/file.txt", "hello\n")
	os.Chmod(filepath.Join(e.dir("copy"), "a.txt"), 0o644)
	os.Chmod(filepath.Join(e.dir("copy"), "unicode.md"), 0o755)
	e.must("copy", "snap", "-m", "edit everything")
	e.must("main", "merge", "copy")
	if !reflect.DeepEqual(e.tree("main"), e.tree("copy")) {
		t.Fatalf("after merge main differs from copy:\n%v\n%v", e.tree("main"), e.tree("copy"))
	}
	if _, err := os.Stat(filepath.Join(e.root, "deep")); !os.IsNotExist(err) {
		t.Fatalf("empty directory was left behind")
	}
	if s := e.must("main", "snap"); !strings.Contains(s, "nothing changed") {
		t.Fatalf("merge left unrecorded edits: %s", s)
	}
}

// A second edit that keeps the size, made right after a snap, must still be seen.
func TestQuickSameSizeEditIsNotMissed(t *testing.T) {
	e := newEnv(t)
	e.write("main", "f", "aaaa\n")
	e.must("main", "snap", "-m", "one")
	for i := 0; i < 20; i++ {
		body := fmt.Sprintf("%04d\n", i)
		st, _ := os.Stat(filepath.Join(e.root, "f"))
		e.write("main", "f", body)
		// Same size AND same timestamp, as on a filesystem with a coarse clock.
		os.Chtimes(filepath.Join(e.root, "f"), st.ModTime(), st.ModTime())
		if s := e.must("main", "snap", "-m", "edit"); strings.Contains(s, "nothing changed") {
			t.Fatalf("edit %d was missed", i)
		}
		e.must("main", "ws", "new", fmt.Sprint("w", i))
		if got := e.read(fmt.Sprint("w", i), "f"); got != body {
			t.Fatalf("edit %d recorded as %q", i, got)
		}
	}
}

func TestIgnoredFilesStayOut(t *testing.T) {
	e := newEnv(t)
	e.write("main", ".gitignore", "node_modules/\n*.log\n")
	e.write("main", "src/a.js", "x\n")
	e.write("main", "node_modules/big/index.js", "junk\n")
	e.write("main", "debug.log", "noise\n")
	e.must("main", "snap", "-m", "import")
	e.must("main", "ws", "new", "w")
	got := e.tree("w")
	if len(got) != 2 || got[".gitignore"] == "" || got["src/a.js"] == "" {
		t.Fatalf("ignored files were versioned: %v", got)
	}
	// An incoming file must not silently overwrite an ignored file sitting at its path.
	e.write("w", "debug.log", "tracked on purpose\n")
	os.WriteFile(filepath.Join(e.dir("w"), ".gitignore"), []byte("node_modules/\n"), 0o644)
	e.must("w", "snap", "-m", "track the log")
	if err := e.cmd("main", "merge", "w"); err == nil || !strings.Contains(err.Error(), "in the way") {
		t.Fatalf("expected a refusal to overwrite the ignored file, got %v", err)
	}
	if e.read("main", "debug.log") != "noise\n" {
		t.Fatalf("the ignored file was overwritten")
	}
}

// ===== merge order never matters =====

func TestMergeOrderNeverMatters(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	rounds := 40
	if testing.Short() {
		rounds = 8
	}
	for it := 0; it < rounds; it++ {
		e := newEnv(t)
		names := []string{"x.txt", "y.txt", "z/w.txt"}
		for _, n := range names {
			e.write("main", n, lines(4+rng.Intn(8), n))
		}
		e.must("main", "snap", "-m", "base")
		agents := []string{"a", "b", "c"}
		for _, a := range agents {
			e.must("main", "ws", "new", a)
		}
		for _, a := range agents {
			for s := 1 + rng.Intn(3); s > 0; s-- {
				n := names[rng.Intn(len(names))]
				switch rng.Intn(10) {
				case 0:
					e.rm(a, n)
				case 1:
					e.write(a, a+"-new.txt", lines(2, a))
				case 2:
					e.write(a, "shared-new.txt", lines(2, a))
				case 3:
					if e.read(a, n) != "<missing>" {
						e.mv(a, n, fmt.Sprintf("moved/%s-%d.txt", a, rng.Intn(2)))
					}
				case 4:
					if e.read(a, n) != "<missing>" {
						e.mv(a, n, "moved/same-target.txt")
					}
				default:
					if cur := e.read(a, n); cur != "<missing>" {
						e.write(a, n, randomEdit(rng, cur, a))
					}
				}
				e.must(a, "snap", "-m", "work")
			}
		}
		var first map[string]string
		for p, perm := range [][]string{{"a", "b", "c"}, {"a", "c", "b"}, {"b", "a", "c"}, {"b", "c", "a"}, {"c", "a", "b"}, {"c", "b", "a"}} {
			ws := fmt.Sprintf("m%d", p)
			e.must("main", "ws", "new", ws)
			for _, a := range perm {
				e.must(ws, "merge", a)
			}
			got := e.tree(ws)
			if first == nil {
				first = got
			} else if !reflect.DeepEqual(first, got) {
				t.Fatalf("round %d: merge order %v gave a different tree\nwant %v\n got %v", it, perm, first, got)
			}
			if s := e.must(ws, "snap"); !strings.Contains(s, "nothing changed") {
				t.Fatalf("round %d: files and state disagree after merging: %s", it, s)
			}
		}
		e.must("a", "merge", "b")
		e.must("c", "merge", "a")
		e.must("b", "merge", "c")
		e.must("a", "merge", "c")
		for _, a := range agents {
			if got := e.tree(a); !reflect.DeepEqual(first, got) {
				t.Fatalf("round %d: agent %s ended on a different tree\nwant %v\n got %v", it, a, first, got)
			}
		}
		// A fresh checkout of the merged state, built from history alone, matches too.
		e.must("a", "ws", "new", "fresh")
		if got := e.tree("fresh"); !reflect.DeepEqual(first, got) {
			t.Fatalf("round %d: fresh copy differs", it)
		}
	}
}

// ===== undo keeps later work =====

func TestUndoKeepsLaterWork(t *testing.T) {
	e := newEnv(t)
	e.write("main", "app.py", "import os\n\ndef main():\n    run()\n")
	e.must("main", "snap", "-m", "base")
	e.write("main", "app.py", "import os\nimport telemetry\n\ndef main():\n    telemetry.start()\n    run()\n")
	e.must("main", "snap", "-m", "add telemetry")
	e.write("main", "app.py", "import os\nimport telemetry\n\ndef main():\n    telemetry.start()\n    run()\n\ndef helper():\n    return 42\n")
	e.must("main", "snap", "-m", "add helper")
	e.write("main", "README", "docs\n")
	e.must("main", "snap", "-m", "add readme")
	before := e.tree("main")
	id := e.idOf("main", "add telemetry")
	e.must("main", "undo", id[:6])
	want := "import os\n\ndef main():\n    run()\n\ndef helper():\n    return 42\n"
	if got := e.read("main", "app.py"); got != want {
		t.Fatalf("undo of an early change lost or kept the wrong lines:\n%q", got)
	}
	if e.read("main", "README") != "docs\n" {
		t.Fatalf("undo removed an unrelated later file")
	}
	if err := e.cmd("main", "undo", id[:6]); err == nil {
		t.Fatalf("undoing twice should be refused")
	}
	e.must("main", "redo", id[:6])
	if !reflect.DeepEqual(before, e.tree("main")) {
		t.Fatalf("redo did not restore the exact tree")
	}
	if err := e.cmd("main", "redo", id[:6]); err == nil {
		t.Fatalf("redoing something that is on should be refused")
	}
}

func TestUndoAnyOrderProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	rounds := 20
	if testing.Short() {
		rounds = 4
	}
	for it := 0; it < rounds; it++ {
		e := newEnv(t)
		e.write("main", "f.txt", lines(6, "base"))
		e.write("main", "g.txt", lines(3, "g"))
		e.must("main", "snap", "-m", "base")
		start := e.tree("main")
		n := 3 + rng.Intn(4)
		var prev map[string]string
		var ids []string
		for i := 0; i < n; i++ {
			prev = e.tree("main")
			f := []string{"f.txt", "g.txt"}[rng.Intn(2)]
			e.write("main", f, randomEdit(rng, e.read("main", f), fmt.Sprint("c", i)))
			if s := e.must("main", "snap", "-m", fmt.Sprint("c", i)); !strings.Contains(s, "nothing changed") {
				ids = append(ids, e.idOf("main", fmt.Sprint("c", i)))
			}
		}
		end := e.tree("main")
		if len(ids) == 0 {
			continue
		}
		e.must("main", "undo", ids[len(ids)-1])
		if got := e.tree("main"); !reflect.DeepEqual(prev, got) {
			t.Fatalf("round %d: undo of the newest change is not the previous tree\nwant %v\n got %v", it, prev, got)
		}
		rest := append([]string{}, ids[:len(ids)-1]...)
		rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
		for _, id := range rest {
			e.must("main", "undo", id)
		}
		if got := e.tree("main"); !reflect.DeepEqual(start, got) {
			t.Fatalf("round %d: undoing everything did not return to the start\nwant %v\n got %v", it, start, got)
		}
		rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		for _, id := range ids {
			e.must("main", "redo", id)
		}
		if got := e.tree("main"); !reflect.DeepEqual(end, got) {
			t.Fatalf("round %d: redoing everything did not return to the end\nwant %v\n got %v", it, end, got)
		}
	}
}

// An undo is a change like any other: it travels by merge and is attributed.
func TestUndoTravelsAndCascades(t *testing.T) {
	e := newEnv(t)
	e.write("main", "f", "a\nb\nc\n")
	e.must("main", "snap", "-m", "base")
	e.write("main", "f", "a\nNEW\nb\nc\n")
	e.must("main", "snap", "-m", "add NEW")
	e.write("main", "f", "a\nNEW edited\nb\nc\n")
	e.must("main", "snap", "-m", "edit NEW")
	e.must("main", "ws", "new", "w")
	out := e.must("w", "undo", e.idOf("w", "add NEW"))
	if !strings.Contains(out, "review") {
		t.Fatalf("undo should point at the change built on it: %s", out)
	}
	e.must("w", "redo", e.idOf("w", "add NEW"))
	e.must("w", "undo", e.idOf("w", "add NEW"), "--cascade", "--agent", "reviewer")
	if got := e.read("w", "f"); got != "a\nb\nc\n" {
		t.Fatalf("cascade undo left %q", got)
	}
	// Bringing back only the first change must not bring back the one built on it.
	e.must("main", "ws", "new", "partial", "--from", "w")
	e.must("partial", "redo", e.idOf("partial", "add NEW"))
	if got := e.read("partial", "f"); got != "a\nNEW\nb\nc\n" {
		t.Fatalf("redo of one change after a cascade gave %q", got)
	}
	e.must("main", "merge", "w")
	if got := e.read("main", "f"); got != "a\nb\nc\n" {
		t.Fatalf("the undo did not travel with the merge: %q", got)
	}
	if log := e.must("main", "log"); !strings.Contains(log, "reviewer") || !strings.Contains(log, "undo: add NEW") {
		t.Fatalf("the undo is not attributed in the log:\n%s", log)
	}
}

// ===== renames =====

// One agent moves a file while another edits it: the edit lands in the moved file.
func TestRenameCarriesConcurrentEdits(t *testing.T) {
	e := newEnv(t)
	e.write("main", "util.py", lines(12, "line"))
	e.write("main", "other.py", "x\n")
	e.must("main", "snap", "-m", "base")
	e.must("main", "ws", "new", "mover")
	e.must("main", "ws", "new", "editor")
	e.mv("mover", "util.py", "lib/helpers.py")
	e.write("mover", "lib/helpers.py", strings.Replace(e.read("mover", "lib/helpers.py"), "line 2\n", "line two\n", 1))
	e.must("mover", "snap", "-m", "move util to lib and tweak")
	e.write("editor", "util.py", strings.Replace(e.read("editor", "util.py"), "line 9\n", "line nine, edited\n", 1))
	e.must("editor", "snap", "-m", "edit line 9")
	for _, order := range [][2]string{{"mover", "editor"}, {"editor", "mover"}} {
		ws := "m-" + order[0]
		e.must("main", "ws", "new", ws)
		e.must(ws, "merge", order[0])
		e.must(ws, "merge", order[1])
		got := e.tree(ws)
		if _, still := got["util.py"]; still || len(got) != 2 {
			t.Fatalf("expected the file only at its new path, got %v", got)
		}
		body := e.read(ws, "lib/helpers.py")
		if !strings.Contains(body, "line two\n") || !strings.Contains(body, "line nine, edited\n") || strings.Contains(body, "line 9\n") {
			t.Fatalf("the concurrent edit did not follow the rename:\n%s", body)
		}
		if cs := e.contests(ws); len(cs) != 0 {
			t.Fatalf("a rename plus an edit elsewhere is not a contest: %v", cs)
		}
		blame := e.must(ws, "blame", "lib/helpers.py")
		if !strings.Contains(blame, "base") || !strings.Contains(blame, "edit line 9") {
			t.Fatalf("history did not follow the rename:\n%s", blame)
		}
	}
}

// Two agents move different files onto one path: both survive and it is reported.
func TestTwoFilesWantOnePath(t *testing.T) {
	e := newEnv(t)
	e.write("main", "a.txt", lines(5, "a"))
	e.write("main", "b.txt", lines(5, "b"))
	e.must("main", "snap", "-m", "base")
	e.must("main", "ws", "new", "x")
	e.must("main", "ws", "new", "y")
	e.mv("x", "a.txt", "final.txt")
	e.must("x", "snap", "-m", "a becomes final")
	e.mv("y", "b.txt", "final.txt")
	e.must("y", "snap", "-m", "b becomes final")
	e.must("x", "merge", "y")
	e.must("y", "merge", "x")
	tx, ty := e.tree("x"), e.tree("y")
	if !reflect.DeepEqual(tx, ty) || len(tx) != 2 {
		t.Fatalf("both files must survive, identically on both sides:\n%v\n%v", tx, ty)
	}
	var bodies []string
	for _, b := range tx {
		bodies = append(bodies, b)
	}
	if !strings.Contains(strings.Join(bodies, ""), "a 1\n") || !strings.Contains(strings.Join(bodies, ""), "b 1\n") {
		t.Fatalf("content was lost: %v", tx)
	}
	cs := e.contests("x")
	if len(cs) != 1 || cs[0]["kind"] != "path" || cs[0]["file"] != "final.txt" {
		t.Fatalf("want one path contest on final.txt, got %v", cs)
	}
	// Settle it by moving the marked copy somewhere else.
	for name := range tx {
		if name != "final.txt" {
			e.mv("x", name, "second.txt")
		}
	}
	e.must("x", "snap", "-m", "give the other file its own name")
	if cs := e.contests("x"); len(cs) != 0 {
		t.Fatalf("contest should be settled: %v", cs)
	}
	e.must("y", "merge", "x")
	if !reflect.DeepEqual(e.tree("x"), e.tree("y")) || e.read("y", "second.txt") == "<missing>" {
		t.Fatalf("settlement did not travel: %v", e.tree("y"))
	}
}

// ===== contests =====

func TestContests(t *testing.T) {
	e := newEnv(t)
	e.write("main", "cfg.txt", lines(10, "setting"))
	e.must("main", "snap", "-m", "base")
	for _, w := range []string{"a", "b", "c"} {
		e.must("main", "ws", "new", w)
	}
	edit := func(ws string, line int, text string) {
		ls := strings.SplitAfter(e.read(ws, "cfg.txt"), "\n")
		ls[line-1] = text + "\n"
		e.write(ws, "cfg.txt", strings.Join(ls, ""))
	}
	edit("a", 2, "A changed 2")
	e.must("a", "snap", "-m", "a edits line 2")
	edit("b", 8, "B changed 8")
	e.must("b", "snap", "-m", "b edits line 8")
	edit("c", 2, "C changed 2")
	e.must("c", "snap", "-m", "c edits line 2")

	e.must("a", "merge", "b")
	if cs := e.contests("a"); len(cs) != 0 {
		t.Fatalf("far-apart edits were reported as a contest: %+v", cs)
	}
	got := e.read("a", "cfg.txt")
	if !strings.Contains(got, "A changed 2\n") || !strings.Contains(got, "B changed 8\n") || strings.Contains(got, "setting 2\n") || strings.Contains(got, "setting 8\n") {
		t.Fatalf("clean merge produced the wrong file:\n%s", got)
	}

	e.must("a", "merge", "c")
	cs := e.contests("a")
	if len(cs) != 1 || cs[0]["file"] != "cfg.txt" || cs[0]["from_line"] != 2.0 || cs[0]["to_line"] != 3.0 {
		t.Fatalf("want one contest at cfg.txt:2-3, got %+v", cs)
	}
	got = e.read("a", "cfg.txt")
	if !strings.Contains(got, "A changed 2\n") || !strings.Contains(got, "C changed 2\n") || strings.Contains(got, "<<<<") {
		t.Fatalf("contested file should hold both lines and no markers:\n%s", got)
	}

	e.write("a", "cfg.txt", strings.Replace(strings.Replace(got, "A changed 2\n", "", 1), "C changed 2\n", "agreed 2\n", 1))
	e.must("a", "snap", "-m", "settle line 2")
	if cs := e.contests("a"); len(cs) != 0 {
		t.Fatalf("contest should be settled, got %+v", cs)
	}
	e.must("b", "merge", "a")
	e.must("c", "merge", "b")
	if cs := e.contests("c"); len(cs) != 0 {
		t.Fatalf("settlement did not travel with the merge: %+v", cs)
	}
}

func TestContestClearedByUndo(t *testing.T) {
	e := newEnv(t)
	e.write("main", "f", lines(5, "l"))
	e.must("main", "snap", "-m", "base")
	e.must("main", "ws", "new", "a")
	e.must("main", "ws", "new", "b")
	e.write("a", "f", strings.Replace(e.read("a", "f"), "l 3\n", "from a\n", 1))
	e.must("a", "snap", "-m", "a")
	e.write("b", "f", strings.Replace(e.read("b", "f"), "l 3\n", "from b\n", 1))
	e.must("b", "snap", "-m", "b")
	e.must("main", "merge", "a")
	e.must("main", "merge", "b")
	cs := e.contests("main")
	if len(cs) != 1 {
		t.Fatalf("want 1 contest, got %+v", cs)
	}
	e.must("main", "undo", cs[0]["b"].(string))
	if cs2 := e.contests("main"); len(cs2) != 0 {
		t.Fatalf("undoing one side should clear the contest: %+v", cs2)
	}
	keep := cs[0]["agent_a"].(string)
	want := "l 1\nl 2\nfrom " + keep + "\nl 4\nl 5\n"
	if got := e.read("main", "f"); got != want {
		t.Fatalf("after undoing one side:\n%q\nwant\n%q", got, want)
	}
	e.must("main", "merge", cs[0]["agent_b"].(string))
	if got := e.read("main", "f"); got != want {
		t.Fatalf("merge resurrected an undone change:\n%q", got)
	}
}

// ===== blame, checks =====

func TestBlameFollowsLinesAcrossMerges(t *testing.T) {
	e := newEnv(t)
	e.write("main", "f", "a\nb\nc\n")
	e.must("main", "snap", "-m", "base", "--agent", "human")
	e.must("main", "ws", "new", "x")
	e.must("main", "ws", "new", "y")
	e.write("x", "f", "a\nfrom x\nb\nc\n")
	e.must("x", "snap", "-m", "x adds", "--agent", "agent-x", "--prompt", "add a line after a")
	e.write("y", "f", "a\nb\nc\nfrom y\n")
	e.must("y", "snap", "-m", "y adds", "--agent", "agent-y")
	e.must("main", "merge", "y")
	e.must("main", "merge", "x")
	var got []string
	for _, row := range e.js("main", "blame", "f").([]any) {
		m := row.(map[string]any)
		p, _ := m["prompt"].(string)
		got = append(got, fmt.Sprintf("%s|%s|%s", m["agent"], p, m["text"]))
	}
	want := []string{"human||a", "agent-x|add a line after a|from x", "human||b", "human||c", "agent-y||from y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blame:\n got %v\nwant %v", got, want)
	}
	why := e.must("main", "why", "f:2")
	if !strings.Contains(why, "agent-x") || !strings.Contains(why, "add a line after a") {
		t.Fatalf("why: %s", why)
	}
}

func TestRestoreLastGreen(t *testing.T) {
	e := newEnv(t)
	e.write("main", "ok", "yes\n")
	e.must("main", "snap", "-m", "good")
	e.must("main", "check", "--", "grep -q yes ok")
	good := e.tree("main")
	e.write("main", "ok", "no\n")
	e.write("main", "junk", "x\n")
	if err := e.cmd("main", "check", "--", "grep -q yes ok"); err == nil {
		t.Fatalf("a failing check must return an error")
	}
	e.write("main", "more", "late\n")
	e.must("main", "snap", "-m", "more broken work")
	broken := e.state("main")
	brokenTree := e.tree("main")
	e.must("main", "restore", "--green")
	if !reflect.DeepEqual(good, e.tree("main")) {
		t.Fatalf("restore --green did not return the passing tree: %v", e.tree("main"))
	}
	if st := e.must("main", "status"); !strings.Contains(st, "exit 0") {
		t.Fatalf("the restored state should carry the passing check: %s", st)
	}
	// The broken work is still there to return to: nothing was thrown away.
	e.must("main", "restore", broken)
	if !reflect.DeepEqual(brokenTree, e.tree("main")) {
		t.Fatalf("work recorded before the restore was lost: %v", e.tree("main"))
	}
}

// ===== scale =====

func TestManyChanges(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	e := newEnv(t)
	for i := 0; i < 40; i++ {
		e.write("main", fmt.Sprintf("src/f%02d.txt", i), lines(100, "line"))
	}
	e.must("main", "snap", "-m", "import")
	e.must("main", "ws", "new", "other")
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 300; i++ {
		ws := []string{"main", "other"}[i%2]
		f := fmt.Sprintf("src/f%02d.txt", rng.Intn(40))
		e.write(ws, f, randomEdit(rng, e.read(ws, f), fmt.Sprint("e", i)))
		e.must(ws, "snap", "-m", "edit")
	}
	e.must("main", "merge", "other")
	e.must("other", "merge", "main")
	if !reflect.DeepEqual(e.tree("main"), e.tree("other")) {
		t.Fatalf("two workspaces with the same 301 changes show different files")
	}
	if s := e.must("main", "snap"); !strings.Contains(s, "nothing changed") {
		t.Fatalf("files and state disagree: %s", s)
	}
}
