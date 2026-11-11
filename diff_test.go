package main

import (
	"math/rand"
	"strings"
	"testing"
)

func applyScript(a, b []string, s []byte) ([]string, bool) {
	var out []string
	i, j := 0, 0
	for _, c := range s {
		switch c {
		case 'k':
			if i >= len(a) || j >= len(b) || a[i] != b[j] {
				return nil, false
			}
			out = append(out, a[i])
			i++
			j++
		case 'd':
			i++
		case 'i':
			out = append(out, b[j])
			j++
		}
	}
	return out, i == len(a) && j == len(b)
}

func TestDiffRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for it := 0; it < 3000; it++ {
		mk := func() []string {
			n := rng.Intn(30)
			xs := make([]string, n)
			for i := range xs {
				xs[i] = string(rune('a' + rng.Intn(5)))
			}
			return xs
		}
		a, b := mk(), mk()
		s := diffLines(a, b)
		got, ok := applyScript(a, b, s)
		if !ok || strings.Join(got, "") != strings.Join(b, "") {
			t.Fatalf("bad script %q for %v -> %v", s, a, b)
		}
		// Minimality: number of kept lines equals the LCS length.
		lcs := make([][]int, len(a)+1)
		for i := range lcs {
			lcs[i] = make([]int, len(b)+1)
		}
		for i := len(a) - 1; i >= 0; i-- {
			for j := len(b) - 1; j >= 0; j-- {
				if a[i] == b[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		if k := strings.Count(string(s), "k"); k != lcs[0][0] {
			t.Fatalf("not minimal: kept %d, lcs %d for %v -> %v", k, lcs[0][0], a, b)
		}
	}
}
