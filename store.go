// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/multivergence/multivergence/internal/pmap"
)

// Store is a trunk and its branches. Its methods are safe to call from
// several goroutines.
type Store struct {
	mu       sync.Mutex
	opts     Options
	st       *state
	events   []Event
	eventSeq uint64
	log      *logFile // nil for a store in memory or a read-only one
	path     string
	readOnly bool
	closed   bool
	broken   error // a failed write left the file in doubt
}

func unixUTC(ns int64) time.Time { return time.Unix(0, ns).UTC() }

// readStoreFile reads the whole file. On Windows, opening a file at the
// moment a compaction renames another over it can fail for an instant, so
// it tries a few times over about half a second before giving up.
func readStoreFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	for i := 0; err != nil && runtime.GOOS == "windows" && i < 50; i++ {
		if errors.Is(err, fs.ErrNotExist) && i == 0 {
			if _, serr := os.Stat(path + ".lock"); serr != nil {
				break // no store here, and none being written
			}
		}
		time.Sleep(10 * time.Millisecond)
		data, err = os.ReadFile(path)
	}
	return data, err
}

func copyOpts(opts *Options) Options {
	if opts == nil {
		return Options{}
	}
	return *opts
}

// New returns an empty store that lives in memory.
func New(opts *Options) *Store {
	return &Store{opts: copyOpts(opts), st: newState()}
}

// Open opens the store in the file at path, creating it if it doesn't exist,
// and replays it. The Store holds the file's lock (path + ".lock") until
// Close: while it is open, no other process can open the file for writing,
// and Open waits up to Options.LockTimeout for the lock. If the last record
// was cut short by a crash, Open drops it; that change never completed.
func Open(path string, opts *Options) (*Store, error) {
	o := copyOpts(opts)
	lf, ld, err := openLog(path, &o)
	if err != nil {
		return nil, err
	}
	return &Store{opts: o, st: ld.st, events: ld.events, eventSeq: ld.eventSeq, log: lf, path: path}, nil
}

// OpenReadOnly reads the store in the file at path without taking the lock,
// so it works while another process has the file open for writing. It sees
// the changes completed when it read the file; Refresh reads it again.
func OpenReadOnly(path string, opts *Options) (*Store, error) {
	s := &Store{opts: copyOpts(opts), path: path, readOnly: true}
	if err := s.Refresh(); err != nil {
		return nil, err
	}
	return s, nil
}

// Refresh re-reads a read-only store's file.
func (s *Store) Refresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readOnly {
		return nil
	}
	data, err := readStoreFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("multivergence: no store at %s: %w", s.path, err)
		}
		return err
	}
	ld, err := load(data, s.opts.historyLimit())
	if err != nil {
		return err
	}
	s.st, s.events, s.eventSeq = ld.st, ld.events, ld.eventSeq
	return nil
}

// Close lets go of the file and its lock. A store in memory just stops
// working.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.log != nil {
		return s.log.close()
	}
	return nil
}

func (s *Store) now() int64 { return s.opts.now().UnixNano() }

// commit applies a change, writes it to the file, and only then makes it
// the store's state.
func (s *Store) commit(o *op) (*applied, error) {
	if s.closed {
		return nil, ErrClosed
	}
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if s.broken != nil {
		return nil, fmt.Errorf("multivergence: an earlier write failed, reopen the store: %w", s.broken)
	}
	// The clock is read under the lock, so the times in the log never go
	// backwards between changes made by different goroutines.
	o.now = s.now()
	if o.rel {
		o.at = o.now + int64(o.in)
		if (o.in > 0 && o.at < o.now) || (o.in < 0 && o.at > o.now) {
			return nil, fmt.Errorf("%w: now plus %v is out of range", ErrBadDeadline, o.in)
		}
		if o.at == 0 {
			o.at = 1 // the Unix epoch itself, not "no deadline"
		}
	}
	t, a, err := apply(s.st, o, s.opts.maxBranches())
	if err != nil {
		return nil, err
	}
	if s.log != nil {
		if err := s.log.append(encodeOp(o)); err != nil {
			s.broken = err
			return nil, err
		}
	}
	s.st = t
	s.events, s.eventSeq = addEvents(s.events, s.eventSeq, a.events, o.now, t.gen, s.opts.historyLimit())
	if s.log != nil {
		limit := s.opts.CompactAfter
		if limit <= 0 {
			limit = max(1<<20, 2*s.log.snapBytes)
		}
		if s.log.due(limit) {
			if err := s.log.compact(encodeSnapshot(s.st, lastEvents(s.events, s.opts.historyLimit()), s.eventSeq)); err != nil {
				// The change is saved; only the reopen after the rename
				// failed. Report it as done, and refuse later changes.
				s.broken = fmt.Errorf("reopening the file after compaction: %w", err)
			}
		}
	}
	return a, nil
}

func (s *Store) change(o *op) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.commit(o)
	if err != nil {
		return err
	}
	if a.killed != nil {
		return a.killed
	}
	return nil
}

// Get returns the value of key on a branch, or on the trunk. It reads dead
// branches too, until the next Promote.
func (s *Store) Get(branch, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.data(branch)
	if err != nil {
		return nil, false, err
	}
	v, ok := m.Get(key)
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

func (s *Store) data(branch string) (pmap.Map, error) {
	if s.closed {
		return pmap.Map{}, ErrClosed
	}
	if branch == Trunk {
		return s.st.trunk, nil
	}
	b, ok := s.st.branches[branch]
	if !ok {
		return pmap.Map{}, fmt.Errorf("%w: %s", ErrNotFound, branch)
	}
	return b.data, nil
}

// Keys returns the keys on a branch, or on the trunk, that start with
// prefix, in order.
func (s *Store) Keys(branch, prefix string) ([]string, error) {
	var out []string
	err := s.Scan(branch, prefix, func(k string, _ []byte) bool { out = append(out, k); return true })
	return out, err
}

// Scan calls fn for each key on a branch, or the trunk, that starts with
// prefix, in key order, until fn returns false. fn must not keep the value
// slice or change it, and must not call the Store.
func (s *Store) Scan(branch, prefix string, fn func(key string, value []byte) bool) error {
	s.mu.Lock()
	m, err := s.data(branch)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	m.Range(prefix, fn)
	return nil
}

// Set writes key on a live branch, or on the trunk. Writing the trunk
// directly makes the branches forked before it stale (see Promote). If the
// write breaks one of the branch's constraints, the branch dies and Set
// returns a *KilledError.
func (s *Store) Set(branch, key string, value []byte) error {
	return s.change(&op{kind: opSet, branch: branch, key: key, val: append([]byte(nil), value...)})
}

// Delete removes key from a live branch, or from the trunk, like Set.
func (s *Store) Delete(branch, key string) error {
	return s.change(&op{kind: opDelete, branch: branch, key: key})
}

// Fork makes a branch called name from the trunk or from a live branch,
// with the given weight. It starts with its source's data. A branch forked
// from a branch also takes its constraints and its deadline; Expire can
// change the deadline afterwards. The source stays as it was: if the forks
// replace it, Discard it.
func (s *Store) Fork(name, from string, weight float64) error {
	return s.change(&op{kind: opFork, branch: name, from: from, weight: weight})
}

// Weight sets a live branch's weight.
func (s *Store) Weight(name string, weight float64) error {
	if name == Trunk {
		return ErrTrunk
	}
	return s.change(&op{kind: opWeight, branch: name, weight: weight})
}

// Constrain adds constraints to a live branch. If one doesn't hold already,
// the branch dies and Constrain returns a *KilledError.
func (s *Store) Constrain(name string, cs ...Constraint) error {
	if name == Trunk {
		return ErrTrunk
	}
	return s.change(&op{kind: opConstrain, branch: name, cons: append([]Constraint(nil), cs...)})
}

// Expire sets the time a live branch dies at. The zero time clears it. A
// time already past kills the branch now, and Expire returns a
// *KilledError.
func (s *Store) Expire(name string, at time.Time) error {
	if name == Trunk {
		return ErrTrunk
	}
	o := &op{kind: opExpire, branch: name}
	if !at.IsZero() {
		if at.Before(minTime) || at.After(maxTime) {
			return fmt.Errorf("%w: %v is outside 1678 to 2262", ErrBadDeadline, at)
		}
		o.at = at.UnixNano()
		if o.at == 0 {
			o.at = 1 // the Unix epoch itself, not "no deadline"
		}
	}
	return s.change(o)
}

// The times a deadline can take: what fits in int64 nanoseconds.
var (
	minTime = time.Unix(0, math.MinInt64+1)
	maxTime = time.Unix(0, math.MaxInt64)
)

// ExpireIn sets a live branch to die after d from now.
func (s *Store) ExpireIn(name string, d time.Duration) error {
	if name == Trunk {
		return ErrTrunk
	}
	return s.change(&op{kind: opExpire, branch: name, rel: true, in: d})
}

// Observe applies one observation, in this order: deadlines that have
// passed, the facts, the constraints, the likelihoods, renormalising, and
// pruning. docs/UPDATES.md sets out each step.
func (s *Store) Observe(ev Evidence) (*Outcome, error) {
	o := &op{kind: opObserve, lik: sortedLikelihoods(ev.Likelihoods), prune: ev.PruneBelow}
	for _, f := range ev.Facts {
		f.Value = append([]byte(nil), f.Value...)
		o.facts = append(o.facts, f)
	}
	if ev.Rest != nil {
		o.hasRest, o.rest = true, *ev.Rest
	}
	return s.outcome(o)
}

// Sweep kills the branches whose deadline has passed. Every change does
// this first; Sweep does it on its own. It writes nothing if nothing died.
func (s *Store) Sweep() (*Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	now := s.now()
	probe := s.st.clone()
	a := &applied{}
	probe.sweep(a, now)
	if len(a.deaths) == 0 {
		return &Outcome{Live: liveOnly(s.st.infos(now))}, nil
	}
	a, err := s.commit(&op{kind: opSweep, now: now})
	if err != nil {
		return nil, err
	}
	return &Outcome{Killed: a.deaths, Live: liveOnly(s.st.infos(now))}, nil
}

func (s *Store) outcome(o *op) (*Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.commit(o)
	if err != nil {
		return nil, err
	}
	return &Outcome{Killed: a.deaths, Live: liveOnly(s.st.infos(o.now))}, nil
}

// Discard kills a live branch.
func (s *Store) Discard(name string) error {
	if name == Trunk {
		return ErrTrunk
	}
	return s.change(&op{kind: opDiscard, branch: name})
}

// Promote makes a live branch's state the trunk and drops every branch of
// this generation, live or dead, and starts the next generation. It is one
// record in the file, so it happens completely or not at all. It refuses a
// branch whose deadline has passed, and a stale branch: one forked before
// a direct write to the trunk, which promoting would lose.
func (s *Store) Promote(name string) error {
	if name == Trunk {
		return ErrTrunk
	}
	return s.change(&op{kind: opPromote, branch: name})
}

// Branch describes one branch of this generation.
func (s *Store) Branch(name string) (BranchInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return BranchInfo{}, ErrClosed
	}
	if _, ok := s.st.branches[name]; !ok {
		return BranchInfo{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	for _, i := range s.st.infos(s.now()) {
		if i.Name == name {
			return i, nil
		}
	}
	return BranchInfo{}, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Branches describes every branch of this generation, live and dead, in the
// order they were made.
func (s *Store) Branches() []BranchInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.st.infos(s.now())
}

// Live describes the live branches, in the order they were made.
func (s *Store) Live() []BranchInfo { return liveOnly(s.Branches()) }

// Best returns the most likely live branch; ties go to the oldest.
func (s *Store) Best() (BranchInfo, bool) { return best(s.Live()) }

// Generation counts the promotes so far.
func (s *Store) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.gen
}

// History returns the kept events, oldest first.
func (s *Store) History() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return lastEvents(s.events, s.opts.historyLimit())
}

// Digest is a fingerprint of the whole state, the same for two stores that
// hold the same trunk and the same branches with the same weights, down to
// the last bit. Tests use it to check that replaying a file gives back
// exactly what was there.
func (s *Store) Digest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return digest(s.st)
}
