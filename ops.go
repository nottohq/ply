package main

// What agents do with a workspace: merge, switch changes off and on, see where
// they collided, check a state, and find which changes break a check.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// ===== merge: a set union =====

func (r *Repo) merge(w *Workspace, otherRoot string, in Info) (int, error) {
	if _, err := r.settle(w, in); err != nil {
		return 0, err
	}
	before := trieCount(r, w.Root)
	w.Root = trieUnion(r, w.Root, otherRoot)
	_, err := r.settle(w, in)
	return trieCount(r, w.Root) - before, err
}

// ===== toggles: switching changes off and on is itself a recorded change =====

func (r *Repo) toggle(w *Workspace, targets []string, in Info) (*Meta, error) {
	sort.Strings(targets)
	ch := &Change{Kind: "toggle", Agent: in.Agent, Session: in.Session, Intent: in.Intent, Prompt: in.Prompt, Time: now(),
		Base: w.Root, Lamport: trieLamport(r, w.Root) + 1, Targets: targets}
	m, err := r.putChange(ch)
	if err != nil {
		return nil, err
	}
	w.Root = trieInsert(r, w.Root, m.entry())
	_, err = r.settle(w, in)
	return m, err
}

// dependents lists changes that are on and edited or built on lines written by id.
func (r *Repo) dependents(id string, v *View) []string {
	m := r.meta(id)
	seen := map[string]bool{}
	for _, f := range m.Files {
		fw := r.weave(f)
		for _, t := range fw.touchers {
			if t.ID == id || seen[t.ID] || !v.on(t.ID) {
				continue
			}
			for _, op := range fw.ops[t.ID] {
				if changeOf(op.A) == id || changeOf(op.E) == id {
					seen[t.ID] = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func (r *Repo) undo(w *Workspace, prefix string, cascade bool, in Info) (string, []string, error) {
	if _, err := r.settle(w, in); err != nil {
		return "", nil, err
	}
	id, err := r.resolve(prefix, w.Root)
	if err != nil {
		return "", nil, err
	}
	v := r.view(w.Root, nil)
	if !v.on(id) {
		return "", nil, fmt.Errorf("%s is already switched off", short(id))
	}
	if r.meta(id).Kind != "edit" {
		return "", nil, fmt.Errorf("%s is not an edit; use `ply redo` to switch a change back on", short(id))
	}
	targets := []string{id}
	deps := r.dependents(id, v)
	if cascade {
		seen := map[string]bool{id: true}
		for queue := deps; len(queue) > 0; queue = queue[1:] {
			if d := queue[0]; !seen[d] {
				seen[d] = true
				targets = append(targets, d)
				queue = append(queue, r.dependents(d, v)...)
			}
		}
		deps = nil
	}
	if in.Intent == "" {
		in.Intent = "undo: " + r.meta(id).Intent
	}
	_, err = r.toggle(w, targets, in)
	return id, deps, err
}

func (r *Repo) redo(w *Workspace, prefix string, in Info) (string, error) {
	if _, err := r.settle(w, in); err != nil {
		return "", err
	}
	id, err := r.resolve(prefix, w.Root)
	if err != nil {
		return "", err
	}
	v := r.view(w.Root, nil)
	// Switch off every toggle holding it off; anything else those toggles
	// were holding off stays off.
	set := map[string]bool{}
	trieToggles(r, w.Root, func(e entry) {
		t := r.meta(e.ID)
		if t == nil || !v.on(t.ID) {
			return
		}
		for _, x := range t.Targets {
			if x == id {
				set[t.ID] = true
				for _, y := range t.Targets {
					if y != id {
						set[y] = true
					}
				}
			}
		}
	})
	if len(set) == 0 {
		return "", fmt.Errorf("%s is not switched off", short(id))
	}
	targets := make([]string, 0, len(set))
	for t := range set {
		targets = append(targets, t)
	}
	if in.Intent == "" {
		in.Intent = "redo: " + r.meta(id).Intent
	}
	_, err = r.toggle(w, targets, in)
	return id, err
}

// unitsSince lists what must be switched off to get back to the content of an
// earlier state: every edit that arrived since, and every toggle since that is on.
func (r *Repo) unitsSince(root, earlier string, v *View) (edits, toggles []string) {
	trieDiff(r, root, earlier, func(e entry) {
		if e.T {
			if v.on(e.ID) {
				toggles = append(toggles, e.ID)
			}
		} else {
			edits = append(edits, e.ID)
		}
	})
	sort.Strings(edits)
	sort.Strings(toggles)
	return
}

func (r *Repo) restore(w *Workspace, target string, in Info) error {
	if _, err := r.settle(w, in); err != nil {
		return err
	}
	extra := 0
	trieDiff(r, target, w.Root, func(entry) { extra++ })
	if extra > 0 {
		return fmt.Errorf("that state holds changes this workspace has never had")
	}
	v := r.view(w.Root, nil)
	edits, toggles := r.unitsSince(w.Root, target, v)
	targets := append(edits, toggles...)
	if len(targets) == 0 {
		return nil
	}
	old := r.view(target, nil).key()
	if in.Intent == "" {
		in.Intent = "restore to state " + short(target)
	}
	if _, err := r.toggle(w, targets, in); err != nil {
		return err
	}
	// Same content as the target, so its check results apply here too.
	if cs := r.checks(old); len(cs) > 0 {
		key := r.view(w.Root, nil).key()
		if len(r.checks(key)) == 0 {
			r.putJSON("k", key, cs)
		}
	}
	return nil
}

// ===== contests: spots two agents changed without seeing each other =====

type Contest struct {
	Kind    string `json:"kind"` // lines | path
	File    string `json:"file"`
	From    int    `json:"from_line,omitempty"`
	To      int    `json:"to_line,omitempty"`
	A       string `json:"a"`
	AgentA  string `json:"agent_a"`
	IntentA string `json:"intent_a"`
	B       string `json:"b"`
	AgentB  string `json:"agent_b"`
	IntentB string `json:"intent_b"`
}

func (r *Repo) fileContests(fw *fileWeave, v *View) []Contest {
	// 1. For every spot (a line deleted, or a line inserted after), who touched it.
	touched := map[string][]*Meta{}
	for _, m := range fw.touchers {
		if !v.on(m.ID) {
			continue
		}
		seen := map[string]bool{}
		for _, op := range fw.ops[m.ID] {
			var key string
			switch op.K {
			case "ins":
				if changeOf(op.A) == m.ID {
					continue
				}
				key = op.A
			case "del":
				key = op.E // deleting a line and inserting after it are the same spot
			default:
				continue
			}
			if !seen[key] {
				seen[key] = true
				touched[key] = append(touched[key], m)
			}
		}
	}
	// 2. Keep pairs where neither agent had the other's change in view.
	type pair struct{ a, b *Meta }
	shared := map[pair][]string{}
	for key, ms := range touched {
		for i := 0; i < len(ms); i++ {
			for j := i + 1; j < len(ms); j++ {
				a, b := ms[i], ms[j]
				if a.ID > b.ID {
					a, b = b, a
				}
				if !trieHas(r, a.Base, b.ID) && !trieHas(r, b.Base, a.ID) {
					shared[pair{a, b}] = append(shared[pair{a, b}], key)
				}
			}
		}
	}
	if len(shared) == 0 {
		return nil
	}
	_, path, _ := fw.place(v)
	lines := fw.view(v)
	var out []Contest
	for p, keys := range shared {
		if r.settled(fw, p.a, p.b, keys, v) {
			continue
		}
		c := Contest{Kind: "lines", File: path, A: p.a.ID, AgentA: p.a.Agent, IntentA: p.a.Intent, B: p.b.ID, AgentB: p.b.Agent, IntentB: p.b.Intent}
		for i, e := range lines {
			if e.ch == p.a || e.ch == p.b {
				if c.From == 0 {
					c.From = i + 1
				}
				c.To = i + 1
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].A+out[i].B < out[j].A+out[j].B
	})
	return out
}

// settled: a later change saw both sides and edited that spot.
func (r *Repo) settled(fw *fileWeave, a, b *Meta, keys []string, v *View) bool {
	for _, m := range fw.touchers {
		if m == a || m == b || !v.on(m.ID) || !trieHas(r, m.Base, a.ID) || !trieHas(r, m.Base, b.ID) {
			continue
		}
		for _, op := range fw.ops[m.ID] {
			ref, ok := "", false
			switch {
			case op.K == "ins" && changeOf(op.A) != m.ID:
				ref, ok = op.A, true
			case op.K == "del":
				ref, ok = op.E, true
			}
			if !ok {
				continue
			}
			if c := changeOf(ref); c == a.ID || c == b.ID {
				return true
			}
			for _, k := range keys {
				if ref == k {
					return true
				}
			}
		}
	}
	return false
}

// recount refreshes the stored contests for the files and paths just touched.
func (r *Repo) recount(w *Workspace, v *View, touched, dirty map[string]bool) {
	if w.Contests == nil {
		w.Contests = map[string][]Contest{}
	}
	for fid := range touched {
		if cs := r.fileContests(r.weave(fid), v); len(cs) > 0 {
			w.Contests[fid] = cs
		} else {
			delete(w.Contests, fid)
		}
	}
	for p := range dirty {
		if p == "" {
			continue
		}
		group := r.wanters(w, p)
		if len(group) < 2 {
			delete(w.Contests, "path:"+p)
			continue
		}
		var cs []Contest
		first := r.namer(group[0], v)
		for _, fid := range group[1:] {
			if other := r.namer(fid, v); first != nil && other != nil {
				cs = append(cs, Contest{Kind: "path", File: p, A: first.ID, AgentA: first.Agent, IntentA: first.Intent, B: other.ID, AgentB: other.Agent, IntentB: other.Intent})
			}
		}
		w.Contests["path:"+p] = cs
	}
}

// namer returns the change that gave a file its current path.
func (r *Repo) namer(fid string, v *View) *Meta {
	var best *nameEl
	for _, n := range r.weave(fid).names {
		if v.on(n.ch.ID) && (best == nil || after(n.ch, n.n, best.ch, best.n)) {
			best = n
		}
	}
	if best == nil {
		return nil
	}
	return best.ch
}

func (w *Workspace) contests() []Contest {
	out := []Contest{}
	for _, cs := range w.Contests {
		out = append(out, cs...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].A+out[i].B < out[j].A+out[j].B
	})
	return out
}

// ===== check: run a command and attach the result to this exact content =====

// runCheck runs cmd in dir with the repository unlocked, so other agents keep working.
func (r *Repo) runCheck(dir, key, cmd string, quiet bool) (Check, error) {
	for _, c := range r.checks(key) {
		if c.Cmd == cmd && quiet {
			return c, nil // this exact content was already checked with this command
		}
	}
	if err := r.release(); err != nil {
		return Check{}, err
	}
	start := time.Now()
	c := exec.Command("sh", "-c", cmd)
	c.Dir = dir
	if !quiet {
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
	}
	runErr := c.Run()
	res := Check{Cmd: cmd, Ms: time.Since(start).Milliseconds(), Time: now()}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.Exit = ee.ExitCode()
	} else if runErr != nil {
		res.Exit = 127
	}
	if err := r.begin(); err != nil {
		return res, err
	}
	return res, r.addCheck(key, res)
}

func (r *Repo) check(w *Workspace, cmd string, in Info) (Check, error) {
	if _, err := r.settle(w, in); err != nil {
		return Check{}, err
	}
	return r.runCheck(r.dir(w), r.view(w.Root, nil).key(), cmd, false)
}

func passed(cs []Check, cmd string) (known, ok bool) {
	for i := len(cs) - 1; i >= 0; i-- {
		if cmd == "" || cs[i].Cmd == cmd {
			return true, cs[i].Exit == 0
		}
	}
	return false, false
}

// lastGreen finds the most recent state of this workspace whose check passed.
func (r *Repo) lastGreen(w *Workspace, cmd string) string {
	seen := map[string]bool{}
	for i := len(w.Trail) - 1; i >= 0 && len(seen) < 300; i-- {
		root := w.Trail[i]
		if seen[root] {
			continue
		}
		seen[root] = true
		if known, ok := passed(r.checks(r.view(root, nil).key()), cmd); known && ok {
			return root
		}
	}
	return ""
}

// ===== culprit: which changes, alone or together, make a check fail =====
//
// Any subset of changes is a valid state here, so the search is not limited to
// a line of history: it can find that two changes are each fine and fail only
// together, which a linear bisect cannot express.

type CulpritResult struct {
	Cmd      string   `json:"cmd"`
	Baseline string   `json:"baseline,omitempty"`
	Runs     int      `json:"runs"`
	Among    int      `json:"among"`
	Culprits []string `json:"culprits"`
	Held     string   `json:"held,omitempty"`
	Note     string   `json:"note,omitempty"`
}

func (r *Repo) culprit(w *Workspace, cmd, baseline string, hold bool, in Info) (*CulpritResult, error) {
	if _, err := r.settle(w, in); err != nil {
		return nil, err
	}
	res := &CulpritResult{Cmd: cmd, Culprits: []string{}}
	scDir := filepath.Join(r.root, ".ply", "tmp", fmt.Sprintf("culprit-%d", os.Getpid()))
	sc, err := r.newWS(w, "tmp-culprit-"+w.Name, scDir)
	if err != nil {
		return nil, err
	}
	if err := r.saveWS(sc); err != nil {
		return nil, err
	}
	defer func() {
		if r.tx != nil {
			r.dropWS(sc)
		}
	}()
	fails := func(off []string) (bool, error) {
		sc.Trial = off
		if err := r.checkout(sc, map[string]bool{}, map[string]bool{}); err != nil {
			return false, err
		}
		if err := r.saveWS(sc); err != nil {
			return false, err
		}
		key := r.view(sc.Root, off).key()
		if known, ok := passed(r.checks(key), cmd); known {
			return !ok, nil
		}
		res.Runs++
		c, err := r.runCheck(scDir, key, cmd, true)
		return c.Exit != 0, err
	}

	bad, err := fails(nil)
	if err != nil {
		return nil, err
	}
	if !bad {
		res.Note = "the check passes on the current state"
		return res, nil
	}
	cur := r.view(w.Root, nil)
	offFor := func(root string) []string {
		e, t := r.unitsSince(w.Root, root, cur)
		return append(e, t...)
	}
	// 1. A passing baseline: given, known from an earlier check, or searched for along the trail.
	if baseline == "" {
		baseline = r.lastGreen(w, cmd)
	}
	if baseline == "" {
		var trail []string
		for _, s := range w.Trail {
			if len(trail) == 0 || trail[len(trail)-1] != s {
				trail = append(trail, s)
			}
		}
		if bad, err := fails(offFor(trail[0])); err != nil {
			return nil, err
		} else if bad {
			res.Note = "no passing state is known, and the oldest state of this workspace fails too"
			return res, nil
		}
		lo, hi := 0, len(trail)-1
		for hi-lo > 1 {
			mid := (lo + hi) / 2
			bad, err := fails(offFor(trail[mid]))
			if err != nil {
				return nil, err
			}
			if bad {
				hi = mid
			} else {
				lo = mid
			}
		}
		baseline = trail[lo]
	} else if bad, err := fails(offFor(baseline)); err != nil {
		return nil, err
	} else if bad {
		res.Note = "the check already fails on the baseline, so nothing newer is to blame"
		return res, nil
	}
	res.Baseline = baseline

	// 2. The units that differ from the baseline. Edits that are already off stay off.
	edits, toggles := r.unitsSince(w.Root, baseline, cur)
	var units, forced []string
	for _, e := range edits {
		if cur.on(e) {
			units = append(units, e)
		} else {
			forced = append(forced, e)
		}
	}
	units = append(units, toggles...)
	res.Among = len(units)
	isUnit := map[string]bool{}
	for _, u := range units {
		isUnit[u] = true
	}
	// A change that edits lines another unit wrote is kept together with it.
	needs := func(id string) []string {
		var out []string
		m := r.meta(id)
		for _, f := range m.Files {
			for _, op := range r.weave(f).ops[id] {
				for _, ref := range []string{changeOf(op.A), changeOf(op.E)} {
					if ref != "" && ref != id && isUnit[ref] {
						out = append(out, ref)
					}
				}
			}
		}
		return out
	}
	closure := func(keep []string) map[string]bool {
		on := map[string]bool{}
		for queue := append([]string{}, keep...); len(queue) > 0; queue = queue[1:] {
			if x := queue[0]; !on[x] {
				on[x] = true
				queue = append(queue, needs(x)...)
			}
		}
		return on
	}
	test := func(keep []string) (bool, error) {
		on := closure(keep)
		off := append([]string{}, forced...)
		for _, u := range units {
			if !on[u] {
				off = append(off, u)
			}
		}
		return fails(off)
	}

	// 3. Delta debugging: shrink to a set where removing any one part makes the check pass.
	keep := append([]string{}, units...)
	n := 2
	for len(keep) >= 2 {
		size := (len(keep) + n - 1) / n
		var chunks [][]string
		for i := 0; i < len(keep); i += size {
			chunks = append(chunks, keep[i:min(i+size, len(keep))])
		}
		reduced := false
		for _, c := range chunks {
			bad, err := test(c)
			if err != nil {
				return nil, err
			}
			if bad {
				keep, n, reduced = append([]string{}, c...), 2, true
				break
			}
		}
		if !reduced && len(chunks) > 2 {
			for i := range chunks {
				var rest []string
				for j, c := range chunks {
					if j != i {
						rest = append(rest, c...)
					}
				}
				bad, err := test(rest)
				if err != nil {
					return nil, err
				}
				if bad {
					keep, n, reduced = rest, max(n-1, 2), true
					break
				}
			}
		}
		if !reduced {
			if n >= len(keep) {
				break
			}
			n = min(len(keep), 2*n)
		}
	}
	// 4. A change is kept together with the changes whose lines it edits. Trim
	//    that set from the newest end: drop any change nothing else in the set
	//    needs, as long as the check still fails without it.
	set := closure(keep)
	for changed := true; changed; {
		changed = false
		needed := map[string]bool{}
		for id := range set {
			for _, d := range needs(id) {
				if set[d] {
					needed[d] = true
				}
			}
		}
		var tips []string
		for id := range set {
			if !needed[id] {
				tips = append(tips, id)
			}
		}
		sort.Strings(tips)
		for _, tip := range tips {
			if len(set) == 1 {
				break
			}
			var rest []string
			for id := range set {
				if id != tip {
					rest = append(rest, id)
				}
			}
			bad, err := test(rest)
			if err != nil {
				return nil, err
			}
			if bad {
				delete(set, tip)
				changed = true
				break
			}
		}
	}
	for id := range set {
		res.Culprits = append(res.Culprits, id)
	}
	sort.Strings(res.Culprits)

	// 5. Optionally hold them back, as an attributed, reversible change.
	if hold {
		in.Intent = fmt.Sprintf("hold back: fails `%s`", cmd)
		m, err := r.toggle(w, append([]string{}, res.Culprits...), in)
		if err != nil {
			return nil, err
		}
		res.Held = m.ID
	}
	return res, nil
}
