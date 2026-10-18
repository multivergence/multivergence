// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package pmap

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// check verifies the treap's order, heap and size fields.
func check(t *testing.T, n *node) {
	t.Helper()
	var walk func(n *node, lo, hi *string) int
	walk = func(n *node, lo, hi *string) int {
		if n == nil {
			return 0
		}
		if (lo != nil && n.key <= *lo) || (hi != nil && n.key >= *hi) {
			t.Fatalf("order broken at %q", n.key)
		}
		if n.prio != Priority(n.key) {
			t.Fatalf("priority of %q is stale", n.key)
		}
		for _, c := range []*node{n.left, n.right} {
			if c != nil && !above(n.prio, n.key, c.prio, c.key) {
				t.Fatalf("heap broken at %q over %q", n.key, c.key)
			}
		}
		s := 1 + walk(n.left, lo, &n.key) + walk(n.right, &n.key, hi)
		if s != n.size {
			t.Fatalf("size of %q is %d, counted %d", n.key, n.size, s)
		}
		return s
	}
	walk(n, nil, nil)
}

func contents(m Map) map[string]string {
	out := map[string]string{}
	m.Range("", func(k string, v []byte) bool { out[k] = string(v); return true })
	return out
}

func TestAgainstModel(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var m Map
	model := map[string]string{}
	versions := []Map{}
	snapshots := []map[string]string{}
	for i := 0; i < 20000; i++ {
		k := fmt.Sprintf("k%03d", rng.Intn(500))
		switch rng.Intn(3) {
		case 0, 1:
			v := fmt.Sprint(i)
			m = m.Set(k, []byte(v))
			model[k] = v
		case 2:
			m = m.Delete(k)
			delete(model, k)
		}
		if i%997 == 0 {
			check(t, m.root)
			versions = append(versions, m)
			cp := map[string]string{}
			for k, v := range model {
				cp[k] = v
			}
			snapshots = append(snapshots, cp)
		}
	}
	check(t, m.root)
	if m.Len() != len(model) {
		t.Fatalf("len %d, model %d", m.Len(), len(model))
	}
	for k, v := range model {
		got, ok := m.Get(k)
		if !ok || string(got) != v {
			t.Fatalf("get %q = %q %v, want %q", k, got, ok, v)
		}
	}
	// Old versions are untouched by later changes.
	for i, old := range versions {
		got := contents(old)
		if fmt.Sprint(got) != fmt.Sprint(snapshots[i]) {
			t.Fatalf("version %d changed", i)
		}
	}
}

func TestCanonicalShape(t *testing.T) {
	keys := []string{}
	for i := 0; i < 2000; i++ {
		keys = append(keys, fmt.Sprintf("key-%d", i))
	}
	var a Map
	for _, k := range keys {
		a = a.Set(k, []byte(k))
	}
	rng := rand.New(rand.NewSource(2))
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	var b Map
	for _, k := range keys {
		b = b.Set(k, []byte("x"))
		b = b.Set("extra-"+k, nil)
	}
	for _, k := range keys {
		b = b.Set(k, []byte(k))
		b = b.Delete("extra-" + k)
	}
	if !Equal(a, b) {
		t.Fatal("same entries, different maps")
	}
	var shape func(n *node) string
	shape = func(n *node) string {
		if n == nil {
			return "."
		}
		return "(" + shape(n.left) + n.key + shape(n.right) + ")"
	}
	if shape(a.root) != shape(b.root) {
		t.Fatal("same entries, different shapes")
	}
	if Equal(a, a.Set("key-1", []byte("other"))) {
		t.Fatal("different values compared equal")
	}
}

func TestRangePrefix(t *testing.T) {
	var m Map
	all := []string{"a", "ab", "abc", "abd", "ac", "b", "ba", "", "a/b", "a\xff"}
	for _, k := range all {
		m = m.Set(k, []byte(k))
	}
	for _, p := range []string{"", "a", "ab", "abc", "b", "c", "a/", "\xff"} {
		var got []string
		m.Range(p, func(k string, _ []byte) bool { got = append(got, k); return true })
		var want []string
		for _, k := range all {
			if strings.HasPrefix(k, p) {
				want = append(want, k)
			}
		}
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("prefix %q: got %q want %q", p, got, want)
		}
	}
	n := 0
	m.Range("", func(string, []byte) bool { n++; return n < 3 })
	if n != 3 {
		t.Fatalf("stopping early visited %d", n)
	}
}

func TestSharing(t *testing.T) {
	var base Map
	for i := 0; i < 10000; i++ {
		base = base.Set(fmt.Sprintf("row/%05d", i), []byte("payload"))
	}
	maps := []Map{base}
	for b := 0; b < 100; b++ {
		m := base
		for c := 0; c < 3; c++ {
			m = m.Set(fmt.Sprintf("row/%05d", (b*37+c*101)%10000), []byte(fmt.Sprint(b)))
		}
		maps = append(maps, m)
	}
	total := Count(maps...)
	// 100 branches of three changes each cost a few paths, not 100 copies.
	if total > 10000+100*3*40 {
		t.Fatalf("%d distinct nodes for 101 maps of 10000 entries", total)
	}
	t.Logf("101 maps of 10000 entries share %d nodes", total)
}

func TestVisitorBuilderRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var base Map
	for i := 0; i < 3000; i++ {
		base = base.Set(fmt.Sprint(rng.Intn(100000)), []byte(fmt.Sprint(i)))
	}
	maps := []Map{base, {}}
	for b := 0; b < 20; b++ {
		m := base
		for c := 0; c < 50; c++ {
			if rng.Intn(4) == 0 {
				m = m.Delete(fmt.Sprint(rng.Intn(100000)))
			} else {
				m = m.Set(fmt.Sprint(rng.Intn(100000)), []byte("b"))
			}
		}
		maps = append(maps, m)
	}
	v := NewVisitor()
	roots := []int{}
	for _, m := range maps {
		roots = append(roots, v.Add(m))
	}
	var b Builder
	for _, r := range v.Order {
		if !b.Push(r) {
			t.Fatal("valid node rejected")
		}
	}
	rebuilt := []Map{}
	for i, id := range roots {
		m, ok := b.Map(id)
		if !ok || !Equal(m, maps[i]) {
			t.Fatalf("map %d differs after rebuild", i)
		}
		check(t, m.root)
		rebuilt = append(rebuilt, m)
	}
	if Count(rebuilt...) != Count(maps...) {
		t.Fatal("sharing lost in rebuild")
	}
}

func TestBuilderRejectsBadTrees(t *testing.T) {
	var b Builder
	if !b.Push(NodeRef{Key: "m"}) {
		t.Fatal("leaf rejected")
	}
	// A right child with a smaller key breaks the order.
	if b.Push(NodeRef{Key: "z", Left: 0, Right: 1}) {
		t.Fatal("misordered child accepted")
	}
	if b.Push(NodeRef{Key: "q", Left: 5}) {
		t.Fatal("unknown child accepted")
	}
	// Find a key whose priority is lower than m's: it can't sit above m.
	pm := Priority("m")
	for i := 0; ; i++ {
		k := fmt.Sprintf("z%d", i)
		if Priority(k) < pm {
			if b.Push(NodeRef{Key: k, Left: 1}) {
				t.Fatal("heap order broken but accepted")
			}
			break
		}
	}
}

func BenchmarkSet(b *testing.B) {
	var m Map
	for i := 0; i < 100000; i++ {
		m = m.Set(fmt.Sprintf("k%07d", i), []byte("v"))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m = m.Set(fmt.Sprintf("k%07d", i%100000), []byte("w"))
	}
}
