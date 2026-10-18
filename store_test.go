// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clock is a fake clock that only moves when told to.
type clock struct{ t time.Time }

func newClock() *clock               { return &clock{t: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)} }
func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func (c *clock) opts() *Options      { return &Options{Now: c.now} }
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func get(t *testing.T, s *Store, b, k string) string {
	t.Helper()
	v, ok, err := s.Get(b, k)
	must(t, err)
	if !ok {
		return "<missing>"
	}
	return string(v)
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestForkIsolation(t *testing.T) {
	s := New(nil)
	must(t, s.Set(Trunk, "city", []byte("Lisbon")))
	must(t, s.Fork("a", Trunk, 1))
	must(t, s.Fork("b", Trunk, 1))
	must(t, s.Set("a", "city", []byte("Porto")))
	must(t, s.Delete("b", "city"))
	must(t, s.Set("b", "only-b", []byte("1")))
	if get(t, s, Trunk, "city") != "Lisbon" || get(t, s, "a", "city") != "Porto" || get(t, s, "b", "city") != "<missing>" {
		t.Fatal("branches leaked into each other or the trunk")
	}
	if get(t, s, "a", "only-b") != "<missing>" || get(t, s, Trunk, "only-b") != "<missing>" {
		t.Fatal("write to b visible elsewhere")
	}
	// A fork from a branch starts with that branch's data.
	must(t, s.Fork("a2", "a", 1))
	if get(t, s, "a2", "city") != "Porto" {
		t.Fatal("fork from a branch lost its data")
	}
	// Changing a returned value can't change the store.
	v, _, _ := s.Get("a", "city")
	v[0] = 'X'
	if get(t, s, "a", "city") != "Porto" {
		t.Fatal("Get returned the store's own bytes")
	}
}

func TestNamesAndErrors(t *testing.T) {
	s := New(nil)
	for _, bad := range []string{"", "trunk", "-x", "a b", "é", string(make([]byte, 65))} {
		if err := s.Fork(bad, Trunk, 1); !errors.Is(err, ErrBadName) {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	must(t, s.Fork("plan-a.v2_x", Trunk, 1))
	if err := s.Fork("plan-a.v2_x", Trunk, 1); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	for _, w := range []float64{0, -1, math.Inf(1), math.NaN()} {
		if err := s.Fork("w", Trunk, w); !errors.Is(err, ErrBadWeight) {
			t.Errorf("weight %v: %v", w, err)
		}
	}
	if err := s.Fork("x", "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Set("nope", "k", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Set(Trunk, "", nil); !errors.Is(err, ErrBadKey) {
		t.Fatal(err)
	}
	if err := s.Set(Trunk, "bad\x00key", nil); !errors.Is(err, ErrBadKey) {
		t.Fatal(err)
	}
	if err := s.Weight(Trunk, 1); !errors.Is(err, ErrTrunk) {
		t.Fatal(err)
	}
	if err := s.Promote(Trunk); !errors.Is(err, ErrTrunk) {
		t.Fatal(err)
	}
	must(t, s.Discard("plan-a.v2_x"))
	if err := s.Set("plan-a.v2_x", "k", nil); !errors.Is(err, ErrNotLive) {
		t.Fatal(err)
	}
	s2 := New(&Options{MaxBranches: 2})
	must(t, s2.Fork("a", Trunk, 1))
	must(t, s2.Fork("b", Trunk, 1))
	if err := s2.Fork("c", Trunk, 1); !errors.Is(err, ErrTooManyBranches) {
		t.Fatal(err)
	}
	must(t, s2.Discard("a"))
	must(t, s2.Fork("c", Trunk, 1))
}

func TestConstraintsKill(t *testing.T) {
	s := New(nil)
	must(t, s.Set(Trunk, "budget", []byte("400")))
	must(t, s.Fork("flight", Trunk, 1))
	// A missing key breaks a numeric constraint at once.
	if err := s.Constrain("flight", AtMost("quote", 400), NotEquals("seats", "0")); !errors.Is(err, ErrKilled) {
		t.Fatal(err)
	}
	info, err := s.Branch("flight")
	must(t, err)
	if info.Live {
		t.Fatal("flight should have died: quote is missing")
	}
	if info.Cause != CauseViolated || info.Detail != "quote <= 400" {
		t.Fatalf("cause %v %q", info.Cause, info.Detail)
	}

	must(t, s.Fork("train", Trunk, 1))
	must(t, s.Set("train", "quote", []byte("180")))
	must(t, s.Constrain("train", AtMost("quote", 400)))
	err = s.Set("train", "quote", []byte("401"))
	var ke *KilledError
	if !errors.As(err, &ke) || !errors.Is(err, ErrKilled) || ke.Cause != CauseViolated {
		t.Fatalf("expected a KilledError, got %v", err)
	}
	// The write that killed it was kept, and the branch is still readable.
	if get(t, s, "train", "quote") != "401" {
		t.Fatal("the fatal write was lost")
	}

	// Constrain on a branch that already breaks it kills it too.
	must(t, s.Fork("bus", Trunk, 1))
	must(t, s.Set("bus", "mode", []byte("bus")))
	if err := s.Constrain("bus", Equals("mode", "coach")); !errors.Is(err, ErrKilled) {
		t.Fatal(err)
	}
	if err := s.Constrain("nope", Present("x")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Constrain("bus", Present("x")); !errors.Is(err, ErrNotLive) {
		t.Fatal(err)
	}
}

func TestConstraintOps(t *testing.T) {
	cases := []struct {
		c       Constraint
		v       string
		present bool
		want    bool
	}{
		{Equals("k", "a"), "a", true, true},
		{Equals("k", "a"), "b", true, false},
		{Equals("k", "a"), "", false, false},
		{NotEquals("k", "a"), "", false, true},
		{NotEquals("k", "a"), "a", true, false},
		{Below("k", 5), "4.9", true, true},
		{Below("k", 5), "5", true, false},
		{AtMost("k", 5), "5", true, true},
		{Above("k", 5), " 6 ", true, true},
		{AtLeast("k", 5), "4", true, false},
		{AtLeast("k", 5), "five", true, false},
		{AtLeast("k", 5), "NaN", true, false},
		{AtLeast("k", 5), "", false, false},
		{Present("k"), "", true, true},
		{Present("k"), "", false, false},
		{Missing("k"), "", false, true},
	}
	for _, c := range cases {
		if got := c.c.holds([]byte(c.v), c.present); got != c.want {
			t.Errorf("%s with %q present=%v: %v", c.c, c.v, c.present, got)
		}
	}
}

func TestParseConstraint(t *testing.T) {
	good := map[string]Constraint{
		"quote/flight <= 400":     AtMost("quote/flight", 400),
		"  seat ==   confirmed  ": Equals("seat", "confirmed"),
		`"two words" == 1`:        Equals("two words", "1"),
		`"q\"k" exists`:           Present(`q"k`),
		`note != "two words"`:     NotEquals("note", "two words"),
		`name == ""`:              Equals("name", ""),
		"hold exists":             Present("hold"),
		"hold absent":             Missing("hold"),
		"t > -1.5e3":              {Key: "t", Op: Gt, Value: "-1.5e3"},
		"t < 2":                   Below("t", 2),
		"t >= 0":                  AtLeast("t", 0),
		`x == "with \"quote\""`:   Equals("x", `with "quote"`),
	}
	for in, want := range good {
		c, err := ParseConstraint(in)
		if err != nil || c != want {
			t.Errorf("%q: %+v %v", in, c, err)
			continue
		}
		back, err := ParseConstraint(c.String())
		if err != nil || back != c {
			t.Errorf("%q does not round-trip through %q", in, c.String())
		}
	}
	for _, bad := range []string{"", "k", "k ~ 1", "k <= abc", "k <= Inf", "k exists 1", "k ==", `k == "open`} {
		if _, err := ParseConstraint(bad); !errors.Is(err, ErrBadConstraint) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestObserveUpdates(t *testing.T) {
	s := New(nil)
	must(t, s.Fork("a", Trunk, 0.5))
	must(t, s.Fork("b", Trunk, 0.3))
	must(t, s.Fork("c", Trunk, 0.2))
	out, err := s.Observe(Evidence{Likelihoods: map[string]float64{"a": 0.2, "b": 0.6, "c": 0.5}})
	must(t, err)
	// Posterior is proportional to prior times likelihood: 0.1, 0.18, 0.1.
	want := map[string]float64{"a": 0.1 / 0.38, "b": 0.18 / 0.38, "c": 0.1 / 0.38}
	sum := 0.0
	for _, i := range out.Live {
		if !near(i.Probability, want[i.Name]) || !near(i.Weight, want[i.Name]) {
			t.Errorf("%s: %v want %v", i.Name, i.Probability, want[i.Name])
		}
		sum += i.Weight
	}
	if !near(sum, 1) {
		t.Fatalf("weights sum to %v", sum)
	}
	if b, _ := out.Best(); b.Name != "b" {
		t.Fatalf("best is %s", b.Name)
	}

	// Every live branch needs a likelihood, unless Rest covers it.
	if _, err := s.Observe(Evidence{Likelihoods: map[string]float64{"a": 1}}); !errors.Is(err, ErrMissingLikelihood) {
		t.Fatal(err)
	}
	before := s.Digest()
	if _, err := s.Observe(Evidence{Likelihoods: map[string]float64{"zzz": 1}, Rest: Likelihood(1)}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, bad := range []float64{-1, math.Inf(1), math.NaN()} {
		if _, err := s.Observe(Evidence{Likelihoods: map[string]float64{"a": bad}, Rest: Likelihood(1)}); !errors.Is(err, ErrBadLikelihood) {
			t.Fatal(bad, err)
		}
	}
	if _, err := s.Observe(Evidence{PruneBelow: 1}); !errors.Is(err, ErrBadPrune) {
		t.Fatal(err)
	}
	if s.Digest() != before {
		t.Fatal("a rejected observation changed the state")
	}

	// Zero rules a branch out; Rest covers the others.
	out, err = s.Observe(Evidence{Likelihoods: map[string]float64{"c": 0}, Rest: Likelihood(1)})
	must(t, err)
	if len(out.Killed) != 1 || out.Killed[0].Branch != "c" || out.Killed[0].Cause != CauseRuledOut {
		t.Fatalf("killed %+v", out.Killed)
	}
	if len(out.Live) != 2 {
		t.Fatalf("live %d", len(out.Live))
	}
	// Naming a branch that's already dead is fine.
	_, err = s.Observe(Evidence{Likelihoods: map[string]float64{"a": 1, "b": 1, "c": 5}})
	must(t, err)
}

func TestObservePrune(t *testing.T) {
	s := New(nil)
	for name, w := range map[string]float64{"a": 0.6, "b": 0.3, "c": 0.06, "d": 0.04} {
		must(t, s.Fork(name, Trunk, w))
	}
	out, err := s.Observe(Evidence{PruneBelow: 0.05})
	must(t, err)
	if len(out.Killed) != 1 || out.Killed[0].Branch != "d" || out.Killed[0].Cause != CausePruned {
		t.Fatalf("killed %+v", out.Killed)
	}
	sum := 0.0
	for _, i := range out.Live {
		sum += i.Weight
	}
	if !near(sum, 1) {
		t.Fatal("not renormalised after pruning", sum)
	}
	// Pruning never kills the most likely branch, even above the threshold.
	out, err = s.Observe(Evidence{PruneBelow: 0.99})
	must(t, err)
	if n, ok := out.Settled(); !ok || n != "a" {
		t.Fatalf("settled on %q %v", n, ok)
	}
	if !near(out.Live[0].Weight, 1) {
		t.Fatal(out.Live[0].Weight)
	}
}

func TestObserveExtremeWeights(t *testing.T) {
	s := New(nil)
	must(t, s.Fork("a", Trunk, 1e300))
	must(t, s.Fork("b", Trunk, 1e300))
	// The sum would overflow without scaling first.
	out, err := s.Observe(Evidence{})
	must(t, err)
	if !near(out.Live[0].Weight, 0.5) {
		t.Fatal(out.Live[0].Weight)
	}
	if _, err := s.Observe(Evidence{Likelihoods: map[string]float64{"a": 1e308, "b": 1e308}}); err != nil {
		t.Fatal(err)
	}
	must(t, s.Weight("a", 1e300))
	if _, err := s.Observe(Evidence{Likelihoods: map[string]float64{"a": 1e300, "b": 1}}); !errors.Is(err, ErrBadLikelihood) {
		t.Fatal("overflow accepted", err)
	}
	// Tiny weights survive renormalising.
	must(t, s.Weight("a", 1e-300))
	must(t, s.Weight("b", 3e-300))
	out, err = s.Observe(Evidence{})
	must(t, err)
	if !near(out.Live[0].Weight, 0.25) {
		t.Fatal(out.Live[0].Weight)
	}
}

func TestFactsAndConstraints(t *testing.T) {
	s := New(nil)
	must(t, s.Set(Trunk, "quote/flight", []byte("0")))
	must(t, s.Fork("flight", Trunk, 0.3))
	must(t, s.Fork("train", Trunk, 0.7))
	must(t, s.Constrain("flight", AtMost("quote/flight", 400)))
	out, err := s.Observe(Evidence{Facts: []Fact{{Key: "quote/flight", Value: []byte("460")}, {Key: "quote/train", Value: []byte("180")}}})
	must(t, err)
	if len(out.Killed) != 1 || out.Killed[0].Branch != "flight" || out.Killed[0].Detail != "quote/flight <= 400" {
		t.Fatalf("killed %+v", out.Killed)
	}
	// Facts reached the trunk and the live branch.
	if get(t, s, Trunk, "quote/train") != "180" || get(t, s, "train", "quote/flight") != "460" {
		t.Fatal("facts not written everywhere")
	}
	// A dead branch doesn't get later facts.
	_, err = s.Observe(Evidence{Facts: []Fact{{Key: "late", Value: []byte("1")}, {Key: "quote/train", Delete: true}}})
	must(t, err)
	if get(t, s, "flight", "late") != "<missing>" || get(t, s, "train", "late") != "1" || get(t, s, "train", "quote/train") != "<missing>" {
		t.Fatal("facts went to the wrong branches")
	}
	// Facts don't make branches stale.
	must(t, s.Promote("train"))
}

func TestDeadlines(t *testing.T) {
	c := newClock()
	s := New(c.opts())
	must(t, s.Fork("bus", Trunk, 1))
	must(t, s.Fork("train", Trunk, 1))
	must(t, s.ExpireIn("bus", 15*time.Minute))
	c.add(14*time.Minute + 59*time.Second)
	if i, _ := s.Branch("bus"); !i.Live {
		t.Fatal("died early")
	}
	c.add(time.Second)
	// Reads see it as expired at once; the next change records it.
	if i, _ := s.Branch("bus"); i.Live || i.Cause != CauseExpired {
		t.Fatalf("not expired on read: %+v", i)
	}
	if len(s.Live()) != 1 || s.Live()[0].Probability != 1 {
		t.Fatal("expired branch still counted")
	}
	out, err := s.Sweep()
	must(t, err)
	if len(out.Killed) != 1 || out.Killed[0].Cause != CauseExpired {
		t.Fatalf("%+v", out.Killed)
	}
	if out, _ := s.Sweep(); len(out.Killed) != 0 {
		t.Fatal("swept twice")
	}
	// A deadline already past kills at once.
	if err := s.Expire("train", c.now().Add(-time.Second)); !errors.Is(err, ErrKilled) {
		t.Fatal(err)
	}
	// Forks take the parent's deadline; Expire with zero clears it.
	must(t, s.Fork("p", Trunk, 1))
	must(t, s.ExpireIn("p", time.Hour))
	must(t, s.Fork("child", "p", 1))
	i, _ := s.Branch("child")
	if !i.Deadline.Equal(c.now().Add(time.Hour)) {
		t.Fatal("deadline not inherited")
	}
	must(t, s.Expire("child", time.Time{}))
	c.add(2 * time.Hour)
	if i, _ := s.Branch("child"); !i.Live {
		t.Fatal("cleared deadline still fired")
	}
	if err := s.Promote("p"); !errors.Is(err, ErrNotLive) {
		t.Fatal("promoted an expired branch", err)
	}
}

func TestPromote(t *testing.T) {
	s := New(nil)
	must(t, s.Set(Trunk, "k", []byte("trunk")))
	must(t, s.Fork("a", Trunk, 1))
	must(t, s.Fork("b", Trunk, 1))
	must(t, s.Set("a", "k", []byte("a")))
	must(t, s.Set("a", "new", []byte("x")))
	must(t, s.Promote("a"))
	if get(t, s, Trunk, "k") != "a" || get(t, s, Trunk, "new") != "x" {
		t.Fatal("trunk is not a's state")
	}
	if s.Generation() != 1 || len(s.Branches()) != 0 {
		t.Fatal("branches survived the promote")
	}
	if _, _, err := s.Get("b", "k"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Names can be used again in the next generation.
	must(t, s.Fork("a", Trunk, 1))

	// A direct trunk write after a fork makes the branch stale.
	must(t, s.Set(Trunk, "k", []byte("moved")))
	if i, _ := s.Branch("a"); !i.Stale {
		t.Fatal("not marked stale")
	}
	if err := s.Promote("a"); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	// A branch forked from a stale branch is stale too; a new fork isn't.
	must(t, s.Fork("a-child", "a", 1))
	if err := s.Promote("a-child"); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	must(t, s.Fork("fresh", Trunk, 1))
	must(t, s.Promote("fresh"))
	if get(t, s, Trunk, "k") != "moved" {
		t.Fatal(get(t, s, Trunk, "k"))
	}
	h := s.History()
	last := h[len(h)-1]
	if last.Kind != "promote" || last.Branch != "fresh" || last.Generation != 2 {
		t.Fatalf("%+v", last)
	}
}

func TestPromoteEventGenerations(t *testing.T) {
	c := newClock()
	s := New(c.opts())
	must(t, s.Fork("keep", Trunk, 1))
	must(t, s.Fork("other", Trunk, 1))
	must(t, s.Fork("late", Trunk, 1))
	must(t, s.ExpireIn("late", time.Minute))
	c.add(2 * time.Minute)
	must(t, s.Promote("keep"))
	for _, e := range s.History() {
		want := uint64(0)
		if e.Kind == "promote" {
			want = 1
		}
		if e.Generation != want {
			t.Errorf("%s %s logged in generation %d, want %d", e.Kind, e.Branch, e.Generation, want)
		}
	}
}

func TestFileReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.mv")
	c := newClock()
	s, err := Open(path, c.opts())
	must(t, err)
	must(t, s.Set(Trunk, "a", []byte("1")))
	must(t, s.Fork("x", Trunk, 0.4))
	must(t, s.Fork("y", Trunk, 0.6))
	must(t, s.Set("x", "cost", []byte("9")))
	must(t, s.Constrain("x", AtMost("cost", 10)))
	must(t, s.ExpireIn("y", time.Hour))
	_, err = s.Observe(Evidence{Likelihoods: map[string]float64{"y": 0.3}, Rest: Likelihood(0.9)})
	must(t, err)
	want, wantHist := s.Digest(), s.History()
	must(t, s.Close())
	if err := s.Set(Trunk, "a", nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}

	s, err = Open(path, c.opts())
	must(t, err)
	if s.Digest() != want {
		t.Fatal("reopened store differs")
	}
	if len(s.History()) != len(wantHist) || s.History()[3] != wantHist[3] {
		t.Fatal("history differs after reopen")
	}
	// A reader sees the same, without the lock.
	r, err := OpenReadOnly(path, c.opts())
	must(t, err)
	if r.Digest() != want {
		t.Fatal("reader differs")
	}
	if err := r.Set(Trunk, "a", nil); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	must(t, s.Set(Trunk, "b", []byte("2")))
	must(t, r.Refresh())
	if r.Digest() != s.Digest() {
		t.Fatal("refresh missed a change")
	}
	must(t, s.Close())
}

func TestFileLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mv")
	s, err := Open(path, nil)
	must(t, err)
	if _, err := Open(path, &Options{LockTimeout: 50 * time.Millisecond}); !errors.Is(err, ErrLocked) {
		t.Fatal("second writer got in:", err)
	}
	must(t, s.Close())
	s, err = Open(path, &Options{LockTimeout: 50 * time.Millisecond})
	must(t, err)
	must(t, s.Close())
}

func TestTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mv")
	s, err := Open(path, nil)
	must(t, err)
	must(t, s.Set(Trunk, "a", []byte("1")))
	must(t, s.Fork("b", Trunk, 1))
	want := s.Digest()
	before, _ := os.Stat(path)
	must(t, s.Set("b", "k", []byte("lost")))
	must(t, s.Close())
	full, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	last := int(info.Size() - before.Size())
	// Cut the last record at every possible point: each must read as the
	// state before it, and the writer must trim the tail.
	for cut := 1; cut < last; cut++ {
		must(t, os.WriteFile(path, full[:len(full)-cut], 0o644))
		r, err := OpenReadOnly(path, nil)
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if r.Digest() != want {
			t.Fatalf("cut %d: wrong state", cut)
		}
	}
	// A flipped bit in the last record is a torn write too.
	bad := append([]byte(nil), full...)
	bad[len(bad)-1] ^= 1
	must(t, os.WriteFile(path, bad, 0o644))
	s, err = Open(path, nil)
	must(t, err)
	if s.Digest() != want {
		t.Fatal("bad last record was kept")
	}
	must(t, s.Close())
	if fi, _ := os.Stat(path); fi.Size() >= info.Size() {
		t.Fatal("the writer didn't trim the torn tail")
	}
	// The same flip in an earlier record is corruption.
	bad = append([]byte(nil), full...)
	bad[len(fileMagic)+recHeaderLen+2] ^= 1
	must(t, os.WriteFile(path, bad, 0o644))
	if _, err := Open(path, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatal("corruption not reported:", err)
	}
	must(t, os.WriteFile(path, []byte("not a store at all"), 0o644))
	if _, err := Open(path, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	// A header cut short when the file was created is a new store.
	must(t, os.WriteFile(path, fileMagic[:3], 0o644))
	s, err = Open(path, nil)
	must(t, err)
	must(t, s.Set(Trunk, "a", []byte("1")))
	must(t, s.Close())
}

func TestCompaction(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain.mv")
	runCompactionScript(t, plain, 1<<40)
	path := filepath.Join(t.TempDir(), "s.mv")
	want, hist, o := runCompactionScript(t, path, 4096)
	fi, _ := os.Stat(path)
	pi, _ := os.Stat(plain)
	if fi.Size()*2 > pi.Size() {
		t.Fatalf("compacted file is %d bytes, uncompacted %d", fi.Size(), pi.Size())
	}
	s, err := Open(path, o)
	must(t, err)
	if s.Digest() != want {
		t.Fatal("state lost in compaction")
	}
	h := s.History()
	if len(h) != len(hist) || h[len(h)-1] != hist[len(hist)-1] {
		t.Fatal("history lost in compaction")
	}
	must(t, s.Close())
	p, err := Open(plain, o)
	must(t, err)
	if p.Digest() != want {
		t.Fatal("compacted and uncompacted stores differ")
	}
	must(t, p.Close())
}

func runCompactionScript(t *testing.T, path string, after int64) (string, []Event, *Options) {
	c := newClock()
	o := c.opts()
	o.CompactAfter = after
	s, err := Open(path, o)
	must(t, err)
	for g := 0; g < 30; g++ {
		for i := 0; i < 20; i++ {
			must(t, s.Set(Trunk, string(rune('a'+i)), []byte{byte(g)}))
		}
		must(t, s.Fork("x", Trunk, 1))
		must(t, s.Fork("y", "x", 2))
		must(t, s.Set("y", "y", []byte("y")))
		must(t, s.Constrain("x", Missing("forbidden")))
		must(t, s.ExpireIn("y", time.Hour))
		_, err := s.Observe(Evidence{Facts: []Fact{{Key: "f", Value: []byte("v")}}})
		must(t, err)
		c.add(time.Minute)
		if g%2 == 0 {
			must(t, s.Promote("y"))
		} else {
			must(t, s.Promote("x"))
		}
	}
	must(t, s.Fork("open", Trunk, 1))
	must(t, s.Set("open", "k", []byte("v")))
	want, hist := s.Digest(), s.History()
	must(t, s.Close())
	return want, hist, o
}

// TestGoroutines has goroutines share one file store. Run with -race.
func TestGoroutines(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "g.mv"), &Options{NoSync: true, CompactAfter: 2048})
	must(t, err)
	defer s.Close()
	done := make(chan error)
	for g := 0; g < 8; g++ {
		go func(g int) {
			var err error
			for i := 0; i < 200 && err == nil; i++ {
				name := "g" + string(rune('a'+g)) + "-" + itoa(i)
				if err = s.Fork(name, Trunk, 1); err != nil {
					break
				}
				if err = s.Set(name, "k", []byte(name)); err != nil {
					break
				}
				s.Get(Trunk, "k")
				s.Live()
				if _, err = s.Observe(Evidence{}); err != nil {
					break
				}
				err = s.Discard(name)
				if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotLive) {
					err = nil // another goroutine promoted, or it was pruned
				}
				if i%50 == 0 {
					if perr := s.Promote(name); perr != nil && !errors.Is(perr, ErrNotLive) && !errors.Is(perr, ErrNotFound) {
						err = perr
					}
				}
			}
			done <- err
		}(g)
	}
	for g := 0; g < 8; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// writeSets makes a file of n Set records and returns its bytes.
func writeSets(t *testing.T, path string, n int) []byte {
	t.Helper()
	s, err := Open(path, nil)
	must(t, err)
	for i := 0; i < n; i++ {
		must(t, s.Set(Trunk, "k"+itoa(i), []byte("value "+itoa(i))))
	}
	must(t, s.Close())
	b, err := os.ReadFile(path)
	must(t, err)
	return b
}

// Review H1: a damaged length field in the middle must not read as a torn
// tail and cut off the records after it.
func TestDamagedLengthIsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mv")
	data := writeSets(t, path, 20)
	first := int(data[headerLen]) | int(data[headerLen+1])<<8
	off := headerLen + recHeaderLen + first // the second record
	data[off+2] ^= 1                        // its length now claims 64 KB more
	must(t, os.WriteFile(path, data, 0o644))
	if _, err := Open(path, nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("opened a file with a damaged length: %v", err)
	}
	if b, _ := os.ReadFile(path); len(b) != len(data) {
		t.Fatal("the file was cut")
	}
}

// Review H2: a compacted file's snapshot can't be torn, so a bad one is
// corruption and the file is left alone.
func TestDamagedSnapshotIsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mv")
	s, err := Open(path, &Options{CompactAfter: 1})
	must(t, err)
	for i := 0; i < 10; i++ {
		must(t, s.Set(Trunk, "k"+itoa(i), []byte("v")))
	}
	must(t, s.Close())
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data[:headerLen], compactMagic) {
		t.Fatal("not compacted")
	}
	for _, at := range []int{headerLen + 2, headerLen + recHeaderLen, len(data) / 2, len(data) - 1} {
		bad := append([]byte(nil), data...)
		bad[at] ^= 1
		must(t, os.WriteFile(path, bad, 0o644))
		if _, err := Open(path, nil); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("flip at %d: %v", at, err)
		}
		if b, _ := os.ReadFile(path); len(b) != len(data) {
			t.Fatalf("flip at %d: the file was cut", at)
		}
	}
}

// Review M3: a tail of zeros, which a filesystem can leave when the file
// grew before its data reached the disk, is torn, and costs nothing.
func TestZeroTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mv")
	data := writeSets(t, path, 5)
	r, err := OpenReadOnly(path, nil)
	must(t, err)
	want := r.Digest()
	must(t, os.WriteFile(path, append(data, make([]byte, 4096)...), 0o644))
	r, err = OpenReadOnly(path, nil)
	must(t, err)
	if r.Digest() != want {
		t.Fatal("reader: state changed")
	}
	s, err := Open(path, nil)
	must(t, err)
	if s.Digest() != want {
		t.Fatal("writer: state changed")
	}
	must(t, s.Close())
	if b, _ := os.ReadFile(path); len(b) != len(data) {
		t.Fatal("zeros not trimmed")
	}
	matches, _ := filepath.Glob(path + ".torn-*")
	if len(matches) != 0 {
		t.Fatal("kept a backup of zeros")
	}
}

// A real torn tail is cut off, and its bytes are kept beside the file.
func TestTornTailBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mv")
	data := writeSets(t, path, 5)
	torn := append(append([]byte(nil), data...), 40, 0, 0, 0, 1, 2, 3, 4, 'p', 'a', 'r', 't')
	must(t, os.WriteFile(path, torn, 0o644))
	s, err := Open(path, nil)
	must(t, err)
	must(t, s.Close())
	matches, _ := filepath.Glob(path + ".torn-*")
	if len(matches) != 1 {
		t.Fatalf("backups: %v", matches)
	}
	b, _ := os.ReadFile(matches[0])
	if !bytes.Equal(b, torn[len(data):]) {
		t.Fatal("backup differs from the tail")
	}
}

// Review H3: deadlines that don't fit in int64 nanoseconds are refused,
// not turned into a time already past.
func TestDeadlineRange(t *testing.T) {
	s := New(nil)
	must(t, s.Fork("a", Trunk, 1))
	for _, at := range []time.Time{time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if err := s.Expire("a", at); !errors.Is(err, ErrBadDeadline) {
			t.Fatalf("%v: %v", at, err)
		}
	}
	if err := s.ExpireIn("a", time.Duration(math.MaxInt64)); !errors.Is(err, ErrBadDeadline) {
		t.Fatal(err)
	}
	must(t, s.Expire("a", time.Date(2262, 1, 1, 0, 0, 0, 0, time.UTC)))
	if i, _ := s.Branch("a"); !i.Live {
		t.Fatal("a far deadline killed the branch")
	}
}

// Review H4: once the history is full, changes stay cheap: the history is
// trimmed in batches, not copied on every change.
func TestHistoryTrimming(t *testing.T) {
	s := New(&Options{HistoryLimit: 100})
	must(t, s.Fork("a", Trunk, 1))
	for i := 0; i < 1000; i++ {
		must(t, s.Weight("a", float64(i+1)))
		if n := len(s.events); n >= 200 {
			t.Fatalf("history holds %d events", n)
		}
	}
	h := s.History()
	if len(h) != 100 || h[99].Detail != "1000" || h[0].Seq+99 != h[99].Seq {
		t.Fatalf("history: %d events, last %+v", len(h), h[len(h)-1])
	}
	start := time.Now()
	big := New(nil)
	must(t, big.Fork("a", Trunk, 1))
	for i := 0; i < 30000; i++ {
		must(t, big.Weight("a", 1))
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("30,000 changes took %v", d)
	}
}

// Review M1: a branch whose share underflows dies instead of staying live
// with weight zero.
func TestWeightUnderflow(t *testing.T) {
	s := New(nil)
	must(t, s.Fork("a", Trunk, 1e300))
	must(t, s.Fork("b", Trunk, 1e-30))
	out, err := s.Observe(Evidence{})
	must(t, err)
	if len(out.Killed) != 1 || out.Killed[0].Branch != "b" || out.Killed[0].Cause != CausePruned {
		t.Fatalf("%+v", out.Killed)
	}
	for _, b := range s.Live() {
		if b.Weight <= 0 {
			t.Fatalf("%s live with weight %v", b.Name, b.Weight)
		}
	}
	// A product that underflows with a non-zero likelihood is not "ruled out".
	s = New(nil)
	must(t, s.Fork("a", Trunk, 1))
	must(t, s.Fork("b", Trunk, 1e-200))
	out, err = s.Observe(Evidence{Likelihoods: map[string]float64{"a": 1, "b": 1e-200}})
	must(t, err)
	if len(out.Killed) != 1 || out.Killed[0].Cause != CausePruned || out.Killed[0].Detail != underflow {
		t.Fatalf("%+v", out.Killed)
	}
}

// Review M2: probabilities and Best stay right when the weights' sum would
// overflow.
func TestHugeWeightsProbability(t *testing.T) {
	s := New(nil)
	must(t, s.Fork("a", Trunk, 1e308))
	must(t, s.Fork("b", Trunk, 1.7e308))
	b, ok := s.Best()
	if !ok || b.Name != "b" || !near(b.Probability, 1.7/2.7) {
		t.Fatalf("%+v", b)
	}
}

// Review M4: a compaction that can't be written is not retried on every
// change.
func TestCompactionBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.mv")
	s, err := Open(path, &Options{CompactAfter: 100, NoSync: true})
	must(t, err)
	// A non-empty directory where the temp file goes: it can't be made.
	must(t, os.Mkdir(path+".compact", 0o755))
	must(t, os.WriteFile(filepath.Join(path+".compact", "x"), nil, 0o644))
	tries := 0
	for i := 0; i < 200; i++ {
		before := s.log.retryAt
		must(t, s.Set(Trunk, "k", []byte(strings.Repeat("v", 50))))
		if s.log.retryAt != before {
			tries++
		}
	}
	if tries > 10 {
		t.Fatalf("compaction tried %d times in 200 changes", tries)
	}
	must(t, s.Close())
}

// Review L3 and L4.
func TestNoOpTrunkDeleteAndRest(t *testing.T) {
	s := New(nil)
	must(t, s.Fork("a", Trunk, 1))
	must(t, s.Delete(Trunk, "missing"))
	if i, _ := s.Branch("a"); i.Stale {
		t.Fatal("deleting nothing made a branch stale")
	}
	if _, err := s.Observe(Evidence{Rest: Likelihood(-1)}); err != nil {
		t.Fatal("Rest is meant to be ignored without likelihoods:", err)
	}
}
