package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The matcher must agree with git itself on a tree of tricky cases.
func TestIgnoreAgreesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write(".gitignore", "node_modules/\n*.log\n!keep.log\n/build\ndocs/**/draft.md\n**/tmp/\nsecret?.txt\n[ab]x.c\n# comment\n\nout/*.bin\n/top-only.txt\ncache\n")
	write("sub/.gitignore", "*.gen\n!important.gen\n/local.txt\n")
	files := []string{"a.txt", "x.log", "keep.log", "sub/y.log", "sub/keep.log", "node_modules/p/i.js", "sub/node_modules/q.js",
		"build/o.o", "sub/build/o.o", "docs/draft.md", "docs/a/b/draft.md", "docs/a/final.md", "tmp/z", "a/tmp/z", "a/tmpfile",
		"secret1.txt", "secret12.txt", "ax.c", "bx.c", "cx.c", "out/a.bin", "out/d/a.bin", "top-only.txt", "sub/top-only.txt",
		"sub/a.gen", "sub/important.gen", "sub/deep/b.gen", "a.gen", "sub/local.txt", "sub/deep/local.txt", "cache/f", "sub/cache", "x/cache/y/z"}
	for _, f := range files {
		write(f, "x")
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%s", out)
	}
	out, err := exec.Command("git", "-C", dir, "ls-files", "-co", "--exclude-standard").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		want[l] = true
	}
	ig := loadIgnore(dir)
	got := map[string]bool{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != dir && ig.skip(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !ig.skip(rel, false) {
			got[rel] = true
		}
		return nil
	})
	for f := range want {
		if !got[f] {
			t.Errorf("git keeps %s, ply ignores it", f)
		}
	}
	for f := range got {
		if !want[f] {
			t.Errorf("git ignores %s, ply keeps it", f)
		}
	}
}
