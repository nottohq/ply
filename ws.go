package main

// Workspaces: a directory an agent works in, and the two moves that keep it
// honest. capture records whatever the agent changed on disk; checkout rewrites
// only the files whose content the new state changes.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func itoa(n int) string                   { return strconv.Itoa(n) }

type Info struct{ Agent, Session, Intent, Prompt, Time string }

type Workspace struct {
	Name      string               `json:"name"`
	Root      string               `json:"root"`                 // the set of changes this workspace holds
	Disk      string               `json:"disk"`                 // the set its files currently reflect
	Trial     []string             `json:"trial,omitempty"`      // extra changes held off (culprit search only)
	DiskTrial []string             `json:"disk_trial,omitempty"` //
	Trail     []string             `json:"trail,omitempty"`      // states it has been in, newest last
	Bare      bool                 `json:"bare,omitempty"`       // no directory: a meeting point for syncing
	Dir       string               `json:"dir,omitempty"`        // directory, when not the default
	LastScan  int64                `json:"last_scan,omitempty"`
	Contests  map[string][]Contest `json:"contests,omitempty"`
}

// idx is what a workspace remembers about one file on disk.
type idx struct {
	Fid   string `json:"f"`
	Size  int64  `json:"s"`
	Mtime int64  `json:"m"`
	Hash  string `json:"h"`
	Exec  bool   `json:"x,omitempty"`
	Want  string `json:"w"` // the path the file asks for (differs from its key only when two files collide)
}

func (r *Repo) dir(w *Workspace) string {
	switch {
	case w.Dir != "":
		return w.Dir
	case w.Name == "main":
		return r.root
	}
	return filepath.Join(r.root, ".ply", "ws", w.Name)
}

func (r *Repo) loadWS(name string) (*Workspace, error) {
	var w Workspace
	if r.tx.Bucket([]byte("w")) == nil || !r.getJSON("w", name, &w) {
		return nil, fmt.Errorf("no workspace %q", name)
	}
	return &w, nil
}

func (r *Repo) saveWS(w *Workspace) error {
	if n := len(w.Trail); n == 0 || w.Trail[n-1] != w.Root {
		w.Trail = append(w.Trail, w.Root)
	}
	if len(w.Trail) > 1000 {
		w.Trail = w.Trail[len(w.Trail)-1000:]
	}
	return r.putJSON("w", w.Name, w)
}

// wsFor picks the workspace a directory belongs to.
func (r *Repo) wsFor(cwd string) (*Workspace, error) {
	abs, _ := filepath.Abs(cwd)
	rel, err := filepath.Rel(filepath.Join(r.root, ".ply", "ws"), abs)
	if err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
		return r.loadWS(strings.Split(filepath.ToSlash(rel), "/")[0])
	}
	return r.loadWS("main")
}

func (r *Repo) getIdx(w *Workspace, path string) *idx {
	var e idx
	if !r.getJSON("x:"+w.Name, path, &e) {
		return nil
	}
	return &e
}

func (r *Repo) shown(w *Workspace, fid string) string {
	return string(r.bucket("y:" + w.Name).Get([]byte(fid)))
}

func (r *Repo) wanters(w *Workspace, path string) []string {
	var fids []string
	r.getJSON("z:"+w.Name, path, &fids)
	return fids
}

func (r *Repo) setWant(w *Workspace, fid, from, to string) {
	if from == to {
		return
	}
	if from != "" {
		var keep []string
		for _, f := range r.wanters(w, from) {
			if f != fid {
				keep = append(keep, f)
			}
		}
		if len(keep) == 0 {
			r.bucket("z:" + w.Name).Delete([]byte(from))
		} else {
			r.putJSON("z:"+w.Name, from, keep)
		}
	}
	if to != "" {
		fids := r.wanters(w, to)
		for _, f := range fids {
			if f == fid {
				return
			}
		}
		fids = append(fids, fid)
		sort.Strings(fids)
		r.putJSON("z:"+w.Name, to, fids)
	}
}

// ===== capture: record whatever changed on disk as one change =====

type diskFile struct {
	path string
	data []byte
	exec bool
	size int64
	mt   int64
	hash string
	old  *idx
}

func (r *Repo) capture(w *Workspace, in Info, touched, dirty map[string]bool) (*Meta, error) {
	if w.Bare {
		return nil, nil
	}
	dir := r.dir(w)
	ig := loadIgnore(dir)
	seen := map[string]bool{}
	var modified, added []*diskFile
	started := time.Now().UnixNano()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != dir && ig.skip(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and devices are not versioned
		}
		old := r.getIdx(w, rel)
		if old == nil && ig.skip(rel, false) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		seen[rel] = true
		f := &diskFile{path: rel, exec: info.Mode()&0o111 != 0, size: info.Size(), mt: info.ModTime().UnixNano(), old: old}
		// Unchanged by size and time, and not modified so recently that the
		// clock could hide a second write: trust it without reading.
		if old != nil && old.Size == f.size && old.Mtime == f.mt && old.Exec == f.exec && f.mt < w.LastScan-2e9 {
			return nil
		}
		if f.data, err = os.ReadFile(p); err != nil {
			return err
		}
		f.hash = hash24(f.data)
		switch {
		case old == nil:
			added = append(added, f)
		case old.Hash == f.hash && old.Exec == f.exec:
			old.Size, old.Mtime = f.size, f.mt
			return r.putJSON("x:"+w.Name, rel, old)
		default:
			modified = append(modified, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	type gone struct {
		path string
		e    idx
	}
	var missing []gone
	c := r.bucket("x:" + w.Name).Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if !seen[string(k)] {
			var e idx
			if json.Unmarshal(v, &e) == nil {
				missing = append(missing, gone{string(k), e})
			}
		}
	}
	w.LastScan = started
	if len(modified)+len(added)+len(missing) == 0 {
		return nil, nil
	}
	v := r.view(w.Disk, w.DiskTrial)

	// A file that vanished and one that appeared with the same or similar
	// content are one file that moved: its lines keep their identity.
	moved := map[int]*diskFile{} // index in missing -> where it went
	taken := map[*diskFile]bool{}
	for i, m := range missing {
		for _, a := range added {
			if !taken[a] && a.hash == m.e.Hash {
				moved[i], taken[a] = a, true
				break
			}
		}
	}
	if len(missing)*len(added) <= 4000 {
		for i, m := range missing {
			if moved[i] != nil {
				continue
			}
			oldLines := map[string]int{}
			n := 0
			for _, e := range r.weave(m.e.Fid).view(v) {
				oldLines[e.text]++
				n++
			}
			var best *diskFile
			bestScore := 0.6
			for _, a := range added {
				if taken[a] || isBinary(a.data) {
					continue
				}
				ls := splitLines(a.data)
				if n < 3 || len(ls) < 3 {
					continue
				}
				left := map[string]int{}
				for k, c := range oldLines {
					left[k] = c
				}
				common := 0
				for _, l := range ls {
					if left[l] > 0 {
						left[l]--
						common++
					}
				}
				if s := 2 * float64(common) / float64(n+len(ls)); s >= bestScore {
					best, bestScore = a, s
				}
			}
			if best != nil {
				moved[i], taken[best] = best, true
			}
		}
	}

	var ops []Op
	n := 0
	type put struct {
		f    *diskFile
		fid  string
		want string
	}
	var puts []put
	sort.Slice(modified, func(i, j int) bool { return modified[i].path < modified[j].path })
	for _, f := range modified {
		fw := r.weave(f.old.Fid)
		if f.old.Exec != f.exec {
			ops = append(ops, Op{K: "name", F: fw.fid, N: n, P: f.old.Want, X: f.exec})
			n++
		}
		if f.old.Hash != f.hash {
			ops = append(ops, lineOps(fw.fid, fw.view(v), f.data, &n)...)
		}
		puts = append(puts, put{f, fw.fid, f.old.Want})
	}
	for i, m := range missing {
		fw := r.weave(m.e.Fid)
		if a := moved[i]; a != nil {
			ops = append(ops, Op{K: "name", F: fw.fid, N: n, P: a.path, X: a.exec})
			n++
			if a.hash != m.e.Hash {
				ops = append(ops, lineOps(fw.fid, fw.view(v), a.data, &n)...)
			}
			r.bucket("x:" + w.Name).Delete([]byte(m.path))
			r.setWant(w, fw.fid, m.e.Want, a.path)
			dirty[m.e.Want], dirty[a.path] = true, true
			puts = append(puts, put{a, fw.fid, a.path})
			continue
		}
		for _, p := range fw.names {
			if p.presence && v.visible(p.ch, p.dels) {
				ops = append(ops, Op{K: "rm", F: fw.fid, E: p.id})
			}
		}
		for _, e := range fw.view(v) {
			ops = append(ops, Op{K: "del", F: fw.fid, E: e.id})
		}
		r.bucket("x:" + w.Name).Delete([]byte(m.path))
		r.bucket("y:" + w.Name).Delete([]byte(fw.fid))
		r.setWant(w, fw.fid, m.e.Want, "")
		dirty[m.e.Want] = true
		touched[fw.fid] = true
	}
	sort.Slice(added, func(i, j int) bool { return added[i].path < added[j].path })
	for _, a := range added {
		if taken[a] {
			continue
		}
		fid := ""
		for k := 0; ; k++ {
			fid = fidOf(a.path, k)
			if ex, _, _ := r.weave(fid).place(v); !ex && r.shown(w, fid) == "" {
				break
			}
		}
		fw := r.weave(fid)
		ops = append(ops, Op{K: "add", F: fid, N: n, P: a.path, X: a.exec})
		n++
		ops = append(ops, lineOps(fid, fw.view(v), a.data, &n)...)
		r.setWant(w, fid, "", a.path)
		dirty[a.path] = true
		puts = append(puts, put{a, fid, a.path})
	}

	ch := &Change{Kind: "edit", Agent: in.Agent, Session: in.Session, Intent: in.Intent, Prompt: in.Prompt, Time: in.Time,
		Base: w.Disk, Lamport: trieLamport(r, w.Disk) + 1, Ops: ops}
	if ch.Intent == "" {
		ch.Intent = "(unlabelled edit)"
	}
	if ch.Time == "" {
		ch.Time = now()
	}
	m, err := r.putChange(ch)
	if err != nil {
		return nil, err
	}
	for _, p := range puts {
		e := idx{Fid: p.fid, Size: p.f.size, Mtime: p.f.mt, Hash: p.f.hash, Exec: p.f.exec, Want: p.want}
		if err := r.putJSON("x:"+w.Name, p.f.path, e); err != nil {
			return nil, err
		}
		r.bucket("y:"+w.Name).Put([]byte(p.fid), []byte(p.f.path))
		touched[p.fid] = true
	}
	w.Root = trieInsert(r, w.Root, m.entry())
	w.Disk = trieInsert(r, w.Disk, m.entry())
	return m, nil
}

// ===== checkout: make the files match the workspace's set =====

func (r *Repo) checkout(w *Workspace, touched, dirty map[string]bool) error {
	a := r.view(w.Disk, w.DiskTrial)
	b := r.view(w.Root, w.Trial)
	flipped := func(id string) {
		if m := r.meta(id); m != nil {
			for _, f := range m.Files {
				touched[f] = true
			}
		}
	}
	trieDiff(r, w.Root, w.Disk, func(e entry) { flipped(e.ID) })
	for id := range a.off {
		if !b.off[id] {
			flipped(id)
		}
	}
	for id := range b.off {
		if !a.off[id] {
			flipped(id)
		}
	}
	defer func() { w.LastScan = time.Now().UnixNano() }()
	if w.Bare {
		w.Disk, w.DiskTrial = w.Root, w.Trial
		r.recount(w, b, touched, dirty)
		return nil
	}

	type plan struct {
		fw       *fileWeave
		affected bool
		exists   bool
		want     string
		exec     bool
	}
	plans := map[string]*plan{}
	for fid := range touched {
		fw := r.weave(fid)
		ex, want, exec := fw.place(b)
		if !ex {
			want = ""
		}
		oldWant := ""
		if d := r.shown(w, fid); d != "" {
			if e := r.getIdx(w, d); e != nil {
				oldWant = e.Want
			}
		}
		if oldWant != want {
			r.setWant(w, fid, oldWant, want)
			dirty[oldWant], dirty[want] = true, true
		}
		plans[fid] = &plan{fw: fw, affected: true, exists: ex, want: want, exec: exec}
	}
	// Two files can ask for one path (two agents rename different files onto
	// it). Both are kept: one gets the path, the other a marked name beside it.
	display := map[string]string{}
	for p := range dirty {
		if p == "" {
			continue
		}
		group := r.wanters(w, p)
		winner := ""
		for _, fid := range group {
			if fid == fidOf(p, 0) {
				winner = fid
			}
		}
		if winner == "" && len(group) > 0 {
			winner = group[0]
		}
		for _, fid := range group {
			if fid == winner {
				display[fid] = p
			} else {
				display[fid] = p + ".ply-" + fid[1:9]
			}
			if plans[fid] == nil {
				plans[fid] = &plan{exists: true, want: p}
			}
		}
	}

	type write struct {
		path, fid, want string
		data            []byte
		exec            bool
	}
	var removes []string
	var writes []write
	for fid, pl := range plans {
		old := r.shown(w, fid)
		next := ""
		if pl.exists {
			if next = display[fid]; next == "" {
				next = old
			}
		}
		var oldIdx *idx
		if old != "" {
			oldIdx = r.getIdx(w, old)
		}
		var data []byte
		exec := pl.exec
		if pl.exists {
			if pl.affected {
				data = content(pl.fw.view(b))
			} else {
				var err error
				if data, err = os.ReadFile(filepath.Join(r.dir(w), filepath.FromSlash(old))); err != nil {
					return err
				}
				exec = oldIdx.Exec
			}
		}
		if old != "" && old == next && oldIdx != nil && oldIdx.Hash == hash24(data) && oldIdx.Exec == exec {
			if oldIdx.Want != pl.want {
				oldIdx.Want = pl.want
				r.putJSON("x:"+w.Name, old, oldIdx)
			}
			continue
		}
		if old != "" && old != next {
			removes = append(removes, old)
			r.bucket("y:" + w.Name).Delete([]byte(fid))
		}
		if next != "" {
			writes = append(writes, write{next, fid, pl.want, data, exec})
		}
	}
	dir := r.dir(w)
	for _, p := range removes {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
		r.bucket("x:" + w.Name).Delete([]byte(p))
		for d := filepath.Dir(full); len(d) > len(dir); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
	for _, wr := range writes {
		full := filepath.Join(dir, filepath.FromSlash(wr.path))
		if r.getIdx(w, wr.path) == nil {
			if _, err := os.Lstat(full); err == nil {
				return fmt.Errorf("%s is in the way: it exists on disk but is ignored, and an incoming change needs that path", wr.path)
			}
		}
		mode := os.FileMode(0o644)
		if wr.exec {
			mode = 0o755
		}
		if err := writeAtomic(full, wr.data, mode); err != nil {
			return err
		}
		st, err := os.Stat(full)
		if err != nil {
			return err
		}
		e := idx{Fid: wr.fid, Size: st.Size(), Mtime: st.ModTime().UnixNano(), Hash: hash24(wr.data), Exec: wr.exec, Want: wr.want}
		if err := r.putJSON("x:"+w.Name, wr.path, e); err != nil {
			return err
		}
		r.bucket("y:"+w.Name).Put([]byte(wr.fid), []byte(wr.path))
	}
	w.Disk, w.DiskTrial = w.Root, w.Trial
	r.recount(w, b, touched, dirty)
	return nil
}

// settle is run by every command: record what the agent did, then bring the
// files up to date with anything that arrived.
func (r *Repo) settle(w *Workspace, in Info) (*Meta, error) {
	touched, dirty := map[string]bool{}, map[string]bool{}
	m, err := r.capture(w, in, touched, dirty)
	if err != nil {
		return nil, err
	}
	if err := r.checkout(w, touched, dirty); err != nil {
		return nil, err
	}
	return m, r.saveWS(w)
}

// writeAtomic: a crash leaves either the old file or the new one.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ply-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// ===== new workspace: copy the files, share the history =====

func (r *Repo) newWS(src *Workspace, name, dir string) (*Workspace, error) {
	nw := &Workspace{Name: name, Root: src.Root, Disk: src.Root, Dir: dir, Trail: []string{src.Root}, Contests: src.Contests}
	for _, b := range []string{"x:", "y:", "z:"} {
		r.tx.DeleteBucket([]byte(b + name))
	}
	to := r.dir(nw)
	if err := os.MkdirAll(to, 0o755); err != nil {
		return nil, err
	}
	if src.Bare {
		nw.Disk = ""
		return nw, r.checkout(nw, map[string]bool{}, map[string]bool{})
	}
	from := r.dir(src)
	type row struct {
		k string
		e idx
	}
	var rows []row
	c := r.bucket("x:" + src.Name).Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		var e idx
		if json.Unmarshal(v, &e) == nil {
			rows = append(rows, row{string(k), e})
		}
	}
	for _, rw := range rows {
		data, err := os.ReadFile(filepath.Join(from, filepath.FromSlash(rw.k)))
		if err != nil {
			return nil, err
		}
		full := filepath.Join(to, filepath.FromSlash(rw.k))
		mode := os.FileMode(0o644)
		if rw.e.Exec {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, data, mode); err != nil {
			return nil, err
		}
		st, err := os.Stat(full)
		if err != nil {
			return nil, err
		}
		rw.e.Size, rw.e.Mtime = st.Size(), st.ModTime().UnixNano()
		r.putJSON("x:"+name, rw.k, rw.e)
		r.bucket("y:"+name).Put([]byte(rw.e.Fid), []byte(rw.k))
	}
	var zs [][2][]byte
	zc := r.bucket("z:" + src.Name).Cursor()
	for k, v := zc.First(); k != nil; k, v = zc.Next() {
		zs = append(zs, [2][]byte{append([]byte{}, k...), append([]byte{}, v...)})
	}
	for _, kv := range zs {
		r.bucket("z:"+name).Put(kv[0], kv[1])
	}
	nw.LastScan = time.Now().UnixNano()
	return nw, nil
}

func (r *Repo) dropWS(w *Workspace) {
	for _, b := range []string{"x:", "y:", "z:"} {
		r.tx.DeleteBucket([]byte(b + w.Name))
	}
	r.bucket("w").Delete([]byte(w.Name))
	if !w.Bare {
		os.RemoveAll(r.dir(w))
	}
}
