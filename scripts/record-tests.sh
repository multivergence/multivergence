#!/bin/sh
# Copyright Multivergence.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs every test, larger than CI runs them (MV_SCALE), the race detector,
# the release plan's cases, the agent demo, the speculation example and the
# smoke check on static release-style binaries, and writes the output to
# test/results/ with a note of the machine they ran on. The tests that
# measure something also write a JSON file of their figures there. The
# numbers in the README and on multivergence.com come from these files.
#
#   sh scripts/record-tests.sh
#
# Needs Go and Python 3.
set -eu
cd "$(dirname "$0")/.."
out=test/results
mkdir -p "$out"
rm -f "$out"/*.json "$out"/*.txt
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
scale=${MV_SCALE:-10}

# Whether the checkout was clean before anything was recorded.
state="at $(git rev-parse --short HEAD 2>/dev/null || echo 'no commit')$(test -z "$(git status --porcelain 2>/dev/null)" || echo ' plus uncommitted changes')"

header() {
	echo "# $1"
	echo "# recorded $(date -u '+%Y-%m-%d %H:%M UTC') $state, Multivergence $(sed -n 's/^const Version = "\(.*\)"$/\1/p' multivergence.go), $(go version | cut -d' ' -f3-)"
	cpu=$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //' || true)
	test -n "$cpu" || cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo 'unknown CPU')
	echo "# $(uname -sr), $cpu, $(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo '?') CPUs"
	echo "# command: $2"
	echo
}

go vet ./...

cmd="MV_SCALE=$scale go test -count=1 -v -timeout 60m ./..."
{ header "Every test, verbose, at $scale times the CI size; the promise tests also write the JSON files here" "$cmd"
  MV_SCALE=$scale MV_RESULTS="$(pwd)/$out" go test -count=1 -v -timeout 60m ./... 2>&1; } > "$out/tests.txt"

cmd="go test -race -count=1 ./..."
{ header "Every test with the race detector, at CI size" "$cmd"; $cmd 2>&1; } > "$out/race.txt"

cmd="python3 -B -m unittest discover -s scripts -p *_test.py -v"
{ header "The release plan's cases" "$cmd"; $cmd 2>&1; } > "$out/release-plan.txt"

cmd="go run ./examples/agent"
{ header "The agent demo: three plans, evidence, one survivor" "$cmd"; go run ./examples/agent 2>&1; } > "$out/demo.txt"

cmd="go run ./examples/speculate"
{ header "Speculative execution: prepare two guesses in branches, promote the right one" "$cmd"; go run ./examples/speculate 2>&1; } > "$out/speculate.txt"

mkdir "$work/bin"
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/bin/multivergence" ./cmd/multivergence
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/bin/multivergence-demo" ./examples/agent
cmd="python3 scripts/smoke.py BIN"
{ header "The release smoke check on static binaries built as the release workflow builds them" "$cmd"
  ls -l "$work/bin" | sed 1d | awk '{print "# " $5 " bytes  " $9}'
  echo
  python3 scripts/smoke.py "$work/bin" 2>&1; } > "$out/smoke.txt"

grep -E '^(ok|FAIL|--- FAIL)' "$out/tests.txt" "$out/race.txt" || true
tail -n 1 "$out/release-plan.txt" "$out/smoke.txt" "$out/demo.txt" 2>/dev/null || true
echo "results written to $out/"
