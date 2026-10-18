// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command routing shows a dispatcher's planner keeping four delivery plans
// open on the morning before a van leaves. Each plan is a branch with its
// route, a weight, a constraint for the van's capacity and, for the plan
// that takes a ferry, the ferry's booking cut-off as its deadline. A late
// order adds weight, a traffic report moves the weights, the cut-off passes,
// and the plan that is best when the dispatcher locks the route is promoted.
//
//	go run ./examples/routing
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	mv "github.com/multivergence/multivergence"
	"github.com/multivergence/multivergence/examples/internal/trace"
)

type plan struct {
	branch, route, van string
	weight, capacity   float64
}

var plans = []plan{
	{"ring-road", "ring road to the north estates, then the centre", "big van", 0.35, 900},
	{"city-centre", "straight through the centre, then the north estates", "big van", 0.30, 900},
	{"river-ferry", "ferry across the river, then the centre", "big van", 0.20, 900},
	{"small-van", "ring road, in the small electric van", "small van", 0.15, 700},
}

// start is the simulated clock's first reading.
var start = time.Date(2026, 11, 5, 7, 30, 0, 0, time.UTC)

func main() {
	t := trace.New("routing", start, os.Stdout)
	run(t)
	t.Exit()
}

func run(t *trace.T) {
	s := t.S
	const hm = "15:04"
	day := t.Clock
	t.Say("Multivergence example: a dispatcher keeps four delivery plans open until the van leaves.")
	t.Say("Simulated clock. Every number below comes from the engine.")

	t.Step(hm, "Today's load goes on the trunk")
	t.Set(mv.Trunk, "parcels", "18")
	t.Set(mv.Trunk, "load_kg", "640")
	t.Set(mv.Trunk, "window", "every stop by 12:00")
	t.Say("  18 parcels, 640 kg, every stop by 12:00")

	t.Step(hm, "The planner forks four plans. Each van's capacity is a constraint on the load")
	for _, p := range plans {
		t.Must(s.Fork(p.branch, mv.Trunk, p.weight))
		t.Set(p.branch, "route", p.route)
		t.Set(p.branch, "van", p.van)
		t.Must(s.Constrain(p.branch, mv.AtMost("load_kg", p.capacity)))
		t.Say("  %-12s  weight %.2f  %-9s  load_kg <= %.0f", p.branch, p.weight, p.van, p.capacity)
	}
	t.Must(s.Constrain("ring-road", mv.NotEquals("road/ring", "closed")))
	t.Must(s.Constrain("city-centre", mv.NotEquals("zone/centre", "restricted")))
	cutoff := day.Add(50 * time.Minute)
	t.Must(s.Expire("river-ferry", cutoff))
	t.Say("  ring-road also needs road/ring != closed; city-centre needs zone/centre != restricted")
	t.Say("  river-ferry must be booked on the ferry by %s: its deadline", cutoff.Format(hm))
	t.Table()

	t.At(day.Add(15 * time.Minute))
	t.Step(hm, "A late order: one more pallet, 120 kg. The new load is a fact for every plan")
	t.Say("  load_kg = 760")
	t.Observe(mv.Evidence{Facts: []mv.Fact{
		{Key: "load_kg", Value: []byte("760")},
		{Key: "parcels", Value: []byte("19")},
	}})
	t.Table()
	t.Check(t.Cause("small-van") == mv.CauseViolated, "the small van can't take 760 kg")

	t.At(day.Add(25 * time.Minute))
	t.Step(hm, "Traffic report: a lane closed on the ring road at junction 4, about 25 minutes of delay")
	t.Say("  The road is open, so ring-road's constraint holds; its chances drop.")
	t.Say("  How likely is this report if the plan is the fastest? ring-road 0.3, the others 0.9")
	t.Observe(mv.Evidence{
		Facts:       []mv.Fact{{Key: "road/ring", Value: []byte("lane closed at J4")}},
		Likelihoods: map[string]float64{"ring-road": 0.3},
		Rest:        mv.Likelihood(0.9),
	})
	t.Table()
	r, c, f := 0.35*0.3, 0.30*0.9, 0.20*0.9
	t.Check(trace.Near(t.Prob("city-centre"), c/(r+c+f)), "city-centre after the traffic report")

	t.At(day.Add(55 * time.Minute))
	t.Step(hm, "The depot manager signs off, five minutes after the ferry's cut-off")
	ferry, _ := s.Branch("river-ferry")
	t.Say("  river-ferry reads as %s already: nothing had to record it", ferry.Cause)
	t.Check(!ferry.Live && ferry.Cause == mv.CauseExpired, "a read shows the ferry plan expired at once")
	t.Table()
	best, _ := s.Best()
	t.Check(best.Name == "city-centre", "city-centre is the best plan left")
	want := c / (r + c)
	t.Check(trace.Near(best.Probability, want), fmt.Sprintf("city-centre is %.4f among the live plans", want))
	t.Say("  The dispatcher locks the best plan: %s at %.1f%%", best.Name, 100*best.Probability)
	t.Must(s.Promote(best.Name))
	t.Say("  trunk: route = %s", t.Get(mv.Trunk, "route"))
	t.Say("  trunk: van = %s, load_kg = %s, parcels = %s", t.Get(mv.Trunk, "van"), t.Get(mv.Trunk, "load_kg"), t.Get(mv.Trunk, "parcels"))
	t.Check(t.Get(mv.Trunk, "route") == plans[1].route, "the trunk holds the city-centre route")
	t.Check(t.Get(mv.Trunk, "load_kg") == "760", "the late order is on the trunk")

	t.Step(hm, "How each plan ended")
	for _, e := range s.History() {
		if e.Kind == "killed" || e.Kind == "promote" || e.Kind == "dropped" {
			t.Say("%s", strings.TrimRight(fmt.Sprintf("  %s  %-8s %-12s %s", e.Time.Format(hm), e.Kind, e.Branch, e.Detail), " "))
		}
	}
	t.End()
}
