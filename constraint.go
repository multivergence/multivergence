// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

package multivergence

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Op is a constraint's comparison.
type Op uint8

const (
	// Eq: the key holds exactly Value.
	Eq Op = iota + 1
	// Ne: the key is missing or holds something other than Value.
	Ne
	// Lt, Le, Gt, Ge compare the key's value as a number with Value. A
	// missing key or one that isn't a number breaks the constraint.
	Lt
	Le
	Gt
	Ge
	// Exists: the key is present. Value is unused.
	Exists
	// Absent: the key is missing. Value is unused.
	Absent
)

var opNames = map[Op]string{Eq: "==", Ne: "!=", Lt: "<", Le: "<=", Gt: ">", Ge: ">=", Exists: "exists", Absent: "absent"}

func (o Op) String() string {
	if s, ok := opNames[o]; ok {
		return s
	}
	return fmt.Sprintf("op(%d)", uint8(o))
}

func (o Op) numeric() bool { return o == Lt || o == Le || o == Gt || o == Ge }

// Constraint is a condition on one key of a branch. While it holds the
// branch stays live; the moment it doesn't, the branch dies.
type Constraint struct {
	Key   string
	Op    Op
	Value string
}

// Helpers for building constraints.
func Equals(key, value string) Constraint    { return Constraint{key, Eq, value} }
func NotEquals(key, value string) Constraint { return Constraint{key, Ne, value} }
func Below(key string, n float64) Constraint { return Constraint{key, Lt, fmtNum(n)} }
func AtMost(key string, n float64) Constraint {
	return Constraint{key, Le, fmtNum(n)}
}
func Above(key string, n float64) Constraint { return Constraint{key, Gt, fmtNum(n)} }
func AtLeast(key string, n float64) Constraint {
	return Constraint{key, Ge, fmtNum(n)}
}
func Present(key string) Constraint { return Constraint{Key: key, Op: Exists} }
func Missing(key string) Constraint { return Constraint{Key: key, Op: Absent} }

func fmtNum(n float64) string { return strconv.FormatFloat(n, 'g', -1, 64) }

func (c Constraint) validate() error {
	if err := checkKey(c.Key); err != nil {
		return fmt.Errorf("%w: %v", ErrBadConstraint, err)
	}
	if _, ok := opNames[c.Op]; !ok {
		return fmt.Errorf("%w: unknown operator %d", ErrBadConstraint, uint8(c.Op))
	}
	if (c.Op == Exists || c.Op == Absent) && c.Value != "" {
		return fmt.Errorf("%w: %s takes no value", ErrBadConstraint, c.Op)
	}
	if c.Op.numeric() {
		if _, ok := parseNum([]byte(c.Value)); !ok {
			return fmt.Errorf("%w: %q is not a number", ErrBadConstraint, c.Value)
		}
	}
	if len(c.Value) > MaxValueLen {
		return fmt.Errorf("%w: value too long", ErrBadConstraint)
	}
	return nil
}

func parseNum(b []byte) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// holds reports whether the constraint holds for a key that has value v
// (present says whether the key exists at all).
func (c Constraint) holds(v []byte, present bool) bool {
	switch c.Op {
	case Exists:
		return present
	case Absent:
		return !present
	case Eq:
		return present && bytes.Equal(v, []byte(c.Value))
	case Ne:
		return !present || !bytes.Equal(v, []byte(c.Value))
	}
	if !present {
		return false
	}
	x, ok := parseNum(v)
	if !ok {
		return false
	}
	y, _ := parseNum([]byte(c.Value))
	switch c.Op {
	case Lt:
		return x < y
	case Le:
		return x <= y
	case Gt:
		return x > y
	case Ge:
		return x >= y
	}
	return false
}

// String writes the constraint the way ParseConstraint reads it.
func (c Constraint) String() string {
	key := c.Key
	if strings.ContainsAny(key, " \t\n\r\"") {
		key = strconv.Quote(key)
	}
	if c.Op == Exists || c.Op == Absent {
		return key + " " + c.Op.String()
	}
	v := c.Value
	if v == "" || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\"\n\r\t") || !utf8.ValidString(v) {
		v = strconv.Quote(v)
	}
	return key + " " + c.Op.String() + " " + v
}

// ParseConstraint reads a constraint written as "key op value", such as
// "quote/flight <= 400" or "seat/hold == confirmed", or as "key exists" or
// "key absent". Put the key in double quotes if it contains spaces or
// quotes; String does that. The value is everything after
// the operator, with the spaces around it trimmed; put it in double quotes
// to keep spaces or to write an empty value.
func ParseConstraint(s string) (Constraint, error) {
	s = strings.TrimSpace(s)
	key, rest, ok := cut(s)
	if strings.HasPrefix(s, "\"") {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			return Constraint{}, fmt.Errorf("%w: bad quoted key in %q", ErrBadConstraint, s)
		}
		key, _ = strconv.Unquote(q)
		rest = strings.TrimSpace(s[len(q):])
		ok = rest != ""
	}
	if !ok {
		return Constraint{}, fmt.Errorf("%w: %q needs an operator", ErrBadConstraint, s)
	}
	opName, value, _ := cut(rest)
	var op Op
	for o, name := range opNames {
		if name == opName {
			op = o
		}
	}
	if op == 0 {
		return Constraint{}, fmt.Errorf("%w: unknown operator %q in %q", ErrBadConstraint, opName, s)
	}
	if op == Exists || op == Absent {
		if value != "" {
			return Constraint{}, fmt.Errorf("%w: %s takes no value", ErrBadConstraint, op)
		}
	} else {
		if value == "" {
			return Constraint{}, fmt.Errorf("%w: %q needs a value", ErrBadConstraint, s)
		}
		if strings.HasPrefix(value, "\"") {
			u, err := strconv.Unquote(value)
			if err != nil {
				return Constraint{}, fmt.Errorf("%w: bad quoted value %s", ErrBadConstraint, value)
			}
			value = u
		}
	}
	c := Constraint{Key: key, Op: op, Value: value}
	if err := c.validate(); err != nil {
		return Constraint{}, err
	}
	return c, nil
}

// cut splits off the first space-separated word and trims what follows.
func cut(s string) (string, string, bool) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, "", false
	}
	return s[:i], strings.TrimSpace(s[i+1:]), true
}
