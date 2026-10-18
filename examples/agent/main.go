// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command multivergence-demo is the agent demo: an agent booking a trip
// forks three plans, gets evidence back from its tools, and the engine
// collapses to the plan that survives. One plan dies when a fare breaks its
// budget, one when its seat hold runs out, and the weights move with each
// tool's answer. The clock is simulated, so the output is the same on every
// run.
//
//	go run ./examples/agent
//	go run ./examples/agent -f trip.mv   # keep the store in a new file
//
// It checks its own results and exits with 1 if anything comes out wrong.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	mv "github.com/multivergence/multivergence"
)

type demo struct {
	s     *mv.Store
	clock time.Time
	out   io.Writer
	fails int
}

func main() {
	file := flag.String("f", "", "keep the store in this file (it must not exist yet)")
	version := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *version {
		fmt.Println("multivergence-demo " + mv.Version)
		return
	}
	d := &demo{clock: time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC), out: os.Stdout}
	opts := &mv.Options{Now: func() time.Time { return d.clock }}
	if *file == "" {
		d.s = mv.New(opts)
	} else {
		if _, err := os.Stat(*file); err == nil {
			fatal(fmt.Errorf("%s exists; give a new file", *file))
		}
		s, err := mv.Open(*file, opts)
		fatal(err)
		defer s.Close()
		d.s = s
	}
	d.run()
	if d.fails > 0 {
		fmt.Fprintf(os.Stderr, "multivergence-demo: %d checks failed\n", d.fails)
		os.Exit(1)
	}
}

func (d *demo) run() {
	s := d.s
	d.say("Multivergence demo: an agent books a trip with three plans in flight at once.")
	d.say("Simulated clock. Every number below comes from the engine.")

	d.step("The request goes on the trunk, the state that is real now")
	d.set(mv.Trunk, "request", "Lisbon to Porto, arrive by 09:00")
	d.set(mv.Trunk, "budget", "400")
	d.say("  trunk: request = Lisbon to Porto, arrive by 09:00; budget = 400")

	d.step("The agent forks three plans. Each gets a weight (its prior), a constraint and its own booking")
	fatal(s.Fork("plan-train", mv.Trunk, 0.5))
	fatal(s.Fork("plan-flight", mv.Trunk, 0.3))
	fatal(s.Fork("plan-bus", mv.Trunk, 0.2))
	plans := []struct{ name, mode, booking, estimate string }{
		{"plan-train", "train", "Intercidades 06:52, arrives 08:41", "190"},
		{"plan-flight", "flight", "TP 1940 07:05, arrives 07:55", "250"},
		{"plan-bus", "bus", "Coach 06:30, arrives 08:35, seat held", "60"},
	}
	for _, p := range plans {
		d.set(p.name, "booking", p.booking)
		d.set(p.name, "fare/"+p.mode, p.estimate) // the plan's working estimate
		fatal(s.Constrain(p.name, mv.AtMost("fare/"+p.mode, 400)))
	}
	fatal(s.ExpireIn("plan-bus", 15*time.Minute))
	d.say("  plan-train   weight 0.5   constraint fare/train <= 400")
	d.say("  plan-flight  weight 0.3   constraint fare/flight <= 400")
	d.say("  plan-bus     weight 0.2   constraint fare/bus <= 400, seat hold runs out at 08:15")
	d.table()
	_, onTrunk, _ := s.Get(mv.Trunk, "booking")
	d.check(!onTrunk, "the trunk has no booking while the plans are open")
	d.say("  The trunk still has no booking: each plan wrote only to its own branch.")

	d.wait(2 * time.Minute)
	d.step("Tool call: fare search. Its answers are facts, written to every live plan")
	d.say("  fare/train = 180, fare/flight = 460, fare/bus = 60")
	o, err := s.Observe(mv.Evidence{Facts: []mv.Fact{
		{Key: "fare/train", Value: []byte("180")},
		{Key: "fare/flight", Value: []byte("460")},
		{Key: "fare/bus", Value: []byte("60")},
	}})
	fatal(err)
	d.killed(o)
	d.table()
	d.check(len(o.Killed) == 1 && o.Killed[0].Branch == "plan-flight" && o.Killed[0].Cause == mv.CauseViolated,
		"the flight dies of its budget constraint")
	d.check(near(prob(o, "plan-train"), 0.5/0.7), "the survivors keep their relative weights: train 5/7")

	d.wait(3 * time.Minute)
	d.step("Tool call: disruption feed. The agent turns it into likelihoods; the engine applies them")
	d.say("  \"Roadworks on the A1: coaches running about 40 minutes late. Trains on time.\"")
	d.say("  How likely is this news if the plan arrives by 09:00?  train 0.9, bus 0.3")
	o, err = s.Observe(mv.Evidence{Likelihoods: map[string]float64{"plan-train": 0.9, "plan-bus": 0.3}})
	fatal(err)
	d.table()
	// Prior 5/7 and 2/7; times 0.9 and 0.3; renormalised.
	wantTrain := (0.5 * 0.9) / (0.5*0.9 + 0.2*0.3)
	d.check(near(prob(o, "plan-train"), wantTrain), fmt.Sprintf("train is %.4f after the update", wantTrain))

	d.wait(15 * time.Minute)
	d.step("The agent waited for the traveller's go-ahead. The coach seat hold ran out at 08:15")
	o, err = s.Sweep()
	fatal(err)
	d.killed(o)
	d.table()
	name, settled := o.Settled()
	d.check(settled && name == "plan-train", "one plan is left, the train")
	if settled {
		d.say("  Settled: " + name + " is the only live plan.")
	}

	d.step("Promote the survivor. Its state becomes the trunk, in one step")
	fatal(s.Promote("plan-train"))
	v, _, _ := s.Get(mv.Trunk, "booking")
	f, _, _ := s.Get(mv.Trunk, "fare/train")
	d.say(fmt.Sprintf("  trunk: booking = %s; fare/train = %s", v, f))
	d.say(fmt.Sprintf("  generation %d; branches left: %d", s.Generation(), len(s.Branches())))
	d.check(string(v) == "Intercidades 06:52, arrives 08:41", "the trunk holds the train booking")
	d.check(len(s.Branches()) == 0 && s.Generation() == 1, "the other plans are gone")

	d.say("")
	if d.fails == 0 {
		d.say("All checks passed.")
	}
}

func (d *demo) say(s string) { fmt.Fprintln(d.out, s) }

func (d *demo) step(title string) {
	fmt.Fprintf(d.out, "\n[%s] %s\n", d.clock.Format("15:04"), title)
}

func (d *demo) wait(t time.Duration) { d.clock = d.clock.Add(t) }

func (d *demo) set(branch, key, val string) {
	if err := d.s.Set(branch, key, []byte(val)); err != nil {
		if errors.Is(err, mv.ErrKilled) {
			d.say("  " + err.Error())
			return
		}
		fatal(err)
	}
}

func (d *demo) killed(o *mv.Outcome) {
	for _, k := range o.Killed {
		why := k.Cause.String()
		if k.Detail != "" {
			why += ": " + k.Detail
		}
		d.say(fmt.Sprintf("  %s died (%s)", k.Branch, why))
	}
}

func (d *demo) table() {
	d.say("")
	d.say("    PLAN          PROBABILITY  STATUS")
	for _, b := range d.s.Branches() {
		bar, p, status := "", "      -    ", "live"
		if b.Live {
			p = fmt.Sprintf("%6.1f%%    ", 100*b.Probability)
			for i := 0; i < int(math.Round(b.Probability*20)); i++ {
				bar += "#"
			}
		} else {
			status = b.Cause.String()
		}
		d.say(strings.TrimRight(fmt.Sprintf("    %-12s  %s %-8s %s", b.Name, p, status, bar), " "))
	}
}

func (d *demo) check(ok bool, what string) {
	if !ok {
		d.fails++
		d.say("  CHECK FAILED: " + what)
	}
}

func prob(o *mv.Outcome, name string) float64 {
	for _, b := range o.Live {
		if b.Name == name {
			return b.Probability
		}
	}
	return -1
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "multivergence-demo:", err)
		os.Exit(1)
	}
}
