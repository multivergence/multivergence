# Notes for Claude

This file says where the next session starts and what comes next. What comes
next is decided with the owner before it goes here, and work starts only when
the owner says go.

## Releases

Releases are automated. To release, raise `Version` in `multivergence.go` and
push to `main`; never ask the owner to publish one. [RELEASING.md](RELEASING.md)
has the procedure and the checks to run afterwards.

## Where things stand

v0.1.0 is the first release (October 2026): Apache 2.0, CI on Linux, macOS and
Windows, and automated releases with smoke-tested binaries for Linux, macOS and
Windows. The code is pure Go with no dependencies outside the standard library:
a canonical copy-on-write treap (`internal/pmap`), branches with weights,
declarative constraints and deadlines, Observe with caller-supplied
likelihoods, Promote with a staleness check, an append-only CRC-checked file
with torn-tail recovery and snapshot compaction, a writer lock, a CLI with JSON
output (`internal/cli`), the agent demo (`examples/agent`, shipped as
`multivergence-demo`) and a speculation example.

Read [docs/UPDATES.md](docs/UPDATES.md) before changing how weights,
constraints or deadlines behave: it is the contract, and replaying old files
depends on it. [docs/FORMAT.md](docs/FORMAT.md) is the file format; op and
record numbers must never change meaning.

Out of scope in 0.1: merging branches, undoing real-world side effects of a
branch, more than one machine. Each change copies the branch table (about
70 µs per write with 1,000 live branches).

## Test results

`sh scripts/record-tests.sh` runs every test at ten times the CI size (set
`MV_SCALE` to change it), the race detector, the demo, the speculation example
and the smoke check on static binaries, and writes them to `test/results/`,
with JSON figures from the promise tests. Every number in the README and on the
site comes from those files; record again after any change that could move
them. It needs Go and Python 3. The site's images are drawn from the same
figures.

## Site

Multivergence.com is in `photozeitgeist/multivergence.com` (October 2026): the
Hugo Launch template copied unchanged, with content and config values only.
Pages: about, contact, quick start, download, reference. Posts: the explainer
(how it works), the launch, how it was tested, and the name. Images are drawn
into static/images. When a recording changes a number, update the site too.

## Next

Nothing is decided. Agree it with the owner before it goes here.
