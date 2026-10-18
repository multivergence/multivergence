# How weights, constraints and deadlines are applied

This is the contract. The engine never works out a probability on its own: the
caller supplies weights and likelihoods, and the engine applies them by the
rules below, the same way every time. Two stores given the same calls end up
with the same weights, bit for bit, and a file replays to exactly the state it
recorded.

## Weights and probabilities

Every branch has a **weight**, a finite number above zero. Fork sets it, Weight
changes it. A branch's **probability** is its weight divided by the total
weight of the live branches. Dead branches have probability 0.

Weights don't have to add up to 1. After an Observe they do, because Observe
renormalises.

Probabilities are worked out when you read them, over the branches that are
live at that moment, scaling by the largest weight first so the total can't
overflow. So a branch whose deadline has passed drops out of the
total as soon as you read, before any change has recorded its death.

## Order of work in every change

Every change (Set, Delete, Fork, Weight, Constrain, Expire, Observe, Sweep,
Discard, Promote) starts the same way:

1. **Deadlines.** Each live branch whose deadline is at or before the change's
   time dies, cause `expired`. Branches are taken in the order they were
   forked.

Then the change itself. If it fails (a bad argument, a dead branch, a stale
promote) nothing at all happens, including step 1, and nothing is written to
the file.

## Observe, step by step

`Observe(Evidence)` runs these steps after the deadlines, in this order. All of
them happen or none of them do.

1. **Facts.** Each fact, in the order given, is written to the trunk and to
   every live branch (or deleted from them, if `Delete` is set). Dead branches
   don't get facts. Facts don't make branches stale, because every branch got
   the same writes.
2. **Constraints.** Each live branch is checked, in fork order. The first of
   its constraints that doesn't hold kills it, cause `violated`, and the
   constraint is recorded as the detail.
3. **Likelihoods.** Each branch still live gets a likelihood:
   - its entry in `Likelihoods`, if there is one;
   - otherwise `Rest`, if it is set;
   - otherwise, if `Likelihoods` is empty, 1;
   - otherwise the whole Observe fails with `ErrMissingLikelihood`.

   Names in `Likelihoods` that belong to dead branches of this generation are
   ignored. Names that never existed fail the Observe with `ErrNotFound`.
   Likelihoods must be finite and not negative.

   The new raw weight is `weight × likelihood`. A branch whose likelihood is 0
   dies, cause `ruled out`. A branch whose likelihood is above 0 but whose raw
   weight underflowed to 0 (both numbers tiny) dies, cause `pruned`, detail
   "weight underflowed to zero". If any raw weight overflows to infinity, the
   Observe fails with `ErrBadLikelihood`. `Rest` is checked only when
   `Likelihoods` isn't empty.
4. **Renormalise.** Let `m` be the largest raw weight among the survivors. Each
   survivor's weight becomes `(raw / m) / S`, where `S` is the sum of
   `raw / m` over the survivors, added in fork order. Dividing by `m` first
   means neither the sum nor the division can overflow. A survivor whose
   share is still too small for a float64 to hold (about 1e-323 or less)
   dies, cause `pruned`, detail "weight underflowed to zero": a live branch
   always has a weight above zero. The survivors' weights now sum to 1, within
   rounding.
5. **Prune.** If `PruneBelow` is above 0 and more than one branch survives,
   each survivor whose weight is below `PruneBelow` dies, cause `pruned`,
   except the survivor with the highest weight (on a tie, the oldest). Pruning
   happens once; if it killed anything, the remaining branches are renormalised
   once more as in step 4.

If every branch dies along the way, that is a valid outcome, not an error: the
facts ruled out every plan. Probabilities are then empty and the trunk still
has the facts.

### A worked example

Three plans start at weights 0.5, 0.3 and 0.2. A fare fact kills the second one
(its constraint `fare/flight <= 400` breaks). The survivors renormalise to
0.5/0.7 and 0.2/0.7, about 0.714 and 0.286.

A second Observe gives likelihoods 0.9 and 0.3. Raw weights are 0.714 × 0.9
and 0.286 × 0.3. Scaled and renormalised, the first is 0.45 / (0.45 + 0.06),
about 0.882, and the second about 0.118. This is Bayes' rule: posterior
proportional to prior times likelihood.

## Constraints

A constraint is a condition on one key of a branch:

| Operator | Holds when |
|---|---|
| `==` | the key exists and its value is exactly the given bytes |
| `!=` | the key is missing, or its value differs |
| `<` `<=` `>` `>=` | the key exists, its value reads as a finite number, and the comparison is true |
| `exists` | the key exists |
| `absent` | the key is missing |

Numbers are read with Go's `strconv.ParseFloat` after trimming spaces. A
missing key or a value that isn't a number breaks a numeric constraint.

Constraints are checked:

- when they are added (Constrain): if one doesn't hold already, the branch dies
  at once;
- after every Set or Delete on the branch: if the write breaks one, the write
  is kept and the branch dies;
- in step 2 of every Observe.

In the first two cases the call returns a `*KilledError` (`errors.Is(err,
ErrKilled)`), which means the change was made and the branch died of it. The
command exits with code 4 for the same thing.

A branch forked from a branch takes a copy of its constraints. Constraints
can't be removed; fork a new branch instead.

## Deadlines

A deadline is a moment between the years 1678 and 2262, the range of int64
nanoseconds; Expire refuses others with `ErrBadDeadline`, and ExpireIn refuses
a duration that would land outside it. A branch dies at the first change made at or after it
(step 1 above), and reads show it as expired from that moment on, even before
any change. Expire with a moment already past kills the branch at once.

A branch forked from a branch takes its deadline; Expire can then move it
later, earlier, or clear it.

## Promote and staleness

Promote makes a live branch's data the trunk, drops every branch of the
generation, live or dead, and adds one to the generation. It is one record in
the file.

A direct Set on the trunk, or a Delete of a key the trunk has, moves it
forward. (Deleting a key the trunk doesn't have changes nothing.) Branches forked before
that write (directly, or through the branch they were forked from) are
**stale**: promoting one would throw the trunk write away, so Promote refuses
it with `ErrStale`. Facts don't count, since every live branch got them too.
Write shared inputs to the trunk before forking.

## Time

Every change records the time it was made, from `Options.Now` (the system clock
by default). Deadlines are compared with that recorded time, so a replay gives
the same deaths whatever the clock says later. The command's clock can be fixed
with the environment variable `MULTIVERGENCE_NOW` (an RFC 3339 time), for tests
and for reproducing a run.
