package main

// Line diff (Myers O(ND)), after trimming the common prefix and suffix.
// Returns an edit script over a and b: 'k' keep, 'd' delete next of a, 'i' insert next of b.

const maxD = 4000 // beyond this many edits, replace the whole middle instead of searching

func diffLines(a, b []string) []byte {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	out := make([]byte, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		out = append(out, 'k')
	}
	out = append(out, myers(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for i := 0; i < suf; i++ {
		out = append(out, 'k')
	}
	return out
}

func myers(a, b []string) []byte {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return replaceAll(n, m)
	}
	// Intern lines so comparisons are integer compares.
	ids := map[string]int{}
	intern := func(xs []string) []int {
		r := make([]int, len(xs))
		for i, s := range xs {
			id, ok := ids[s]
			if !ok {
				id = len(ids)
				ids[s] = id
			}
			r[i] = id
		}
		return r
	}
	x, y := intern(a), intern(b)
	max := n + m
	if max > maxD {
		max = maxD
	}
	off := max + 1
	v := make([]int, 2*max+3)
	var trace [][]int // trace[d] holds v[-d-1..d+1] as it was before round d
	found := false
	for d := 0; d <= max && !found; d++ {
		snap := make([]int, 2*d+3)
		copy(snap, v[off-d-1:off+d+2])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var px int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				px = v[off+k+1]
			} else {
				px = v[off+k-1] + 1
			}
			py := px - k
			for px < n && py < m && x[px] == y[py] {
				px++
				py++
			}
			v[off+k] = px
			if px >= n && py >= m {
				found = true
				break
			}
		}
	}
	if !found {
		return replaceAll(n, m)
	}
	rev := make([]byte, 0, n+m)
	cx, cy := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		t := trace[d]
		at := func(k int) int { return t[k+d+1] }
		k := cx - cy
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		if d == 0 {
			px, py = 0, 0
		}
		for cx > px && cy > py {
			rev = append(rev, 'k')
			cx--
			cy--
		}
		if d > 0 {
			if cx == px {
				rev = append(rev, 'i')
				cy--
			} else {
				rev = append(rev, 'd')
				cx--
			}
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

func replaceAll(n, m int) []byte {
	out := make([]byte, 0, n+m)
	for i := 0; i < n; i++ {
		out = append(out, 'd')
	}
	for i := 0; i < m; i++ {
		out = append(out, 'i')
	}
	return out
}
