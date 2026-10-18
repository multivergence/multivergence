// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multivergence/multivergence/internal/pmap"
)

// TestMain lets the test binary act as a child process for the crash and
// contention tests.
func TestMain(m *testing.M) {
	switch os.Getenv("MV_CHILD") {
	case "crash":
		crashChild()
		return
	case "writer":
		writerChild()
		return
	case "reader":
		readerChild()
		return
	}
	os.Exit(m.Run())
}

// ---- Isolation ------------------------------------------------------------

// model is the obvious way to do branches: a full copy of every map. The
// engine must agree with it on every read.
type model struct {
	trunk    map[string]string
	trunkVer int
	branches map[string]*modelBranch
	order    []string
}

type modelBranch struct {
	data map[string]string
	base int
	dead bool
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestIsolationAgainstModel runs random writes, deletes, forks (from the
// trunk and from branches), facts, discards and promotes on the engine and
// on full copies, and compares every key of every branch after every
// change. No constraints or deadlines here, so nothing dies by surprise;
// other tests cover those.
func TestIsolationAgainstModel(t *testing.T) {
	seeds := rounds(8)
	changes, reads := 0, 0
	for seed := int64(0); seed < int64(seeds); seed++ {
		rng := rand.New(rand.NewSource(seed))
		path := filepath.Join(t.TempDir(), "iso.mv")
		s, err := Open(path, &Options{NoSync: true, CompactAfter: int64(512 + rng.Intn(8192))})
		must(t, err)
		m := &model{trunk: map[string]string{}, branches: map[string]*modelBranch{}}
		forks := 0
		live := func() []string {
			var out []string
			for _, n := range m.order {
				if !m.branches[n].dead {
					out = append(out, n)
				}
			}
			return out
		}
		pick := func() string {
			l := live()
			if len(l) == 0 || rng.Intn(5) == 0 {
				return Trunk
			}
			return l[rng.Intn(len(l))]
		}
		for i := 0; i < 1000; i++ {
			k := "k" + strconv.Itoa(rng.Intn(40))
			v := strconv.Itoa(rng.Intn(1e6))
			switch r := rng.Intn(100); {
			case r < 40:
				b := pick()
				must(t, s.Set(b, k, []byte(v)))
				if b == Trunk {
					m.trunk[k] = v
					m.trunkVer++
				} else {
					m.branches[b].data[k] = v
				}
			case r < 50:
				b := pick()
				must(t, s.Delete(b, k))
				if b == Trunk {
					if _, ok := m.trunk[k]; ok {
						delete(m.trunk, k)
						m.trunkVer++
					}
				} else {
					delete(m.branches[b].data, k)
				}
			case r < 70:
				if len(live()) >= 40 {
					continue
				}
				forks++
				name := "b" + strconv.Itoa(forks)
				from := pick()
				must(t, s.Fork(name, from, 1))
				nb := &modelBranch{}
				if from == Trunk {
					nb.data, nb.base = copyMap(m.trunk), m.trunkVer
				} else {
					nb.data, nb.base = copyMap(m.branches[from].data), m.branches[from].base
				}
				m.branches[name] = nb
				m.order = append(m.order, name)
			case r < 80:
				var facts []Fact
				for j := rng.Intn(3); j >= 0; j-- {
					fk := "k" + strconv.Itoa(rng.Intn(40))
					if rng.Intn(4) == 0 {
						facts = append(facts, Fact{Key: fk, Delete: true})
					} else {
						facts = append(facts, Fact{Key: fk, Value: []byte(v)})
					}
				}
				_, err := s.Observe(Evidence{Facts: facts})
				must(t, err)
				apply := func(d map[string]string) {
					for _, f := range facts {
						if f.Delete {
							delete(d, f.Key)
						} else {
							d[f.Key] = string(f.Value)
						}
					}
				}
				apply(m.trunk)
				for _, n := range live() {
					apply(m.branches[n].data)
				}
			case r < 88:
				l := live()
				if len(l) == 0 {
					continue
				}
				n := l[rng.Intn(len(l))]
				must(t, s.Discard(n))
				m.branches[n].dead = true
			default:
				l := live()
				if len(l) == 0 {
					continue
				}
				n := l[rng.Intn(len(l))]
				err := s.Promote(n)
				if m.branches[n].base != m.trunkVer {
					if !errors.Is(err, ErrStale) {
						t.Fatalf("seed %d: promote of a stale branch: %v", seed, err)
					}
					continue
				}
				must(t, err)
				m.trunk = m.branches[n].data
				m.trunkVer++
				m.branches = map[string]*modelBranch{}
				m.order = nil
			}
			changes++
			reads += compareModel(t, s, m)
		}
		// The file must give back the same thing after a reopen.
		want := s.Digest()
		must(t, s.Close())
		s, err = Open(path, nil)
		must(t, err)
		if s.Digest() != want {
			t.Fatalf("seed %d: reopened store differs", seed)
		}
		reads += compareModel(t, s, m)
		must(t, s.Close())
	}
	t.Logf("%d seeds, %d changes, %d reads compared, all equal", seeds, changes, reads)
	record(t, "isolation", map[string]any{
		"seeds": seeds, "changes": changes, "reads_compared": reads, "mismatches": 0,
		"what": "Every key of the trunk and of every branch, live or dead, compared with a model that copies every map in full, after every change and again after reopening the file.",
	})
}

// compareModel checks every key of every branch and returns how many reads
// it made.
func compareModel(t *testing.T, s *Store, m *model) int {
	t.Helper()
	reads := 0
	check := func(branch string, want map[string]string) {
		got := map[string]string{}
		must(t, s.Scan(branch, "", func(k string, v []byte) bool { got[k] = string(v); return true }))
		if len(got) != len(want) {
			t.Fatalf("%s: %d keys, model has %d", branch, len(got), len(want))
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: %s = %q, model has %q", branch, k, got[k], v)
			}
			reads++
		}
	}
	check(Trunk, m.trunk)
	for _, n := range m.order {
		check(n, m.branches[n].data)
		info, err := s.Branch(n)
		must(t, err)
		if info.Live == m.branches[n].dead {
			t.Fatalf("%s: live %v, model dead %v", n, info.Live, m.branches[n].dead)
		}
	}
	if len(s.Branches()) != len(m.order) {
		t.Fatalf("%d branches, model has %d", len(s.Branches()), len(m.order))
	}
	return reads
}

// ---- Crashes --------------------------------------------------------------

const crashChanges = 400

// crashChild runs a script against a file and prints "ack N" after change N
// has returned, until the parent kills it.
func crashChild() {
	seed, _ := strconv.ParseInt(os.Getenv("MV_SEED"), 10, 64)
	sc := newScript(seed)
	sc.big, sc.promotes = true, true
	o := sc.opts()
	o.NoSync = os.Getenv("MV_SYNC") == ""
	o.CompactAfter = 2048
	s, err := Open(os.Getenv("MV_FILE"), o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	w := bufio.NewWriter(os.Stdout)
	for i := 1; i <= crashChanges; i++ {
		if _, err := sc.step(s); err != nil && !expected(err) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintf(w, "ack %d\n", i)
		w.Flush()
	}
	s.Close()
}

// TestCrashMidChange kills a writing process with SIGKILL at random moments,
// many of them in the middle of a promote or a compaction, then reopens the
// file. The state must be exactly the state after the last change the
// process confirmed, or after the one it was making when it died: never a
// mix, never part of a promote. Then the store must go on working.
func TestCrashMidChange(t *testing.T) {
	if testing.Short() {
		t.Skip("starts many processes")
	}
	exe, err := os.Executable()
	must(t, err)
	n := rounds(100)
	stats := map[string]int{}
	dir := t.TempDir()
	for round := 0; round < n; round++ {
		seed := int64(1000 + round)
		path := filepath.Join(dir, fmt.Sprintf("crash-%d.mv", round))
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "MV_CHILD=crash", "MV_SEED="+strconv.FormatInt(seed, 10), "MV_FILE="+path)
		if round%4 == 0 {
			cmd.Env = append(cmd.Env, "MV_SYNC=1") // a quarter of the rounds sync every change
			stats["rounds with fsync on every change"]++
		}
		stdout, err := cmd.StdoutPipe()
		must(t, err)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		must(t, cmd.Start())
		rng := rand.New(rand.NewSource(seed))
		target := 1 + rng.Intn(crashChanges-20)
		sc := bufio.NewScanner(stdout)
		last := 0
		for last < target && sc.Scan() {
			fmt.Sscanf(sc.Text(), "ack %d", &last)
		}
		time.Sleep(time.Duration(rng.Intn(400)) * time.Microsecond)
		cmd.Process.Kill()
		for sc.Scan() {
			fmt.Sscanf(sc.Text(), "ack %d", &last)
		}
		werr := cmd.Wait()
		if strings.TrimSpace(stderr.String()) != "" {
			t.Fatalf("round %d: child failed: %s", round, stderr.String())
		}
		killed := werr != nil
		if _, err := os.Stat(path + ".compact"); err == nil {
			stats["killed mid-compaction, leaving the unfinished file"]++
		}

		want := digests(t, seed, min(last+1, crashChanges))
		s, err := Open(path, nil)
		if err != nil {
			t.Fatalf("round %d: reopen after kill at change %d: %v", round, last, err)
		}
		got := s.Digest()
		// The change in flight matters only if it would change the state;
		// a refused one (a stale promote, a dead branch) writes nothing.
		real := last < crashChanges && want[last] != want[last+1]
		switch {
		case got == want[last]:
			stats["reopened at the last confirmed change"]++
		case real && got == want[last+1]:
			stats["reopened with the change in flight completed"]++
		default:
			t.Fatalf("round %d: after change %d the file holds neither state %d nor %d", round, last, last, last+1)
		}
		switch {
		case !killed || last >= crashChanges:
			stats["child finished before the kill"]++
		case !real:
			stats["killed with a refused change in flight"]++
		default:
			sc2 := newScript(seed)
			sc2.big, sc2.promotes = true, true
			mem := New(sc2.opts())
			for i := 0; i < last; i++ {
				sc2.step(mem)
			}
			kind, _ := sc2.step(mem)
			stats["killed with a "+kind+" in flight"]++
			if kind == "promote" {
				if got == want[last+1] {
					stats["promote in flight: completed"]++
				} else {
					stats["promote in flight: not started"]++
				}
			}
		}
		// The store keeps working after the crash.
		must(t, s.Set(Trunk, "after-crash", []byte("ok")))
		must(t, s.Fork("recovered", Trunk, 1))
		must(t, s.Promote("recovered"))
		must(t, s.Close())
	}
	t.Logf("%d kills: %v", n, stats)
	record(t, "crash", map[string]any{
		"rounds": n, "outcomes": stats, "wrong_states": 0,
		"changes_per_run": crashChanges, "compact_after_bytes": 2048,
		"what": "A writer process killed with SIGKILL at a random point of a random script (15% of changes are promote attempts, many of them refused as stale or dead; compaction every 2 KB; fsync on in a quarter of the rounds), then the file reopened: the state equals the state after the last confirmed change or after the change in flight, and the store goes on working. Changes in flight are counted by kind only when they would change the state.",
	})
}

// ---- Determinism ----------------------------------------------------------

// TestDeterministicReplay runs the same script into a store in memory and
// one in a file, compacting at random sizes, then reopens the file: all of
// them must hold bit-for-bit the same weights and data.
func TestDeterministicReplay(t *testing.T) {
	seeds := rounds(20)
	total, compacted := 0, 0
	for seed := int64(0); seed < int64(seeds); seed++ {
		sc := newScript(seed)
		a := New(sc.opts())
		counts := sc.run(t, a, 1500)
		total += 1500

		sc = newScript(seed)
		path := filepath.Join(t.TempDir(), "r.mv")
		o := sc.opts()
		o.NoSync = true
		o.CompactAfter = int64(1024 + (seed%20)*997)
		b, err := Open(path, o)
		must(t, err)
		sc.run(t, b, 1500)
		if a.Digest() != b.Digest() {
			t.Fatalf("seed %d: memory and file differ", seed)
		}
		must(t, b.Close())
		c, err := Open(path, nil)
		must(t, err)
		if c.Digest() != a.Digest() {
			t.Fatalf("seed %d: replayed file differs", seed)
		}
		ha, hc := a.History(), c.History()
		if len(ha) != len(hc) {
			t.Fatalf("seed %d: history has %d events, replayed %d", seed, len(ha), len(hc))
		}
		for k := range ha {
			if ha[k] != hc[k] {
				t.Fatalf("seed %d: event %d differs after replay: %+v %+v", seed, k, ha[k], hc[k])
			}
		}
		if b.log != nil && b.log.snapBytes > 0 {
			compacted++
		}
		r, err := OpenReadOnly(path, nil)
		must(t, err)
		if r.Digest() != a.Digest() {
			t.Fatalf("seed %d: reader differs", seed)
		}
		must(t, c.Close())
		if seed == 0 {
			t.Logf("seed 0: %s", joinCounts(counts))
		}
	}
	record(t, "determinism", map[string]any{
		"seeds": seeds, "changes": total, "differences": 0, "runs_that_compacted": compacted,
		"what": "The same random script run in memory, into a file with compaction at varying sizes, reopened, and read without the lock: the same digest every time, which covers every key, every weight to the last bit, every constraint, deadline and fate; and the same history, event for event.",
	})
}

// ---- Deadlines and constraints over a long schedule -----------------------

// TestLongSchedule runs 30 simulated days of forks, deadlines from a minute
// to three days, numeric constraints and facts, with a model of when each
// branch must die. A branch must die of its deadline at the first change
// made at or after it, never before, and of a constraint at the first
// change that breaks it.
func TestLongSchedule(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	clock := start
	s := New(&Options{Now: func() time.Time { return clock }})
	rng := rand.New(rand.NewSource(7))
	type expect struct {
		deadline   time.Time
		lo, hi     float64
		dead       bool
		cause      Cause
		died       time.Time
		forkedAt   time.Time
		generation uint64
	}
	exp := map[string]*expect{}
	temp := 20.0
	must(t, s.Set(Trunk, "temp", []byte(fmtNum(temp))))
	end := start.Add(30 * 24 * time.Hour)
	forks, expired, violated, promotes, steps, checks := 0, 0, 0, 0, 0, 0
	maxLive := 0
	// apply the model's view of a change at time now.
	sweepModel := func(now time.Time) {
		for _, e := range exp {
			if !e.dead && e.generation == s.Generation() && !e.deadline.After(now) {
				e.dead, e.cause, e.died = true, CauseExpired, now
			}
		}
	}
	checkModel := func(now time.Time) {
		for _, b := range s.Branches() {
			e := exp[b.Name]
			checks++
			if b.Live == e.dead {
				t.Fatalf("%s at %v: live %v, model dead %v (%v)", b.Name, now, b.Live, e.dead, e.cause)
			}
			if !b.Live && b.Cause == CauseExpired && !b.Died.Equal(e.died) && !b.Died.Equal(e.deadline) {
				t.Fatalf("%s expired at %v, model says %v", b.Name, b.Died, e.died)
			}
			if !b.Live && b.Cause != e.cause {
				t.Fatalf("%s died of %v, model says %v", b.Name, b.Cause, e.cause)
			}
			if b.Live && !b.Deadline.After(now) {
				t.Fatalf("%s is live past its deadline", b.Name)
			}
		}
	}
	for clock.Before(end) {
		clock = clock.Add(time.Duration(rng.Intn(600)) * time.Second)
		steps++
		// Reads between changes already see expired branches as dead.
		checkModelRead := func() {
			for _, b := range s.Branches() {
				if e := exp[b.Name]; !e.dead && !e.deadline.After(clock) && b.Live {
					t.Fatalf("%s reads as live past its deadline", b.Name)
				}
			}
		}
		checkModelRead()
		switch r := rng.Intn(100); {
		case r < 35 && len(s.Live()) < 400:
			forks++
			name := "b" + strconv.Itoa(forks)
			sweepModel(clock)
			must(t, s.Fork(name, Trunk, 1))
			d := clock.Add(time.Minute + time.Duration(rng.Int63n(int64(72*time.Hour))))
			sweepModel(clock)
			must(t, s.Expire(name, d))
			e := &expect{deadline: d, lo: temp - 3 - rng.Float64()*20, hi: temp + 3 + rng.Float64()*20, forkedAt: clock, generation: s.Generation()}
			exp[name] = e
			sweepModel(clock)
			must(t, s.Constrain(name, AtLeast("temp", e.lo), AtMost("temp", e.hi)))
		case r < 80:
			temp += rng.NormFloat64() * 0.5
			sweepModel(clock)
			for _, e := range exp {
				if !e.dead && e.generation == s.Generation() && (temp < e.lo || temp > e.hi) {
					e.dead, e.cause, e.died = true, CauseViolated, clock
				}
			}
			_, err := s.Observe(Evidence{Facts: []Fact{{Key: "temp", Value: []byte(fmtNum(temp))}}})
			must(t, err)
		case r < 81 && rng.Intn(8) == 0:
			if b, ok := s.Best(); ok {
				sweepModel(clock)
				must(t, s.Promote(b.Name))
				promotes++
				// Generation moved on; the old branches are gone.
			}
		default:
			sweepModel(clock)
			_, err := s.Sweep()
			must(t, err)
		}
		checkModel(clock)
		if l := len(s.Live()); l > maxLive {
			maxLive = l
		}
	}
	for _, e := range exp {
		if e.dead && e.cause == CauseExpired {
			expired++
			if e.died.Before(e.deadline) {
				t.Fatal("a branch expired before its deadline")
			}
		}
		if e.dead && e.cause == CauseViolated {
			violated++
		}
	}
	t.Logf("%d steps over 30 days: %d forks, %d expired, %d violated, %d promotes, %d checks, up to %d live", steps, forks, expired, violated, promotes, checks, maxLive)
	record(t, "schedule", map[string]any{
		"simulated_days": 30, "changes": steps, "forks": forks, "expired": expired, "violated": violated,
		"promotes": promotes, "branch_checks": checks, "max_live": maxLive, "early_deaths": 0, "late_deaths": 0,
		"what": "30 simulated days of forks with deadlines from 1 minute to 3 days and numeric constraints on a value that drifts with each fact. Every branch is checked after every change against a model: deadlines fire at the first change at or after them and reads see them at once; constraints fire at the first fact that breaks them.",
	})
}

// ---- Cost -----------------------------------------------------------------

func heapNow() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestCost measures what branches cost: memory, distinct map nodes, fork
// time and file size, for 10, 100 and 1,000 branches over a trunk of
// 10,000 keys, each branch changing 3 keys, against full copies.
func TestCost(t *testing.T) {
	const keys, valLen, changes = 10000, 100, 3
	val := strings.Repeat("v", valLen)
	var results []map[string]any
	for _, n := range []int{10, 100, 1000} {
		s := New(nil)
		for i := 0; i < keys; i++ {
			must(t, s.Set(Trunk, fmt.Sprintf("row/%05d", i), []byte(val)))
		}
		trunkNodes := pmap.Count(s.st.trunk)
		before := heapNow()
		forkStart := time.Now()
		for b := 0; b < n; b++ {
			must(t, s.Fork("b"+strconv.Itoa(b), Trunk, 1))
		}
		forkTime := time.Since(forkStart)
		writeStart := time.Now()
		for b := 0; b < n; b++ {
			for c := 0; c < changes; c++ {
				must(t, s.Set("b"+strconv.Itoa(b), fmt.Sprintf("row/%05d", (b*7919+c*104729)%keys), []byte("changed")))
			}
		}
		writeTime := time.Since(writeStart)
		after := heapNow()
		maps := []pmap.Map{s.st.trunk}
		for _, name := range s.st.order {
			maps = append(maps, s.st.branches[name].data)
		}
		nodes := pmap.Count(maps...)

		// One full copy of the trunk, measured; n of them cost n times it.
		cb := heapNow()
		full := map[string][]byte{}
		s.Scan(Trunk, "", func(k string, v []byte) bool {
			full[strings.Clone(k)] = append([]byte(nil), v...)
			return true
		})
		oneCopy := heapNow() - cb
		runtime.KeepAlive(full)

		// The file, compacted: shared nodes are written once.
		path := filepath.Join(t.TempDir(), "cost.mv")
		f, err := Open(path, &Options{NoSync: true, CompactAfter: 1 << 40})
		must(t, err)
		f.st = s.st
		must(t, f.log.compact(encodeSnapshot(f.st, nil, 0)))
		fileSize := f.log.snapBytes
		must(t, f.Close())
		re, err := Open(path, nil)
		must(t, err)
		if re.Digest() != s.Digest() {
			t.Fatal("compacted cost file differs")
		}
		must(t, re.Close())
		trunkOnly := len(encodeSnapshot(&state{trunk: s.st.trunk, branches: map[string]*branch{}}, nil, 0))

		perBranch := float64(after-before) / float64(n)
		r := map[string]any{
			"branches": n, "trunk_keys": keys, "value_bytes": valLen, "keys_changed_per_branch": changes,
			"heap_added_bytes": after - before, "heap_per_branch_bytes": int(perBranch),
			"one_full_copy_bytes": oneCopy, "full_copies_bytes": oneCopy * uint64(n),
			"ratio_full_copies_to_branches": float64(oneCopy*uint64(n)) / float64(after-before),
			"distinct_nodes":                nodes, "trunk_nodes": trunkNodes, "nodes_if_copied": trunkNodes * (n + 1),
			"fork_ns_per_branch":  forkTime.Nanoseconds() / int64(n),
			"write_ns_per_change": writeTime.Nanoseconds() / int64(n*changes),
			"snapshot_file_bytes": fileSize, "trunk_snapshot_bytes": trunkOnly,
			"snapshot_if_copied_bytes": trunkOnly * (n + 1),
		}
		t.Logf("%4d branches: %.1f KB per branch against %.1f MB per full copy; %d nodes against %d copied; file %.1f MB against %.1f MB copied",
			n, perBranch/1024, float64(oneCopy)/1e6, nodes, trunkNodes*(n+1), float64(fileSize)/1e6, float64(trunkOnly*(n+1))/1e6)
		results = append(results, r)
	}
	record(t, "cost", map[string]any{
		"runs": results,
		"what": "A trunk of 10,000 keys with 100-byte values; 10, 100 and 1,000 branches forked from it, each changing 3 keys. Heap is measured with runtime.MemStats after GC; a full copy is one measured deep copy of the trunk, times the number of branches. The file is one compacted snapshot.",
	})
}

// ---- Many processes -------------------------------------------------------

// writerChild opens the store, adds one to a counter on the trunk, closes
// it, and does that MV_LOOPS times. Every tenth time it forks, writes and
// promotes too, so promotes and compactions happen while others wait.
func writerChild() {
	path, id := os.Getenv("MV_FILE"), os.Getenv("MV_ID")
	loops, _ := strconv.Atoi(os.Getenv("MV_LOOPS"))
	for i := 0; i < loops; i++ {
		s, err := Open(path, &Options{LockTimeout: 2 * time.Minute, CompactAfter: 4096})
		if err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			os.Exit(2)
		}
		v, _, err := s.Get(Trunk, "counter")
		n, _ := strconv.Atoi(string(v))
		if err == nil {
			err = s.Set(Trunk, "counter", []byte(strconv.Itoa(n+1)))
		}
		if err == nil && i%10 == 0 {
			name := fmt.Sprintf("w%s-%d", id, i)
			if err = s.Fork(name, Trunk, 1); err == nil {
				if err = s.Set(name, "last-writer", []byte(id)); err == nil {
					err = s.Promote(name)
				}
			}
		}
		if err == nil {
			err = s.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
}

// readerChild reads the store without the lock until MV_DONE exists, and
// checks the counter never goes backwards.
func readerChild() {
	path, done := os.Getenv("MV_FILE"), os.Getenv("MV_DONE")
	last, reads := -1, 0
	for {
		if _, err := os.Stat(done); err == nil {
			break
		}
		s, err := OpenReadOnly(path, nil)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(2)
		}
		v, _, _ := s.Get(Trunk, "counter")
		n, _ := strconv.Atoi(string(v))
		if n < last {
			fmt.Fprintf(os.Stderr, "counter went back from %d to %d\n", last, n)
			os.Exit(2)
		}
		last = n
		reads++
	}
	fmt.Println(reads)
}

// TestManyProcesses has several processes add to one counter through the
// file lock while others read without it. No update may be lost, no reader
// may see the counter go back, and no reader may see a damaged file, with
// promotes and compactions renaming the file under them.
func TestManyProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("starts many processes")
	}
	exe, err := os.Executable()
	must(t, err)
	dir := t.TempDir()
	path := filepath.Join(dir, "shared.mv")
	done := filepath.Join(dir, "done")
	writers, loops, readers := 8, rounds(50), 3
	start := time.Now()
	var procs []*exec.Cmd
	var readOuts []*strings.Builder
	for r := 0; r < readers; r++ {
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "MV_CHILD=reader", "MV_FILE="+path, "MV_DONE="+done)
		out := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = out, out
		must(t, cmd.Start())
		procs = append(procs, cmd)
		readOuts = append(readOuts, out)
	}
	var ws []*exec.Cmd
	var wouts []*strings.Builder
	for w := 0; w < writers; w++ {
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), "MV_CHILD=writer", "MV_FILE="+path, "MV_ID="+strconv.Itoa(w), "MV_LOOPS="+strconv.Itoa(loops))
		out := &strings.Builder{}
		cmd.Stdout, cmd.Stderr = out, out
		must(t, cmd.Start())
		ws = append(ws, cmd)
		wouts = append(wouts, out)
	}
	for i, cmd := range ws {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("writer %d: %v: %s", i, err, wouts[i].String())
		}
	}
	elapsed := time.Since(start)
	must(t, os.WriteFile(done, nil, 0o644))
	totalReads := 0
	for i, cmd := range procs {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("reader %d: %v: %s", i, err, readOuts[i].String())
		}
		n, _ := strconv.Atoi(strings.TrimSpace(readOuts[i].String()))
		totalReads += n
	}
	s, err := Open(path, nil)
	must(t, err)
	v, _, _ := s.Get(Trunk, "counter")
	gen := s.Generation()
	must(t, s.Close())
	if string(v) != strconv.Itoa(writers*loops) {
		t.Fatalf("counter is %s, want %d", v, writers*loops)
	}
	t.Logf("%d writers x %d updates = %s, %d promotes, %d reads by %d readers, %v", writers, loops, v, gen, totalReads, readers, elapsed.Round(time.Millisecond))
	record(t, "processes", map[string]any{
		"writers": writers, "updates_per_writer": loops, "final_counter": writers * loops, "lost_updates": 0,
		"promotes": gen, "readers": readers, "reads": totalReads, "reader_errors": 0, "seconds": elapsed.Seconds(),
		"what": "Writer processes each open the file, read a counter, write it plus one, close, over and over; every tenth time they also fork, write and promote. Compaction is set to every 4 KB, so the file is renamed often. Reader processes read without the lock the whole time.",
	})
}
