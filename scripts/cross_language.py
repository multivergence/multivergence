#!/usr/bin/env python3
# Copyright Multivergence.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Drives the multivergence command from Python, the way an agent written in
# another language would, and checks every answer. The probabilities are
# worked out here, independently, and compared with what the engine says.
#
#   python3 scripts/cross_language.py path/to/multivergence
#
# Exits with 1 on the first failed check.
import json
import os
import subprocess
import sys
import tempfile

BIN = sys.argv[1] if len(sys.argv) > 1 else "multivergence"
checks = 0


def run(*args, now="2026-11-02T08:00:00Z", expect=0, stdin=None):
    env = dict(os.environ, MULTIVERGENCE_FILE=STORE, MULTIVERGENCE_NOW=now)
    # Bytes in and out: in text mode Windows would turn \n into \r\n on stdin.
    p = subprocess.run([BIN, "-json", *args], env=env, capture_output=True,
                       input=stdin.encode() if stdin is not None else None)
    p.stdout, p.stderr = p.stdout.decode(), p.stderr.decode()
    if p.returncode != expect:
        fail(f"{' '.join(args)}: exit {p.returncode}, wanted {expect}\n{p.stdout}{p.stderr}")
    return json.loads(p.stdout) if p.stdout.strip() else None


def check(ok, what):
    global checks
    if not ok:
        fail(what)
    checks += 1
    print("ok  " + what)


def fail(what):
    print("FAIL " + what)
    sys.exit(1)


def probs(rows):
    return {r["name"]: r["probability"] for r in rows if r["live"]}


def close(a, b):
    return abs(a - b) < 1e-12


with tempfile.TemporaryDirectory() as d:
    STORE = os.path.join(d, "trip.mv")

    check(run("version")["version"], "version answers")
    run("set", "budget", "400")
    run("set", "request", "-", stdin="Lisbon to Porto\nby 09:00\n")
    got = run("get", "request")
    check(got["found"] and got["value"] == "Lisbon to Porto\nby 09:00\n", "a value from stdin keeps its newlines")

    prior = {"plan-train": 0.5, "plan-flight": 0.3, "plan-bus": 0.2}
    for name, w in prior.items():
        run("fork", name, "-weight", str(w))
    for name, mode, est in [("plan-train", "train", "190"), ("plan-flight", "flight", "250"), ("plan-bus", "bus", "60")]:
        run("set", "-b", name, "fare/" + mode, est)
        run("constrain", name, f"fare/{mode} <= 400")
    run("expire", "plan-bus", "-at", "2026-11-02T08:15:00Z")
    rows = run("branches")
    check([r["name"] for r in rows] == list(prior), "branches come back in the order they were forked")
    for name, p in probs(rows).items():
        check(close(p, prior[name]), f"{name} starts at {prior[name]}")
    check(run("get", "booking", expect=3)["found"] is False, "the trunk is untouched by the plans (exit 3: no such key)")
    check(run("get", "-b", "plan-flight", "fare/flight")["value"] == "250", "a branch reads its own write")

    out = run("observe", "-fact", "fare/train=180", "-fact", "fare/flight=460", "-fact", "fare/bus=60",
              now="2026-11-02T08:02:00Z")
    check([k["branch"] for k in out["killed"]] == ["plan-flight"], "the fare fact kills the flight")
    check(out["killed"][0]["cause"] == "violated" and out["killed"][0]["detail"] == "fare/flight <= 400",
          "the death names the constraint")
    left = {k: v for k, v in prior.items() if k != "plan-flight"}
    total = sum(left.values())
    for name, p in probs(out["live"]).items():
        check(close(p, left[name] / total), f"{name} renormalised to {left[name] / total:.6f}")

    lik = {"plan-train": 0.9, "plan-bus": 0.3}
    out = run("observe", "-l", "plan-train=0.9", "-l", "plan-bus=0.3", now="2026-11-02T08:05:00Z")
    post = {k: left[k] * lik[k] for k in left}
    z = sum(post.values())
    for name, p in probs(out["live"]).items():
        check(close(p, post[name] / z), f"{name} is {post[name] / z:.6f} after Bayes, worked out in Python")

    out = run("observe", "-l", "plan-train=1", now="2026-11-02T08:06:00Z", expect=1)
    check(out["kind"] == "missing_likelihood", "a live branch without a likelihood is refused")

    out = run("set", "-b", "plan-train", "fare/train", "999", now="2026-11-02T08:07:00Z", expect=4)
    check(out["killed"]["branch"] == "plan-train" and out["killed"]["cause"] == "violated",
          "a write that breaks a constraint exits 4 and names the death")
    rows = run("branches", "-live", now="2026-11-02T08:07:00Z")
    check([r["name"] for r in rows] == ["plan-bus"], "only the bus is left")

    rows = run("branches", "-live", now="2026-11-02T08:15:00Z")
    check(rows == [], "at its deadline the bus reads as dead before any write")
    out = run("sweep", now="2026-11-02T08:15:00Z")
    check(out["killed"][0]["cause"] == "expired", "sweep records the expiry")

    run("fork", "retry", "-weight", "1", now="2026-11-02T08:16:00Z")
    run("set", "-b", "retry", "booking", "Intercidades 06:52", now="2026-11-02T08:16:00Z")
    out = run("promote", "retry", now="2026-11-02T08:17:00Z")
    check(out["generation"] == 1, "promote starts generation 1")
    check(run("get", "booking")["value"] == "Intercidades 06:52", "the trunk holds the promoted booking")
    check(run("branches") == [], "promote drops every branch")

    run("fork", "late", now="2026-11-02T08:18:00Z")
    run("set", "budget", "300", now="2026-11-02T08:19:00Z")
    out = run("promote", "late", now="2026-11-02T08:20:00Z", expect=1)
    check(out["kind"] == "stale", "a branch forked before a direct trunk write can't be promoted")

    out = run("get", "-b", "nope", "k", expect=1)
    check(out["kind"] == "not_found", "an unknown branch is not_found")
    p = subprocess.run([BIN, "get", "missing"], env=dict(os.environ, MULTIVERGENCE_FILE=STORE), capture_output=True)
    check(p.returncode == 3 and p.stdout == b"", "a missing key exits 3 with nothing printed")
    hist = run("history", "-n", "100")
    kinds = [e["kind"] for e in hist]
    check("promote" in kinds and "killed" in kinds and "observe" in kinds, "the history records forks, deaths and promotes")

print(f"cross-language: {checks} checks passed")
