// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command fixes shows a coding agent trying three fixes for one bug, each in
// its own branch. A bug report says Median gets even-length input wrong. The
// agent drafts three patches and gives each a weight, its confidence. A
// patch that changes the function's signature dies on the spot, because
// every branch carries the constraint that the public API stays the same. A
// review note lowers one patch's weight, the full test suite kills it, and
// the patch that passes is promoted, so the fix lands on the trunk in one
// step. The patches are real Go functions and the tests really run.
//
//	go run ./examples/fixes
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	mv "github.com/multivergence/multivergence"
	"github.com/multivergence/multivergence/examples/internal/trace"
)

const api = "func Median(xs []float64) float64"

// A candidate is one version of Median: its source, as the agent would
// write it to the file, and the same code compiled into this program so the
// tests can run it.
type candidate struct {
	branch string
	weight float64
	about  string
	src    string
	sig    string
	fn     func([]float64) float64
}

var original = candidate{
	src: `func Median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}`,
	sig: api,
	fn: func(xs []float64) float64 {
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		return s[len(s)/2]
	},
}

var candidates = []candidate{
	{
		branch: "patch-a", weight: 0.5, about: "average the two middle values",
		src: `func Median(xs []float64) float64 {
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 0 {
		return (xs[n/2-1] + xs[n/2]) / 2
	}
	return xs[n/2]
}`,
		sig: api,
		fn: func(xs []float64) float64 {
			sort.Float64s(xs)
			n := len(xs)
			if n%2 == 0 {
				return (xs[n/2-1] + xs[n/2]) / 2
			}
			return xs[n/2]
		},
	},
	{
		branch: "patch-b", weight: 0.3, about: "sort a copy, then average the two middle values",
		src: `func Median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 0 {
		return (s[n/2-1] + s[n/2]) / 2
	}
	return s[n/2]
}`,
		sig: api,
		fn: func(xs []float64) float64 {
			s := append([]float64(nil), xs...)
			sort.Float64s(s)
			n := len(s)
			if n%2 == 0 {
				return (s[n/2-1] + s[n/2]) / 2
			}
			return s[n/2]
		},
	},
	{
		branch: "patch-c", weight: 0.2, about: "return an error for empty input as well",
		src: `func Median(xs []float64) (float64, error) {
	if len(xs) == 0 {
		return 0, errors.New("median of nothing")
	}
	...
}`,
		sig: "func Median(xs []float64) (float64, error)",
	},
}

// The test suite. Each test gets fresh input, so one test can't spoil
// another.
type test struct {
	name string
	run  func(func([]float64) float64) bool
}

var suite = []test{
	{"TestOdd", func(m func([]float64) float64) bool { return m([]float64{3, 1, 2}) == 2 }},
	{"TestEven", func(m func([]float64) float64) bool { return m([]float64{4, 1, 3, 2}) == 2.5 }},
	{"TestSingle", func(m func([]float64) float64) bool { return m([]float64{7}) == 7 }},
	{"TestNegative", func(m func([]float64) float64) bool { return m([]float64{-5, -1, -3}) == -3 }},
	{"TestKeepsInput", func(m func([]float64) float64) bool {
		xs := []float64{3, 1, 2}
		m(xs)
		return xs[0] == 3 && xs[1] == 1 && xs[2] == 2
	}},
}

// start is the simulated clock's first reading.
var start = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

func main() {
	t := trace.New("fixes", start, os.Stdout)
	run(t)
	t.Exit()
}

func run(t *trace.T) {
	s := t.S
	const hm = "15:04:05"
	t.Say("Multivergence example: a coding agent tries three fixes, each in its own branch.")
	t.Say("Simulated clock. The patches are real Go functions and the tests really run.")

	t.Step(hm, "The bug report and the current code go on the trunk")
	t.Set(mv.Trunk, "src/stats.go", original.src)
	t.Set(mv.Trunk, "api/Median", original.sig)
	t.Set(mv.Trunk, "bug", "Median([1 2 3 4]) returns 3, want 2.5")
	t.Say("  bug: Median([1 2 3 4]) returns 3, want 2.5")
	t.Say("  The current code fails %s.", strings.Join(failing(original.fn), ", "))

	t.Step(hm, "The agent drafts three patches. Every branch must keep the public API")
	for _, c := range candidates {
		t.Must(s.Fork(c.branch, mv.Trunk, c.weight))
		t.Must(s.Constrain(c.branch, mv.Equals("api/Median", api), mv.NotEquals("tests/result", "fail")))
		t.Say("  %s  weight %.1f  %s", c.branch, c.weight, c.about)
	}
	t.Say("  constraints on each: api/Median == %q, tests/result != fail", api)
	t.Wait(40 * time.Second)
	for _, c := range candidates {
		t.Set(c.branch, "src/stats.go", c.src)
		t.Set(c.branch, "api/Median", c.sig)
	}
	t.Table()
	t.Check(t.Cause("patch-c") == mv.CauseViolated, "patch-c dies for changing the signature")
	t.Check(t.Get(mv.Trunk, "src/stats.go") == original.src, "the trunk still has the original code")
	t.Say("  patch-c is gone before any test ran on it. The trunk still has the original code.")

	t.Wait(25 * time.Second)
	t.Step(hm, "Quick check: the case from the bug report, on each live patch")
	for _, c := range live(t) {
		ok := suite[1].run(c.fn)
		t.Set(c.branch, "tests/quick", passFail(ok))
		t.Say("  %s  TestEven %s", c.branch, passFail(ok))
		t.Check(ok, c.branch+" passes the reported case")
	}
	t.Say("  Both pass, so the quick check doesn't tell them apart. Weights stay as they are.")

	t.Wait(30 * time.Second)
	t.Step(hm, "Review: the agent reads each diff. One sorts the caller's slice in place")
	like := map[string]float64{}
	for _, c := range live(t) {
		if strings.Contains(c.src, "sort.Float64s(xs)") {
			like[c.branch] = 0.4
			t.Say("  %s  sorts xs in place, which callers may not expect: likelihood 0.4", c.branch)
		} else {
			like[c.branch] = 1
			t.Say("  %s  leaves xs alone: likelihood 1", c.branch)
		}
	}
	t.Observe(mv.Evidence{Likelihoods: like})
	t.Table()
	// After patch-c died: 0.5/0.8 and 0.3/0.8. Times 0.4 and 1, renormalised.
	want := 0.3 / (0.5*0.4 + 0.3)
	t.Check(trace.Near(t.Prob("patch-b"), want), fmt.Sprintf("patch-b is %.4f after the review", want))

	t.Wait(2 * time.Minute)
	t.Step(hm, "Full test suite on each live patch")
	for _, c := range live(t) {
		bad := failing(c.fn)
		t.Set(c.branch, "tests/failed", fmt.Sprint(len(bad)))
		if len(bad) == 0 {
			t.Say("  %s  %d of %d pass", c.branch, len(suite), len(suite))
			t.Set(c.branch, "tests/result", "pass")
		} else {
			t.Say("  %s  %d of %d pass; fails %s", c.branch, len(suite)-len(bad), len(suite), strings.Join(bad, ", "))
			t.Set(c.branch, "tests/result", "fail")
		}
	}
	t.Table()
	t.Check(t.Cause("patch-a") == mv.CauseViolated, "patch-a dies when its tests fail")
	o, err := s.Sweep()
	t.Must(err)
	name, settled := o.Settled()
	t.Check(settled && name == "patch-b", "patch-b is the one fix left")

	t.Step(hm, "Promote the fix that passed. The trunk gets its code in one step")
	t.Must(s.Promote("patch-b"))
	t.Say("  trunk src/stats.go is now patch-b; tests/result = %s", t.Get(mv.Trunk, "tests/result"))
	t.Say("  generation %d; branches left: %d", s.Generation(), len(s.Branches()))
	t.Check(t.Get(mv.Trunk, "src/stats.go") == candidates[1].src, "the trunk holds patch-b's code")
	t.Check(len(failing(candidates[1].fn)) == 0, "the promoted code passes every test")
	t.End()
}

// live returns the candidates whose branches are still live.
func live(t *trace.T) []candidate {
	var out []candidate
	for _, c := range candidates {
		if t.Prob(c.branch) >= 0 {
			out = append(out, c)
		}
	}
	return out
}

// failing runs the suite and names the tests that fail.
func failing(fn func([]float64) float64) []string {
	var bad []string
	for _, x := range suite {
		if !x.run(fn) {
			bad = append(bad, x.name)
		}
	}
	return bad
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}
