// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/multivergence/multivergence/internal/pmap"
)

// digest hashes everything a state holds: counters, the trunk, and every
// branch with its weight's exact bits, its constraints, deadline and fate.
func digest(s *state) string {
	h := sha256.New()
	e := &enc{}
	flush := func() { h.Write(e.b); e.b = e.b[:0] }
	writeMap := func(m pmap.Map) {
		e.u(uint64(m.Len()))
		m.Range("", func(k string, v []byte) bool {
			e.str(k)
			e.bytes(v)
			flush()
			return true
		})
	}
	e.u(s.gen)
	e.u(s.nextSeq)
	e.u(s.trunkVer)
	writeMap(s.trunk)
	e.u(uint64(len(s.order)))
	for _, name := range s.order {
		b := s.branches[name]
		e.str(b.name)
		e.str(b.parent)
		e.u(b.seq)
		e.f(b.weight)
		e.u(b.base)
		encodeCons(e, b.cons)
		e.i(b.deadline)
		e.i(b.created)
		e.bool1(b.dead)
		e.byte1(byte(b.cause))
		e.str(b.detail)
		e.i(b.died)
		writeMap(b.data)
	}
	flush()
	return hex.EncodeToString(h.Sum(nil))
}
