package main

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

type memNodes map[string]*node

func (m memNodes) getNode(h string) *node { return m[h] }
func (m memNodes) putNode(n *node) string {
	h := nodeHash(encodeNode(n))
	m[h] = n
	return h
}

func TestTrieIsCanonicalAndCorrect(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	s := memNodes{}
	for it := 0; it < 40; it++ {
		n := rng.Intn(600)
		var es []entry
		for i := 0; i < n; i++ {
			es = append(es, entry{ID: fmt.Sprintf("%024x", rng.Uint64()>>uint(rng.Intn(40))), L: rng.Intn(100), T: rng.Intn(20) == 0})
		}
		uniq := map[string]entry{}
		for _, e := range es {
			if _, ok := uniq[e.ID]; !ok {
				uniq[e.ID] = e
			}
		}
		// Same set built in two orders, and as a union of two halves, has one root.
		a, b := "", ""
		for _, e := range es {
			a = trieInsert(s, a, uniq[e.ID])
		}
		for _, i := range rng.Perm(len(es)) {
			b = trieInsert(s, b, uniq[es[i].ID])
		}
		x, y := "", ""
		cut := 0
		if n > 0 {
			cut = rng.Intn(n)
		}
		for i, e := range es {
			if i < cut || i%7 == 0 {
				x = trieInsert(s, x, uniq[e.ID])
			}
			if i >= cut {
				y = trieInsert(s, y, uniq[e.ID])
			}
		}
		u1, u2 := trieUnion(s, x, y), trieUnion(s, y, x)
		if a != b || a != u1 || a != u2 {
			t.Fatalf("round %d: roots differ: %s %s %s %s", it, a, b, u1, u2)
		}
		if trieCount(s, a) != len(uniq) {
			t.Fatalf("count %d want %d", trieCount(s, a), len(uniq))
		}
		maxL, toggles := 0, 0
		for _, e := range uniq {
			if !trieHas(s, a, e.ID) {
				t.Fatalf("missing %s", e.ID)
			}
			if e.L > maxL {
				maxL = e.L
			}
			if e.T {
				toggles++
			}
		}
		got := 0
		trieToggles(s, a, func(entry) { got++ })
		if trieLamport(s, a) != maxL || got != toggles || trieHas(s, a, "ffffffffffffffffffffffff") {
			t.Fatalf("summaries wrong")
		}
		// Diff: entries of the whole set that x lacks.
		var want, have []string
		inX := map[string]bool{}
		trieEach(s, x, func(e entry) { inX[e.ID] = true })
		for id := range uniq {
			if !inX[id] {
				want = append(want, id)
			}
		}
		trieDiff(s, a, x, func(e entry) { have = append(have, e.ID) })
		sort.Strings(want)
		sort.Strings(have)
		if fmt.Sprint(want) != fmt.Sprint(have) {
			t.Fatalf("diff wrong: %d vs %d", len(want), len(have))
		}
	}
}
