// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/multivergence/multivergence/examples/internal/trace"
)

func TestExample(t *testing.T) {
	var out strings.Builder
	x := trace.New("routing", start, &out)
	run(x)
	if x.Fails > 0 || !strings.Contains(out.String(), "All checks passed.") {
		t.Fatalf("example failed:\n%s", out.String())
	}
}
