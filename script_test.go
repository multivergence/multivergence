// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// script makes a random but repeatable run of changes. What it does next can
// depend on the store (which branches are live), so two stores given the
// same seed and the same changes stay in step. The clock is part of the
// script: it moves a random amount before each change.
type script struct {
	rng   *rand.Rand
	clock time.Time
	forks int
	big   bool // sometimes write values of several KB
	// promotes makes promotes land more often: few direct trunk writes
	// (which make branches stale), and promotes go to a live branch.
	promotes bool
}

func newScript(seed int64) *script {
	return &script{rng: rand.New(rand.NewSource(seed)), clock: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
}

func (sc *script) now() time.Time { return sc.clock }

func (sc *script) opts() *Options { return &Options{Now: sc.now} }

// expected reports whether an error is one the script can cause on purpose:
// a change the rules refuse, not a fault.
func expected(err error) bool {
	for _, e := range []error{ErrNotFound, ErrNotLive, ErrStale, ErrTooManyBranches, ErrKilled, ErrMissingLikelihood, ErrExists} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

func (sc *script) key() string { return "k" + strconv.Itoa(sc.rng.Intn(24)) }

func (sc *script) value() []byte {
	n := sc.rng.Intn(40)
	if sc.big && sc.rng.Intn(20) == 0 {
		n = 2000 + sc.rng.Intn(8000)
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + sc.rng.Intn(26))
	}
	return b
}

// pick returns a live branch, the trunk (when allowed), or a name that
// doesn't exist, so refused changes get exercised too.
func (sc *script) pick(s *Store, trunk bool) string {
	live := s.Live()
	r := sc.rng.Intn(20)
	switch {
	case r == 0:
		return "ghost"
	case trunk && (len(live) == 0 || (r < 4 && (!sc.promotes || sc.rng.Intn(8) == 0))):
		return Trunk
	case len(live) == 0:
		return "ghost"
	}
	return live[sc.rng.Intn(len(live))].Name
}

// step makes one change and returns its name and error.
func (sc *script) step(s *Store) (string, error) {
	sc.clock = sc.clock.Add(time.Duration(sc.rng.Intn(300)) * time.Second)
	if sc.promotes && sc.rng.Intn(5) == 0 {
		if b, ok := s.Best(); ok && !b.Stale {
			return "promote", s.Promote(b.Name)
		}
	}
	r := sc.rng.Intn(100)
	switch {
	case r < 28:
		return "set", s.Set(sc.pick(s, true), sc.key(), sc.value())
	case r < 33:
		return "delete", s.Delete(sc.pick(s, true), sc.key())
	case r < 48:
		sc.forks++
		return "fork", s.Fork("b"+strconv.Itoa(sc.forks), sc.pick(s, true), 0.1+sc.rng.Float64())
	case r < 52:
		return "weight", s.Weight(sc.pick(s, false), 0.05+sc.rng.Float64())
	case r < 57:
		c := AtMost(sc.key(), float64(sc.rng.Intn(1000)))
		if sc.rng.Intn(2) == 0 {
			c = NotEquals(sc.key(), "x")
		}
		return "constrain", s.Constrain(sc.pick(s, false), c)
	case r < 62:
		return "expire", s.Expire(sc.pick(s, false), sc.clock.Add(time.Duration(sc.rng.Intn(3600)-300)*time.Second))
	case r < 78:
		ev := Evidence{}
		for i := sc.rng.Intn(3); i > 0; i-- {
			f := Fact{Key: sc.key(), Value: []byte(strconv.Itoa(sc.rng.Intn(1200)))}
			if sc.rng.Intn(6) == 0 {
				f = Fact{Key: f.Key, Delete: true}
			}
			ev.Facts = append(ev.Facts, f)
		}
		if sc.rng.Intn(3) > 0 {
			ev.Likelihoods = map[string]float64{}
			for _, b := range s.Live() {
				if sc.rng.Intn(3) > 0 {
					x := sc.rng.Float64()
					if sc.rng.Intn(12) == 0 {
						x = 0
					}
					ev.Likelihoods[b.Name] = x
				}
			}
			if sc.rng.Intn(4) > 0 {
				ev.Rest = Likelihood(sc.rng.Float64())
			}
		}
		if sc.rng.Intn(4) == 0 {
			ev.PruneBelow = sc.rng.Float64() * 0.2
		}
		_, err := s.Observe(ev)
		return "observe", err
	case r < 81:
		return "discard", s.Discard(sc.pick(s, false))
	case r < 96:
		// Promote the most likely branch, or a random one.
		name := sc.pick(s, false)
		if b, ok := s.Best(); ok && (sc.promotes || sc.rng.Intn(2) == 0) {
			name = b.Name
		}
		return "promote", s.Promote(name)
	default:
		_, err := s.Sweep()
		return "sweep", err
	}
}

// run makes n changes and returns how many there were of each kind and how
// many were refused.
func (sc *script) run(t testing.TB, s *Store, n int) map[string]int {
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		kind, err := sc.step(s)
		counts[kind]++
		if err != nil {
			if !expected(err) {
				t.Fatalf("change %d (%s): %v", i, kind, err)
			}
			counts["refused"]++
		}
	}
	return counts
}

// digests replays a crash-test script (big values, promote-heavy) in memory and returns the digest after each
// number of changes, from 0 to n.
func digests(t testing.TB, seed int64, n int) []string {
	sc := newScript(seed)
	sc.big, sc.promotes = true, true
	s := New(sc.opts())
	out := []string{s.Digest()}
	for i := 0; i < n; i++ {
		if _, err := sc.step(s); err != nil && !expected(err) {
			t.Fatalf("change %d: %v", i, err)
		}
		out = append(out, s.Digest())
	}
	return out
}

// rounds scales a test's size with MV_SCALE, which the recording script
// raises.
func rounds(n int) int {
	if v, err := strconv.Atoi(os.Getenv("MV_SCALE")); err == nil && v > 0 {
		return n * v
	}
	return n
}

// record writes a test's figures to test/results when MV_RESULTS names
// that directory.
func record(t testing.TB, name string, v map[string]any) {
	dir := os.Getenv("MV_RESULTS")
	if dir == "" {
		return
	}
	v["test"] = t.Name()
	v["recorded"] = time.Now().UTC().Format(time.RFC3339)
	v["go"] = runtime.Version()
	v["os"] = runtime.GOOS + "/" + runtime.GOARCH
	v["cpus"] = runtime.NumCPU()
	v["version"] = Version
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func joinCounts(m map[string]int) string {
	var parts []string
	for _, k := range []string{"set", "delete", "fork", "weight", "constrain", "expire", "observe", "discard", "promote", "sweep", "refused"} {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
