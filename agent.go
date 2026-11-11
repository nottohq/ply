package main

// The two ways an agent harness talks to ply without a human typing commands:
// an MCP server, and Claude Code hooks that record every edit automatically.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var stdin io.Reader = os.Stdin

// ===== remaining commands =====

func more(r *Repo, w *Workspace, cwd, cmd string, f flags, out io.Writer, done, emit func(any, func()) (any, error)) (any, error) {
	arg := func(i int) string {
		if i < len(f.args) {
			return f.args[i]
		}
		return ""
	}
	target := func(name string) (string, error) {
		if name == "" {
			name = "origin"
		}
		var t string
		if !r.getJSON("r", name, &t) {
			if strings.ContainsAny(name, "/.:") {
				return name, nil
			}
			return "", fmt.Errorf("no remote named %q (add one with `ply remote add %s <path|url>`)", name, name)
		}
		return t, nil
	}
	ref := f.from
	if ref == "" {
		ref = "main"
	}
	switch cmd {
	case "remote":
		if arg(0) == "add" && arg(1) != "" && arg(2) != "" {
			t := arg(2)
			if !strings.HasPrefix(t, "http") {
				t = absFrom(cwd, t)
			}
			if err := r.putJSON("r", arg(1), t); err != nil {
				return nil, err
			}
			return done(map[string]string{arg(1): t}, func() { fmt.Fprintf(out, "%s -> %s\n", arg(1), t) })
		}
		rows := map[string]string{}
		c := r.bucket("r").Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var t string
			json.Unmarshal(v, &t)
			rows[string(k)] = t
		}
		return emit(rows, func() {
			for k, v := range rows {
				fmt.Fprintf(out, "%s -> %s\n", k, v)
			}
		})

	case "pull", "push":
		t, err := target(arg(0))
		if err != nil {
			return nil, err
		}
		if abs, _ := filepath.Abs(t); abs == r.root {
			return nil, fmt.Errorf("that remote is this repository")
		}
		p, err := openPeer(t)
		if err != nil {
			return nil, err
		}
		defer p.Close()
		var n int
		if cmd == "pull" {
			n, err = r.pull(w, p, ref, f.info)
		} else {
			n, err = r.push(w, p, ref, f.info)
		}
		if err != nil {
			return nil, err
		}
		verb := map[string]string{"pull": "took", "push": "sent"}[cmd]
		return done(map[string]any{verb: n, "state": w.Root, "contests": w.contests()}, func() {
			fmt.Fprintf(out, "%s %d change(s); state %s\n", verb, n, short(w.Root))
			if cmd == "pull" {
				printContests(out, w.contests())
			}
		})

	case "git":
		switch arg(0) {
		case "commit":
			sha, n, err := r.gitCommit(w, f.info.Intent, f.info)
			if err != nil {
				return nil, err
			}
			return done(map[string]any{"commit": sha, "changes": n}, func() { fmt.Fprintf(out, "git commit %s, crediting %d change(s)\n", sha, n) })
		case "import":
			n := f.n
			if n == 30 {
				n = 200
			}
			got, err := r.gitImport(w, n, out)
			if err != nil {
				return nil, err
			}
			return done(map[string]any{"imported": got, "state": w.Root}, func() { fmt.Fprintf(out, "brought in %d commit(s) as changes\n", got) })
		}
		return nil, fmt.Errorf("use: ply git commit | ply git import")

	case "export":
		if arg(0) == "" {
			return nil, fmt.Errorf("use: ply export <dir>")
		}
		n, err := r.exportGit(w, arg(0), f.info)
		if err != nil {
			return nil, err
		}
		return done(map[string]any{"commits": n}, func() { fmt.Fprintf(out, "wrote %d commit(s) to %s\n", n, arg(0)) })

	case "hook":
		return nil, r.hook(w, stdin, f, done)
	}
	return nil, fmt.Errorf("unknown command %q (try `ply help`)", cmd)
}

// ===== Claude Code hooks =====

const hookConfig = `{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "ply hook" } ] }
    ],
    "PostToolUse": [
      { "matcher": "Edit|Write|MultiEdit|NotebookEdit|Bash", "hooks": [ { "type": "command", "command": "ply hook" } ] }
    ],
    "Stop": [
      { "hooks": [ { "type": "command", "command": "ply hook" } ] }
    ]
  }
}
`

// hook records an edit the moment a tool makes it, with the prompt that led to
// it. It prints nothing: on some events a hook's output is fed back to the model.
func (r *Repo) hook(w *Workspace, in io.Reader, f flags, done func(any, func()) (any, error)) error {
	var ev struct {
		Event     string `json:"hook_event_name"`
		Session   string `json:"session_id"`
		Prompt    string `json:"prompt"`
		Tool      string `json:"tool_name"`
		AgentType string `json:"agent_type"`
		Input     struct {
			FilePath string `json:"file_path"`
			Command  string `json:"command"`
		} `json:"tool_input"`
	}
	b, _ := io.ReadAll(in)
	if json.Unmarshal(b, &ev) != nil || ev.Event == "" {
		return fmt.Errorf("ply hook expects the hook's JSON on standard input")
	}
	key := "prompt:" + ev.Session
	if ev.Event == "UserPromptSubmit" {
		if err := r.putJSON("s", key, ev.Prompt); err != nil {
			return err
		}
		_, err := done(nil, func() {})
		return err
	}
	info := f.info
	if os.Getenv("PLY_AGENT") == "" {
		info.Agent = "claude"
		if ev.AgentType != "" {
			info.Agent = "claude:" + ev.AgentType
		}
	}
	info.Session = ev.Session
	r.getJSON("s", key, &info.Prompt)
	switch {
	case ev.Input.FilePath != "":
		info.Intent = ev.Tool + " " + filepath.Base(ev.Input.FilePath)
	case ev.Input.Command != "":
		c := strings.Join(strings.Fields(ev.Input.Command), " ")
		if len(c) > 72 {
			c = c[:72] + "…"
		}
		info.Intent = "Bash: " + c
	default:
		info.Intent = "edits at end of turn"
	}
	if _, err := r.settle(w, info); err != nil {
		return err
	}
	_, err := done(nil, func() {})
	return err
}

// ===== MCP server (stdio) =====

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	cmd         string
	build       func(a map[string]any, f *flags)
}

func str(a map[string]any, k string) string { s, _ := a[k].(string); return s }

func schema(required []string, props map[string]string) map[string]any {
	p := map[string]any{}
	for k, d := range props {
		t := "string"
		if strings.HasPrefix(d, "bool:") {
			t, d = "boolean", d[5:]
		} else if strings.HasPrefix(d, "int:") {
			t, d = "integer", d[4:]
		}
		p[k] = map[string]any{"type": t, "description": d}
	}
	s := map[string]any{"type": "object", "properties": p}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func tools() []tool {
	return []tool{
		{Name: "ply_snap", cmd: "snap", Description: "Record everything that changed on disk as one change. Call after each coherent edit; say why in intent.",
			InputSchema: schema([]string{"intent"}, map[string]string{"intent": "Why this change was made, in a few words", "prompt": "The user request that led to it"}),
			build:       func(a map[string]any, f *flags) { f.info.Intent, f.info.Prompt = str(a, "intent"), str(a, "prompt") }},
		{Name: "ply_status", cmd: "status", Description: "State of this workspace: unrecorded edits, last check, open contests.", InputSchema: schema(nil, nil)},
		{Name: "ply_log", cmd: "log", Description: "Recent changes, newest first, with agent and intent.",
			InputSchema: schema(nil, map[string]string{"n": "int:How many (default 30)"}),
			build: func(a map[string]any, f *flags) {
				if n, ok := a["n"].(float64); ok && n > 0 {
					f.n = int(n)
				}
			}},
		{Name: "ply_why", cmd: "why", Description: "Who wrote one line, in which change, with what intent and prompt.",
			InputSchema: schema([]string{"file", "line"}, map[string]string{"file": "Path in the workspace", "line": "int:Line number, from 1"}),
			build: func(a map[string]any, f *flags) {
				n, _ := a["line"].(float64)
				f.args = []string{fmt.Sprintf("%s:%d", str(a, "file"), int(n))}
			}},
		{Name: "ply_blame", cmd: "blame", Description: "Agent, intent and prompt for every line of a file.",
			InputSchema: schema([]string{"file"}, map[string]string{"file": "Path in the workspace"}),
			build:       func(a map[string]any, f *flags) { f.args = []string{str(a, "file")} }},
		{Name: "ply_workspace_new", cmd: "ws", Description: "Create a parallel workspace (its own directory) for another agent. Returns the directory.",
			InputSchema: schema([]string{"name"}, map[string]string{"name": "Simple name", "from": "Workspace to start from (default: this one)"}),
			build:       func(a map[string]any, f *flags) { f.args, f.from = []string{"new", str(a, "name")}, str(a, "from") }},
		{Name: "ply_merge", cmd: "merge", Description: "Take another workspace's changes. Merge order never matters. With check, any change that makes the command fail is found and held back, so the workspace stays passing.",
			InputSchema: schema([]string{"workspace"}, map[string]string{"workspace": "Workspace to take changes from", "check": "Shell command that must keep passing, e.g. the test command"}),
			build:       func(a map[string]any, f *flags) { f.args, f.check = []string{str(a, "workspace")}, str(a, "check") }},
		{Name: "ply_contests", cmd: "contests", Description: "Spots where two agents changed the same lines or path without seeing each other. Both versions are in the file; edit the spot to settle it.", InputSchema: schema(nil, nil)},
		{Name: "ply_undo", cmd: "undo", Description: "Switch one earlier change off while keeping everything done after it.",
			InputSchema: schema([]string{"change"}, map[string]string{"change": "Change id or prefix", "cascade": "bool:Also switch off changes that edited its lines", "reason": "Why"}),
			build: func(a map[string]any, f *flags) {
				f.args, f.info.Intent = []string{str(a, "change")}, str(a, "reason")
				f.cascade, _ = a["cascade"].(bool)
			}},
		{Name: "ply_redo", cmd: "redo", Description: "Switch a change back on.",
			InputSchema: schema([]string{"change"}, map[string]string{"change": "Change id or prefix"}),
			build:       func(a map[string]any, f *flags) { f.args = []string{str(a, "change")} }},
		{Name: "ply_check", cmd: "check", Description: "Run a command (tests, build) and attach the result to this exact content.",
			InputSchema: schema([]string{"command"}, map[string]string{"command": "Shell command"}),
			build:       func(a map[string]any, f *flags) { f.args = []string{str(a, "command")} }},
		{Name: "ply_restore", cmd: "restore", Description: "Return to an earlier state, or to the last state whose check passed. Nothing is discarded.",
			InputSchema: schema(nil, map[string]string{"state": "State id; leave out for the last passing state"}),
			build: func(a map[string]any, f *flags) {
				if s := str(a, "state"); s != "" {
					f.args = []string{s}
				}
			}},
		{Name: "ply_culprit", cmd: "culprit", Description: "Find which change, or which combination of changes, makes a command fail. Works across merged parallel work, not only along a line of history.",
			InputSchema: schema([]string{"command"}, map[string]string{"command": "Shell command that currently fails", "hold": "bool:Hold the culprits back afterwards"}),
			build: func(a map[string]any, f *flags) {
				f.args = []string{str(a, "command")}
				f.hold, _ = a["hold"].(bool)
			}},
		{Name: "ply_pull", cmd: "pull", Description: "Take changes from a remote repository.",
			InputSchema: schema(nil, map[string]string{"remote": "Remote name, path or URL (default origin)"}),
			build: func(a map[string]any, f *flags) {
				if s := str(a, "remote"); s != "" {
					f.args = []string{s}
				}
			}},
		{Name: "ply_push", cmd: "push", Description: "Send this workspace's changes to a remote repository. A push is never rejected: both sides' work is combined.",
			InputSchema: schema(nil, map[string]string{"remote": "Remote name, path or URL (default origin)"}),
			build: func(a map[string]any, f *flags) {
				if s := str(a, "remote"); s != "" {
					f.args = []string{s}
				}
			}},
	}
}

func serveMCP(cwd string, in io.Reader, out io.Writer) error {
	all := tools()
	agent := os.Getenv("PLY_AGENT")
	enc := json.NewEncoder(out) // one JSON message per line, nothing else on stdout
	reply := func(id json.RawMessage, result any, code int, msg string) {
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if msg != "" {
			m["error"] = map[string]any{"code": code, "message": msg}
		} else {
			m["result"] = result
		}
		enc.Encode(m)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string         `json:"protocolVersion"`
				Name            string         `json:"name"`
				Arguments       map[string]any `json:"arguments"`
				ClientInfo      struct {
					Name string `json:"name"`
				} `json:"clientInfo"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			reply(nil, nil, -32700, "parse error")
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification: nothing to answer
		}
		switch req.Method {
		case "initialize":
			version := req.Params.ProtocolVersion
			if version != "2025-06-18" && version != "2025-03-26" && version != "2024-11-05" {
				version = "2025-06-18"
			}
			if agent == "" && req.Params.ClientInfo.Name != "" {
				agent = req.Params.ClientInfo.Name
			}
			reply(req.ID, map[string]any{
				"protocolVersion": version,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "ply", "version": "0.2.0"},
				"instructions":    "ply is version control for agents. Call ply_snap after each coherent edit. Use ply_merge with a check to combine parallel work safely, ply_contests to see collisions, ply_culprit when a check fails.",
			}, 0, "")
		case "ping":
			reply(req.ID, map[string]any{}, 0, "")
		case "tools/list":
			reply(req.ID, map[string]any{"tools": all}, 0, "")
		case "tools/call":
			var t *tool
			for i := range all {
				if all[i].Name == req.Params.Name {
					t = &all[i]
				}
			}
			if t == nil {
				reply(req.ID, nil, -32602, "Unknown tool: "+req.Params.Name)
				continue
			}
			f := parse(nil)
			f.json = true
			if agent != "" {
				f.info.Agent = agent
			}
			if t.build != nil {
				t.build(req.Params.Arguments, &f)
			}
			dir := cwd
			if d := str(req.Params.Arguments, "dir"); d != "" {
				dir = d
			}
			var buf bytes.Buffer
			_, err := exec1(dir, t.cmd, f, &buf)
			text := strings.TrimSpace(buf.String())
			_, failedCheck := err.(exitCode)
			if err != nil && !failedCheck {
				reply(req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, 0, "")
				continue
			}
			reply(req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}, 0, "")
		default:
			reply(req.ID, nil, -32601, "Method not found: "+req.Method)
		}
	}
	return sc.Err()
}
