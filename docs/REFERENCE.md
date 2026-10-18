# Reference

The Go package is `github.com/multivergence/multivergence`. The command is
`multivergence`. [UPDATES.md](UPDATES.md) has the exact rules for weights,
constraints and deadlines, and [FORMAT.md](FORMAT.md) the file.

## Go API

### Opening a store

```go
s := multivergence.New(opts)                      // in memory
s, err := multivergence.Open("trip.mv", opts)     // a file; created if missing; holds the lock
r, err := multivergence.OpenReadOnly("trip.mv", opts) // no lock; r.Refresh() reads again
defer s.Close()
```

`opts` can be nil. `Options`:

| Field | Default | Meaning |
|---|---|---|
| `Now func() time.Time` | `time.Now` | The clock. Every change records the time it used. |
| `MaxBranches int` | 4096 | Most live branches at once. Fork beyond it fails with `ErrTooManyBranches`. |
| `LockTimeout time.Duration` | 30s | How long `Open` waits for another writer. |
| `NoSync bool` | false | Skip `fsync` after each change. A process crash still loses nothing; a power cut can lose the last changes. |
| `CompactAfter int64` | max(1 MiB, 2 × snapshot) | Log bytes since the last snapshot that start a compaction. |
| `HistoryLimit int` | 10000 | Events kept in the history (`History` returns at most this many). |

A `Store` is safe to use from several goroutines.

### Data

The trunk is called `multivergence.Trunk` (`"trunk"`). These take a branch name
or `Trunk`:

```go
v, found, err := s.Get(branch, key)   // dead branches can be read until the next Promote
err := s.Set(branch, key, value)      // on a live branch or the trunk
err := s.Delete(branch, key)
keys, err := s.Keys(branch, prefix)   // in key order
err := s.Scan(branch, prefix, func(key string, value []byte) bool { ... })
```

Keys are 1 to 4096 bytes of UTF-8 without NUL. Values are up to 16 MiB.

A direct write to the trunk makes the branches forked before it stale (see
Promote). A write to a branch that breaks one of its constraints is kept, the
branch dies, and the call returns a `*KilledError`.

### Branches

```go
err := s.Fork(name, from, weight)       // from: Trunk or a live branch
err := s.Weight(name, weight)
err := s.Constrain(name, cs ...Constraint)
err := s.Expire(name, at)               // zero time clears it; 1678 to 2262 only
err := s.ExpireIn(name, d)              // d from now
out, err := s.Observe(Evidence{...})
out, err := s.Sweep()                   // record expired branches; writes nothing if none
err := s.Discard(name)
err := s.Promote(name)
```

Names are 1 to 64 characters: letters, digits, `.`, `_` and `-`, starting with
a letter or digit. `trunk` is taken. A name can't be used twice in one
generation, even after the branch died; after a Promote it can.

A fork from a branch starts with that branch's data, constraints and deadline.
The source is left as it was. If the forks replace it, Discard it.

Weights are finite numbers above zero.

### Constraints

```go
multivergence.Equals("seat", "held")
multivergence.NotEquals("status", "cancelled")
multivergence.Below("eta", 540)    // <
multivergence.AtMost("fare", 400)  // <=
multivergence.Above("seats", 0)    // >
multivergence.AtLeast("score", 0.8) // >=
multivergence.Present("hold")
multivergence.Missing("error")
c, err := multivergence.ParseConstraint("fare/flight <= 400")
c.String()                          // "fare/flight <= 400"
```

`ParseConstraint` reads `key op value`, or `key exists` / `key absent`. Put the
key in double quotes if it has spaces or quotes in it, and the value in double
quotes to keep spaces around it or to write an empty value. `String` writes
constraints the same way, so the two round-trip.

### Evidence and outcomes

```go
type Evidence struct {
    Facts       []Fact             // written to the trunk and every live branch first
    Likelihoods map[string]float64 // per branch
    Rest        *float64           // for live branches not in Likelihoods; multivergence.Likelihood(0.5)
    PruneBelow  float64            // kill branches below this probability, except the best
}
type Fact struct {
    Key    string
    Value  []byte
    Delete bool
}
type Outcome struct {
    Killed []Death       // in the order they died: Branch, Cause, Detail
    Live   []BranchInfo  // after the call, in fork order, with probabilities
}
name, ok := out.Settled() // the one live branch, if only one is left
best, ok := out.Best()    // the most likely live branch
```

If `Likelihoods` is empty, `Rest` is ignored (not even checked) and weights
just renormalise. If it isn't, every live branch needs an entry or `Rest`.

### Reading branches

```go
infos := s.Branches()  // this generation, live and dead, in fork order
live := s.Live()
best, ok := s.Best()
info, err := s.Branch(name)
gen := s.Generation()  // number of promotes so far
events := s.History()  // fork, weight, constrain, expire, observe, killed, promote, dropped
```

`BranchInfo` has `Name`, `Parent`, `Live`, `Weight`, `Probability`,
`Constraints`, `Deadline`, `Created`, `Cause`, `Detail`, `Died`, `Stale` and
`Keys` (the number of keys). Causes: `CauseViolated`, `CauseExpired`,
`CauseRuledOut`, `CausePruned`, `CauseDiscarded`.

### Errors

| Error | When |
|---|---|
| `ErrNotFound` | no branch by that name in this generation |
| `ErrNotLive` | the branch is dead |
| `ErrExists` | the name is taken in this generation |
| `ErrBadName`, `ErrBadKey`, `ErrBadValue` | names, keys or values outside the limits |
| `ErrBadWeight` | a weight that isn't a finite number above zero |
| `ErrBadLikelihood` | a likelihood below zero or not finite, or one that would overflow a weight |
| `ErrBadPrune` | `PruneBelow` below 0, or 1 or above |
| `ErrBadDeadline` | a deadline outside 1678 to 2262, or `ExpireIn` past that |
| `ErrMissingLikelihood` | a live branch has no likelihood and there is no `Rest` |
| `ErrBadConstraint` | a constraint that can't be parsed or makes no sense |
| `ErrTrunk` | Weight, Constrain, Expire, Discard or Promote on the trunk |
| `ErrStale` | Promote of a branch forked before a direct trunk write |
| `ErrTooManyBranches` | Fork beyond `MaxBranches` |
| `ErrKilled` | the change was made and the branch died of it (`*KilledError` has the details) |
| `ErrReadOnly`, `ErrClosed` | a change on a read-only or closed store |
| (wrapped I/O error) | a write or sync failed, or an earlier one did: reopen the store |
| `ErrCorrupt` | the file is damaged, or not a store |
| `ErrLocked` | another process held the file for longer than `LockTimeout` |

## The command

```
multivergence [-f file] [-json] <command> [arguments]
```

The store is the file given by `-f`, or `$MULTIVERGENCE_FILE`, or
`multivergence.mv` in the current directory. Commands that change it create it
if needed. Flags may go before or after a command's other arguments; `--` ends
the flags, so `multivergence set -- k -5` writes `-5`.

| Command | Does |
|---|---|
| `get [-b branch] key` | print a value; exit 3 if the key is missing |
| `keys [-b branch] [-values] [prefix]` | list keys in order; with `-values`, key, a tab and the value, Go-quoted if it holds a tab, a newline or other unprintable bytes |
| `set [-b branch] key value` | write a value; `-` reads it from stdin |
| `del [-b branch] key` | delete a key |
| `fork name [-from branch] [-weight w]` | fork the trunk or a live branch (weight 1 by default) |
| `weight name w` | set a branch's weight |
| `constrain name 'key op value'...` | add constraints |
| `expire name -in 15m` \| `-at RFC3339` \| `-clear` | set or clear a deadline |
| `observe [-fact k=v]... [-unset k]... [-l name=x]... [-rest x] [-prune p]` | apply evidence; facts go in the order given |
| `sweep` | record branches whose deadline has passed |
| `discard name` | kill a branch |
| `promote name` | make a branch the trunk |
| `branches [-live]` | this generation's branches |
| `best` | the most likely live branch (exit 3 if none) |
| `history [-n count]` | recent events |
| `version` | the version |

Every command takes `-f`, `-json` and `-wait` (how long to wait for another
writer: 30s by default, 0 not at all). With `-json` each command prints one
JSON value, and the exit codes stay the same; a value that isn't valid UTF-8
comes back as `value_base64` instead of `value`. A failure prints
`{"error": "...", "kind": "..."}` with kinds such as `not_found`, `not_live`,
`stale`, `missing_likelihood`, `bad_deadline`, `locked` and `no_store`.

Exit codes: 0 done; 1 error; 2 bad usage; 3 the key is missing (`get`) or no
branch is live (`best`); 4 the change was made and the branch died of it.

`MULTIVERGENCE_NOW`, an RFC 3339 time, fixes the command's clock, for tests and
for replaying a scenario. Any other value is a usage error.
