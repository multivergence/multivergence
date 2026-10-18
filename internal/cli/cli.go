// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package cli is the multivergence command. Run takes the arguments and the
// standard streams, so tests can drive it without starting a process.
package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	mv "github.com/multivergence/multivergence"
)

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitUsage    = 2
	ExitNotFound = 3 // get: the key isn't there
	ExitKilled   = 4 // the change was made, and the branch died of it
)

const usage = `multivergence: forkable state for agents that think in alternatives

Usage: multivergence [-f file] [-json] <command> [arguments]

The store is the file given by -f, or $MULTIVERGENCE_FILE, or
multivergence.mv in the current directory. Commands that change it create
it if it doesn't exist.

Reading
  get [-b branch] key            print a value (exit 3 if the key is missing)
  keys [-b branch] [-values] [prefix]
                                 list keys, in order
  branches [-live]               every branch of this generation
  best                           the most likely live branch
  history [-n count]             recent events

Writing data (on the trunk unless -b names a branch)
  set [-b branch] key value      write a value ("-" reads it from stdin)
  del [-b branch] key            delete a key

Branches
  fork name [-from branch] [-weight w]
                                 fork the trunk, or a live branch
  weight name w                  set a branch's weight
  constrain name 'key op value'...
                                 add constraints: ==, !=, <, <=, >, >=,
                                 'key exists', 'key absent'
  expire name -in 15m | -at 2026-10-18T12:00:00Z | -clear
  observe [-fact key=value]... [-unset key]... [-l name=likelihood]...
          [-rest likelihood] [-prune p]
                                 apply evidence: facts, constraints,
                                 likelihoods, renormalise, prune
  sweep                          kill branches whose deadline has passed
  discard name                   kill a branch
  promote name                   make a branch the trunk, drop the rest

Other
  version                        print the version

Every command takes -f, -json and -wait (how long to wait for another
writer: 30s by default, 0 not at all). With -json, output is one JSON value;
a value that isn't UTF-8 comes back as value_base64 instead of value.
Exit codes: 0 done, 1 error, 2 bad usage, 3 key not found, 4 the change was
made and the branch died of it (a constraint broke or its deadline passed).
`

type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	file           string
	json           bool
	wait           time.Duration
	now            time.Time // from MULTIVERGENCE_NOW, if set
}

// Run runs one command and returns the exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{stdin: stdin, stdout: stdout, stderr: stderr}
	e.file = os.Getenv("MULTIVERGENCE_FILE")
	if e.file == "" {
		e.file = "multivergence.mv"
	}
	e.wait = 30 * time.Second
	top := flag.NewFlagSet("multivergence", flag.ContinueOnError)
	top.SetOutput(io.Discard)
	e.common(top)
	if err := top.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return ExitOK
		}
		return e.usageErr(err.Error())
	}
	rest := top.Args()
	if len(rest) == 0 {
		fmt.Fprint(stderr, usage)
		return ExitUsage
	}
	if v := os.Getenv("MULTIVERGENCE_NOW"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return e.usageErr("MULTIVERGENCE_NOW must be an RFC 3339 time, such as 2026-11-02T08:00:00Z")
		}
		e.now = t
	}
	cmd, cargs := rest[0], rest[1:]
	run, ok := commands[cmd]
	if !ok {
		if cmd == "help" {
			fmt.Fprint(stdout, usage)
			return ExitOK
		}
		return e.usageErr("unknown command " + strconv.Quote(cmd))
	}
	return run(e, cargs)
}

var commands map[string]func(*env, []string) int

func init() {
	commands = map[string]func(*env, []string) int{
		"version": cmdVersion, "get": cmdGet, "keys": cmdKeys, "branches": cmdBranches,
		"best": cmdBest, "history": cmdHistory, "set": cmdSet, "del": cmdDel,
		"fork": cmdFork, "weight": cmdWeight, "constrain": cmdConstrain, "expire": cmdExpire,
		"observe": cmdObserve, "sweep": cmdSweep, "discard": cmdDiscard, "promote": cmdPromote,
	}
}

func (e *env) common(fs *flag.FlagSet) {
	fs.StringVar(&e.file, "f", e.file, "store file")
	fs.BoolVar(&e.json, "json", e.json, "print JSON")
	fs.DurationVar(&e.wait, "wait", e.wait, "how long to wait for another writer")
}

// parse reads a command's flags, which may come before or after its other
// arguments, and checks the number of arguments.
func (e *env) parse(fs *flag.FlagSet, args []string, min, max int) ([]string, bool) {
	e.common(fs)
	fs.SetOutput(io.Discard)
	var pos, after []string
	for k, a := range args {
		if a == "--" {
			args, after = args[:k], args[k+1:]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			e.usageErr(fs.Name() + ": " + err.Error())
			return nil, false
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	pos = append(pos, after...)
	if len(pos) < min || (max >= 0 && len(pos) > max) {
		e.usageErr(fs.Name() + ": wrong number of arguments")
		return nil, false
	}
	return pos, true
}

func (e *env) usageErr(msg string) int {
	if e.json {
		e.emit(map[string]any{"error": msg, "kind": "usage"})
	} else {
		fmt.Fprintf(e.stderr, "multivergence: %s\nRun 'multivergence help' for usage.\n", msg)
	}
	return ExitUsage
}

func (e *env) fail(err error) int {
	var ke *mv.KilledError
	if errors.As(err, &ke) {
		if e.json {
			e.emit(map[string]any{"ok": true, "killed": death(mv.Death{Branch: ke.Branch, Cause: ke.Cause, Detail: ke.Detail})})
		} else {
			fmt.Fprintf(e.stdout, "branch %s died: %s\n", ke.Branch, why(ke.Cause, ke.Detail))
		}
		return ExitKilled
	}
	if e.json {
		e.emit(map[string]any{"error": err.Error(), "kind": errKind(err)})
	} else {
		fmt.Fprintf(e.stderr, "%v\n", err)
	}
	return ExitError
}

func errKind(err error) string {
	kinds := []struct {
		err  error
		name string
	}{
		{mv.ErrNotFound, "not_found"}, {mv.ErrNotLive, "not_live"}, {mv.ErrExists, "exists"},
		{mv.ErrBadName, "bad_name"}, {mv.ErrBadKey, "bad_key"}, {mv.ErrBadValue, "bad_value"},
		{mv.ErrBadWeight, "bad_weight"}, {mv.ErrBadLikelihood, "bad_likelihood"},
		{mv.ErrMissingLikelihood, "missing_likelihood"}, {mv.ErrBadPrune, "bad_prune"},
		{mv.ErrBadConstraint, "bad_constraint"}, {mv.ErrBadDeadline, "bad_deadline"}, {mv.ErrTrunk, "trunk"}, {mv.ErrStale, "stale"},
		{mv.ErrTooManyBranches, "too_many_branches"}, {mv.ErrCorrupt, "corrupt"}, {mv.ErrLocked, "locked"},
	}
	for _, k := range kinds {
		if errors.Is(err, k.err) {
			return k.name
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return "no_store"
	}
	return "error"
}

func (e *env) emit(v any) {
	enc := json.NewEncoder(e.stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func (e *env) options() *mv.Options {
	o := &mv.Options{LockTimeout: e.wait}
	if e.wait <= 0 {
		o.LockTimeout = time.Nanosecond // -wait 0: don't wait at all
	}
	if !e.now.IsZero() {
		t := e.now
		o.Now = func() time.Time { return t }
	}
	return o
}

// write opens the store for writing, runs fn, and closes it.
func (e *env) write(fn func(s *mv.Store) error) error {
	s, err := mv.Open(e.file, e.options())
	if err != nil {
		return err
	}
	ferr := fn(s)
	cerr := s.Close()
	if ferr != nil {
		return ferr
	}
	return cerr
}

// read opens the store read-only.
func (e *env) read() (*mv.Store, error) { return mv.OpenReadOnly(e.file, e.options()) }

func (e *env) okLine(text string, v map[string]any) int {
	if e.json {
		if v == nil {
			v = map[string]any{}
		}
		v["ok"] = true
		e.emit(v)
	} else if text != "" {
		fmt.Fprintln(e.stdout, text)
	}
	return ExitOK
}

func cmdVersion(e *env, args []string) int {
	if _, ok := e.parse(flag.NewFlagSet("version", flag.ContinueOnError), args, 0, 0); !ok {
		return ExitUsage
	}
	return e.okLine("multivergence "+mv.Version, map[string]any{"version": mv.Version})
}

func cmdGet(e *env, args []string) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	b := fs.String("b", mv.Trunk, "branch")
	pos, ok := e.parse(fs, args, 1, 1)
	if !ok {
		return ExitUsage
	}
	s, err := e.read()
	if err != nil {
		return e.fail(err)
	}
	v, found, err := s.Get(*b, pos[0])
	if err != nil {
		return e.fail(err)
	}
	if e.json {
		out := map[string]any{"branch": *b, "key": pos[0], "found": found}
		if found {
			putValue(out, v)
		}
		e.emit(out)
		if !found {
			return ExitNotFound
		}
		return ExitOK
	}
	if !found {
		return ExitNotFound
	}
	e.stdout.Write(v)
	if len(v) == 0 || v[len(v)-1] != '\n' {
		fmt.Fprintln(e.stdout)
	}
	return ExitOK
}

func cmdKeys(e *env, args []string) int {
	fs := flag.NewFlagSet("keys", flag.ContinueOnError)
	b := fs.String("b", mv.Trunk, "branch")
	values := fs.Bool("values", false, "print values too")
	pos, ok := e.parse(fs, args, 0, 1)
	if !ok {
		return ExitUsage
	}
	prefix := ""
	if len(pos) == 1 {
		prefix = pos[0]
	}
	s, err := e.read()
	if err != nil {
		return e.fail(err)
	}
	out := []map[string]any{}
	var lines []string
	err = s.Scan(*b, prefix, func(k string, v []byte) bool {
		item := map[string]any{"key": k}
		line := k
		if *values {
			putValue(item, v)
			line += "\t" + textValue(v)
		}
		out = append(out, item)
		lines = append(lines, line)
		return true
	})
	if err != nil {
		return e.fail(err)
	}
	if e.json {
		e.emit(out)
		return ExitOK
	}
	for _, l := range lines {
		fmt.Fprintln(e.stdout, l)
	}
	return ExitOK
}

func cmdSet(e *env, args []string) int {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	b := fs.String("b", mv.Trunk, "branch")
	pos, ok := e.parse(fs, args, 2, 2)
	if !ok {
		return ExitUsage
	}
	val := []byte(pos[1])
	if pos[1] == "-" {
		var err error
		if val, err = io.ReadAll(io.LimitReader(e.stdin, mv.MaxValueLen+1)); err != nil {
			return e.fail(err)
		}
	}
	if err := e.write(func(s *mv.Store) error { return s.Set(*b, pos[0], val) }); err != nil {
		return e.fail(err)
	}
	return e.okLine("", nil)
}

func cmdDel(e *env, args []string) int {
	fs := flag.NewFlagSet("del", flag.ContinueOnError)
	b := fs.String("b", mv.Trunk, "branch")
	pos, ok := e.parse(fs, args, 1, 1)
	if !ok {
		return ExitUsage
	}
	if err := e.write(func(s *mv.Store) error { return s.Delete(*b, pos[0]) }); err != nil {
		return e.fail(err)
	}
	return e.okLine("", nil)
}

func cmdFork(e *env, args []string) int {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	from := fs.String("from", mv.Trunk, "branch to fork")
	w := fs.Float64("weight", 1, "weight")
	pos, ok := e.parse(fs, args, 1, 1)
	if !ok {
		return ExitUsage
	}
	if err := e.write(func(s *mv.Store) error { return s.Fork(pos[0], *from, *w) }); err != nil {
		return e.fail(err)
	}
	return e.okLine("", nil)
}

func cmdWeight(e *env, args []string) int {
	pos, ok := e.parse(flag.NewFlagSet("weight", flag.ContinueOnError), args, 2, 2)
	if !ok {
		return ExitUsage
	}
	w, err := strconv.ParseFloat(pos[1], 64)
	if errors.Is(err, strconv.ErrRange) {
		err = nil // out of range: the engine refuses it as a bad weight
	}
	if err != nil {
		return e.usageErr("weight: " + strconv.Quote(pos[1]) + " is not a number")
	}
	if err := e.write(func(s *mv.Store) error { return s.Weight(pos[0], w) }); err != nil {
		return e.fail(err)
	}
	return e.okLine("", nil)
}

func cmdConstrain(e *env, args []string) int {
	pos, ok := e.parse(flag.NewFlagSet("constrain", flag.ContinueOnError), args, 2, -1)
	if !ok {
		return ExitUsage
	}
	var cs []mv.Constraint
	for _, p := range pos[1:] {
		c, err := mv.ParseConstraint(p)
		if err != nil {
			return e.fail(err)
		}
		cs = append(cs, c)
	}
	if err := e.write(func(s *mv.Store) error { return s.Constrain(pos[0], cs...) }); err != nil {
		return e.fail(err)
	}
	return e.okLine("", nil)
}

func cmdExpire(e *env, args []string) int {
	fs := flag.NewFlagSet("expire", flag.ContinueOnError)
	in := fs.Duration("in", 0, "die after this long")
	at := fs.String("at", "", "die at this RFC 3339 time")
	clear := fs.Bool("clear", false, "remove the deadline")
	pos, ok := e.parse(fs, args, 1, 1)
	if !ok {
		return ExitUsage
	}
	set := 0
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "in" || f.Name == "at" || f.Name == "clear" {
			set++
		}
	})
	if set != 1 {
		return e.usageErr("expire: give one of -in, -at or -clear")
	}
	var t time.Time
	if *at != "" {
		var err error
		if t, err = time.Parse(time.RFC3339Nano, *at); err != nil {
			return e.usageErr("expire: -at needs an RFC 3339 time such as 2026-10-18T12:00:00Z")
		}
	}
	err := e.write(func(s *mv.Store) error {
		switch {
		case *clear:
			return s.Expire(pos[0], time.Time{})
		case *at != "":
			return s.Expire(pos[0], t)
		}
		return s.ExpireIn(pos[0], *in)
	})
	if err != nil {
		return e.fail(err)
	}
	return e.okLine("", nil)
}

// factList collects -fact and -unset in the order given.
type factList struct{ facts *[]mv.Fact }

func (f factList) String() string { return "" }

type setFact struct{ factList }

func (f setFact) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("-fact needs key=value")
	}
	*f.facts = append(*f.facts, mv.Fact{Key: k, Value: []byte(val)})
	return nil
}

type unsetFact struct{ factList }

func (f unsetFact) Set(v string) error {
	*f.facts = append(*f.facts, mv.Fact{Key: v, Delete: true})
	return nil
}

type likList map[string]float64

func (l likList) String() string { return "" }
func (l likList) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("-l needs name=likelihood")
	}
	x, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return fmt.Errorf("-l %s: %q is not a number", k, val)
	}
	l[k] = x
	return nil
}

func cmdObserve(e *env, args []string) int {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	var facts []mv.Fact
	lik := likList{}
	fs.Var(setFact{factList{&facts}}, "fact", "key=value")
	fs.Var(unsetFact{factList{&facts}}, "unset", "key")
	fs.Var(lik, "l", "name=likelihood")
	rest := fs.String("rest", "", "likelihood for live branches not given with -l")
	prune := fs.Float64("prune", 0, "kill branches below this probability")
	if _, ok := e.parse(fs, args, 0, 0); !ok {
		return ExitUsage
	}
	ev := mv.Evidence{Facts: facts, Likelihoods: lik, PruneBelow: *prune}
	if *rest != "" {
		x, err := strconv.ParseFloat(*rest, 64)
		if err != nil {
			return e.usageErr("observe: -rest " + strconv.Quote(*rest) + " is not a number")
		}
		ev.Rest = &x
	}
	var out *mv.Outcome
	err := e.write(func(s *mv.Store) error {
		var err error
		out, err = s.Observe(ev)
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	return e.outcome(out)
}

func cmdSweep(e *env, args []string) int {
	if _, ok := e.parse(flag.NewFlagSet("sweep", flag.ContinueOnError), args, 0, 0); !ok {
		return ExitUsage
	}
	var out *mv.Outcome
	err := e.write(func(s *mv.Store) error {
		var err error
		out, err = s.Sweep()
		return err
	})
	if err != nil {
		return e.fail(err)
	}
	return e.outcome(out)
}

func (e *env) outcome(out *mv.Outcome) int {
	if e.json {
		killed := []map[string]any{}
		for _, d := range out.Killed {
			killed = append(killed, death(d))
		}
		live := []map[string]any{}
		for _, i := range out.Live {
			live = append(live, branchJSON(i))
		}
		v := map[string]any{"ok": true, "killed": killed, "live": live, "settled": nil}
		if name, ok := out.Settled(); ok {
			v["settled"] = name
		}
		e.emit(v)
		return ExitOK
	}
	for _, d := range out.Killed {
		fmt.Fprintf(e.stdout, "killed %s: %s\n", d.Branch, why(d.Cause, d.Detail))
	}
	e.table(out.Live)
	if name, ok := out.Settled(); ok {
		fmt.Fprintf(e.stdout, "settled: %s is the only live branch\n", name)
	}
	return ExitOK
}

func cmdDiscard(e *env, args []string) int {
	pos, ok := e.parse(flag.NewFlagSet("discard", flag.ContinueOnError), args, 1, 1)
	if !ok {
		return ExitUsage
	}
	if err := e.write(func(s *mv.Store) error { return s.Discard(pos[0]) }); err != nil {
		return e.fail(err)
	}
	return e.okLine("", nil)
}

func cmdPromote(e *env, args []string) int {
	pos, ok := e.parse(flag.NewFlagSet("promote", flag.ContinueOnError), args, 1, 1)
	if !ok {
		return ExitUsage
	}
	var gen uint64
	err := e.write(func(s *mv.Store) error {
		if err := s.Promote(pos[0]); err != nil {
			return err
		}
		gen = s.Generation()
		return nil
	})
	if err != nil {
		return e.fail(err)
	}
	return e.okLine(fmt.Sprintf("promoted %s: generation %d", pos[0], gen), map[string]any{"promoted": pos[0], "generation": gen})
}

func cmdBranches(e *env, args []string) int {
	fs := flag.NewFlagSet("branches", flag.ContinueOnError)
	liveOnly := fs.Bool("live", false, "live branches only")
	if _, ok := e.parse(fs, args, 0, 0); !ok {
		return ExitUsage
	}
	s, err := e.read()
	if err != nil {
		return e.fail(err)
	}
	all := s.Branches()
	if *liveOnly {
		all = s.Live()
	}
	if e.json {
		out := []map[string]any{}
		for _, i := range all {
			out = append(out, branchJSON(i))
		}
		e.emit(out)
		return ExitOK
	}
	if len(all) == 0 {
		fmt.Fprintf(e.stdout, "no branches (generation %d)\n", s.Generation())
		return ExitOK
	}
	e.table(all)
	return ExitOK
}

func cmdBest(e *env, args []string) int {
	if _, ok := e.parse(flag.NewFlagSet("best", flag.ContinueOnError), args, 0, 0); !ok {
		return ExitUsage
	}
	s, err := e.read()
	if err != nil {
		return e.fail(err)
	}
	b, ok := s.Best()
	if e.json {
		if !ok {
			e.emit(map[string]any{"best": nil})
			return ExitNotFound
		}
		e.emit(map[string]any{"best": branchJSON(b)})
		return ExitOK
	}
	if !ok {
		fmt.Fprintln(e.stderr, "no live branches")
		return ExitNotFound
	}
	fmt.Fprintln(e.stdout, b.Name)
	return ExitOK
}

func cmdHistory(e *env, args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	n := fs.Int("n", 50, "how many events")
	if _, ok := e.parse(fs, args, 0, 0); !ok {
		return ExitUsage
	}
	s, err := e.read()
	if err != nil {
		return e.fail(err)
	}
	h := s.History()
	if *n >= 0 && len(h) > *n {
		h = h[len(h)-*n:]
	}
	if e.json {
		out := []map[string]any{}
		for _, ev := range h {
			out = append(out, map[string]any{"seq": ev.Seq, "time": ev.Time.Format(time.RFC3339Nano),
				"generation": ev.Generation, "kind": ev.Kind, "branch": ev.Branch, "detail": ev.Detail})
		}
		e.emit(out)
		return ExitOK
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	for _, ev := range h {
		b := ev.Branch
		if b == "" {
			b = "-"
		}
		fmt.Fprintf(tw, "%s\tgen %d\t%s\t%s\t%s\n", ev.Time.Format(time.RFC3339), ev.Generation, ev.Kind, b, ev.Detail)
	}
	tw.Flush()
	return ExitOK
}

// putValue adds a value to JSON output: as a string when it is UTF-8, and
// as base64 otherwise, so no byte is lost.
func putValue(out map[string]any, v []byte) {
	if utf8.Valid(v) {
		out["value"] = string(v)
	} else {
		out["value_base64"] = base64.StdEncoding.EncodeToString(v)
	}
}

// textValue prints a value on one line: as it is, or Go-quoted if it holds a
// tab, a newline or anything else that would make the output ambiguous.
func textValue(v []byte) string {
	for _, r := range string(v) {
		if r == '\t' || r == '\n' || r == '\r' || r == utf8.RuneError || !strconv.IsPrint(r) {
			return strconv.Quote(string(v))
		}
	}
	if len(v) > 0 && v[0] == '"' {
		return strconv.Quote(string(v))
	}
	return string(v)
}

func why(c mv.Cause, detail string) string {
	if detail == "" {
		return c.String()
	}
	return c.String() + " (" + detail + ")"
}

func death(d mv.Death) map[string]any {
	return map[string]any{"branch": d.Branch, "cause": d.Cause.String(), "detail": d.Detail}
}

func branchJSON(i mv.BranchInfo) map[string]any {
	cons := []string{}
	for _, c := range i.Constraints {
		cons = append(cons, c.String())
	}
	v := map[string]any{
		"name": i.Name, "parent": i.Parent, "live": i.Live, "weight": i.Weight,
		"probability": i.Probability, "constraints": cons, "deadline": nil,
		"created": i.Created.Format(time.RFC3339Nano), "stale": i.Stale, "keys": i.Keys,
	}
	if !i.Deadline.IsZero() {
		v["deadline"] = i.Deadline.Format(time.RFC3339Nano)
	}
	if !i.Live {
		v["cause"] = i.Cause.String()
		v["detail"] = i.Detail
		v["died"] = i.Died.Format(time.RFC3339Nano)
	}
	return v
}

func (e *env) table(rows []mv.BranchInfo) {
	if len(rows) == 0 {
		fmt.Fprintln(e.stdout, "no live branches")
		return
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BRANCH\tPROB\tSTATUS\tFROM\tDEADLINE\tCONSTRAINTS")
	for _, i := range rows {
		prob, status := "-", "live"
		if i.Live {
			prob = strconv.FormatFloat(100*i.Probability, 'f', 1, 64) + "%"
			if i.Stale {
				status = "live, stale"
			}
		} else {
			status = why(i.Cause, i.Detail)
		}
		dl := "-"
		if !i.Deadline.IsZero() {
			dl = i.Deadline.Format(time.RFC3339)
		}
		cons := make([]string, len(i.Constraints))
		for k, c := range i.Constraints {
			cons[k] = c.String()
		}
		sort.Strings(cons)
		cs := strings.Join(cons, "; ")
		if cs == "" {
			cs = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", i.Name, prob, status, i.Parent, dl, cs)
	}
	tw.Flush()
}
