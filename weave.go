package main

// One file's whole history, and how to read a version out of it.

import (
	"bytes"
	"encoding/base64"
	"sort"
	"strings"
	"unicode/utf8"
)

type elem struct {
	id     string
	ch     *Meta
	n      int
	anchor string
	text   string
	bin    bool
	dels   []*Meta
}

// nameEl is a claim about where the file lives. An "add" also asserts that the
// file exists (presence); a later "rm" removes that assertion.
type nameEl struct {
	id       string
	ch       *Meta
	n        int
	path     string
	exec     bool
	presence bool
	dels     []*Meta
}

type fileWeave struct {
	fid      string
	lines    map[string]*elem
	children map[string][]*elem
	names    []*nameEl
	ops      map[string][]Op // change id -> its ops on this file
	touchers []*Meta
}

func (r *Repo) weave(fid string) *fileWeave {
	if fw, ok := r.weaves[fid]; ok {
		return fw
	}
	fw := &fileWeave{fid: fid, lines: map[string]*elem{}, children: map[string][]*elem{}, ops: map[string][]Op{}}
	prefix := []byte(fid + "\x00")
	c := r.bucket("f").Cursor()
	type rec struct {
		id  string
		raw []byte
	}
	var recs []rec
	for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
		recs = append(recs, rec{string(k[len(prefix):]), append([]byte{}, v...)})
	}
	for _, rc := range recs {
		m := r.meta(rc.id)
		if m == nil {
			continue
		}
		var ops []Op
		if jsonUnmarshal(unpack(rc.raw), &ops) != nil {
			continue
		}
		for i := range ops {
			ops[i].F = fid
			if strings.HasPrefix(ops[i].A, "~") {
				ops[i].A = m.ID + ops[i].A[1:]
			}
		}
		fw.ops[m.ID] = ops
		fw.touchers = append(fw.touchers, m)
		for _, op := range ops {
			switch op.K {
			case "ins":
				texts := op.L
				if len(texts) == 0 {
					texts = []string{op.T}
				}
				anchor := op.A
				for i, t := range texts {
					e := &elem{id: elemID(m.ID, op.N+i), ch: m, n: op.N + i, anchor: anchor, text: t, bin: op.B}
					fw.lines[e.id] = e
					fw.children[anchor] = append(fw.children[anchor], e)
					anchor = e.id
				}
			case "add", "name":
				fw.names = append(fw.names, &nameEl{id: elemID(m.ID, op.N), ch: m, n: op.N, path: op.P, exec: op.X, presence: op.K == "add"})
			}
		}
	}
	for _, m := range fw.touchers {
		for _, op := range fw.ops[m.ID] {
			switch op.K {
			case "del":
				if e := fw.lines[op.E]; e != nil {
					e.dels = append(e.dels, m)
				}
			case "rm":
				for _, p := range fw.names {
					if p.id == op.E {
						p.dels = append(p.dels, m)
					}
				}
			}
		}
	}
	// Siblings placed after the same line: the newest goes first, so an
	// insertion lands directly after its anchor. Ties break on change id, so
	// the order is a function of the changes alone, never of who merged first.
	for _, kids := range fw.children {
		sort.Slice(kids, func(i, j int) bool { return after(kids[i].ch, kids[i].n, kids[j].ch, kids[j].n) })
	}
	r.weaves[fid] = fw
	return fw
}

// after reports whether (a, an) is newer than (b, bn) in the fixed total order.
func after(a *Meta, an int, b *Meta, bn int) bool {
	if a.Lamport != b.Lamport {
		return a.Lamport > b.Lamport
	}
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	return an < bn
}

// ===== Views =====

// A View answers "is this change on?" for one state.
type View struct {
	r    *Repo
	root string
	off  map[string]bool
	memo map[string]bool
}

// view works out which changes are switched off. A toggle switches its targets
// off while it is itself on; a toggle can be the target of a later toggle
// (that is redo). Later toggles are decided first.
func (r *Repo) view(root string, trial []string) *View {
	v := &View{r: r, root: root, off: map[string]bool{}, memo: map[string]bool{}}
	for _, id := range trial {
		v.off[id] = true
	}
	var toggles []*Meta
	trieToggles(r, root, func(e entry) {
		if m := r.meta(e.ID); m != nil {
			toggles = append(toggles, m)
		}
	})
	sort.Slice(toggles, func(i, j int) bool { return after(toggles[i], 0, toggles[j], 0) })
	for _, t := range toggles {
		if v.off[t.ID] {
			continue
		}
		for _, target := range t.Targets {
			v.off[target] = true
		}
	}
	return v
}

func (v *View) on(id string) bool {
	if v.off[id] {
		return false
	}
	if x, ok := v.memo[id]; ok {
		return x
	}
	x := trieHas(v.r, v.root, id)
	v.memo[id] = x
	return x
}

func (v *View) key() string {
	ids := make([]string, 0, len(v.off))
	for id := range v.off {
		if trieHas(v.r, v.root, id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return hash24([]byte(v.root + "|" + strings.Join(ids, ",")))
}

func (v *View) visible(ch *Meta, dels []*Meta) bool {
	if !v.on(ch.ID) {
		return false
	}
	for _, d := range dels {
		if v.on(d.ID) {
			return false
		}
	}
	return true
}

// lines returns the file's visible lines in order.
func (fw *fileWeave) view(v *View) []*elem {
	var out []*elem
	stack := make([]*elem, 0, 64)
	push := func(anchor string) {
		kids := fw.children[anchor]
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, kids[i])
		}
	}
	push("")
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if v.visible(e.ch, e.dels) {
			out = append(out, e)
		}
		push(e.id)
	}
	return out
}

// place says whether the file exists in this view and where it wants to live.
// The newest name that is on wins, so a rename by one agent carries another
// agent's concurrent edits along with it.
func (fw *fileWeave) place(v *View) (exists bool, path string, exec bool) {
	var best *nameEl
	for _, n := range fw.names {
		if !v.on(n.ch.ID) {
			continue
		}
		if n.presence && v.visible(n.ch, n.dels) {
			exists = true
		}
		if best == nil || after(n.ch, n.n, best.ch, best.n) {
			best = n
		}
	}
	if best == nil {
		return false, "", false
	}
	return exists, best.path, best.exec
}

func content(lines []*elem) []byte {
	var buf bytes.Buffer
	for _, e := range lines {
		if e.bin {
			raw, _ := base64.StdEncoding.DecodeString(e.text)
			buf.Write(raw)
		} else {
			buf.WriteString(e.text)
		}
	}
	return buf.Bytes()
}

func isBinary(b []byte) bool { return bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) }

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := strings.SplitAfter(string(b), "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// lineOps turns "these visible lines should become this content" into ops.
func lineOps(fid string, old []*elem, data []byte, n *int) []Op {
	var ops []Op
	oldBin := false
	for _, e := range old {
		oldBin = oldBin || e.bin
	}
	if oldBin || isBinary(data) {
		for _, e := range old {
			ops = append(ops, Op{K: "del", F: fid, E: e.id})
		}
		if isBinary(data) {
			ops = append(ops, Op{K: "ins", F: fid, N: *n, T: base64.StdEncoding.EncodeToString(data), B: true})
			*n++
			return ops
		}
		old = nil
	}
	a := make([]string, len(old))
	for i, e := range old {
		a[i] = e.text
	}
	b := splitLines(data)
	anchor := ""
	run := -1 // index in ops of the run being extended, or -1
	i, j := 0, 0
	for _, step := range diffLines(a, b) {
		switch step {
		case 'k':
			anchor, run = old[i].id, -1
			i++
			j++
		case 'd':
			ops = append(ops, Op{K: "del", F: fid, E: old[i].id})
			i++
		case 'i':
			// Consecutive new lines form one run: each sits after the one before.
			if run < 0 {
				ops = append(ops, Op{K: "ins", F: fid, N: *n, A: anchor})
				run = len(ops) - 1
			}
			ops[run].L = append(ops[run].L, b[j])
			*n++
			j++
		}
	}
	return ops
}
