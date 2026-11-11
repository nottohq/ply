package main

// ply — version control shaped for agents.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const usage = `ply — version control shaped for agents

Record
  ply init                          start a repository here
  ply snap -m "why"                 record everything that changed as one change
  ply status                        state, unrecorded edits, open contests
  ply log [-n 30]                   recent changes, newest first
  ply blame <file>                  who wrote each line, and why
  ply why <file>:<line>             the change, intent and prompt behind one line

Work in parallel
  ply ws new <name> [--from <ws>]   a workspace for another agent
  ply ws list | ply ws rm <name>
  ply merge <ws> [--check "cmd"]    take another workspace's changes; the order of merges never matters.
                                    With --check, anything that breaks the command is held back.
  ply contests                      spots two agents changed without seeing each other

Go back
  ply undo <change> [--cascade]     switch one change off; later work stays
  ply redo <change>                 switch it back on
  ply check -- <command>            run a command and attach the result to this exact content
  ply restore <state> | --green     return to a state, or to the last one whose check passed
  ply culprit [--hold] -- <command> find which changes, alone or together, make the command fail

Share
  ply clone <path|url> [dir]        copy a repository
  ply remote add <name> <path|url>  name another repository
  ply push [remote] | ply pull [remote]
  ply serve [--addr :7171]          serve this repository over HTTP

Git
  ply git commit [-m msg]           commit the current files to git, crediting each agent
  ply git import [-n 200]           bring existing git history in as changes
  ply export <dir>                  write this history out as a new git repository

Agents
  ply mcp                           run as an MCP server (stdio)
  ply hook                          record edits from Claude Code hooks (reads the hook JSON on stdin)
  ply hook config                   print the settings.json block that wires the hooks up

Flags: --agent --session --prompt -m --json   Env: PLY_AGENT PLY_SESSION PLY_PROMPT
`

type flags struct {
	info    Info
	json    bool
	from    string
	check   string
	addr    string
	hold    bool
	cascade bool
	bare    bool
	n       int
	args    []string
}

func parse(argv []string) flags {
	f := flags{info: Info{Agent: os.Getenv("PLY_AGENT"), Session: os.Getenv("PLY_SESSION"), Prompt: os.Getenv("PLY_PROMPT")}, n: 30, addr: "127.0.0.1:7171"}
	for i := 0; i < len(argv); i++ {
		next := func() string {
			if i+1 < len(argv) {
				i++
				return argv[i]
			}
			return ""
		}
		switch argv[i] {
		case "--agent":
			f.info.Agent = next()
		case "--session":
			f.info.Session = next()
		case "--prompt":
			f.info.Prompt = next()
		case "-m", "--intent":
			f.info.Intent = next()
		case "--from":
			f.from = next()
		case "--check":
			f.check = next()
		case "--addr":
			f.addr = next()
		case "-n":
			fmt.Sscanf(next(), "%d", &f.n)
		case "--json":
			f.json = true
		case "--hold":
			f.hold = true
		case "--cascade":
			f.cascade = true
		case "--bare":
			f.bare = true
		case "--":
			f.args = append(f.args, argv[i+1:]...)
			i = len(argv)
		default:
			f.args = append(f.args, argv[i])
		}
	}
	if f.info.Agent == "" {
		f.info.Agent = os.Getenv("USER")
	}
	if f.info.Agent == "" {
		f.info.Agent = "unknown"
	}
	return f
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Print(usage)
		return
	}
	cwd, _ := os.Getwd()
	if err := run(cwd, os.Args[1], parse(os.Args[2:]), os.Stdout); err != nil {
		if code, ok := err.(exitCode); ok {
			os.Exit(int(code))
		}
		fmt.Fprintln(os.Stderr, "ply:", err)
		os.Exit(1)
	}
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit code %d", int(e)) }

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

type changeRow struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Agent   string `json:"agent"`
	Intent  string `json:"intent"`
	Prompt  string `json:"prompt,omitempty"`
	Time    string `json:"time"`
	Files   int    `json:"files"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	On      bool   `json:"on"`
}

func rowOf(m *Meta, v *View) changeRow {
	return changeRow{m.ID, m.Kind, m.Agent, m.Intent, m.Prompt, m.Time, len(m.Files), m.Ins, m.Del, v == nil || v.on(m.ID)}
}

// run executes one command. It returns its result as a value too, so the MCP
// server can call the same code.
func run(cwd, cmd string, f flags, out io.Writer) error {
	_, err := exec1(cwd, cmd, f, out)
	return err
}

func exec1(cwd, cmd string, f flags, out io.Writer) (result any, err error) {
	emit := func(v any, text func()) (any, error) {
		if f.json {
			b, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				return nil, err
			}
			fmt.Fprintln(out, string(b))
			return v, nil
		}
		text()
		return v, nil
	}
	arg := func(i int) string {
		if i < len(f.args) {
			return f.args[i]
		}
		return ""
	}
	if cmd == "init" {
		if err := initRepo(cwd); err != nil {
			return nil, err
		}
		if f.bare {
			r, err := openRepo(cwd)
			if err != nil {
				return nil, err
			}
			defer r.close()
			w, _ := r.loadWS("main")
			w.Bare = true
			if err := r.saveWS(w); err != nil {
				return nil, err
			}
			if err := r.commit(); err != nil {
				return nil, err
			}
		}
		excludeFromGit(cwd)
		return emit(map[string]any{"root": cwd}, func() { fmt.Fprintln(out, "ply repository ready in", cwd) })
	}
	if cmd == "mcp" {
		return nil, serveMCP(cwd, stdin, out)
	}
	if cmd == "hook" && len(f.args) > 0 && f.args[0] == "config" {
		fmt.Fprint(out, hookConfig)
		return nil, nil
	}
	if cmd == "clone" {
		if arg(0) == "" {
			return nil, fmt.Errorf("use: ply clone <path|url> [dir]")
		}
		dir := arg(1)
		if dir == "" {
			dir = strings.TrimSuffix(filepath.Base(strings.TrimRight(arg(0), "/")), ".ply")
		}
		dir = absFrom(cwd, dir)
		src := arg(0)
		if !strings.HasPrefix(src, "http") {
			src = absFrom(cwd, src)
		}
		if entries, _ := os.ReadDir(dir); len(entries) > 0 {
			return nil, fmt.Errorf("%s is not empty", dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := initRepo(dir); err != nil {
			return nil, err
		}
		if _, err := exec1(dir, "remote", flags{args: []string{"add", "origin", src}, info: f.info}, io.Discard); err != nil {
			return nil, err
		}
		f.args = nil
		return exec1(dir, "pull", f, out)
	}

	root, err := findRoot(cwd)
	if err != nil {
		return nil, err
	}
	if cmd == "serve" {
		return nil, serveHTTP(root, f.addr, out)
	}
	r, err := openRepo(root)
	if err != nil {
		return nil, err
	}
	defer r.close()
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	w, err := r.wsFor(cwd)
	if err != nil {
		return nil, err
	}
	done := func(v any, text func()) (any, error) {
		if err := r.commit(); err != nil {
			return nil, err
		}
		return emit(v, text)
	}

	switch cmd {
	case "snap":
		m, err := r.settle(w, f.info)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return done(map[string]any{"change": nil, "state": w.Root}, func() { fmt.Fprintln(out, "nothing changed") })
		}
		return done(map[string]any{"change": m.ID, "state": w.Root, "files": len(m.Files), "added": m.Ins, "removed": m.Del, "contests": w.contests()}, func() {
			fmt.Fprintf(out, "%s  +%d -%d in %d file(s)  %s\n", short(m.ID), m.Ins, m.Del, len(m.Files), m.Intent)
		})

	case "status":
		// Report unrecorded edits without recording them: scan inside a transaction that is thrown away.
		before := w.Root
		m, err := r.capture(w, f.info, map[string]bool{}, map[string]bool{})
		if err != nil {
			return nil, err
		}
		var pending []string
		if m != nil {
			v := r.view(w.Root, nil)
			for _, fid := range m.Files {
				_, p, _ := r.weave(fid).place(v)
				pending = append(pending, p)
			}
			sort.Strings(pending)
		}
		r.tx.Rollback()
		r.tx = nil
		if err := r.begin2(); err != nil {
			return nil, err
		}
		w, _ = r.wsFor(cwd)
		checks := r.checks(r.view(before, nil).key())
		cs := w.contests()
		behind := w.Root != w.Disk
		return emit(map[string]any{"workspace": w.Name, "state": w.Root, "changes": trieCount(r, w.Root), "pending": pending, "contests": cs, "checks": checks, "files_out_of_date": behind}, func() {
			fmt.Fprintf(out, "workspace %s  state %s  %d change(s)\n", w.Name, short(w.Root), trieCount(r, w.Root))
			if len(checks) > 0 {
				last := checks[len(checks)-1]
				fmt.Fprintf(out, "check: %s -> exit %d\n", last.Cmd, last.Exit)
			} else {
				fmt.Fprintln(out, "check: this state has not been checked")
			}
			if behind {
				fmt.Fprintln(out, "new changes have arrived; any ply command will bring the files up to date")
			}
			fmt.Fprintf(out, "%d file(s) with edits not yet recorded\n", len(pending))
			for _, p := range pending {
				fmt.Fprintln(out, "  ", p)
			}
			printContests(out, cs)
		})

	case "log":
		v := r.view(w.Root, nil)
		var rows []changeRow
		seen := map[string]bool{}
		trail := w.Trail
		for i := len(trail) - 1; i >= 0 && len(rows) < f.n; i-- {
			prev := ""
			if i > 0 {
				prev = trail[i-1]
			}
			var batch []changeRow
			trieDiff(r, trail[i], prev, func(e entry) {
				if m := r.meta(e.ID); m != nil && !seen[e.ID] && trieHas(r, w.Root, e.ID) {
					seen[e.ID] = true
					batch = append(batch, rowOf(m, v))
				}
			})
			sort.Slice(batch, func(a, b int) bool { return after(r.meta(batch[a].ID), 0, r.meta(batch[b].ID), 0) })
			rows = append(rows, batch...)
		}
		if len(rows) > f.n {
			rows = rows[:f.n]
		}
		return emit(rows, func() {
			for _, x := range rows {
				mark := " "
				if !x.On {
					mark = "x"
				}
				if x.Kind == "toggle" {
					mark = "~"
				}
				fmt.Fprintf(out, "%s %s  %-12.12s +%-4d -%-4d %s\n", mark, short(x.ID), x.Agent, x.Added, x.Removed, x.Intent)
			}
		})

	case "blame", "why":
		file, want := arg(0), 0
		if cmd == "why" {
			i := strings.LastIndex(file, ":")
			if i < 0 {
				return nil, fmt.Errorf("use: ply why <file>:<line>")
			}
			fmt.Sscanf(file[i+1:], "%d", &want)
			file = file[:i]
		}
		if _, err := r.settle(w, f.info); err != nil {
			return nil, err
		}
		rel := file
		if abs, err := filepath.Abs(filepath.Join(cwd, file)); err == nil {
			if rr, err := filepath.Rel(r.dir(w), abs); err == nil && !strings.HasPrefix(rr, "..") {
				rel = filepath.ToSlash(rr)
			}
		}
		e := r.getIdx(w, rel)
		if e == nil {
			return nil, fmt.Errorf("no such file in this workspace: %s", file)
		}
		type row struct {
			Line   int    `json:"line"`
			Change string `json:"change"`
			Agent  string `json:"agent"`
			Intent string `json:"intent"`
			Prompt string `json:"prompt,omitempty"`
			Time   string `json:"time"`
			Text   string `json:"text"`
		}
		var rows []row
		for i, el := range r.weave(e.Fid).view(r.view(w.Root, nil)) {
			if want == 0 || want == i+1 {
				rows = append(rows, row{i + 1, el.ch.ID, el.ch.Agent, el.ch.Intent, el.ch.Prompt, el.ch.Time, strings.TrimRight(el.text, "\n")})
			}
		}
		if cmd == "why" && len(rows) == 0 {
			return nil, fmt.Errorf("%s has no line %d", file, want)
		}
		return done(rows, func() {
			for _, x := range rows {
				if cmd == "why" {
					fmt.Fprintf(out, "change %s by %s at %s\nintent: %s\nprompt: %s\nline:   %s\n", short(x.Change), x.Agent, x.Time, x.Intent, x.Prompt, x.Text)
				} else {
					fmt.Fprintf(out, "%4d  %s  %-10.10s %-28.28s | %s\n", x.Line, short(x.Change), x.Agent, x.Intent, x.Text)
				}
			}
		})

	case "ws":
		switch arg(0) {
		case "new":
			name := arg(1)
			if name == "" || name == "main" || strings.ContainsAny(name, "/\\.: ") || strings.HasPrefix(name, "tmp-") {
				return nil, fmt.Errorf("give the workspace a simple name")
			}
			if _, err := r.loadWS(name); err == nil {
				return nil, fmt.Errorf("workspace %q already exists", name)
			}
			src := w
			if f.from != "" {
				if src, err = r.loadWS(f.from); err != nil {
					return nil, err
				}
			}
			if _, err := r.settle(src, f.info); err != nil {
				return nil, err
			}
			nw, err := r.newWS(src, name, "")
			if err != nil {
				return nil, err
			}
			if err := r.saveWS(nw); err != nil {
				return nil, err
			}
			return done(map[string]any{"workspace": name, "dir": r.dir(nw), "state": nw.Root}, func() { fmt.Fprintln(out, r.dir(nw)) })
		case "rm":
			x, err := r.loadWS(arg(1))
			if err != nil {
				return nil, err
			}
			if x.Name == "main" {
				return nil, fmt.Errorf("the main workspace cannot be removed")
			}
			if _, err := r.settle(x, f.info); err != nil {
				return nil, err
			}
			r.dropWS(x)
			return done(map[string]any{"removed": x.Name}, func() {
				fmt.Fprintf(out, "removed workspace %s (its changes stay in the repository)\n", x.Name)
			})
		default:
			var rows []map[string]any
			c := r.bucket("w").Cursor()
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				if x, err := r.loadWS(string(k)); err == nil && !strings.HasPrefix(x.Name, "tmp-") {
					rows = append(rows, map[string]any{"workspace": x.Name, "dir": r.dir(x), "state": x.Root, "changes": trieCount(r, x.Root), "bare": x.Bare})
				}
			}
			return emit(rows, func() {
				for _, x := range rows {
					fmt.Fprintf(out, "%-14s %s  %v change(s)  %s\n", x["workspace"], short(x["state"].(string)), x["changes"], x["dir"])
				}
			})
		}

	case "merge":
		other, err := r.loadWS(arg(0))
		if err != nil {
			return nil, err
		}
		if other.Name == w.Name {
			return nil, fmt.Errorf("that is this workspace")
		}
		if _, err := r.settle(other, Info{Agent: other.Name, Intent: "(unlabelled edit, recorded at merge)"}); err != nil {
			return nil, err
		}
		before := w.Root
		if _, err := r.settle(w, f.info); err != nil {
			return nil, err
		}
		before = w.Root
		added, err := r.merge(w, other.Root, f.info)
		if err != nil {
			return nil, err
		}
		res := map[string]any{"took": added, "state": w.Root}
		var held *CulpritResult
		if f.check != "" && added > 0 {
			c, err := r.check(w, f.check, f.info)
			if err != nil {
				return nil, err
			}
			res["check"] = c
			if c.Exit != 0 {
				if held, err = r.culprit(w, f.check, before, true, f.info); err != nil {
					return nil, err
				}
				res["held_back"] = held
				res["state"] = w.Root
			}
		}
		res["contests"] = w.contests()
		return done(res, func() {
			fmt.Fprintf(out, "took %d change(s) from %s  state %s\n", added, other.Name, short(w.Root))
			if held != nil {
				printCulprit(out, r, held)
			}
			printContests(out, w.contests())
		})

	case "contests":
		if _, err := r.settle(w, f.info); err != nil {
			return nil, err
		}
		cs := w.contests()
		return done(cs, func() { printContests(out, cs) })

	case "undo":
		id, deps, err := r.undo(w, arg(0), f.cascade, f.info)
		if err != nil {
			return nil, err
		}
		return done(map[string]any{"undone": id, "state": w.Root, "built_on_it": deps}, func() {
			fmt.Fprintf(out, "switched off %s (%s)  state %s\n", short(id), r.meta(id).Intent, short(w.Root))
			for _, d := range deps {
				fmt.Fprintf(out, "  review: %s (%s) edited lines it wrote; --cascade switches those off too\n", short(d), r.meta(d).Intent)
			}
		})

	case "redo":
		id, err := r.redo(w, arg(0), f.info)
		if err != nil {
			return nil, err
		}
		return done(map[string]any{"redone": id, "state": w.Root}, func() { fmt.Fprintf(out, "switched on %s  state %s\n", short(id), short(w.Root)) })

	case "check":
		if len(f.args) == 0 {
			return nil, fmt.Errorf("use: ply check -- <command>")
		}
		res, err := r.check(w, strings.Join(f.args, " "), f.info)
		if err != nil {
			return nil, err
		}
		v, err := done(map[string]any{"state": w.Root, "check": res}, func() {
			fmt.Fprintf(out, "state %s: exit %d in %dms\n", short(w.Root), res.Exit, res.Ms)
		})
		if err == nil && res.Exit != 0 {
			return v, exitCode(res.Exit)
		}
		return v, err

	case "restore":
		target := arg(0)
		if target == "--green" || target == "" {
			if target = r.lastGreen(w, ""); target == "" {
				return nil, fmt.Errorf("no state of this workspace has a passing check")
			}
		} else {
			hit := map[string]bool{}
			for _, s := range w.Trail {
				if strings.HasPrefix(s, target) {
					hit[s] = true
				}
			}
			if len(hit) != 1 {
				return nil, fmt.Errorf("%q does not name one state this workspace has been in", target)
			}
			for s := range hit {
				target = s
			}
		}
		if err := r.restore(w, target, f.info); err != nil {
			return nil, err
		}
		return done(map[string]any{"state": w.Root, "content_of": target}, func() {
			fmt.Fprintf(out, "now at state %s, with the content of %s\n", short(w.Root), short(target))
		})

	case "culprit":
		if len(f.args) == 0 {
			return nil, fmt.Errorf("use: ply culprit [--hold] -- <command>")
		}
		res, err := r.culprit(w, strings.Join(f.args, " "), "", f.hold, f.info)
		if err != nil {
			return nil, err
		}
		return done(res, func() { printCulprit(out, r, res) })
	}
	return more(r, w, cwd, cmd, f, out, done, emit)
}

// begin2 reopens a transaction on the already-open database.
func (r *Repo) begin2() error {
	tx, err := r.db.Begin(true)
	if err != nil {
		return err
	}
	r.tx = tx
	r.nodes, r.metas, r.weaves = map[string]*node{}, map[string]*Meta{}, map[string]*fileWeave{}
	return nil
}

func printContests(out io.Writer, cs []Contest) {
	if len(cs) == 0 {
		fmt.Fprintln(out, "no contests")
		return
	}
	for _, c := range cs {
		if c.Kind == "path" {
			fmt.Fprintf(out, "contest: two files want the path %s\n", c.File)
		} else {
			fmt.Fprintf(out, "contest %s:%d-%d\n", c.File, c.From, c.To)
		}
		fmt.Fprintf(out, "  %s %s: %s\n  %s %s: %s\n", short(c.A), c.AgentA, c.IntentA, short(c.B), c.AgentB, c.IntentB)
	}
}

func printCulprit(out io.Writer, r *Repo, res *CulpritResult) {
	if res.Note != "" {
		fmt.Fprintln(out, res.Note)
		return
	}
	fmt.Fprintf(out, "`%s` fails because of %d change(s), found among %d in %d run(s):\n", res.Cmd, len(res.Culprits), res.Among, res.Runs)
	for _, id := range res.Culprits {
		m := r.meta(id)
		fmt.Fprintf(out, "  %s  %-12.12s %s\n", short(id), m.Agent, m.Intent)
	}
	if len(res.Culprits) > 1 {
		fmt.Fprintln(out, "  (these fail together; each smaller combination passes)")
	}
	if res.Held != "" {
		fmt.Fprintf(out, "held back by %s; `ply redo <change>` brings one back\n", short(res.Held))
	}
}

func absFrom(cwd, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}
