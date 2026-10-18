// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command speculate shows speculative execution with Multivergence: start
// the likely next step before you know it's needed, in a branch, and throw
// it away cheaply if you were wrong.
//
// A document of 100,000 lines sits on the trunk. Before the user acts, the
// program guesses what they will do next and prepares each guess in its own
// branch: reformatting the most-edited section, or renaming a term
// throughout. Then the real action arrives. A guess that matches is
// promoted, so the work is already done; the others are discarded, and
// cost no copy of the document.
//
//	go run ./examples/speculate
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	mv "github.com/multivergence/multivergence"
)

const lines = 100_000

func main() {
	base := heap()
	s := mv.New(nil)
	for i := 0; i < lines; i++ {
		check(s.Set(mv.Trunk, fmt.Sprintf("line/%06d", i), []byte(fmt.Sprintf("Line %d of the specification, about the widget.", i))))
	}
	before := heap()

	start := time.Now()
	check(s.Fork("guess-reformat", mv.Trunk, 0.7))
	check(s.Fork("guess-rename", mv.Trunk, 0.3))
	forked := time.Since(start)

	// Prepare both guesses. The reformat touches 200 lines; the rename
	// touches every line that mentions "widget", here 1 in 50.
	for i := 40_000; i < 40_200; i++ {
		k := fmt.Sprintf("line/%06d", i)
		v, _, _ := s.Get(mv.Trunk, k)
		check(s.Set("guess-reformat", k, []byte("  "+string(v))))
	}
	for i := 0; i < lines; i += 50 {
		k := fmt.Sprintf("line/%06d", i)
		v, _, _ := s.Get(mv.Trunk, k)
		check(s.Set("guess-rename", k, []byte(strings.ReplaceAll(string(v), "widget", "gadget"))))
	}
	after := heap()

	fmt.Printf("Trunk: %d lines. Two guesses prepared in branches.\n", lines)
	fmt.Printf("Forking both took %v; the branches changed 200 and %d lines.\n", forked.Round(time.Microsecond), lines/50)
	fmt.Printf("The trunk takes about %.1f MB. Both prepared branches together added about %.1f MB.\n",
		float64(before-base)/1e6, float64(after-before)/1e6)

	// The user's real action arrives: they rename the term.
	actual := "guess-rename"
	fmt.Printf("\nThe user renames widget to gadget. The %s branch already did it.\n", actual)
	check(s.Discard("guess-reformat"))
	check(s.Promote(actual))
	v, _, _ := s.Get(mv.Trunk, "line/000050")
	fmt.Printf("Promoted. Trunk line 50 now reads: %s\n", v)
	if !strings.Contains(string(v), "gadget") {
		fmt.Fprintln(os.Stderr, "speculate: the promoted work is missing")
		os.Exit(1)
	}
}

func heap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "speculate:", err)
		os.Exit(1)
	}
}
