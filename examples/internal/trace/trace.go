// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package trace holds what the use examples share: a simulated clock, a
// printed trace of each step with a table of the branches, and checks that
// count failures instead of stopping.
package trace

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	mv "github.com/multivergence/multivergence"
)

// T is one run of an example.
type T struct {
	S     *mv.Store
	Clock time.Time
	Out   io.Writer
	Fails int
	Name  string // the example's name, for errors
}

// New starts a run in memory with a simulated clock at start.
func New(name string, start time.Time, out io.Writer) *T {
	t := &T{Clock: start, Out: out, Name: name}
	t.S = mv.New(&mv.Options{Now: func() time.Time { return t.Clock }})
	return t
}

// Say prints a line.
func (t *T) Say(format string, a ...any) { fmt.Fprintf(t.Out, format+"\n", a...) }

// Step prints a step heading with the simulated time.
func (t *T) Step(layout, title string) {
	fmt.Fprintf(t.Out, "\n[%s] %s\n", t.Clock.Format(layout), title)
}

// Wait moves the clock forward.
func (t *T) Wait(d time.Duration) { t.Clock = t.Clock.Add(d) }

// At moves the clock to a time, which must not be earlier than now.
func (t *T) At(when time.Time) {
	if when.Before(t.Clock) {
		t.Must(fmt.Errorf("clock moved back to %v", when))
	}
	t.Clock = when
}

// Set writes a key and reports a branch that died of it.
func (t *T) Set(branch, key, val string) (died bool) {
	err := t.S.Set(branch, key, []byte(val))
	if errors.Is(err, mv.ErrKilled) {
		t.Say("  %s", strings.TrimPrefix(err.Error(), "multivergence: "))
		return true
	}
	t.Must(err)
	return false
}

// Get reads a key, or "" when it is missing.
func (t *T) Get(branch, key string) string {
	v, _, err := t.S.Get(branch, key)
	t.Must(err)
	return string(v)
}

// Observe applies evidence and prints the deaths.
func (t *T) Observe(ev mv.Evidence) *mv.Outcome {
	o, err := t.S.Observe(ev)
	t.Must(err)
	t.Killed(o)
	return o
}

// Killed prints the branches an outcome killed.
func (t *T) Killed(o *mv.Outcome) {
	for _, k := range o.Killed {
		why := k.Cause.String()
		if k.Detail != "" {
			why += ": " + k.Detail
		}
		t.Say("  %s died (%s)", k.Branch, why)
	}
}

// Table prints every branch with its probability or why it died.
func (t *T) Table() {
	t.Say("")
	t.Say("    %-14s  PROBABILITY  STATUS", "BRANCH")
	for _, b := range t.S.Branches() {
		bar, p, status := "", "      -    ", "live"
		if b.Live {
			p = fmt.Sprintf("%6.1f%%    ", 100*b.Probability)
			bar = strings.Repeat("#", int(math.Round(b.Probability*20)))
		} else {
			status = b.Cause.String()
		}
		t.Say("%s", strings.TrimRight(fmt.Sprintf("    %-14s  %s %-9s %s", b.Name, p, status, bar), " "))
	}
}

// Prob is a branch's probability now, or -1 if it is dead or missing.
func (t *T) Prob(name string) float64 {
	b, err := t.S.Branch(name)
	if err != nil || !b.Live {
		return -1
	}
	return b.Probability
}

// Cause is why a branch died, CauseNone while it lives.
func (t *T) Cause(name string) mv.Cause {
	b, err := t.S.Branch(name)
	t.Must(err)
	return b.Cause
}

// Check counts a failed check and says which.
func (t *T) Check(ok bool, what string) {
	if !ok {
		t.Fails++
		t.Say("  CHECK FAILED: %s", what)
	}
}

// End prints the closing line.
func (t *T) End() {
	t.Say("")
	if t.Fails == 0 {
		t.Say("All checks passed.")
	}
}

// Must stops the program on an error.
func (t *T) Must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", t.Name, err)
		os.Exit(1)
	}
}

// Near reports whether two probabilities agree to 12 places.
func Near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// Exit ends main with 1 if any check failed.
func (t *T) Exit() {
	if t.Fails > 0 {
		fmt.Fprintf(os.Stderr, "%s: %d checks failed\n", t.Name, t.Fails)
		os.Exit(1)
	}
}
