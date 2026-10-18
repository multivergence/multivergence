// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/multivergence/multivergence/internal/pmap"
)

// opKind is the type of one change. The numbers are written to files, so
// they never change.
type opKind byte

const (
	opSet       opKind = 1
	opDelete    opKind = 2
	opFork      opKind = 3
	opWeight    opKind = 4
	opConstrain opKind = 5
	opExpire    opKind = 6
	opObserve   opKind = 7
	opDiscard   opKind = 8
	opPromote   opKind = 9
	opSweep     opKind = 10
)

type likelihood struct {
	name string
	x    float64
}

// op is one change, exactly as the log records it. Applying the same ops in
// the same order to the same state always gives the same result.
type op struct {
	kind    opKind
	now     int64 // unix nanoseconds the change was made at
	branch  string
	from    string
	key     string
	val     []byte
	weight  float64
	cons    []Constraint
	at      int64 // Expire: the deadline, or 0 to clear it
	facts   []Fact
	lik     []likelihood // sorted by name
	hasRest bool
	rest    float64
	prune   float64
	// Not written to the log: ExpireIn's relative deadline, turned into at
	// once the change's time is known.
	rel bool
	in  time.Duration
}

type branch struct {
	name     string
	parent   string
	seq      uint64
	data     pmap.Map
	weight   float64
	base     uint64 // trunk version when its line was forked from the trunk
	cons     []Constraint
	deadline int64 // 0 for none
	created  int64
	dead     bool
	cause    Cause
	detail   string
	died     int64
}

// state is everything a store holds. A change never edits a state in place:
// it builds a new one that shares what didn't change, so a change that fails
// halfway leaves nothing behind.
type state struct {
	gen      uint64
	nextSeq  uint64
	trunk    pmap.Map
	trunkVer uint64
	branches map[string]*branch // this generation's branches, live and dead
	order    []string           // their names in the order they were made
}

func newState() *state { return &state{branches: map[string]*branch{}} }

func (s *state) clone() *state {
	t := *s
	t.branches = make(map[string]*branch, len(s.branches))
	for k, v := range s.branches {
		t.branches[k] = v
	}
	t.order = s.order[:len(s.order):len(s.order)]
	return &t
}

// mut returns a copy of the branch that this state owns and may change.
func (s *state) mut(name string) *branch {
	b := *s.branches[name]
	s.branches[name] = &b
	return &b
}

func (s *state) liveCount() int {
	n := 0
	for _, name := range s.order {
		if !s.branches[name].dead {
			n++
		}
	}
	return n
}

// eventDraft is an event before the store gives it a number and a time.
type eventDraft struct {
	kind, branch, detail string
	before               bool // belongs to the generation before this change
}

type applied struct {
	events []eventDraft
	deaths []Death
	killed *KilledError // set when the target branch died of this change
}

func (a *applied) event(kind, branch, detail string) {
	a.events = append(a.events, eventDraft{kind: kind, branch: branch, detail: detail})
}

func (s *state) kill(a *applied, name string, cause Cause, detail string, now int64) {
	b := s.mut(name)
	b.dead, b.cause, b.detail, b.died = true, cause, detail, now
	a.deaths = append(a.deaths, Death{Branch: name, Cause: cause, Detail: detail})
	d := cause.String()
	if detail != "" {
		d += ": " + detail
	}
	a.event("killed", name, d)
}

// sweep kills the live branches whose deadline has come.
func (s *state) sweep(a *applied, now int64) {
	for _, name := range s.order {
		b := s.branches[name]
		if !b.dead && b.deadline != 0 && b.deadline <= now {
			s.kill(a, name, CauseExpired, "", now)
		}
	}
}

// violated returns the first constraint of b that doesn't hold.
func violated(b *branch) (Constraint, bool) {
	for _, c := range b.cons {
		v, ok := b.data.Get(c.Key)
		if !c.holds(v, ok) {
			return c, true
		}
	}
	return Constraint{}, false
}

func validName(name string) error {
	if name == Trunk {
		return fmt.Errorf("%w: %q is the trunk", ErrBadName, name)
	}
	if name == "" || len(name) > MaxNameLen {
		return fmt.Errorf("%w: %q must be 1 to %d characters", ErrBadName, name, MaxNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || (c != '.' && c != '_' && c != '-')) {
			return fmt.Errorf("%w: %q may use letters, digits, '.', '_' and '-', and must start with a letter or digit", ErrBadName, name)
		}
	}
	return nil
}

func checkKey(key string) error {
	if key == "" || len(key) > MaxKeyLen || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
		return fmt.Errorf("%w: keys are 1 to %d bytes of UTF-8 without NUL", ErrBadKey, MaxKeyLen)
	}
	return nil
}

func goodWeight(w float64) bool { return w > 0 && !math.IsInf(w, 0) && !math.IsNaN(w) }

// live returns the named branch if it exists and is live.
func (s *state) live(name string) (*branch, error) {
	b, ok := s.branches[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if b.dead {
		return nil, fmt.Errorf("%w: %s died (%s)", ErrNotLive, name, b.cause)
	}
	return b, nil
}

// apply carries out one change and returns the new state. maxLive is the
// live-branch limit, or 0 to skip it, as replay does: a change in the log
// was allowed when it was made.
func apply(s *state, o *op, maxLive int) (*state, *applied, error) {
	t := s.clone()
	a := &applied{}
	t.sweep(a, o.now)
	var err error
	switch o.kind {
	case opSet, opDelete:
		err = t.write(a, o)
	case opFork:
		err = t.fork(a, o, maxLive)
	case opWeight:
		var b *branch
		if b, err = t.live(o.branch); err == nil {
			if !goodWeight(o.weight) {
				return nil, nil, ErrBadWeight
			}
			t.mut(b.name).weight = o.weight
			a.event("weight", o.branch, fmtNum(o.weight))
		}
	case opConstrain:
		err = t.constrain(a, o)
	case opExpire:
		err = t.expire(a, o)
	case opObserve:
		err = t.observe(a, o)
	case opDiscard:
		if _, err = t.live(o.branch); err == nil {
			t.kill(a, o.branch, CauseDiscarded, "", o.now)
		}
	case opPromote:
		err = t.promote(a, o)
	case opSweep:
	default:
		err = fmt.Errorf("%w: unknown change %d", ErrCorrupt, o.kind)
	}
	if err != nil {
		return nil, nil, err
	}
	return t, a, nil
}

func (t *state) write(a *applied, o *op) error {
	if err := checkKey(o.key); err != nil {
		return err
	}
	if len(o.val) > MaxValueLen {
		return ErrBadValue
	}
	if o.branch == Trunk {
		if o.kind == opSet {
			t.trunk = t.trunk.Set(o.key, o.val)
		} else {
			if _, ok := t.trunk.Get(o.key); !ok {
				return nil // nothing to delete, so nothing goes stale
			}
			t.trunk = t.trunk.Delete(o.key)
		}
		t.trunkVer++
		return nil
	}
	b, err := t.live(o.branch)
	if err != nil {
		return err
	}
	nb := t.mut(b.name)
	if o.kind == opSet {
		nb.data = nb.data.Set(o.key, o.val)
	} else {
		nb.data = nb.data.Delete(o.key)
	}
	if c, bad := violated(nb); bad {
		t.kill(a, nb.name, CauseViolated, c.String(), o.now)
		a.killed = &KilledError{Branch: nb.name, Cause: CauseViolated, Detail: c.String()}
	}
	return nil
}

func (t *state) fork(a *applied, o *op, maxLive int) error {
	if err := validName(o.branch); err != nil {
		return err
	}
	if _, ok := t.branches[o.branch]; ok {
		return fmt.Errorf("%w: %s", ErrExists, o.branch)
	}
	if !goodWeight(o.weight) {
		return ErrBadWeight
	}
	if maxLive > 0 && t.liveCount() >= maxLive {
		return fmt.Errorf("%w: the limit is %d", ErrTooManyBranches, maxLive)
	}
	nb := &branch{name: o.branch, parent: o.from, seq: t.nextSeq, weight: o.weight, created: o.now}
	if o.from == Trunk {
		nb.data, nb.base = t.trunk, t.trunkVer
	} else {
		p, err := t.live(o.from)
		if err != nil {
			return err
		}
		nb.data, nb.base = p.data, p.base
		nb.cons = append([]Constraint(nil), p.cons...)
		nb.deadline = p.deadline
	}
	t.nextSeq++
	t.branches[nb.name] = nb
	t.order = append(t.order, nb.name)
	a.event("fork", nb.name, fmt.Sprintf("from %s, weight %s", o.from, fmtNum(o.weight)))
	return nil
}

func (t *state) constrain(a *applied, o *op) error {
	b, err := t.live(o.branch)
	if err != nil {
		return err
	}
	if len(o.cons) == 0 {
		return fmt.Errorf("%w: none given", ErrBadConstraint)
	}
	for _, c := range o.cons {
		if err := c.validate(); err != nil {
			return err
		}
	}
	nb := t.mut(b.name)
	nb.cons = append(append([]Constraint(nil), nb.cons...), o.cons...)
	for _, c := range o.cons {
		a.event("constrain", nb.name, c.String())
	}
	if c, bad := violated(nb); bad {
		t.kill(a, nb.name, CauseViolated, c.String(), o.now)
		a.killed = &KilledError{Branch: nb.name, Cause: CauseViolated, Detail: c.String()}
	}
	return nil
}

func (t *state) expire(a *applied, o *op) error {
	b, err := t.live(o.branch)
	if err != nil {
		return err
	}
	nb := t.mut(b.name)
	nb.deadline = o.at
	if o.at == 0 {
		a.event("expire", nb.name, "no deadline")
		return nil
	}
	a.event("expire", nb.name, "at "+time.Unix(0, o.at).UTC().Format(time.RFC3339Nano))
	if o.at <= o.now {
		t.kill(a, nb.name, CauseExpired, "", o.now)
		a.killed = &KilledError{Branch: nb.name, Cause: CauseExpired}
	}
	return nil
}

func (t *state) observe(a *applied, o *op) error {
	for _, f := range o.facts {
		if err := checkKey(f.Key); err != nil {
			return err
		}
		if len(f.Value) > MaxValueLen {
			return ErrBadValue
		}
	}
	if o.prune < 0 || o.prune >= 1 || math.IsNaN(o.prune) {
		return ErrBadPrune
	}
	if o.hasRest && len(o.lik) > 0 && (o.rest < 0 || math.IsInf(o.rest, 0) || math.IsNaN(o.rest)) {
		return ErrBadLikelihood
	}
	lik := make(map[string]float64, len(o.lik))
	for _, l := range o.lik {
		if _, ok := t.branches[l.name]; !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, l.name)
		}
		if l.x < 0 || math.IsInf(l.x, 0) || math.IsNaN(l.x) {
			return fmt.Errorf("%w: %s", ErrBadLikelihood, l.name)
		}
		lik[l.name] = l.x
	}

	// 1. Facts, on the trunk and every live branch. They don't make
	// branches stale: every branch got the same writes.
	for _, f := range o.facts {
		if f.Delete {
			t.trunk = t.trunk.Delete(f.Key)
		} else {
			t.trunk = t.trunk.Set(f.Key, f.Value)
		}
		for _, name := range t.order {
			if b := t.branches[name]; !b.dead {
				nb := t.mut(name)
				if f.Delete {
					nb.data = nb.data.Delete(f.Key)
				} else {
					nb.data = nb.data.Set(f.Key, f.Value)
				}
			}
		}
	}

	// 2. Constraints.
	for _, name := range t.order {
		b := t.branches[name]
		if b.dead {
			continue
		}
		if c, bad := violated(b); bad {
			t.kill(a, name, CauseViolated, c.String(), o.now)
		}
	}

	// 3. Likelihoods: every live branch needs one, from the list or Rest.
	w, xs := map[string]float64{}, map[string]float64{}
	var live []string
	for _, name := range t.order {
		if b := t.branches[name]; !b.dead {
			live = append(live, name)
			x, ok := lik[name]
			switch {
			case ok:
			case len(lik) == 0:
				x = 1
			case o.hasRest:
				x = o.rest
			default:
				return fmt.Errorf("%w: %s", ErrMissingLikelihood, name)
			}
			xs[name] = x
			w[name] = b.weight * x
			if math.IsInf(w[name], 0) || math.IsNaN(w[name]) {
				return fmt.Errorf("%w: %s's weight would overflow", ErrBadLikelihood, name)
			}
		}
	}
	for _, name := range live {
		switch {
		case xs[name] == 0:
			t.kill(a, name, CauseRuledOut, "", o.now)
		case w[name] == 0:
			t.kill(a, name, CausePruned, underflow, o.now)
		}
	}

	// 4. Renormalise the survivors to sum to 1, scaling by the largest
	// first so neither the sum nor the division can overflow.
	survivors := renormalise(t, a, o.now, live, w)

	// 5. Prune below the threshold, never the most likely branch, then
	// renormalise once more.
	if o.prune > 0 && len(survivors) > 1 {
		top := survivors[0]
		for _, name := range survivors {
			if t.branches[name].weight > t.branches[top].weight {
				top = name
			}
		}
		var kept []string
		for _, name := range survivors {
			if name != top && t.branches[name].weight < o.prune {
				t.kill(a, name, CausePruned, "below "+fmtNum(o.prune), o.now)
				continue
			}
			kept = append(kept, name)
		}
		if len(kept) < len(survivors) {
			ww := map[string]float64{}
			for _, name := range kept {
				ww[name] = t.branches[name].weight
			}
			renormalise(t, a, o.now, kept, ww)
		}
	}
	a.event("observe", "", fmt.Sprintf("%s, %s, %d live after", plural(len(o.facts), "fact"), plural(len(o.lik), "likelihood"), t.liveCount()))
	return nil
}

// underflow is the detail for a branch whose weight became too small for a
// float64 to hold.
const underflow = "weight underflowed to zero"

// renormalise sets each live branch's weight to its share of w, adding in
// creation order so the result is the same every time. A branch whose share
// is too small to represent dies (pruned), since a live branch must have a
// weight above zero. It returns the branches it kept.
func renormalise(t *state, a *applied, now int64, names []string, w map[string]float64) []string {
	m := 0.0
	for _, name := range names {
		if !t.branches[name].dead && w[name] > m {
			m = w[name]
		}
	}
	if m == 0 {
		return nil
	}
	sum := 0.0
	for _, name := range names {
		if !t.branches[name].dead {
			sum += w[name] / m
		}
	}
	var kept []string
	for _, name := range names {
		if t.branches[name].dead {
			continue
		}
		nw := (w[name] / m) / sum
		if nw == 0 {
			t.kill(a, name, CausePruned, underflow, now)
			continue
		}
		t.mut(name).weight = nw
		kept = append(kept, name)
	}
	return kept
}

func (t *state) promote(a *applied, o *op) error {
	b, err := t.live(o.branch)
	if err != nil {
		return err
	}
	// Deaths from the sweep at the start of this change belong to the
	// generation that is ending.
	for i := range a.events {
		a.events[i].before = true
	}
	if b.base != t.trunkVer {
		return fmt.Errorf("%w: %s", ErrStale, o.branch)
	}
	for _, name := range t.order {
		if x := t.branches[name]; !x.dead && name != b.name {
			a.events = append(a.events, eventDraft{kind: "dropped", branch: name, before: true})
		}
	}
	t.trunk = b.data
	t.trunkVer++
	t.gen++
	t.branches = map[string]*branch{}
	t.order = nil
	a.event("promote", b.name, fmt.Sprintf("generation %d", t.gen))
	return nil
}

// info describes branch b as of now. Probability is only filled by the
// callers that know the total.
func (s *state) info(b *branch, now int64) BranchInfo {
	i := BranchInfo{
		Name: b.name, Parent: b.parent, Live: !b.dead, Weight: b.weight,
		Constraints: append([]Constraint(nil), b.cons...),
		Created:     time.Unix(0, b.created).UTC(),
		Cause:       b.cause, Detail: b.detail,
		Stale: b.base != s.trunkVer,
		Keys:  b.data.Len(),
	}
	if b.deadline != 0 {
		i.Deadline = time.Unix(0, b.deadline).UTC()
	}
	if b.dead {
		i.Died = time.Unix(0, b.died).UTC()
	} else if b.deadline != 0 && b.deadline <= now {
		// Past its deadline but not yet swept: it reads as expired.
		i.Live, i.Cause, i.Died = false, CauseExpired, i.Deadline
	}
	return i
}

// infos describes every branch of this generation in creation order, with
// probabilities over the branches live at now.
func (s *state) infos(now int64) []BranchInfo {
	out := make([]BranchInfo, 0, len(s.order))
	m := 0.0
	for _, name := range s.order {
		i := s.info(s.branches[name], now)
		if i.Live && i.Weight > m {
			m = i.Weight
		}
		out = append(out, i)
	}
	// Scale by the largest weight first, so a sum of very large weights
	// can't overflow.
	sum := 0.0
	for _, i := range out {
		if i.Live {
			sum += i.Weight / m
		}
	}
	for k := range out {
		if out[k].Live && sum > 0 {
			out[k].Probability = (out[k].Weight / m) / sum
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func liveOnly(all []BranchInfo) []BranchInfo {
	var out []BranchInfo
	for _, i := range all {
		if i.Live {
			out = append(out, i)
		}
	}
	return out
}

func sortedLikelihoods(m map[string]float64) []likelihood {
	out := make([]likelihood, 0, len(m))
	for k, v := range m {
		out = append(out, likelihood{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}
