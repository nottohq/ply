package main

// A state is a set of change ids. It is stored as a Merkle trie keyed by the
// hex digits of the id, so that:
//   - the root hash names the set (same set, same hash, whatever order it grew in);
//   - membership is a short walk;
//   - "what do you have that I don't" skips every subtree both sides share.
// Sets only ever grow: switching a change off is itself a change (see toggles).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

const leafMax = 32

type entry struct {
	ID string `json:"i"`
	L  int    `json:"l"`           // lamport
	T  bool   `json:"t,omitempty"` // toggle (switches other changes off)
}

type node struct {
	Leaf []entry     `json:"e,omitempty"`
	Kids *[16]string `json:"k,omitempty"`
	N    int         `json:"n"` // entries below
	L    int         `json:"m"` // highest lamport below
	T    int         `json:"t"` // toggles below
}

type nodeStore interface {
	getNode(hash string) *node
	putNode(n *node) string
}

func nodeHash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:12]) }

func encodeNode(n *node) []byte { b, _ := json.Marshal(n); return b }

func nib(id string, depth int) int {
	c := id[depth]
	if c >= 'a' {
		return int(c-'a') + 10
	}
	return int(c - '0')
}

func leafOf(es []entry) *node {
	n := &node{Leaf: es, N: len(es)}
	for _, e := range es {
		if e.L > n.L {
			n.L = e.L
		}
		if e.T {
			n.T++
		}
	}
	return n
}

func build(s nodeStore, es []entry, depth int) string {
	if len(es) == 0 {
		return ""
	}
	if len(es) <= leafMax || depth >= len(es[0].ID) {
		sort.Slice(es, func(i, j int) bool { return es[i].ID < es[j].ID })
		return s.putNode(leafOf(es))
	}
	var parts [16][]entry
	for _, e := range es {
		k := nib(e.ID, depth)
		parts[k] = append(parts[k], e)
	}
	var kids [16]string
	for i := range parts {
		kids[i] = build(s, parts[i], depth+1)
	}
	return s.putNode(branchOf(s, &kids))
}

func branchOf(s nodeStore, kids *[16]string) *node {
	n := &node{Kids: kids}
	for _, k := range kids {
		if k == "" {
			continue
		}
		c := s.getNode(k)
		n.N += c.N
		n.T += c.T
		if c.L > n.L {
			n.L = c.L
		}
	}
	return n
}

func trieInsert(s nodeStore, root string, e entry) string { return insertAt(s, root, e, 0) }

func insertAt(s nodeStore, root string, e entry, depth int) string {
	if root == "" {
		return s.putNode(leafOf([]entry{e}))
	}
	n := s.getNode(root)
	if n.Kids == nil {
		for _, x := range n.Leaf {
			if x.ID == e.ID {
				return root
			}
		}
		es := append(append([]entry{}, n.Leaf...), e)
		return build(s, es, depth)
	}
	kids := *n.Kids
	k := nib(e.ID, depth)
	next := insertAt(s, kids[k], e, depth+1)
	if next == kids[k] {
		return root
	}
	kids[k] = next
	return s.putNode(branchOf(s, &kids))
}

func trieHas(s nodeStore, root, id string) bool {
	for depth := 0; root != ""; depth++ {
		n := s.getNode(root)
		if n.Kids == nil {
			for _, x := range n.Leaf {
				if x.ID == id {
					return true
				}
			}
			return false
		}
		root = n.Kids[nib(id, depth)]
	}
	return false
}

func trieEach(s nodeStore, root string, fn func(entry)) {
	if root == "" {
		return
	}
	n := s.getNode(root)
	if n.Kids == nil {
		for _, e := range n.Leaf {
			fn(e)
		}
		return
	}
	for _, k := range n.Kids {
		trieEach(s, k, fn)
	}
}

// trieToggles visits only the toggles, skipping subtrees that hold none.
func trieToggles(s nodeStore, root string, fn func(entry)) {
	if root == "" {
		return
	}
	n := s.getNode(root)
	if n.T == 0 {
		return
	}
	if n.Kids == nil {
		for _, e := range n.Leaf {
			if e.T {
				fn(e)
			}
		}
		return
	}
	for _, k := range n.Kids {
		trieToggles(s, k, fn)
	}
}

// trieDiff visits every entry of a that is not in b.
func trieDiff(s nodeStore, a, b string, fn func(entry)) { diffAt(s, a, b, 0, fn) }

func diffAt(s nodeStore, a, b string, depth int, fn func(entry)) {
	if a == b || a == "" {
		return
	}
	if b == "" {
		trieEach(s, a, fn)
		return
	}
	na, nb := s.getNode(a), s.getNode(b)
	switch {
	case na.Kids != nil && nb.Kids != nil:
		for i := range na.Kids {
			diffAt(s, na.Kids[i], nb.Kids[i], depth+1, fn)
		}
	case na.Kids == nil:
		for _, e := range na.Leaf {
			if !hasAt(s, b, e.ID, depth) {
				fn(e)
			}
		}
	default:
		skip := map[string]bool{}
		for _, e := range nb.Leaf {
			skip[e.ID] = true
		}
		trieEach(s, a, func(e entry) {
			if !skip[e.ID] {
				fn(e)
			}
		})
	}
}

func hasAt(s nodeStore, root, id string, depth int) bool {
	for ; root != ""; depth++ {
		n := s.getNode(root)
		if n.Kids == nil {
			for _, x := range n.Leaf {
				if x.ID == id {
					return true
				}
			}
			return false
		}
		root = n.Kids[nib(id, depth)]
	}
	return false
}

// trieUnion returns the set holding everything in a or b.
func trieUnion(s nodeStore, a, b string) string { return unionAt(s, a, b, 0) }

func unionAt(s nodeStore, a, b string, depth int) string {
	if a == b || b == "" {
		return a
	}
	if a == "" {
		return b
	}
	na, nb := s.getNode(a), s.getNode(b)
	if na.Kids != nil && nb.Kids != nil {
		var kids [16]string
		for i := range kids {
			kids[i] = unionAt(s, na.Kids[i], nb.Kids[i], depth+1)
		}
		return s.putNode(branchOf(s, &kids))
	}
	if na.Kids == nil { // fold the leaf into the other side
		a, b, na = b, a, nb
	}
	out := a
	trieEach(s, b, func(e entry) { out = insertAt(s, out, e, depth) })
	return out
}

func trieCount(s nodeStore, root string) int {
	if root == "" {
		return 0
	}
	return s.getNode(root).N
}

func trieLamport(s nodeStore, root string) int {
	if root == "" {
		return 0
	}
	return s.getNode(root).L
}
