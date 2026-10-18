// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type result struct {
	code   int
	stdout string
	stderr string
}

func run(t *testing.T, file string, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(append([]string{"-f", file}, args...), strings.NewReader(stdin), &out, &errb)
	return result{code, out.String(), errb.String()}
}

func TestCommands(t *testing.T) {
	t.Setenv("MULTIVERGENCE_NOW", "2026-11-02T08:00:00Z")
	f := filepath.Join(t.TempDir(), "s.mv")
	steps := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"version"}, 0, "multivergence 0.1.0"},
		{[]string{"set", "budget", "400"}, 0, ""},
		{[]string{"get", "budget"}, 0, "400\n"},
		{[]string{"get", "nope"}, ExitNotFound, ""},
		{[]string{"fork", "a", "-weight", "3"}, 0, ""},
		{[]string{"fork", "-weight", "1", "b"}, 0, ""}, // flags before or after
		{[]string{"set", "-b", "a", "fare", "100"}, 0, ""},
		{[]string{"constrain", "a", "fare <= 200", "fare exists"}, 0, ""},
		{[]string{"keys", "-b", "a", "-values"}, 0, "budget\t400\nfare\t100\n"},
		{[]string{"keys", "-b", "a", "f"}, 0, "fare\n"},
		{[]string{"best"}, 0, "a\n"},
		{[]string{"set", "-b", "a", "fare", "250"}, ExitKilled, "branch a died: violated (fare <= 200)\n"},
		{[]string{"best"}, 0, "b\n"},
		{[]string{"expire", "b", "-in", "10m"}, 0, ""},
		{[]string{"expire", "b"}, ExitUsage, ""},
		{[]string{"expire", "b", "-in", "1m", "-clear"}, ExitUsage, ""},
		{[]string{"weight", "b", "x"}, ExitUsage, ""},
		{[]string{"observe", "-l", "b=0.5"}, 0, ""},
		{[]string{"discard", "b"}, 0, ""},
		{[]string{"fork", "c"}, 0, ""},
		{[]string{"set", "-b", "c", "x", "-"}, 0, ""},
		{[]string{"set", "-b", "c", "--", "y", "-5"}, 0, ""},
		{[]string{"get", "-b", "c", "y"}, 0, "-5\n"},
		{[]string{"promote", "c"}, 0, "promoted c: generation 1\n"},
		{[]string{"get", "x"}, 0, "from stdin\n"},
		{[]string{"branches"}, 0, "no branches (generation 1)\n"},
		{[]string{"promote", "c"}, ExitError, ""},
		{[]string{"bogus"}, ExitUsage, ""},
		{[]string{"get"}, ExitUsage, ""},
	}
	for _, s := range steps {
		r := run(t, f, "from stdin", s.args...)
		if r.code != s.code {
			t.Fatalf("%v: exit %d, want %d\n%s%s", s.args, r.code, s.code, r.stdout, r.stderr)
		}
		if s.out != "" && r.stdout != s.out && !strings.HasPrefix(r.stdout, s.out) {
			t.Fatalf("%v: printed %q, want %q", s.args, r.stdout, s.out)
		}
	}
	r := run(t, f, "", "history")
	for _, want := range []string{"fork", "killed", "observe", "promote"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("history has no %s event:\n%s", want, r.stdout)
		}
	}
}

func TestJSON(t *testing.T) {
	t.Setenv("MULTIVERGENCE_NOW", "2026-11-02T08:00:00Z")
	f := filepath.Join(t.TempDir(), "s.mv")
	run(t, f, "", "fork", "a", "-weight", "1")
	run(t, f, "", "fork", "b", "-weight", "3")
	r := run(t, f, "", "-json", "observe", "-l", "a=1", "-rest", "0")
	var out struct {
		OK      bool
		Killed  []map[string]string
		Live    []map[string]any
		Settled *string
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatal(err, r.stdout)
	}
	if !out.OK || len(out.Killed) != 1 || out.Killed[0]["branch"] != "b" || out.Killed[0]["cause"] != "ruled out" {
		t.Fatalf("%s", r.stdout)
	}
	if out.Settled == nil || *out.Settled != "a" {
		t.Fatalf("not settled: %s", r.stdout)
	}
	r = run(t, f, "", "-json", "promote", "zzz")
	var e map[string]string
	json.Unmarshal([]byte(r.stdout), &e)
	if r.code != ExitError || e["kind"] != "not_found" {
		t.Fatalf("%d %s", r.code, r.stdout)
	}
	r = run(t, f, "", "-json", "nonsense")
	json.Unmarshal([]byte(r.stdout), &e)
	if r.code != ExitUsage || e["kind"] != "usage" {
		t.Fatalf("%d %s", r.code, r.stdout)
	}
	// Reading a file that isn't there.
	r = run(t, filepath.Join(t.TempDir(), "none.mv"), "", "-json", "branches")
	json.Unmarshal([]byte(r.stdout), &e)
	if r.code != ExitError || e["kind"] != "no_store" {
		t.Fatalf("%d %s", r.code, r.stdout)
	}
}

// TestCrossLanguage builds the command and runs scripts/cross_language.py
// against it: Python driving the store through the CLI, checking the
// engine's probabilities against its own arithmetic.
func TestCrossLanguage(t *testing.T) {
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "python3"}
	}
	py := ""
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			py = p
			break
		}
	}
	if py == "" {
		t.Skip("no Python")
	}
	bin := filepath.Join(t.TempDir(), "multivergence")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "github.com/multivergence/multivergence/cmd/multivergence")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(py, "-B", filepath.Join("..", "..", "scripts", "cross_language.py"), bin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if dir := os.Getenv("MV_RESULTS"); dir != "" {
		os.WriteFile(filepath.Join(dir, "cross-language.txt"), out, 0o644)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	t.Log(lines[len(lines)-1])
}

func TestReviewFixes(t *testing.T) {
	f := filepath.Join(t.TempDir(), "s.mv")
	t.Setenv("MULTIVERGENCE_NOW", "not a time")
	if r := run(t, f, "", "version"); r.code != ExitUsage {
		t.Fatalf("a bad MULTIVERGENCE_NOW was accepted: %d", r.code)
	}
	t.Setenv("MULTIVERGENCE_NOW", "2026-11-02T08:00:00Z")
	run(t, f, "", "set", "x", "1")
	if r := run(t, f, "", "-json", "get", "missing"); r.code != ExitNotFound || !strings.Contains(r.stdout, `"found":false`) {
		t.Fatalf("json get of a missing key: %d %s", r.code, r.stdout)
	}
	if r := run(t, f, "", "-json", "best"); r.code != ExitNotFound || !strings.Contains(r.stdout, `"best":null`) {
		t.Fatalf("json best with no branches: %d %s", r.code, r.stdout)
	}
	run(t, f, "", "fork", "b")
	for _, w := range []string{"1e309", "NaN", "-1"} {
		r := run(t, f, "", "-json", "weight", "b", "--", w)
		if r.code != ExitError || !strings.Contains(r.stdout, "bad_weight") {
			t.Fatalf("weight %s: %d %s", w, r.code, r.stdout)
		}
	}
	run(t, f, "\xff\x00bin", "set", "raw", "-")
	run(t, f, "a\tb\nc", "set", "tabs", "-")
	r := run(t, f, "", "-json", "get", "raw")
	if !strings.Contains(r.stdout, `"value_base64":"/wBiaW4="`) {
		t.Fatalf("binary value: %s", r.stdout)
	}
	r = run(t, f, "", "keys", "-values")
	if !strings.Contains(r.stdout, "tabs\t\"a\\tb\\nc\"\n") || strings.Count(r.stdout, "\n") != 3 {
		t.Fatalf("keys -values: %q", r.stdout)
	}
	if r := run(t, f, "", "expire", "b", "-at", "3000-01-01T00:00:00Z"); r.code != ExitError {
		t.Fatalf("a deadline in 3000: %d %s%s", r.code, r.stdout, r.stderr)
	}
}
