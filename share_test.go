package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// at runs a command in any directory (several repositories in one test).
func at(t *testing.T, dir, agent, name string, args ...string) string {
	t.Helper()
	s, err := tryAt(dir, agent, name, args...)
	if err != nil {
		t.Fatalf("ply %s %v in %s: %v", name, args, dir, err)
	}
	return s
}

func tryAt(dir, agent, name string, args ...string) (string, error) {
	f := parse(args)
	f.info.Agent = agent
	var buf bytes.Buffer
	err := run(dir, name, f, &buf)
	return buf.String(), err
}

func put(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func get(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// Two people and a meeting point. Both push without pulling first; nobody is rejected.
func TestPushIsNeverRejected(t *testing.T) {
	base := t.TempDir()
	hub, a, b := filepath.Join(base, "hub"), filepath.Join(base, "a"), filepath.Join(base, "b")
	os.MkdirAll(hub, 0o755)
	os.MkdirAll(a, 0o755)
	at(t, hub, "hub", "init", "--bare")
	at(t, a, "ann", "init")
	put(t, a, "f.txt", lines(10, "line"))
	at(t, a, "ann", "snap", "-m", "start")
	at(t, a, "ann", "remote", "add", "origin", hub)
	at(t, a, "ann", "push")
	at(t, base, "bob", "clone", hub, "b")
	if !reflect.DeepEqual(readTree(a), readTree(b)) {
		t.Fatalf("clone differs from the original")
	}
	put(t, a, "f.txt", strings.Replace(get(a, "f.txt"), "line 2\n", "ann was here\n", 1))
	put(t, a, "ann.txt", "a\n")
	at(t, a, "ann", "snap", "-m", "ann's work")
	put(t, b, "f.txt", strings.Replace(get(b, "f.txt"), "line 8\n", "bob was here\n", 1))
	os.Remove(filepath.Join(b, "nothing"))
	at(t, b, "bob", "snap", "-m", "bob's work")
	at(t, a, "ann", "push")
	at(t, b, "bob", "push") // bob has not seen ann's work; a git push here would be refused
	at(t, a, "ann", "pull")
	at(t, b, "bob", "pull")
	ta, tb := readTree(a), readTree(b)
	if !reflect.DeepEqual(ta, tb) {
		t.Fatalf("the two sides differ after syncing:\n%v\n%v", ta, tb)
	}
	body := get(a, "f.txt")
	if !strings.Contains(body, "ann was here\n") || !strings.Contains(body, "bob was here\n") || get(b, "ann.txt") != "a\n" {
		t.Fatalf("work was lost in sync:\n%s", body)
	}
	if !strings.Contains(at(t, b, "bob", "blame", "f.txt"), "ann's work") {
		t.Fatalf("authorship did not travel")
	}
	// An undo made on one machine reaches the other.
	id := ""
	var rows []map[string]any
	json.Unmarshal([]byte(at(t, a, "ann", "log", "--json")), &rows)
	for _, r := range rows {
		if r["intent"] == "bob's work" {
			id = r["id"].(string)
		}
	}
	at(t, a, "ann", "undo", id)
	at(t, a, "ann", "push")
	at(t, b, "bob", "pull")
	if strings.Contains(get(b, "f.txt"), "bob was here") || !reflect.DeepEqual(readTree(a), readTree(b)) {
		t.Fatalf("the undo did not reach the other machine")
	}
}

// Pushing into a workspace someone is in the middle of editing loses nothing.
func TestPushIntoABusyWorkspace(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	os.MkdirAll(a, 0o755)
	at(t, a, "ann", "init")
	put(t, a, "f.txt", lines(6, "l"))
	at(t, a, "ann", "snap", "-m", "start")
	at(t, base, "bob", "clone", a, "b")
	put(t, a, "f.txt", strings.Replace(get(a, "f.txt"), "l 1\n", "ann, not yet recorded\n", 1)) // unrecorded
	put(t, b, "f.txt", strings.Replace(get(b, "f.txt"), "l 6\n", "bob pushed this\n", 1))
	put(t, b, "new.txt", "from bob\n")
	at(t, b, "bob", "snap", "-m", "bob")
	at(t, b, "bob", "push")
	if get(a, "new.txt") != "<missing>" {
		t.Fatalf("a push must not rewrite files under someone's hands")
	}
	if st := at(t, a, "ann", "status"); !strings.Contains(st, "new changes have arrived") || !strings.Contains(st, "1 file(s) with edits not yet recorded") {
		t.Fatalf("status should show both the arrival and the unrecorded edit:\n%s", st)
	}
	at(t, a, "ann", "snap", "-m", "ann")
	body := get(a, "f.txt")
	if !strings.Contains(body, "ann, not yet recorded\n") || !strings.Contains(body, "bob pushed this\n") || get(a, "new.txt") != "from bob\n" {
		t.Fatalf("after catching up:\n%s", body)
	}
	at(t, b, "bob", "pull")
	if !reflect.DeepEqual(readTree(a), readTree(b)) {
		t.Fatalf("sides differ")
	}
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestSyncOverHTTP(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	os.MkdirAll(hub, 0o755)
	at(t, hub, "hub", "init", "--bare")
	addr := freeAddr(t)
	t.Setenv("PLY_TOKEN", "s3cret")
	go serveHTTP(hub, addr, new(bytes.Buffer))
	url := "http://" + addr
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	dirs := []string{filepath.Join(base, "r0"), filepath.Join(base, "r1"), filepath.Join(base, "r2")}
	os.MkdirAll(dirs[0], 0o755)
	at(t, dirs[0], "r0", "init")
	for i := 0; i < 5; i++ {
		put(t, dirs[0], fmt.Sprintf("src/f%d.txt", i), lines(20, "x"))
	}
	put(t, dirs[0], "bin.dat", "\x00\x01\x02")
	at(t, dirs[0], "r0", "snap", "-m", "start")
	at(t, dirs[0], "r0", "remote", "add", "origin", url)
	at(t, dirs[0], "r0", "push")
	at(t, base, "r1", "clone", url, "r1")
	at(t, base, "r2", "clone", url, "r2")
	rng := rand.New(rand.NewSource(9))
	for step := 0; step < 60; step++ {
		i := rng.Intn(3)
		d, who := dirs[i], fmt.Sprint("r", i)
		switch rng.Intn(5) {
		case 0:
			at(t, d, who, "pull")
		case 1:
			at(t, d, who, "push")
		default:
			f := fmt.Sprintf("src/f%d.txt", rng.Intn(5))
			if cur := get(d, f); cur != "<missing>" {
				put(t, d, f, randomEdit(rng, cur, who))
			}
			at(t, d, who, "snap", "-m", fmt.Sprint("step ", step))
		}
	}
	for round := 0; round < 2; round++ {
		for i, d := range dirs {
			at(t, d, fmt.Sprint("r", i), "push")
		}
		for i, d := range dirs {
			at(t, d, fmt.Sprint("r", i), "pull")
		}
	}
	for i := 1; i < 3; i++ {
		if !reflect.DeepEqual(readTree(dirs[0]), readTree(dirs[i])) {
			t.Fatalf("repository %d differs after syncing over HTTP", i)
		}
	}
	// Without the token the server refuses.
	t.Setenv("PLY_TOKEN", "wrong")
	if _, err := tryAt(dirs[0], "r0", "pull"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected a 401 with the wrong token, got %v", err)
	}
}

// A remote that alters a change in transit is caught: the id is the content.
type lyingPeer struct{ Peer }

func (p lyingPeer) Changes(ids []string) ([]*Wire, error) {
	cs, err := p.Peer.Changes(ids)
	for _, c := range cs {
		c.Head = json.RawMessage(strings.Replace(string(c.Head), `"agent":"ann"`, `"agent":"mallory"`, 1))
	}
	return cs, err
}

func TestTamperedChangeIsRefused(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	os.MkdirAll(a, 0o755)
	os.MkdirAll(b, 0o755)
	at(t, a, "ann", "init")
	put(t, a, "f", "x\n")
	at(t, a, "ann", "snap", "-m", "one")
	at(t, b, "bob", "init")
	r, err := openRepo(b)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	w, _ := r.loadWS("main")
	p, err := openPeer(a)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := r.pull(w, lyingPeer{p}, "main", Info{Agent: "bob"}); err == nil || !strings.Contains(err.Error(), "does not match its id") {
		t.Fatalf("a tampered change must be refused, got %v", err)
	}
}

// ===== git =====

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGitImportCommitExport(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	commit := func(author, msg string) {
		gitT(t, dir, "add", "-A")
		gitT(t, dir, "-c", "user.name="+author, "-c", "user.email="+author+"@x", "commit", "-q", "-m", msg)
	}
	put(t, dir, "app.py", "def a():\n    return 1\n")
	put(t, dir, "old.txt", "to be removed\n")
	commit("dana", "first version")
	put(t, dir, "app.py", "def a():\n    return 1\n\ndef b():\n    return 2\n")
	commit("eli", "add b")
	gitT(t, dir, "mv", "app.py", "core.py")
	os.Remove(filepath.Join(dir, "old.txt"))
	put(t, dir, "core.py", "def a():\n    return 1\n\ndef b():\n    return 2\n\ndef c():\n    return 3\n")
	commit("dana", "rename to core and add c")
	put(t, dir, "core.py", get(dir, "core.py")+"# not committed yet\n")
	before := readTree(dir)

	at(t, dir, "me", "init")
	at(t, dir, "me", "git", "import")
	if !reflect.DeepEqual(before, readTree(dir)) {
		t.Fatalf("importing changed the working files")
	}
	if strings.Contains(gitT(t, dir, "status", "--porcelain"), ".ply") {
		t.Fatalf("git sees ply's directory")
	}
	blame := at(t, dir, "me", "blame", "core.py")
	for _, want := range []string{"dana", "eli", "add b", "rename to core and add c", "first version", "work not yet committed"} {
		if !strings.Contains(blame, want) {
			t.Fatalf("blame after import lacks %q:\n%s", want, blame)
		}
	}
	// History that predates ply can be undone selectively.
	var rows []map[string]any
	json.Unmarshal([]byte(at(t, dir, "me", "log", "--json")), &rows)
	for _, r := range rows {
		if r["intent"] == "add b" {
			at(t, dir, "me", "undo", r["id"].(string))
		}
	}
	if got := get(dir, "core.py"); strings.Contains(got, "def b") || !strings.Contains(got, "def c") || !strings.Contains(got, "def a") {
		t.Fatalf("undo of an imported commit gave:\n%s", got)
	}
	// Commit back to git, crediting the agents.
	at(t, dir, "agent-7", "snap", "-m", "tidy")
	put(t, dir, "notes.md", "n\n")
	at(t, dir, "agent-7", "snap", "-m", "add notes")
	out := at(t, dir, "me", "git", "commit", "-m", "drop b, add notes")
	msg := gitT(t, dir, "log", "-1", "--format=%B")
	if !strings.Contains(out, "git commit") || !strings.Contains(msg, "Co-authored-by: agent-7") || !strings.Contains(msg, "add notes") || !strings.Contains(msg, "Ply-State:") {
		t.Fatalf("git commit message:\n%s", msg)
	}
	if gitT(t, dir, "status", "--porcelain") != "" {
		t.Fatalf("git tree not clean after ply git commit")
	}
	// And out again as a fresh git repository, one commit per change that is on.
	exp := filepath.Join(t.TempDir(), "exported")
	at(t, dir, "me", "export", exp)
	if !reflect.DeepEqual(readTree(dir), readTree(exp)) {
		t.Fatalf("exported files differ")
	}
	log := gitT(t, exp, "log", "--format=%an: %s", "--reverse")
	if !strings.HasPrefix(log, "dana: first version") || strings.Contains(log, "add b") || !strings.Contains(log, "agent-7: add notes") {
		t.Fatalf("exported history:\n%s", log)
	}
}

// ===== agents =====

func TestMCPServer(t *testing.T) {
	e := newEnv(t)
	e.write("main", "f.txt", "one\n")
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-harness","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ply_snap","arguments":{"intent":"first","prompt":"make a file"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ply_why","arguments":{"file":"f.txt","line":1}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"ply_check","arguments":{"command":"false"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"ply_undo","arguments":{"change":"zzzz"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"ping"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveMCP(e.root, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	replies := map[float64]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not one JSON message per line: %q", line)
		}
		replies[m["id"].(float64)] = m
	}
	if len(replies) != 8 {
		t.Fatalf("want 8 replies (none for the notification), got %d", len(replies))
	}
	init := replies[1]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize: %v", init)
	}
	list := replies[2]["result"].(map[string]any)["tools"].([]any)
	for _, x := range list {
		tl := x.(map[string]any)
		if tl["name"] == "" || tl["description"] == "" || tl["inputSchema"].(map[string]any)["type"] != "object" {
			t.Fatalf("bad tool definition: %v", tl)
		}
	}
	text := func(id float64) (string, bool) {
		r := replies[id]["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
	}
	if s, isErr := text(4); isErr || !strings.Contains(s, `"agent": "test-harness"`) || !strings.Contains(s, `"prompt": "make a file"`) {
		t.Fatalf("ply_why through MCP: %s", s)
	}
	if s, isErr := text(5); isErr || !strings.Contains(s, `"exit": 1`) {
		t.Fatalf("a failing check is a result, not a tool error: %s", s)
	}
	if _, isErr := text(6); !isErr {
		t.Fatalf("a bad change id should be a tool error")
	}
	if replies[7]["error"].(map[string]any)["code"].(float64) != -32602 {
		t.Fatalf("unknown tool should be a protocol error: %v", replies[7])
	}
}

func TestClaudeCodeHooks(t *testing.T) {
	e := newEnv(t)
	hook := func(payload string) string {
		stdin = strings.NewReader(payload)
		defer func() { stdin = os.Stdin }()
		f := parse(nil)
		var buf bytes.Buffer
		if err := run(e.root, "hook", f, &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	t.Setenv("PLY_AGENT", "")
	out := hook(`{"session_id":"s-1","cwd":"/x","hook_event_name":"UserPromptSubmit","prompt":"add a greeting"}`)
	e.write("main", "hello.py", "print('hi')\n")
	out += hook(`{"session_id":"s-1","hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"` + filepath.Join(e.root, "hello.py") + `","content":"x"},"tool_use_id":"t1"}`)
	out += hook(`{"session_id":"s-1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`)
	e.write("main", "made-by-script.txt", "x\n")
	out += hook(`{"session_id":"s-1","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"python gen.py   --all"}}`)
	if out != "" {
		t.Fatalf("hooks must print nothing, printed %q", out)
	}
	why := e.must("main", "why", "hello.py:1")
	for _, want := range []string{"claude", "add a greeting", "Write hello.py"} {
		if !strings.Contains(why, want) {
			t.Fatalf("hook-recorded change lacks %q:\n%s", want, why)
		}
	}
	log := e.must("main", "log")
	if strings.Count(log, "\n") != 2 || !strings.Contains(log, "Bash: python gen.py --all") {
		t.Fatalf("want two changes (the no-op ls records nothing):\n%s", log)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(e.must("main", "hook", "config")), &cfg); err != nil || cfg["hooks"] == nil {
		t.Fatalf("hook config is not valid JSON")
	}
}
