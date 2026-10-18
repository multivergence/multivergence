#!/usr/bin/env python3
# Copyright Multivergence.com 2026
# SPDX-License-Identifier: Apache-2.0
"""Checks a directory of built Multivergence binaries on the machine that runs them.

    python3 scripts/smoke.py BIN_DIR [EXPECTED_VERSION]

BIN_DIR holds multivergence and multivergence-demo (with .exe on Windows).
Both must report the expected version (Version in multivergence.go unless
given). Then: the agent demo runs and passes its own checks, in memory and in
a file; the command reads the demo's file back; scripts/cross_language.py
drives the command through a full fork, observe, expire and promote cycle
and checks every probability; and eight processes fork at the same moment
through the file lock, and all eight branches must be there. The release
workflow runs it on every binary it ships, on the machine type that binary
is for; any failure stops the release.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parent.parent
WINDOWS = os.name == "nt"


class Failure(Exception):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


def run(args, timeout=120, env=None, expect=0):
    r = subprocess.run([str(a) for a in args], capture_output=True, text=True, timeout=timeout, env=env)
    require(r.returncode == expect, f"{Path(str(args[0])).name} {' '.join(str(a) for a in args[1:])} exited {r.returncode}:\n{r.stdout[-3000:]}{r.stderr[-3000:]}")
    return r.stdout


def source_version():
    text = (ROOT / "multivergence.go").read_text(encoding="utf-8")
    found = re.findall(r'^const Version = "(.*)"$', text, re.M)
    require(len(found) == 1, "multivergence.go has no single Version")
    return found[0]


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    bindir = Path(sys.argv[1]).resolve()
    want = sys.argv[2] if len(sys.argv) > 2 else source_version()
    ext = ".exe" if WINDOWS else ""
    mv = bindir / ("multivergence" + ext)
    demo = bindir / ("multivergence-demo" + ext)
    for b in (mv, demo):
        require(b.is_file(), f"{b} is missing")

    out = run([mv, "version"]).strip()
    require(out == f"multivergence {want}", f"multivergence reports {out!r}, want {want}")
    out = run([demo, "-version"]).strip()
    require(out == f"multivergence-demo {want}", f"multivergence-demo reports {out!r}, want {want}")
    print(f"ok  both binaries report {want}")

    out = run([demo])
    require("All checks passed." in out and "plan-flight died (violated: fare/flight <= 400)" in out, "demo output:\n" + out)
    print("ok  the agent demo passes its checks in memory")

    with tempfile.TemporaryDirectory() as d:
        trip = Path(d) / "trip.mv"
        out = run([demo, "-f", trip])
        require("All checks passed." in out, "demo with a file:\n" + out)
        env = dict(os.environ, MULTIVERGENCE_FILE=str(trip))
        booking = run([mv, "get", "booking"], env=env).strip()
        require(booking == "Intercidades 06:52, arrives 08:41", f"the demo's file holds {booking!r}")
        hist = json.loads(run([mv, "-json", "history"], env=env))
        kinds = [e["kind"] for e in hist]
        require(kinds.count("killed") == 2 and kinds[-1] == "promote", f"history kinds {kinds}")
        print("ok  the demo's file reads back through the command")

        out = run([sys.executable, "-B", ROOT / "scripts" / "cross_language.py", mv])
        print("ok  " + out.strip().splitlines()[-1])

        shared = Path(d) / "shared.mv"
        env = dict(os.environ, MULTIVERGENCE_FILE=str(shared))
        procs = [subprocess.Popen([str(mv), "fork", f"p{i}"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                 for i in range(8)]
        for p in procs:
            _, err = p.communicate(timeout=120)
            require(p.returncode == 0, f"a concurrent fork failed: {err.decode(errors='replace')}")
        names = sorted(b["name"] for b in json.loads(run([mv, "-json", "branches"], env=env)))
        require(names == [f"p{i}" for i in range(8)], f"after 8 concurrent forks: {names}")
        print("ok  eight processes forked at once through the file lock")
    print(f"smoke check passed: {bindir}")


if __name__ == "__main__":
    try:
        main()
    except (Failure, subprocess.TimeoutExpired) as e:
        print(f"SMOKE CHECK FAILED: {e}")
        sys.exit(1)
