// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command holds shows an agent booking a venue for a team offsite with
// several holds open at once. Each venue's hold is a branch with the hold's
// expiry as its deadline, the venue's capacity as a constraint on the head
// count, and the budget as a constraint on the quote. Quotes, a new head
// count and a team poll arrive over two days; holds die as they stop
// fitting, one hold is extended before it runs out, and the venue that
// confirms is promoted. The program then lists the holds to release: a
// branch is data, and letting the venues know is the caller's job.
//
//	go run ./examples/holds
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	mv "github.com/multivergence/multivergence"
	"github.com/multivergence/multivergence/examples/internal/trace"
)

type venue struct {
	branch   string
	weight   float64
	capacity float64
	hold     time.Duration
	estimate string // from the venue's price list
	quote    string // what the venue quotes back
}

var venues = []venue{
	{"hall-a", 0.3, 45, 48 * time.Hour, "5200", "5200"},
	{"loft-b", 0.3, 60, 24 * time.Hour, "5500", "5400"},
	{"barn-c", 0.2, 80, 72 * time.Hour, "5800", "7200"},
	{"villa-d", 0.2, 50, 72 * time.Hour, "5900", "5900"},
}

const budget = 6000

// start is the simulated clock's first reading.
var start = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

func main() {
	t := trace.New("holds", start, os.Stdout)
	run(t)
	t.Exit()
}

func run(t *trace.T) {
	s := t.S
	const dhm = "Mon 15:04"
	day0 := t.Clock
	t.Say("Multivergence example: an agent books an offsite venue with four holds open at once.")
	t.Say("Simulated clock over two days. Every probability below comes from the engine.")

	t.Step(dhm, "The request goes on the trunk before any hold is placed")
	t.Set(mv.Trunk, "event", "team offsite, Thursday and Friday")
	t.Set(mv.Trunk, "headcount", "40")
	t.Say("  headcount = 40, budget = %d", budget)

	t.Step(dhm, "Four venues agree to hold the dates. Each hold is a branch")
	for _, v := range venues {
		t.Must(s.Fork(v.branch, mv.Trunk, v.weight))
		t.Set(v.branch, "venue", v.branch)
		t.Set(v.branch, "quote/total", v.estimate)
		t.Must(s.Constrain(v.branch, mv.AtMost("headcount", v.capacity), mv.AtMost("quote/total", budget)))
		t.Must(s.Expire(v.branch, t.Clock.Add(v.hold)))
		t.Say("  %-8s  weight %.1f  seats %2.0f  est. %s  hold until %s", v.branch, v.weight, v.capacity, v.estimate, t.Clock.Add(v.hold).Format(dhm))
	}
	t.Say("  constraints on each: headcount <= its seats, quote/total <= %d", budget)
	t.Table()

	t.At(day0.Add(5 * time.Hour))
	t.Step(dhm, "Quotes come back. Each is written to its own hold only")
	for _, v := range venues {
		t.Say("  %-8s  quote %s", v.branch, v.quote)
		t.Set(v.branch, "quote/total", v.quote)
	}
	t.Table()
	t.Check(t.Cause("barn-c") == mv.CauseViolated, "barn-c dies on its quote")

	t.At(day0.Add(23 * time.Hour))
	t.Step(dhm, "loft-b's hold ends in an hour. The agent asks; the venue extends it to 18:00")
	t.Must(s.Expire("loft-b", day0.Add(33*time.Hour)))
	b, _ := s.Branch("loft-b")
	t.Say("  loft-b deadline now %s", b.Deadline.Format(dhm))
	t.Check(b.Deadline.Equal(day0.Add(33*time.Hour)), "loft-b's deadline moved to Tuesday 18:00")

	t.At(day0.Add(26 * time.Hour))
	t.Step(dhm, "Six more people sign up. The head count is a fact, written to every hold")
	t.Say("  headcount = 46")
	t.Observe(mv.Evidence{Facts: []mv.Fact{{Key: "headcount", Value: []byte("46")}}})
	t.Table()
	t.Check(t.Cause("hall-a") == mv.CauseViolated, "hall-a dies: 46 people, 45 seats")

	t.At(day0.Add(30 * time.Hour))
	t.Step(dhm, "The team poll closes. The agent turns it into likelihoods")
	t.Say("  loft-b 0.7, villa-d 0.3")
	t.Observe(mv.Evidence{Likelihoods: map[string]float64{"loft-b": 0.7, "villa-d": 0.3}})
	t.Table()
	lb, vd := 0.3*0.7, 0.2*0.3
	t.Check(trace.Near(t.Prob("loft-b"), lb/(lb+vd)), "loft-b after the poll")

	t.At(day0.Add(30*time.Hour + 30*time.Minute))
	t.Step(dhm, "The manager approves and loft-b confirms the booking. Promote it")
	var release []string
	for _, x := range s.Live() {
		if x.Name != "loft-b" {
			release = append(release, x.Name)
		}
	}
	t.Must(s.Promote("loft-b"))
	t.Say("  trunk: venue = %s, quote/total = %s, headcount = %s", t.Get(mv.Trunk, "venue"), t.Get(mv.Trunk, "quote/total"), t.Get(mv.Trunk, "headcount"))
	t.Say("  holds to release with the venues: %v", release)
	t.Check(t.Get(mv.Trunk, "venue") == "loft-b", "the trunk holds the loft-b booking")
	t.Check(len(release) == 1 && release[0] == "villa-d", "villa-d's hold is the one to release")
	t.Say("  Without the extension, loft-b's hold would have run out at %s,", day0.Add(24*time.Hour).Format(dhm))
	t.Say("  before the new head count and the poll arrived.")

	t.Step(dhm, "The history shows each hold's fate")
	for _, e := range s.History() {
		if e.Kind == "killed" || e.Kind == "expire" || e.Kind == "promote" || e.Kind == "dropped" {
			t.Say("%s", strings.TrimRight(fmt.Sprintf("  %s  %-8s %-8s %s", e.Time.Format(dhm), e.Kind, e.Branch, e.Detail), " "))
		}
	}
	t.End()
}
