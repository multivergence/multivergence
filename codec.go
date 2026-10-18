// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"encoding/binary"
	"math"

	"github.com/multivergence/multivergence/internal/pmap"
)

// recSnapshot marks a snapshot record, the first record of a compacted file.
const recSnapshot byte = 0x7F

type enc struct{ b []byte }

func (e *enc) byte1(v byte)   { e.b = append(e.b, v) }
func (e *enc) u(v uint64)     { e.b = binary.AppendUvarint(e.b, v) }
func (e *enc) i(v int64)      { e.b = binary.AppendVarint(e.b, v) }
func (e *enc) f(v float64)    { e.b = binary.LittleEndian.AppendUint64(e.b, math.Float64bits(v)) }
func (e *enc) bytes(v []byte) { e.u(uint64(len(v))); e.b = append(e.b, v...) }
func (e *enc) str(v string)   { e.u(uint64(len(v))); e.b = append(e.b, v...) }
func (e *enc) bool1(v bool)   { e.byte1(b2u(v)) }
func b2u(v bool) byte {
	if v {
		return 1
	}
	return 0
}

type dec struct {
	b   []byte
	bad bool
}

func (d *dec) byte1() byte {
	if len(d.b) < 1 {
		d.bad = true
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *dec) u() uint64 {
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.bad = true
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) i() int64 {
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.bad = true
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *dec) f() float64 {
	if len(d.b) < 8 {
		d.bad = true
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(d.b))
	d.b = d.b[8:]
	return v
}

func (d *dec) bytes() []byte {
	n := d.u()
	if d.bad || n > uint64(len(d.b)) {
		d.bad = true
		return nil
	}
	v := append([]byte(nil), d.b[:n]...)
	d.b = d.b[n:]
	return v
}

func (d *dec) str() string { return string(d.bytes()) }

func (d *dec) bool1() bool {
	switch d.byte1() {
	case 0:
		return false
	case 1:
		return true
	}
	d.bad = true
	return false
}

// count reads a length and checks it against the bytes left, at least min
// bytes per item, so a corrupt length can't ask for a huge allocation.
func (d *dec) count(min int) int {
	n := d.u()
	if d.bad || n > uint64(len(d.b)/min) {
		d.bad = true
		return 0
	}
	return int(n)
}

func encodeOp(o *op) []byte {
	e := &enc{}
	e.byte1(byte(o.kind))
	e.i(o.now)
	switch o.kind {
	case opSet:
		e.str(o.branch)
		e.str(o.key)
		e.bytes(o.val)
	case opDelete:
		e.str(o.branch)
		e.str(o.key)
	case opFork:
		e.str(o.branch)
		e.str(o.from)
		e.f(o.weight)
	case opWeight:
		e.str(o.branch)
		e.f(o.weight)
	case opConstrain:
		e.str(o.branch)
		encodeCons(e, o.cons)
	case opExpire:
		e.str(o.branch)
		e.i(o.at)
	case opObserve:
		e.u(uint64(len(o.facts)))
		for _, f := range o.facts {
			e.str(f.Key)
			e.bool1(f.Delete)
			e.bytes(f.Value)
		}
		e.u(uint64(len(o.lik)))
		for _, l := range o.lik {
			e.str(l.name)
			e.f(l.x)
		}
		e.bool1(o.hasRest)
		e.f(o.rest)
		e.f(o.prune)
	case opDiscard, opPromote:
		e.str(o.branch)
	case opSweep:
	}
	return e.b
}

func encodeCons(e *enc, cs []Constraint) {
	e.u(uint64(len(cs)))
	for _, c := range cs {
		e.str(c.Key)
		e.byte1(byte(c.Op))
		e.str(c.Value)
	}
}

func decodeCons(d *dec) []Constraint {
	n := d.count(3)
	var cs []Constraint
	for k := 0; k < n && !d.bad; k++ {
		cs = append(cs, Constraint{Key: d.str(), Op: Op(d.byte1()), Value: d.str()})
	}
	return cs
}

func decodeOp(p []byte) (*op, bool) {
	d := &dec{b: p}
	o := &op{kind: opKind(d.byte1())}
	o.now = d.i()
	switch o.kind {
	case opSet:
		o.branch, o.key, o.val = d.str(), d.str(), d.bytes()
	case opDelete:
		o.branch, o.key = d.str(), d.str()
	case opFork:
		o.branch, o.from, o.weight = d.str(), d.str(), d.f()
	case opWeight:
		o.branch, o.weight = d.str(), d.f()
	case opConstrain:
		o.branch = d.str()
		o.cons = decodeCons(d)
	case opExpire:
		o.branch, o.at = d.str(), d.i()
	case opObserve:
		n := d.count(3)
		for k := 0; k < n && !d.bad; k++ {
			f := Fact{Key: d.str(), Delete: d.bool1()}
			f.Value = d.bytes()
			o.facts = append(o.facts, f)
		}
		n = d.count(9)
		for k := 0; k < n && !d.bad; k++ {
			o.lik = append(o.lik, likelihood{d.str(), d.f()})
		}
		o.hasRest = d.bool1()
		o.rest = d.f()
		o.prune = d.f()
	case opDiscard, opPromote:
		o.branch = d.str()
	case opSweep:
	default:
		return nil, false
	}
	if d.bad || len(d.b) != 0 {
		return nil, false
	}
	return o, true
}

// encodeSnapshot writes the whole state and the kept history as one record.
// Map nodes shared between the trunk and the branches are written once, so
// the file keeps the sharing and a reload gets it back.
func encodeSnapshot(s *state, events []Event, eventSeq uint64) []byte {
	e := &enc{}
	e.byte1(recSnapshot)
	e.u(s.gen)
	e.u(s.nextSeq)
	e.u(s.trunkVer)
	e.u(eventSeq)

	v := pmap.NewVisitor()
	trunkID := v.Add(s.trunk)
	ids := make([]int, len(s.order))
	for k, name := range s.order {
		ids[k] = v.Add(s.branches[name].data)
	}
	e.u(uint64(len(v.Order)))
	for _, n := range v.Order {
		e.str(n.Key)
		e.bytes(n.Val)
		e.u(uint64(n.Left))
		e.u(uint64(n.Right))
	}
	e.u(uint64(trunkID))

	e.u(uint64(len(s.order)))
	for k, name := range s.order {
		b := s.branches[name]
		e.str(b.name)
		e.str(b.parent)
		e.u(b.seq)
		e.u(uint64(ids[k]))
		e.f(b.weight)
		e.u(b.base)
		encodeCons(e, b.cons)
		e.i(b.deadline)
		e.i(b.created)
		e.bool1(b.dead)
		e.byte1(byte(b.cause))
		e.str(b.detail)
		e.i(b.died)
	}

	e.u(uint64(len(events)))
	for _, ev := range events {
		e.u(ev.Seq)
		e.i(ev.Time.UnixNano())
		e.u(ev.Generation)
		e.str(ev.Kind)
		e.str(ev.Branch)
		e.str(ev.Detail)
	}
	return e.b
}

func decodeSnapshot(p []byte) (*state, []Event, uint64, bool) {
	d := &dec{b: p}
	if d.byte1() != recSnapshot {
		return nil, nil, 0, false
	}
	s := newState()
	s.gen = d.u()
	s.nextSeq = d.u()
	s.trunkVer = d.u()
	eventSeq := d.u()

	var bld pmap.Builder
	n := d.count(4)
	for k := 0; k < n && !d.bad; k++ {
		r := pmap.NodeRef{Key: d.str(), Val: d.bytes()}
		r.Left, r.Right = int(d.u()), int(d.u())
		if d.bad || !bld.Push(r) {
			return nil, nil, 0, false
		}
	}
	var ok bool
	if s.trunk, ok = bld.Map(int(d.u())); !ok {
		return nil, nil, 0, false
	}

	nb := d.count(20)
	for k := 0; k < nb && !d.bad; k++ {
		b := &branch{name: d.str(), parent: d.str(), seq: d.u()}
		if b.data, ok = bld.Map(int(d.u())); !ok {
			return nil, nil, 0, false
		}
		b.weight = d.f()
		b.base = d.u()
		b.cons = decodeCons(d)
		b.deadline = d.i()
		b.created = d.i()
		b.dead = d.bool1()
		b.cause = Cause(d.byte1())
		b.detail = d.str()
		b.died = d.i()
		if _, dup := s.branches[b.name]; dup || d.bad || !sensible(s, b) {
			return nil, nil, 0, false
		}
		s.branches[b.name] = b
		s.order = append(s.order, b.name)
	}

	ne := d.count(6)
	events := make([]Event, 0, ne)
	for k := 0; k < ne && !d.bad; k++ {
		ev := Event{Seq: d.u()}
		ev.Time = unixUTC(d.i())
		ev.Generation = d.u()
		ev.Kind, ev.Branch, ev.Detail = d.str(), d.str(), d.str()
		events = append(events, ev)
	}
	if d.bad || len(d.b) != 0 {
		return nil, nil, 0, false
	}
	return s, events, eventSeq, true
}

// sensible checks a decoded branch the way the changes that made it were
// checked, so a damaged snapshot that still passes its checksum can't load
// a state the engine could never have made.
func sensible(s *state, b *branch) bool {
	if validName(b.name) != nil {
		return false
	}
	if _, ok := s.branches[b.parent]; !ok && b.parent != Trunk {
		return false
	}
	for _, c := range b.cons {
		if c.validate() != nil {
			return false
		}
	}
	if b.dead {
		return b.cause >= CauseViolated && b.cause <= CauseDiscarded &&
			b.weight >= 0 && !math.IsInf(b.weight, 0) && !math.IsNaN(b.weight)
	}
	return b.cause == CauseNone && goodWeight(b.weight) && b.detail == "" && b.died == 0
}
