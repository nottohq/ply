package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var _ = rand.Int

type env struct {
	t    *testing.T
	root string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, root: t.TempDir()}
	e.must("main", "init")
	return e
}

func (e *env) dir(ws string) string {
	if ws == "main" {
		return e.root
	}
	return filepath.Join(e.root, ".ply", "ws", ws)
}

// out runs one ply command the way a separate process would and returns what it printed.
func (e *env) out(ws, name string, args ...string) (string, error) {
	f := parse(args)
	named := false
	for _, a := range args {
		named = named || a == "--agent"
	}
	if !named {
		f.info.Agent = ws
	}
	var buf bytes.Buffer
	err := run(e.dir(ws), name, f, &buf)
	return buf.String(), err
}

func (e *env) cmd(ws, name string, args ...string) error {
	_, err := e.out(ws, name, args...)
	return err
}

func (e *env) must(ws, name string, args ...string) string {
	e.t.Helper()
	s, err := e.out(ws, name, args...)
	if err != nil {
		e.t.Fatalf("ply %s %v in %s: %v", name, args, ws, err)
	}
	return s
}

// js runs a command with --json and decodes the result.
func (e *env) js(ws, name string, args ...string) any {
	e.t.Helper()
	s := e.must(ws, name, append([]string{"--json"}, args...)...)
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		e.t.Fatalf("ply %s --json printed %q", name, s)
	}
	return v
}

func (e *env) contests(ws string) []map[string]any {
	var out []map[string]any
	for _, c := range e.js(ws, "contests").([]any) {
		out = append(out, c.(map[string]any))
	}
	return out
}

// idOf finds a change by its intent.
func (e *env) idOf(ws, intent string) string {
	e.t.Helper()
	for _, c := range e.js(ws, "log", "-n", "100000").([]any) {
		if m := c.(map[string]any); m["intent"] == intent {
			return m["id"].(string)
		}
	}
	e.t.Fatalf("no change with intent %q in %s", intent, ws)
	return ""
}

func (e *env) state(ws string) string {
	return e.js(ws, "status").(map[string]any)["state"].(string)
}

func (e *env) write(ws, name, body string) {
	e.t.Helper()
	p := filepath.Join(e.dir(ws), name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(ws, name string) string {
	b, err := os.ReadFile(filepath.Join(e.dir(ws), name))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (e *env) rm(ws, name string) { os.Remove(filepath.Join(e.dir(ws), name)) }

func (e *env) mv(ws, from, to string) {
	e.t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(e.dir(ws), to)), 0o755)
	if err := os.Rename(filepath.Join(e.dir(ws), from), filepath.Join(e.dir(ws), to)); err != nil {
		e.t.Fatal(err)
	}
}

func readTree(dir string) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".ply" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, _ := os.ReadFile(p)
		info, _ := d.Info()
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = fmt.Sprintf("%v:%s", info.Mode()&0o111 != 0, b)
		return nil
	})
	return out
}

func (e *env) tree(ws string) map[string]string { return readTree(e.dir(ws)) }

func lines(n int, prefix string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

func randomEdit(rng *rand.Rand, body string, tag string) string {
	ls := strings.SplitAfter(body, "\n")
	if ls[len(ls)-1] == "" {
		ls = ls[:len(ls)-1]
	}
	for k := 1 + rng.Intn(3); k > 0; k-- {
		pos := rng.Intn(len(ls) + 1)
		switch rng.Intn(3) {
		case 0:
			ls = append(ls[:pos], append([]string{fmt.Sprintf("%s add %d\n", tag, rng.Intn(1000))}, ls[pos:]...)...)
		case 1:
			if len(ls) > 0 {
				p := pos % len(ls)
				ls = append(ls[:p], ls[p+1:]...)
			}
		default:
			if len(ls) > 0 {
				ls[pos%len(ls)] = fmt.Sprintf("%s edit %d\n", tag, rng.Intn(1000))
			}
		}
	}
	return strings.Join(ls, "")
}
