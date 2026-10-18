# Releasing Multivergence

Releases go out on their own. Nobody clicks Publish. This file is the record of
how that works, for whoever (person or Claude session) works on this repo next.

## The short version

1. Raise `const Version = "X.Y.Z"` in `multivergence.go`.
2. Write what changed into the commit message.
3. Push the commit to `main`.

When the `test` workflow passes on that commit (the full suite and the smoke
check on Linux, macOS and Windows), the `release` workflow builds the binaries,
smoke-tests each on its own kind of machine, then tags the commit `vX.Y.Z` and
publishes the release "Multivergence vX.Y.Z" with the archives and SHA256SUMS
attached. If any build or check fails, nothing is tagged and nothing goes
public. Fix it, push again, and the next green run picks it up.

Pushing without raising the version is fine: the release workflow sees the tag
already exists and stops quietly.

## What it does, step by step

[.github/workflows/release.yml](.github/workflows/release.yml) starts when a
`test` run finishes; the push alone doesn't start it.

- **plan.** Goes on only if the `test` run passed, came from a push to `main` in
  this repo, and `main` still points at that commit (if `main` moved on, the run
  for the newer commit decides). Reads `Version` from `multivergence.go`. Stops
  quietly if `vX.Y.Z` is already a tag. Fails loudly if the version is lower
  than the newest tag. [scripts/release-plan.py](scripts/release-plan.py) has the
  rules, and [scripts/release_plan_test.py](scripts/release_plan_test.py) its
  cases.
- **build.** Multivergence is pure Go with no cgo, so one Linux runner builds
  `multivergence` and `multivergence-demo` for every target from that exact
  commit, and checks the Linux binaries are static.
- **check.** Each archive is unpacked on its own kind of machine (Linux x86-64,
  Linux ARM64, macOS Apple silicon, Windows) and runs
  [scripts/smoke.py](scripts/smoke.py). The macOS Intel binaries are checked for
  their architecture.
- **release.** Runs only after every build and check passed. Writes SHA256SUMS
  and the notes, then creates the tag and the release in one step with
  `gh release create --target <commit>`, on the repo's own `GITHUB_TOKEN`
  (`contents: write` on this job only). A `concurrency` group per tag means two
  runs for the same version can't publish twice.

The smoke check, on each binary set: both binaries report the expected version;
the agent demo passes its own checks in memory and in a file; the command reads
the demo's file back; `scripts/cross_language.py` drives the command from
Python through forks, constraints, facts, likelihoods, a deadline, a promote and
a stale promote, and checks every probability against its own arithmetic; and
eight processes fork at the same moment through the file lock.

## Files that go out

| System | File |
|---|---|
| Linux, x86-64 (static) | multivergence_linux_amd64.tar.gz |
| Linux, ARM64 (static) | multivergence_linux_arm64.tar.gz |
| macOS, Apple silicon | multivergence_darwin_arm64.tar.gz |
| macOS, Intel | multivergence_darwin_amd64.tar.gz |
| Windows, x86-64 | multivergence_windows_amd64.zip |
| Checksums | SHA256SUMS |

Each archive holds the two binaries, LICENSE, NOTICE, THIRD_PARTY_LICENSES.md
and README.md. Archive names carry no version on purpose: links like
`https://github.com/multivergence/multivergence/releases/latest/download/multivergence_linux_amd64.tar.gz`
keep working from one release to the next, so the README and the site never
need editing for a release.

## After a release

A Claude session can't create tags or releases itself, which is why the workflow
does it on the repository's token. After pushing a raised version, check:

1. The `test` run and then the `release` run finished green.
2. The release exists with all six files.
3. The `latest/download` links resolve, and a downloaded archive's SHA256
   matches SHA256SUMS.
4. A downloaded Linux binary runs: `multivergence version` names the release,
   and `multivergence-demo` ends with "All checks passed."

## By hand

- A release published by hand gets the binaries attached by the same workflow.
- To rebuild the files of an existing tag, run the `release` workflow by hand
  with the tag (`gh workflow run release --repo multivergence/multivergence -f tag=vX.Y.Z`).
