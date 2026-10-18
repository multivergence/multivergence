// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package multivergence is an embeddable state engine for branching futures.
//
// A Store holds the trunk, the state that is real now, and any number of
// branches forked from it. A branch is a cheap copy-on-write fork of the
// state, and it carries three things besides its data:
//
//   - a weight: how likely this future is, relative to the other branches
//   - constraints: conditions on its keys that must stay true for it to stay
//     valid, such as "quote/flight <= 400"
//   - a deadline: when it stops mattering
//
// Observations arrive as Evidence: facts about the world, written to the
// trunk and to every live branch, and likelihoods the caller supplies for
// each branch. The engine applies them by fixed rules: it multiplies each
// weight by its likelihood and renormalises, and a branch dies when a
// constraint breaks, its deadline passes, its likelihood is zero, or it falls
// below a pruning threshold. When one branch is chosen, Promote makes its
// state the trunk and drops the rest.
//
// The engine never works out probabilities itself. Where likelihoods come
// from (a model, a tool's answer, a rule of thumb) is the caller's business;
// the engine applies them the same way every time, so the same calls always
// give the same weights.
//
// A Store lives in memory (New) or in a single file (Open), an append-only
// log where every change is one checksummed record, written and synced
// before the call returns. Promote is one record, so after a crash the file
// holds the state from before the promote or the state after it, never part
// of one.
package multivergence

import (
	"errors"
	"fmt"
	"time"
)

// Version is the version of this package and of the multivergence command.
const Version = "0.1.0"

// Trunk is the name of the trunk, the state that is real now. Get, Set,
// Delete, Keys and Fork accept it wherever they take a branch name.
const Trunk = "trunk"

// Limits on names, keys and values.
const (
	MaxNameLen  = 64
	MaxKeyLen   = 4096
	MaxValueLen = 16 << 20
)

// Errors returned by the Store. Use errors.Is to test for them.
var (
	ErrNotFound          = errors.New("multivergence: no such branch")
	ErrNotLive           = errors.New("multivergence: branch is not live")
	ErrExists            = errors.New("multivergence: a branch with that name exists in this generation")
	ErrBadName           = errors.New("multivergence: bad branch name")
	ErrBadKey            = errors.New("multivergence: bad key")
	ErrBadValue          = errors.New("multivergence: value too large")
	ErrBadWeight         = errors.New("multivergence: weight must be a finite number above zero")
	ErrBadLikelihood     = errors.New("multivergence: likelihood must be a finite number, zero or above")
	ErrMissingLikelihood = errors.New("multivergence: no likelihood for a live branch, and no Rest")
	ErrBadPrune          = errors.New("multivergence: PruneBelow must be at least 0 and below 1")
	ErrBadConstraint     = errors.New("multivergence: bad constraint")
	ErrBadDeadline       = errors.New("multivergence: deadline out of range")
	ErrTrunk             = errors.New("multivergence: not allowed on the trunk")
	ErrStale             = errors.New("multivergence: the trunk changed after this branch was forked")
	ErrTooManyBranches   = errors.New("multivergence: too many live branches")
	ErrKilled            = errors.New("multivergence: the change was made and the branch died")
	ErrReadOnly          = errors.New("multivergence: store is read-only")
	ErrClosed            = errors.New("multivergence: store is closed")
	ErrCorrupt           = errors.New("multivergence: file is corrupt")
	ErrLocked            = errors.New("multivergence: another process holds the file")
)

// KilledError says that a Set, Delete, Constrain or Expire was carried out
// and the branch died because of it: a constraint broke, or the deadline is
// already past. errors.Is(err, ErrKilled) is true for it.
type KilledError struct {
	Branch string
	Cause  Cause
	Detail string
}

func (e *KilledError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("multivergence: branch %s died: %s", e.Branch, e.Cause)
	}
	return fmt.Sprintf("multivergence: branch %s died: %s (%s)", e.Branch, e.Cause, e.Detail)
}

// Is makes errors.Is(err, ErrKilled) true.
func (e *KilledError) Is(target error) bool { return target == ErrKilled }

// Cause is why a branch died.
type Cause uint8

const (
	// CauseNone: the branch is live.
	CauseNone Cause = iota
	// CauseViolated: one of its constraints stopped holding.
	CauseViolated
	// CauseExpired: its deadline passed.
	CauseExpired
	// CauseRuledOut: an observation gave it likelihood zero.
	CauseRuledOut
	// CausePruned: its probability fell below an observation's PruneBelow.
	CausePruned
	// CauseDiscarded: the caller discarded it.
	CauseDiscarded
)

func (c Cause) String() string {
	switch c {
	case CauseNone:
		return "live"
	case CauseViolated:
		return "violated"
	case CauseExpired:
		return "expired"
	case CauseRuledOut:
		return "ruled out"
	case CausePruned:
		return "pruned"
	case CauseDiscarded:
		return "discarded"
	}
	return fmt.Sprintf("cause(%d)", uint8(c))
}

// Fact is something now known about the world. Observe writes it to the
// trunk and to every live branch, or deletes the key when Delete is set.
type Fact struct {
	Key    string
	Value  []byte
	Delete bool
}

// Evidence is one observation.
type Evidence struct {
	// Facts are written, in order, to the trunk and every live branch
	// before constraints are checked.
	Facts []Fact
	// Likelihoods gives, for named branches, how likely this evidence is
	// if that branch is the true future. Each live branch's weight is
	// multiplied by its likelihood. Names of branches that are already dead
	// are ignored; names that never existed are an error.
	Likelihoods map[string]float64
	// Rest is the likelihood for live branches missing from Likelihoods.
	// If it is nil and Likelihoods is not empty, every live branch must be
	// listed. If Likelihoods is empty, Rest is ignored and weights only
	// renormalise.
	Rest *float64
	// PruneBelow, if above zero, kills live branches whose probability
	// after the update is below it, except the most likely one.
	PruneBelow float64
}

// Likelihood returns a pointer to x, for Evidence.Rest.
func Likelihood(x float64) *float64 { return &x }

// Death is one branch that died during a call.
type Death struct {
	Branch string
	Cause  Cause
	Detail string
}

// BranchInfo describes a branch as of now.
type BranchInfo struct {
	Name   string
	Parent string // Trunk, or the branch it was forked from
	Live   bool
	// Weight is the stored weight. Probability is Weight divided by the
	// total weight of the live branches, and 0 for a dead branch.
	Weight      float64
	Probability float64
	Constraints []Constraint
	Deadline    time.Time // zero if none
	Created     time.Time
	Cause       Cause  // why it died; CauseNone while live
	Detail      string // which constraint broke, for CauseViolated
	Died        time.Time
	// Stale is true when the trunk was written directly after this branch
	// (or the branch it descends from) was forked from it. Promote refuses
	// a stale branch.
	Stale bool
	Keys  int
}

// Outcome is what an Observe or Sweep did.
type Outcome struct {
	// Killed lists the branches that died, in the order they died.
	Killed []Death
	// Live lists the live branches after the call, in the order they were
	// created, with their new probabilities.
	Live []BranchInfo
}

// Settled returns the one live branch, if exactly one is left.
func (o *Outcome) Settled() (string, bool) {
	if len(o.Live) == 1 {
		return o.Live[0].Name, true
	}
	return "", false
}

// Best returns the most likely live branch; ties go to the oldest.
func (o *Outcome) Best() (BranchInfo, bool) {
	return best(o.Live)
}

func best(live []BranchInfo) (BranchInfo, bool) {
	var b BranchInfo
	found := false
	for _, x := range live {
		if !found || x.Probability > b.Probability {
			b, found = x, true
		}
	}
	return b, found
}

// Event is one entry in a store's history.
type Event struct {
	Seq        uint64
	Time       time.Time
	Generation uint64
	// Kind is fork, weight, constrain, expire, observe, killed, promote or
	// dropped (a live branch let go by a promote).
	Kind   string
	Branch string
	Detail string
}

// Options configure a Store. The zero value is fine.
type Options struct {
	// Now is the clock. Deadlines are checked against it, and every change
	// records the time it used, so replaying a file gives the same result
	// whatever the clock says later. Defaults to time.Now.
	Now func() time.Time
	// MaxBranches caps the live branches. Fork fails beyond it. Defaults to
	// 4096.
	MaxBranches int
	// LockTimeout is how long Open waits for another process to let go of
	// the file. Defaults to 30 seconds.
	LockTimeout time.Duration
	// NoSync skips fsync after each change. Faster, but a power cut can
	// lose the last changes. A process crash still can't.
	NoSync bool
	// CompactAfter is how many bytes of log, written since the last
	// snapshot, start a compaction. Defaults to the larger of 1 MiB and
	// twice the last snapshot.
	CompactAfter int64
	// HistoryLimit is how many events the history keeps. Defaults to
	// 10000.
	HistoryLimit int
}

func (o *Options) now() time.Time {
	if o != nil && o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Options) maxBranches() int {
	if o != nil && o.MaxBranches > 0 {
		return o.MaxBranches
	}
	return 4096
}

func (o *Options) lockTimeout() time.Duration {
	if o != nil && o.LockTimeout > 0 {
		return o.LockTimeout
	}
	return 30 * time.Second
}

func (o *Options) historyLimit() int {
	if o != nil && o.HistoryLimit > 0 {
		return o.HistoryLimit
	}
	return 10000
}
