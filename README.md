# Multivergence

Forkable state for agents that think in alternatives.

Multivergence is a small embeddable state engine. You fork the current state
into several possible futures, cheaply. Each fork carries how likely it is,
what must stay true for it to hold, and when it stops mattering. As evidence
arrives, the unlikely and the impossible drop away, and when one future wins,
you promote it: its state becomes the real one, in a single step.

It's a Go library and a command, `multivergence`, that works on the same store
file from any language. Apache 2.0. Website: [multivergence.com](https://multivergence.com).

## Why

Agents try several approaches before they commit: three plans for a
refactor, two booking options, a few sequences of tool calls. Today they fake
it with git worktrees, container snapshots, or alternatives stuffed into the
prompt. Those are heavy copies. None of them knows which branch is more
likely, when a branch should die, or what evidence would settle it.

Multivergence makes that a state primitive. A branch is a copy-on-write fork,
so a hundred branches cost what they changed, not a hundred copies. And each
one has three things attached:

- **a weight**: how likely this future is, next to the others;
- **constraints**: conditions on its keys, like `fare/flight <= 400`, that must
  hold for it to stay valid;
- **a deadline**: when it stops mattering, like a seat hold running out.

Evidence comes in two kinds. **Facts** are things now known about the world;
they're written to every live branch, and any branch whose constraints break
dies. **Likelihoods** say how likely the evidence is under each branch; the
engine multiplies and renormalises (Bayes' rule). The engine never works out a
probability on its own. Where likelihoods come from (a model, a tool's answer,
a rule of thumb) is up to you. The engine applies them the same way every
time, so the same calls always give the same weights, to the last bit.

## In a minute

```sh
export MULTIVERGENCE_FILE=trip.mv
multivergence set budget 400

multivergence fork plan-train  -weight 0.5
multivergence fork plan-flight -weight 0.3
multivergence fork plan-bus    -weight 0.2
multivergence set -b plan-flight fare/flight 250       # the plan's own estimate
multivergence constrain plan-flight 'fare/flight <= 400'
multivergence expire plan-bus -in 15m                  # the coach seat hold

# A fare search comes back. Facts go to every live plan.
multivergence observe -fact fare/flight=460
#  killed plan-flight: violated (fare/flight <= 400)
#  BRANCH      PROB   STATUS  ...
#  plan-train  71.4%  live
#  plan-bus    28.6%  live

# A disruption feed: coaches running late. How likely is that news under each plan?
multivergence observe -l plan-train=0.9 -l plan-bus=0.3
#  plan-train  88.2%  live
#  plan-bus    11.8%  live

# The agent goes with the train. Promote makes it the trunk and drops the rest.
multivergence promote plan-train
#  promoted plan-train: generation 1
```

Every command takes `-json` for machine-readable output, so an agent in Python
or TypeScript can drive it with `subprocess`. Exit code 4 means "the change was
made and the branch died of it".

The same thing from Go:

```go
s, err := multivergence.Open("trip.mv", nil)
if err != nil { ... }
defer s.Close()

s.Fork("plan-train", multivergence.Trunk, 0.5)
s.Fork("plan-flight", multivergence.Trunk, 0.3)
s.Fork("plan-bus", multivergence.Trunk, 0.2)
s.ExpireIn("plan-bus", 15*time.Minute)
s.Set("plan-flight", "fare/flight", []byte("250"))
s.Constrain("plan-flight", multivergence.AtMost("fare/flight", 400))

out, err := s.Observe(multivergence.Evidence{
    Facts:       []multivergence.Fact{{Key: "fare/flight", Value: []byte("460")}},
    Likelihoods: map[string]float64{"plan-train": 0.9, "plan-bus": 0.3},
    Rest:        multivergence.Likelihood(1), // for any plan not listed
})
for _, d := range out.Killed {
    fmt.Println(d.Branch, "died:", d.Cause, d.Detail)
}
if name, ok := out.Settled(); ok {
    s.Promote(name)
}
```

### The demo

`multivergence-demo` (in the release archives, or `go run ./examples/agent`)
runs the whole trip with a simulated clock: three plans, a fare search, a
disruption feed, a seat hold that runs out, and a promote. It prints each
step's probabilities and checks every number. `go run ./examples/speculate`
shows speculative execution: start the likely next step in a branch before you
know it's needed, and throw it away cheaply if you were wrong.

## Install

Ready-built binaries for Linux (static, x86-64 and ARM64), macOS and Windows:

| System | Download |
|---|---|
| Linux, x86-64 | [multivergence_linux_amd64.tar.gz](https://github.com/multivergence/multivergence/releases/latest/download/multivergence_linux_amd64.tar.gz) |
| Linux, ARM64 | [multivergence_linux_arm64.tar.gz](https://github.com/multivergence/multivergence/releases/latest/download/multivergence_linux_arm64.tar.gz) |
| macOS, Apple silicon | [multivergence_darwin_arm64.tar.gz](https://github.com/multivergence/multivergence/releases/latest/download/multivergence_darwin_arm64.tar.gz) |
| macOS, Intel | [multivergence_darwin_amd64.tar.gz](https://github.com/multivergence/multivergence/releases/latest/download/multivergence_darwin_amd64.tar.gz) |
| Windows, x86-64 | [multivergence_windows_amd64.zip](https://github.com/multivergence/multivergence/releases/latest/download/multivergence_windows_amd64.zip) |

Checksums are in [SHA256SUMS](https://github.com/multivergence/multivergence/releases/latest/download/SHA256SUMS).
The macOS binaries aren't signed by Apple; allow them in System Settings, or run
`xattr -d com.apple.quarantine multivergence multivergence-demo`.

From Go (1.24 or later), with no dependencies outside the standard library:

```sh
go get github.com/multivergence/multivergence@latest
go install github.com/multivergence/multivergence/cmd/multivergence@latest
```

## How it works

**Trunk and branches.** The trunk is the state that is real now. A branch is
forked from the trunk, or from another branch, and starts with its data. Writes
to a branch stay in it. The data structure under both is a persistent
copy-on-write map (a treap with hashed priorities, so the same keys always
give the same tree), and a fork shares every node it doesn't change. A branch
of a 10,000-key trunk that changes three keys costs about 4 KB.

**Observe.** One call applies one piece of evidence, in a fixed order: branches
past their deadline die; the facts are written to the trunk and every live
branch; branches whose constraints broke die; each live branch's weight is
multiplied by its likelihood (zero rules it out); the survivors are
renormalised to sum to 1; and, if you asked for it, branches below a threshold
are pruned, never the most likely one. [docs/UPDATES.md](docs/UPDATES.md) has
the exact rules and a worked example.

**Promote.** Makes a live branch's state the trunk, drops every branch, and
starts the next generation. It refuses a branch whose deadline has passed, and
a stale branch: one forked before a direct write to the trunk, which promoting
would throw away. Write shared inputs to the trunk before you fork.

**The file.** An append-only log: every change is one checksummed record,
written and synced before the call returns. Promote is one record, so after a
crash the file holds the state from before it or after it, never part of one.
A crash can only cut short the last record, and opening the file drops it. When
the log grows, it is compacted into one snapshot that keeps the sharing between
branches, written to a new file and renamed into place. One process writes at a
time, through a lock file; readers need no lock.
[docs/FORMAT.md](docs/FORMAT.md) has the details.

## What it is not

- **Not a database with branches.** Dolt and Neon branch a whole database into
  named copies, and Dolt can merge them back. Multivergence branches carry a
  weight, a set of conditions and a deadline, and the engine prunes them as
  evidence arrives.
  You could keep a Multivergence store next to either.
- **Not a search algorithm.** Monte Carlo tree search decides which branches to
  explore. Multivergence holds the state of the branches you chose to explore,
  and keeps their probabilities straight. A search could run on top of it.
- **Not an inference engine.** It never decides how likely something is. You
  supply likelihoods; it applies them consistently.

## Other uses

Anything that holds several possible futures at once until something settles
them:

- **Speculative execution.** Prepare the likely next step before it's asked
  for; promote it if the guess was right, discard it if not.
- **Logistics.** Several delivery or routing scenarios held at once until
  traffic or a dispatch decision settles them.
- **Tentative reservations.** Inventory held for competing orders, each hold a
  branch with a deadline.
- **Simulation and game search.** Positions as branches, evaluations as
  likelihoods.
- **Optimistic interfaces.** More than one pending outcome on screen until the
  server answers.

## How it was tested

Every number here comes from [test/results](test/results), which
`sh scripts/record-tests.sh` writes. These were recorded on Linux x86-64 with 2
CPUs.

- **Isolation.** Random writes, deletes, forks, facts, discards and promotes on
  the engine and on a model that copies every map in full: 66,618 changes over
  80 runs, and after every change every key of every branch compared, 86.8
  million reads, with no difference.
- **Crashes.** A writer process killed with SIGKILL at a random moment, 1,000
  times, with compaction set to every 2 KB and fsync on in a quarter of the
  runs. 106 kills landed in the middle of a promote that would change the state
  (in 33 it had reached the file, in 73 it hadn't), and 9 in the middle of a
  compaction. Every time, the reopened file held exactly the state after the
  last confirmed change or after the one in flight, and the store went on
  working.
- **Replay.** 300,000 random changes over 200 runs, in memory, in a file
  compacted at varying sizes (every run compacted), reopened, and read without
  the lock: the same digest and the same history every time, weights included
  to the last bit.
- **Deadlines and constraints.** 30 simulated days, 2,990 forks with deadlines
  from a minute to three days and constraints on a drifting value, checked
  against a model after every change: 1,152 expired and 609 broke a constraint,
  each exactly when it should, none early, none late.
- **Many processes.** 8 writer processes adding to one counter 500 times each,
  with a promote every tenth time, while 3 readers read without the lock: the
  counter ended at 4,000, and 41,135 reads saw no damage and no step back.
- **Cost.** On a trunk of 10,000 keys with 100-byte values, a branch that
  changes 3 keys added about 3.9 KB of memory against 2.1 MB for a full copy.
  1,000 such branches share 52,886 map nodes, where full copies would hold 10
  million, and compact to a 5.9 MB file against 1.1 GB written in full.
- **From Python.** `scripts/cross_language.py` drives the command through the
  whole cycle and checks every probability against its own arithmetic: 26
  checks.

CI runs all of this, at a smaller size, on Linux, macOS and Windows on every
push, with the race detector on Linux and a second Linux run on Go 1.24, the
oldest version go.mod allows. Before a release goes out, the binaries for Linux
(x86-64 and ARM64), macOS on Apple silicon and Windows each run the smoke check
on their own kind of machine; the macOS Intel binaries are checked for their
architecture.

## Limits

This is version 0.1.

- **No merging.** Promote takes one branch and drops the rest. Merging states
  that diverged under different assumptions is a hard problem, left for later.
- **No side effects.** A branch is data. If a branch takes a real action, such
  as paying for a ticket, nothing undoes it when the branch dies. Keep real
  actions until after the promote, or track them yourself.
- **One machine.** One process writes a file at a time; there's no replication.
- **The trunk can't move under open branches.** Writing the trunk directly makes
  earlier branches stale, and Promote refuses them. Facts are the way to tell
  every branch something new.
- **Changes slow down with many branches.** Each change copies the table of
  branches, so a write takes about 4 µs with 10 live branches and about 70 µs
  with 1,000. Fine for agents, whose tool calls take milliseconds or more;
  not built for millions of branches.
- **Compaction writes the whole store.** It happens when the log reaches twice
  the last snapshot, so its cost is spread out, but a very large store pays it
  in one go.

## Documentation

- [docs/REFERENCE.md](docs/REFERENCE.md): the Go API and every command
- [docs/UPDATES.md](docs/UPDATES.md): the exact rules for weights, constraints
  and deadlines
- [docs/FORMAT.md](docs/FORMAT.md): the file, crashes, compaction and locking
- [RELEASING.md](RELEASING.md): how releases go out

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
