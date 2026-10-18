// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command incident shows an on-call agent narrowing down the cause of an
// outage. Checkout slows down; the agent holds four hypotheses at once, each
// a branch with a weight and the fix it would apply. Facts from the status
// page and the logs go to every branch, a hypothesis whose condition a fact
// breaks dies, and the agent's likelihoods for each clue move the weights.
// When one hypothesis passes 90%, its fix is promoted to the trunk.
//
// The likelihoods are the agent's judgment, as a person on call would make
// it: how likely is this clue if that hypothesis is true? The engine applies
// them by fixed rules and keeps the record.
//
//	go run ./examples/incident
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	mv "github.com/multivergence/multivergence"
	"github.com/multivergence/multivergence/examples/internal/trace"
)

type hypothesis struct {
	branch, cause, fix string
	weight             float64
}

var hypotheses = []hypothesis{
	{"db-pool", "the database connection pool is exhausted", "raise the pool from 50 to 120 and recycle idle connections", 0.35},
	{"bad-deploy", "release 4.12 broke checkout", "roll back to 4.11", 0.30},
	{"cache-node", "cache node cache-3 is failing and misses reach the database", "drain cache-3", 0.20},
	{"payments-api", "the payment provider is slow", "switch to the backup provider", 0.15},
}

// start is the simulated clock's first reading.
var start = time.Date(2026, 11, 4, 14, 2, 0, 0, time.UTC)

func main() {
	t := trace.New("incident", start, os.Stdout)
	run(t)
	t.Exit()
}

func run(t *trace.T) {
	s := t.S
	const hm = "15:04"
	t.Say("Multivergence example: an on-call agent narrows down the cause of an outage.")
	t.Say("Simulated clock. The likelihoods are the agent's judgment; the engine applies them.")

	t.Step(hm, "Alert: checkout p99 latency 4.8 s, normally 300 ms")
	t.Set(mv.Trunk, "alert", "checkout p99 4.8 s")
	t.Set(mv.Trunk, "release", "4.12, deployed 13:58")

	t.Step(hm, "The agent forks one branch per hypothesis, each with the fix it would apply")
	for _, h := range hypotheses {
		t.Must(s.Fork(h.branch, mv.Trunk, h.weight))
		t.Set(h.branch, "cause", h.cause)
		t.Set(h.branch, "fix", h.fix)
		t.Say("  %-12s  weight %.2f  %s", h.branch, h.weight, h.cause)
	}
	t.Must(s.Constrain("payments-api", mv.NotEquals("payments/status", "normal")))
	t.Say("  payments-api only holds while the provider isn't normal: payments/status != normal")
	t.Table()

	t.Wait(3 * time.Minute)
	t.Step(hm, "Clue 1: the provider's status page and its p99. Facts go to every branch")
	t.Say("  payments/status = normal, payments/p99_ms = 210")
	t.Observe(mv.Evidence{Facts: []mv.Fact{
		{Key: "payments/status", Value: []byte("normal")},
		{Key: "payments/p99_ms", Value: []byte("210")},
	}})
	t.Table()
	t.Check(t.Cause("payments-api") == mv.CauseViolated, "payments-api dies on the status fact")

	t.Wait(4 * time.Minute)
	t.Step(hm, "Clue 2: the logs show 1,200 \"timeout acquiring connection\" errors in 5 minutes")
	t.Say("  How likely is this clue under each hypothesis? db-pool 0.9, bad-deploy 0.5, cache-node 0.1")
	t.Say("  Anything left below 5%% is dropped (PruneBelow 0.05)")
	t.Observe(mv.Evidence{
		Facts:       []mv.Fact{{Key: "logs/pool_timeouts_5m", Value: []byte("1200")}},
		Likelihoods: map[string]float64{"db-pool": 0.9, "bad-deploy": 0.5, "cache-node": 0.1},
		PruneBelow:  0.05,
	})
	t.Table()
	t.Check(t.Cause("cache-node") == mv.CausePruned, "cache-node is pruned")
	db, dep := 0.35*0.9, 0.30*0.5
	t.Check(trace.Near(t.Prob("db-pool"), db/(db+dep)), "db-pool after clue 2")

	t.Wait(5 * time.Minute)
	t.Step(hm, "Clue 3: a canary on release 4.11 shows the same timeouts")
	t.Say("  A rollback wouldn't help if 4.11 fails too: db-pool 0.9, bad-deploy 0.1")
	t.Observe(mv.Evidence{
		Facts:       []mv.Fact{{Key: "canary/4.11", Value: []byte("same timeouts")}},
		Likelihoods: map[string]float64{"db-pool": 0.9, "bad-deploy": 0.1},
	})
	t.Table()
	db, dep = db*0.9, dep*0.1
	want := db / (db + dep)
	t.Check(trace.Near(t.Prob("db-pool"), want), fmt.Sprintf("db-pool is %.4f after clue 3", want))

	t.Wait(time.Minute)
	t.Step(hm, "Policy: act when one hypothesis passes 90%. Promote it")
	best, _ := s.Best()
	t.Check(best.Name == "db-pool" && best.Probability > 0.9, "db-pool is above 90%")
	t.Say("  best: %s at %.1f%%. bad-deploy, still live at %.1f%%, is dropped by the promote", best.Name, 100*best.Probability, 100*t.Prob("bad-deploy"))
	t.Must(s.Promote(best.Name))
	t.Say("  trunk: cause = %s", t.Get(mv.Trunk, "cause"))
	t.Say("  trunk: fix = %s", t.Get(mv.Trunk, "fix"))
	t.Say("  The clues stay on the trunk too: payments/status = %s, logs/pool_timeouts_5m = %s",
		t.Get(mv.Trunk, "payments/status"), t.Get(mv.Trunk, "logs/pool_timeouts_5m"))
	t.Check(t.Get(mv.Trunk, "fix") == hypotheses[0].fix, "the trunk holds the pool fix")
	t.Check(t.Get(mv.Trunk, "canary/4.11") == "same timeouts", "facts reached the trunk")

	t.Step(hm, "The history is the incident timeline")
	for _, e := range s.History() {
		if e.Kind == "killed" || e.Kind == "promote" || e.Kind == "observe" {
			t.Say("%s", strings.TrimRight(fmt.Sprintf("  %s  %-8s %-12s %s", e.Time.Format(hm), e.Kind, e.Branch, e.Detail), " "))
		}
	}
	t.End()
}
