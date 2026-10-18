// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package pmap is a persistent, copy-on-write sorted map from string keys to
// byte values. Every change returns a new Map and leaves the old one as it
// was; the two share every node the change didn't touch, so a branch that
// changes three keys of a million costs about three paths of nodes, not a
// million entries.
//
// The tree is a treap whose priorities come from a hash of the key. For a
// given set of keys there is exactly one such tree, whatever order the keys
// were added in, so two maps holding the same entries have the same shape.
package pmap

import (
	"bytes"
	"strings"
)

type node struct {
	key         string
	val         []byte
	prio        uint64
	left, right *node
	size        int
}

// Map is an immutable sorted map. The zero Map is empty and ready to use.
type Map struct {
	root *node
}

// Priority is the hash a key's place in the tree comes from: FNV-1a 64,
// then the splitmix64 finaliser to spread the bits.
func Priority(key string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	h ^= h >> 31
	return h
}

// above reports whether a node with priority pa and key ka sits above one
// with pb and kb. Equal priorities are broken by key, so the order is total
// and the tree is unique.
func above(pa uint64, ka string, pb uint64, kb string) bool {
	if pa != pb {
		return pa > pb
	}
	return ka < kb
}

func size(n *node) int {
	if n == nil {
		return 0
	}
	return n.size
}

func mk(key string, val []byte, prio uint64, l, r *node) *node {
	return &node{key: key, val: val, prio: prio, left: l, right: r, size: 1 + size(l) + size(r)}
}

// Len is the number of entries.
func (m Map) Len() int { return size(m.root) }

// Get returns the value stored under key. The slice is shared with the map
// and must not be changed.
func (m Map) Get(key string) ([]byte, bool) {
	n := m.root
	for n != nil {
		switch {
		case key < n.key:
			n = n.left
		case key > n.key:
			n = n.right
		default:
			return n.val, true
		}
	}
	return nil, false
}

// Set returns a map with key set to val. The map keeps val as it is, so the
// caller must not change it afterwards.
func (m Map) Set(key string, val []byte) Map {
	return Map{root: insert(m.root, key, val, Priority(key))}
}

func insert(n *node, key string, val []byte, prio uint64) *node {
	if n == nil {
		return mk(key, val, prio, nil, nil)
	}
	if key == n.key {
		return mk(n.key, val, n.prio, n.left, n.right)
	}
	if above(prio, key, n.prio, n.key) {
		l, r := split(n, key)
		return mk(key, val, prio, l, r)
	}
	if key < n.key {
		return mk(n.key, n.val, n.prio, insert(n.left, key, val, prio), n.right)
	}
	return mk(n.key, n.val, n.prio, n.left, insert(n.right, key, val, prio))
}

// split divides n into the keys below key and the keys above it. key itself
// is not in n when this is called.
func split(n *node, key string) (*node, *node) {
	if n == nil {
		return nil, nil
	}
	if n.key < key {
		l, r := split(n.right, key)
		return mk(n.key, n.val, n.prio, n.left, l), r
	}
	l, r := split(n.left, key)
	return l, mk(n.key, n.val, n.prio, r, n.right)
}

// Delete returns a map without key. If key isn't there, it returns m itself.
func (m Map) Delete(key string) Map {
	root, found := remove(m.root, key)
	if !found {
		return m
	}
	return Map{root: root}
}

func remove(n *node, key string) (*node, bool) {
	if n == nil {
		return nil, false
	}
	switch {
	case key < n.key:
		l, ok := remove(n.left, key)
		if !ok {
			return n, false
		}
		return mk(n.key, n.val, n.prio, l, n.right), true
	case key > n.key:
		r, ok := remove(n.right, key)
		if !ok {
			return n, false
		}
		return mk(n.key, n.val, n.prio, n.left, r), true
	}
	return merge(n.left, n.right), true
}

// merge joins two trees where every key of a is below every key of b.
func merge(a, b *node) *node {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if above(a.prio, a.key, b.prio, b.key) {
		return mk(a.key, a.val, a.prio, a.left, merge(a.right, b))
	}
	return mk(b.key, b.val, b.prio, merge(a, b.left), b.right)
}

// Range calls fn for each entry whose key starts with prefix, in key order,
// until fn returns false.
func (m Map) Range(prefix string, fn func(key string, val []byte) bool) {
	rangeNode(m.root, prefix, fn)
}

// rangeNode walks the subtree in order and reports whether to go on. The
// keys with the prefix are one contiguous run that starts at the prefix.
func rangeNode(n *node, prefix string, fn func(string, []byte) bool) bool {
	if n == nil {
		return true
	}
	if n.key < prefix {
		return rangeNode(n.right, prefix, fn)
	}
	if !rangeNode(n.left, prefix, fn) {
		return false
	}
	if !strings.HasPrefix(n.key, prefix) {
		return false
	}
	if !fn(n.key, n.val) {
		return false
	}
	return rangeNode(n.right, prefix, fn)
}

// Equal reports whether two maps hold the same entries. Because the shape is
// unique, shared subtrees are recognised by pointer and skipped.
func Equal(a, b Map) bool {
	return equalNode(a.root, b.root)
}

func equalNode(a, b *node) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.size != b.size || a.key != b.key || !bytes.Equal(a.val, b.val) {
		return false
	}
	return equalNode(a.left, b.left) && equalNode(a.right, b.right)
}

// Node is one tree node as Walk shows it, for writing a map out with its
// sharing kept.
type Node struct {
	Key   string
	Val   []byte
	Left  *Node
	Right *Node
}

// Visitor gives each distinct node of several maps an id, children before
// parents, so nodes shared between maps are written once.
type Visitor struct {
	ids   map[*node]int
	Order []NodeRef
}

// NodeRef is a node in the order a Visitor met it. Left and Right are the
// ids of its children plus one, with 0 for none.
type NodeRef struct {
	Key         string
	Val         []byte
	Left, Right int
}

// NewVisitor starts an empty visit.
func NewVisitor() *Visitor { return &Visitor{ids: map[*node]int{}} }

// Add visits a map's nodes and returns the id of its root plus one, or 0
// for an empty map.
func (v *Visitor) Add(m Map) int { return v.visit(m.root) }

func (v *Visitor) visit(n *node) int {
	if n == nil {
		return 0
	}
	if id, ok := v.ids[n]; ok {
		return id + 1
	}
	l := v.visit(n.left)
	r := v.visit(n.right)
	id := len(v.Order)
	v.ids[n] = id
	v.Order = append(v.Order, NodeRef{Key: n.key, Val: n.val, Left: l, Right: r})
	return id + 1
}

// Builder rebuilds maps from NodeRefs written by a Visitor, keeping the
// sharing between them.
type Builder struct {
	nodes []*node
}

// Push adds the next node. Its children must have been pushed already, and
// the result must still be a valid treap; Push reports false if not.
func (b *Builder) Push(r NodeRef) bool {
	var l, rt *node
	if r.Left < 0 || r.Left > len(b.nodes) || r.Right < 0 || r.Right > len(b.nodes) {
		return false
	}
	if r.Left > 0 {
		l = b.nodes[r.Left-1]
	}
	if r.Right > 0 {
		rt = b.nodes[r.Right-1]
	}
	p := Priority(r.Key)
	if l != nil && (l.key >= r.Key || !above(p, r.Key, l.prio, l.key) || maxKey(l) >= r.Key) {
		return false
	}
	if rt != nil && (rt.key <= r.Key || !above(p, r.Key, rt.prio, rt.key) || minKey(rt) <= r.Key) {
		return false
	}
	b.nodes = append(b.nodes, mk(r.Key, r.Val, p, l, rt))
	return true
}

func maxKey(n *node) string {
	for n.right != nil {
		n = n.right
	}
	return n.key
}

func minKey(n *node) string {
	for n.left != nil {
		n = n.left
	}
	return n.key
}

// Map returns the map rooted at id (a node id plus one, or 0 for empty).
func (b *Builder) Map(id int) (Map, bool) {
	if id == 0 {
		return Map{}, true
	}
	if id < 0 || id > len(b.nodes) {
		return Map{}, false
	}
	return Map{root: b.nodes[id-1]}, true
}

// Count is the number of distinct nodes reachable from the given maps: the
// memory the maps really take, with shared nodes counted once.
func Count(maps ...Map) int {
	v := NewVisitor()
	for _, m := range maps {
		v.Add(m)
	}
	return len(v.Order)
}
