package main

// The model.
//
// A change is an immutable set of line operations made by one agent for one
// stated intent. Every line ever written gets a permanent id and a permanent
// position (it is anchored after another line). A version is nothing more than
// a SET of changes: a line is visible when the change that wrote it is on and
// no change that is on deleted it.
//
// Because position never depends on which changes are on:
//   - merging is set union, so the order agents merge in cannot matter;
//   - undoing a change keeps everything written after it;
//   - every visible line knows which agent wrote it and why.
//
// Switching a change off is itself a change (a "toggle"), so sets only grow,
// every undo is attributed, and two machines can always combine their work.

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ===== Stored types =====

type Op struct {
	K string   `json:"k"`           // add | rm | name | ins | del
	F string   `json:"f,omitempty"` // file id (left out when stored under its file)
	N int      `json:"n,omitempty"` // add/name/ins: index of the element this op creates
	A string   `json:"a,omitempty"` // ins: element it sits after ("" = top, "~:n" = own element n)
	E string   `json:"e,omitempty"` // del/rm: element removed
	T string   `json:"t,omitempty"` // ins: one line, newline included
	L []string `json:"l,omitempty"` // ins: a run of lines, each sitting after the one before
	B bool     `json:"b,omitempty"` // ins: T is base64 of a whole binary file
	P string   `json:"p,omitempty"` // add/name: path
	X bool     `json:"x,omitempty"` // add/name: executable
}

type Change struct {
	ID      string   `json:"-"`
	Kind    string   `json:"kind"` // edit | toggle
	Agent   string   `json:"agent"`
	Session string   `json:"session,omitempty"`
	Intent  string   `json:"intent"`
	Prompt  string   `json:"prompt,omitempty"`
	Time    string   `json:"time"`
	Base    string   `json:"base"`    // the set the author was looking at
	Lamport int      `json:"lamport"` // 1 + the highest lamport in Base
	Ops     []Op     `json:"ops,omitempty"`
	Targets []string `json:"targets,omitempty"` // toggle: changes it switches off
}

// Meta is a change without its line content: cheap to load.
type Meta struct {
	ID      string   `json:"-"`
	Kind    string   `json:"kind"`
	Agent   string   `json:"agent"`
	Session string   `json:"session,omitempty"`
	Intent  string   `json:"intent"`
	Prompt  string   `json:"prompt,omitempty"`
	Time    string   `json:"time"`
	Base    string   `json:"base"`
	Lamport int      `json:"lamport"`
	Targets []string `json:"targets,omitempty"`
	Files   []string `json:"files,omitempty"`
	Hashes  []string `json:"hashes,omitempty"` // hash of each file's ops, parallel to Files
	Ins     int      `json:"ins,omitempty"`
	Del     int      `json:"del,omitempty"`
}

func (m *Meta) entry() entry { return entry{ID: m.ID, L: m.Lamport, T: m.Kind == "toggle"} }

type Check struct {
	Cmd  string `json:"cmd"`
	Exit int    `json:"exit"`
	Ms   int64  `json:"ms"`
	Time string `json:"time"`
}

var buckets = []string{"m", "f", "n", "w", "k", "r", "s"}

// ===== Repo =====

type Repo struct {
	root   string
	db     *bolt.DB
	tx     *bolt.Tx
	nodes  map[string]*node
	metas  map[string]*Meta
	weaves map[string]*fileWeave
}

func hash24(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:12]) }

func elemID(change string, n int) string { return fmt.Sprintf("%s:%d", change, n) }

func changeOf(elem string) string {
	if i := strings.IndexByte(elem, ':'); i > 0 {
		return elem[:i]
	}
	return ""
}

// fidOf gives a new file its identity. It comes from the path it is born at,
// so two agents who create the same file independently are editing one file.
func fidOf(path string, k int) string {
	if k > 0 {
		path = fmt.Sprintf("%s\x00%d", path, k)
	}
	return "f" + hash24([]byte(path))[:16]
}

func findRoot(from string) (string, error) {
	dir, err := filepath.Abs(from)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".ply", "db")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside a ply repository (run `ply init`)")
		}
		dir = parent
	}
}

func initRepo(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, ".ply", "ws"), 0o755); err != nil {
		return err
	}
	r, err := openRepo(dir)
	if err != nil {
		return err
	}
	defer r.close()
	for _, b := range buckets {
		if _, err := r.tx.CreateBucketIfNotExists([]byte(b)); err != nil {
			return err
		}
	}
	if _, err := r.loadWS("main"); err != nil {
		if err := r.saveWS(&Workspace{Name: "main"}); err != nil {
			return err
		}
	}
	return r.commit()
}

// openRepo takes the repository lock and starts one transaction: a command
// either lands completely or not at all.
func openRepo(root string) (*Repo, error) {
	r := &Repo{root: root}
	return r, r.begin()
}

func (r *Repo) begin() error {
	db, err := bolt.Open(filepath.Join(r.root, ".ply", "db"), 0o644, &bolt.Options{Timeout: 120 * time.Second, NoFreelistSync: true})
	if err != nil {
		return fmt.Errorf("could not open the repository (is another ply command stuck?): %w", err)
	}
	tx, err := db.Begin(true)
	if err != nil {
		db.Close()
		return err
	}
	r.db, r.tx = db, tx
	r.nodes, r.metas, r.weaves = map[string]*node{}, map[string]*Meta{}, map[string]*fileWeave{}
	return nil
}

func (r *Repo) commit() error {
	if r.tx == nil {
		return nil
	}
	err := r.tx.Commit()
	r.tx = nil
	return err
}

func (r *Repo) close() {
	if r.tx != nil {
		r.tx.Rollback()
		r.tx = nil
	}
	if r.db != nil {
		r.db.Close()
		r.db = nil
	}
}

// release commits and lets other agents in while a long command runs.
func (r *Repo) release() error {
	if err := r.commit(); err != nil {
		return err
	}
	r.close()
	return nil
}

func (r *Repo) bucket(name string) *bolt.Bucket {
	b := r.tx.Bucket([]byte(name))
	if b == nil {
		b, _ = r.tx.CreateBucketIfNotExists([]byte(name))
	}
	return b
}

func (r *Repo) getJSON(bucket, key string, v any) bool {
	b := r.bucket(bucket).Get([]byte(key))
	if b == nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

func (r *Repo) putJSON(bucket, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return r.bucket(bucket).Put([]byte(key), b)
}

// ===== Trie nodes =====

func (r *Repo) getNode(h string) *node {
	if n, ok := r.nodes[h]; ok {
		return n
	}
	var n node
	if b := r.rawNode(h); b == nil || json.Unmarshal(b, &n) != nil {
		panic("ply: state node " + h + " is missing from the store")
	}
	r.nodes[h] = &n
	return &n
}

func (r *Repo) rawNode(h string) []byte { return unpack(r.bucket("n").Get([]byte(h))) }

func (r *Repo) putRawNode(h string, b []byte) {
	if r.bucket("n").Get([]byte(h)) == nil {
		r.bucket("n").Put([]byte(h), pack(b))
	}
}

func (r *Repo) putNode(n *node) string {
	b := encodeNode(n)
	h := nodeHash(b)
	if _, ok := r.nodes[h]; !ok {
		r.nodes[h] = n
		r.putRawNode(h, b)
	}
	return h
}

func (r *Repo) hasNode(h string) bool {
	if _, ok := r.nodes[h]; ok {
		return true
	}
	return r.bucket("n").Get([]byte(h)) != nil
}

// ===== Changes =====

func (r *Repo) meta(id string) *Meta {
	if m, ok := r.metas[id]; ok {
		return m
	}
	var m Meta
	if !r.getJSON("m", id, &m) {
		return nil
	}
	m.ID = id
	r.metas[id] = &m
	return &m
}

// A change travels between repositories as its header plus one block of ops
// per file. Its id is a hash over the header and the hash of each block, so a
// receiver can verify it, and one file's history can be read without touching
// another's.
type Wire struct {
	Head  json.RawMessage            `json:"head"`
	Files map[string]json.RawMessage `json:"files,omitempty"`
}

type head struct {
	Kind    string   `json:"kind"`
	Agent   string   `json:"agent"`
	Session string   `json:"session,omitempty"`
	Intent  string   `json:"intent"`
	Prompt  string   `json:"prompt,omitempty"`
	Time    string   `json:"time"`
	Base    string   `json:"base"`
	Lamport int      `json:"lamport"`
	Targets []string `json:"targets,omitempty"`
}

func wireID(w *Wire) (string, []string, []string) {
	fids := make([]string, 0, len(w.Files))
	for fid := range w.Files {
		fids = append(fids, fid)
	}
	sort.Strings(fids)
	var b bytes.Buffer
	b.Write(w.Head)
	hashes := make([]string, len(fids))
	for i, fid := range fids {
		hashes[i] = hash24(w.Files[fid])
		fmt.Fprintf(&b, "\n%s:%s", fid, hashes[i])
	}
	return hash24(b.Bytes()), fids, hashes
}

func (r *Repo) putChange(c *Change) (*Meta, error) {
	hd, err := json.Marshal(head{c.Kind, c.Agent, c.Session, c.Intent, c.Prompt, c.Time, c.Base, c.Lamport, c.Targets})
	if err != nil {
		return nil, err
	}
	w := &Wire{Head: hd, Files: map[string]json.RawMessage{}}
	perFile := map[string][]Op{}
	var order []string
	for _, op := range c.Ops {
		fid := op.F
		op.F = ""
		if _, ok := perFile[fid]; !ok {
			order = append(order, fid)
		}
		perFile[fid] = append(perFile[fid], op)
	}
	for _, fid := range order {
		if w.Files[fid], err = json.Marshal(perFile[fid]); err != nil {
			return nil, err
		}
	}
	return r.storeWire(w)
}

// storeWire verifies nothing by itself: the id IS the content hash, so a
// change that was altered in transit simply arrives under a different id.
func (r *Repo) storeWire(w *Wire) (*Meta, error) {
	id, fids, hashes := wireID(w)
	if m := r.meta(id); m != nil {
		return m, nil
	}
	var h head
	if err := json.Unmarshal(w.Head, &h); err != nil {
		return nil, err
	}
	if h.Kind != "edit" && h.Kind != "toggle" {
		return nil, fmt.Errorf("change %s has an unknown kind %q", id, h.Kind)
	}
	m := &Meta{ID: id, Kind: h.Kind, Agent: h.Agent, Session: h.Session, Intent: h.Intent, Prompt: h.Prompt, Time: h.Time, Base: h.Base, Lamport: h.Lamport, Targets: h.Targets, Files: fids, Hashes: hashes}
	for _, fid := range fids {
		var ops []Op
		if err := json.Unmarshal(w.Files[fid], &ops); err != nil {
			return nil, err
		}
		for _, op := range ops {
			switch op.K {
			case "ins":
				m.Ins += max(1, len(op.L))
			case "del":
				m.Del++
			}
		}
		if err := r.bucket("f").Put([]byte(fid+"\x00"+id), pack(w.Files[fid])); err != nil {
			return nil, err
		}
		delete(r.weaves, fid)
	}
	if err := r.putJSON("m", id, m); err != nil {
		return nil, err
	}
	r.metas[id] = m
	return m, nil
}

// wire rebuilds the travelling form of a stored change.
func (r *Repo) wire(id string) *Wire {
	m := r.meta(id)
	if m == nil {
		return nil
	}
	hd, _ := json.Marshal(head{m.Kind, m.Agent, m.Session, m.Intent, m.Prompt, m.Time, m.Base, m.Lamport, m.Targets})
	w := &Wire{Head: hd, Files: map[string]json.RawMessage{}}
	for _, fid := range m.Files {
		w.Files[fid] = unpack(r.bucket("f").Get([]byte(fid + "\x00" + id)))
	}
	return w
}

// pack compresses a block when that makes it smaller. First byte: 0 plain, 1 deflate.
func pack(b []byte) []byte {
	if len(b) > 256 {
		var buf bytes.Buffer
		buf.WriteByte(1)
		zw, _ := flate.NewWriter(&buf, flate.BestSpeed)
		zw.Write(b)
		zw.Close()
		if buf.Len() < len(b) {
			return buf.Bytes()
		}
	}
	return append([]byte{0}, b...)
}

func unpack(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	if b[0] == 1 {
		out, _ := io.ReadAll(flate.NewReader(bytes.NewReader(b[1:])))
		return out
	}
	return append([]byte{}, b[1:]...)
}

// resolve turns an id prefix into one change id among a set.
func (r *Repo) resolve(prefix, root string) (string, error) {
	var hits []string
	if len(prefix) >= 4 {
		c := r.bucket("m").Cursor()
		for k, _ := c.Seek([]byte(prefix)); k != nil && bytes.HasPrefix(k, []byte(prefix)); k, _ = c.Next() {
			if root == "" || trieHas(r, root, string(k)) {
				hits = append(hits, string(k))
			}
		}
	}
	if len(hits) == 0 {
		return "", fmt.Errorf("no change here matches %q (use at least 4 characters)", prefix)
	}
	if len(hits) > 1 {
		return "", fmt.Errorf("%q matches %d changes; use more characters", prefix, len(hits))
	}
	return hits[0], nil
}

// ===== Checks =====

func (r *Repo) checks(key string) []Check {
	var all []Check
	r.getJSON("k", key, &all)
	return all
}

func (r *Repo) addCheck(key string, c Check) error {
	return r.putJSON("k", key, append(r.checks(key), c))
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
