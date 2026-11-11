package main

// Living next to git: commit ply's state into git, bring git history in, and
// write a ply history out as a git repository.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func git(dir string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	b, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(b)))
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// excludeFromGit keeps ply's own directory out of git without touching .gitignore.
func excludeFromGit(dir string) {
	info := filepath.Join(dir, ".git", "info")
	if st, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !st.IsDir() {
		return
	}
	p := filepath.Join(info, "exclude")
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), ".ply/") {
		return
	}
	os.MkdirAll(info, 0o755)
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		b = append(b, '\n')
	}
	os.WriteFile(p, append(b, []byte(".ply/\n")...), 0o644)
}

// gitCommit commits the workspace's files to git, crediting every agent whose
// change is new since the last such commit.
func (r *Repo) gitCommit(w *Workspace, msg string, in Info) (string, int, error) {
	if w.Name != "main" {
		return "", 0, fmt.Errorf("commit to git from the main workspace (merge this one into it first)")
	}
	if _, err := git(r.root, "rev-parse", "--git-dir"); err != nil {
		return "", 0, fmt.Errorf("%s is not a git repository", r.root)
	}
	if _, err := r.settle(w, in); err != nil {
		return "", 0, err
	}
	excludeFromGit(r.root)
	var mark struct{ Root string }
	r.getJSON("s", "git-mark", &mark)
	v := r.view(w.Root, nil)
	var news []*Meta
	trieDiff(r, w.Root, mark.Root, func(e entry) {
		if m := r.meta(e.ID); m != nil && m.Kind == "edit" && v.on(m.ID) {
			news = append(news, m)
		}
	})
	sort.Slice(news, func(i, j int) bool { return after(news[j], 0, news[i], 0) })
	if _, err := git(r.root, "add", "-A"); err != nil {
		return "", 0, err
	}
	if out, _ := git(r.root, "status", "--porcelain"); out == "" {
		return "", 0, fmt.Errorf("git has nothing to commit")
	}
	var b strings.Builder
	if msg == "" {
		msg = fmt.Sprintf("%d change(s) recorded in ply", len(news))
		if len(news) == 1 {
			msg = news[0].Intent
		}
	}
	b.WriteString(msg + "\n\n")
	agents := map[string]bool{}
	for i, m := range news {
		if i < 50 {
			fmt.Fprintf(&b, "- %s (%s, ply %s)\n", m.Intent, m.Agent, short(m.ID))
		}
		agents[m.Agent] = true
	}
	if len(news) > 50 {
		fmt.Fprintf(&b, "- and %d more\n", len(news)-50)
	}
	fmt.Fprintf(&b, "\nPly-State: %s\n", w.Root)
	names := make([]string, 0, len(agents))
	for a := range agents {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		fmt.Fprintf(&b, "Co-authored-by: %s <%s@ply.local>\n", a, strings.Map(func(c rune) rune {
			if c == ' ' || c == '<' || c == '>' {
				return '-'
			}
			return c
		}, a))
	}
	if _, err := git(r.root, "commit", "-q", "-m", b.String()); err != nil {
		return "", 0, err
	}
	sha, _ := git(r.root, "rev-parse", "--short", "HEAD")
	mark.Root = w.Root
	return sha, len(news), r.putJSON("s", "git-mark", mark)
}

// gitImport turns the last n commits on the current branch into changes, so
// blame and undo reach back into history that predates ply.
func (r *Repo) gitImport(w *Workspace, n int, out io.Writer) (int, error) {
	if w.Name != "main" || w.Root != "" {
		return 0, fmt.Errorf("import git history into a fresh repository, before anything else is recorded")
	}
	list, err := git(r.root, "rev-list", "--first-parent", "--reverse", "HEAD")
	if err != nil {
		return 0, err
	}
	commits := strings.Fields(list)
	if n > 0 && len(commits) > n {
		commits = commits[len(commits)-n:]
	}
	if len(commits) == 0 {
		return 0, fmt.Errorf("this git repository has no commits")
	}
	tmp := filepath.Join(r.root, ".ply", "tmp", "git-import")
	os.RemoveAll(tmp)
	git(r.root, "worktree", "prune")
	if _, err := git(r.root, "worktree", "add", "-f", "--detach", tmp, commits[0]); err != nil {
		return 0, err
	}
	defer func() {
		git(r.root, "worktree", "remove", "--force", tmp)
		os.RemoveAll(tmp)
	}()
	imp := &Workspace{Name: "tmp-git-import", Dir: tmp}
	for _, b := range []string{"x:", "y:", "z:"} {
		r.tx.DeleteBucket([]byte(b + imp.Name))
	}
	done := 0
	for i, c := range commits {
		if i > 0 {
			if _, err := git(tmp, "checkout", "-q", "--detach", c); err != nil {
				return done, err
			}
		}
		meta, err := git(r.root, "show", "-s", "--format=%an%x00%s%x00%aI", c)
		if err != nil {
			return done, err
		}
		f := strings.SplitN(meta, "\x00", 3)
		imp.LastScan = 1 << 62 // git rewrites files faster than timestamps tick: always compare content
		m, err := r.settle(imp, Info{Agent: f[0], Intent: f[1], Time: f[2], Session: "git:" + c[:12]})
		if err != nil {
			return done, err
		}
		if m != nil {
			done++
		}
	}
	// Hand the history to main. Its files are whatever is checked out, edits and
	// all, so every file is compared by content on the next scan.
	type row struct {
		k string
		e idx
	}
	var rows []row
	c := r.bucket("x:" + imp.Name).Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var e idx
		if json.Unmarshal(v, &e) == nil {
			rows = append(rows, row{string(k), e})
		}
	}
	for _, rw := range rows {
		rw.e.Mtime = 0
		r.putJSON("x:"+w.Name, rw.k, rw.e)
		r.bucket("y:"+w.Name).Put([]byte(rw.e.Fid), []byte(rw.k))
		r.setWant(w, rw.e.Fid, "", rw.e.Want)
	}
	w.Root, w.Disk, w.Trail = imp.Root, imp.Root, imp.Trail
	for _, b := range []string{"x:", "y:", "z:"} {
		r.tx.DeleteBucket([]byte(b + imp.Name))
	}
	r.bucket("w").Delete([]byte(imp.Name))
	r.putJSON("s", "git-mark", map[string]string{"Root": w.Root})
	if _, err := r.settle(w, Info{Agent: "ply", Intent: "work not yet committed to git at import"}); err != nil {
		return done, err
	}
	return done, nil
}

// exportGit replays the changes that are on, oldest first, as one commit each.
func (r *Repo) exportGit(w *Workspace, dir string, in Info) (int, error) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return 0, fmt.Errorf("%s is not empty", dir)
	}
	if _, err := r.settle(w, in); err != nil {
		return 0, err
	}
	abs, _ := filepath.Abs(dir)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return 0, err
	}
	if _, err := git(abs, "init", "-q"); err != nil {
		return 0, err
	}
	v := r.view(w.Root, nil)
	var edits []*Meta
	trieEach(r, w.Root, func(e entry) {
		if m := r.meta(e.ID); m != nil && m.Kind == "edit" && v.on(m.ID) {
			edits = append(edits, m)
		}
	})
	sort.Slice(edits, func(i, j int) bool { return after(edits[j], 0, edits[i], 0) })
	sc := &Workspace{Name: "tmp-export", Dir: abs, Root: w.Root}
	for _, b := range []string{"x:", "y:", "z:"} {
		r.tx.DeleteBucket([]byte(b + sc.Name))
	}
	defer func() {
		for _, b := range []string{"x:", "y:", "z:"} {
			r.tx.DeleteBucket([]byte(b + sc.Name))
		}
	}()
	for i, m := range edits {
		off := make([]string, 0, len(edits)-i-1)
		for _, later := range edits[i+1:] {
			off = append(off, later.ID)
		}
		sc.Trial = off
		if err := r.checkout(sc, map[string]bool{}, map[string]bool{}); err != nil {
			return i, err
		}
		if _, err := git(abs, "add", "-A"); err != nil {
			return i, err
		}
		msg := fmt.Sprintf("%s\n\nPly-Change: %s\nPly-Agent: %s", m.Intent, m.ID, m.Agent)
		if m.Prompt != "" {
			msg += "\nPly-Prompt: " + strings.ReplaceAll(m.Prompt, "\n", " ")
		}
		if _, err := git(abs, "-c", "user.name="+m.Agent, "-c", "user.email="+m.Agent+"@ply.local", "commit", "-q", "--allow-empty", "--date", m.Time, "-m", msg); err != nil {
			return i, err
		}
	}
	return len(edits), nil
}
