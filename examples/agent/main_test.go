// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	mv "github.com/multivergence/multivergence"
)

func TestDemo(t *testing.T) {
	for _, file := range []bool{false, true} {
		var out strings.Builder
		d := &demo{clock: time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC), out: &out}
		opts := &mv.Options{Now: func() time.Time { return d.clock }}
		d.s = mv.New(opts)
		if file {
			s, err := mv.Open(filepath.Join(t.TempDir(), "trip.mv"), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			d.s = s
		}
		d.run()
		if d.fails > 0 || !strings.Contains(out.String(), "All checks passed.") {
			t.Fatalf("demo failed (file %v):\n%s", file, out.String())
		}
	}
}
