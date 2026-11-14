package sieve

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// How strings are compared (RFC 5228 2.7): a comparator says when two
// strings are equal and which is larger, a match type says what of a
// string has to be equal.

// foldASCII is i;ascii-casemap's view of a string (RFC 4790 9.2): the
// letters a to z as capitals, everything else as it is.
func foldASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'a' && c <= 'z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'a' && b[j] <= 'z' {
					b[j] -= 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// numeric is i;ascii-numeric's view (RFC 4790 9.1): the digits a string
// starts with, as a number; a string that starts with none is larger than
// every number and equal to every other such string.
func numeric(s string) (n uint64, isNumber bool) {
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	if i == 0 {
		return 0, false
	}
	digits := strings.TrimLeft(s[:i], "0")
	if len(digits) > 19 {
		return 1<<64 - 1, true
	}
	n, _ = strconv.ParseUint("0"+digits, 10, 64)
	return n, true
}

// compare orders two strings under a comparator: -1, 0 or 1.
func compare(comparator, a, b string) int {
	switch comparator {
	case "i;ascii-numeric":
		x, xn := numeric(a)
		y, yn := numeric(b)
		switch {
		case !xn && !yn:
			return 0
		case !xn:
			return 1
		case !yn:
			return -1
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case "i;ascii-casemap":
		a, b = foldASCII(a), foldASCII(b)
	}
	return strings.Compare(a, b)
}

func relate(relation string, cmp int) bool {
	switch relation {
	case "gt":
		return cmp > 0
	case "ge":
		return cmp >= 0
	case "lt":
		return cmp < 0
	case "le":
		return cmp <= 0
	case "eq":
		return cmp == 0
	case "ne":
		return cmp != 0
	}
	return false
}

// maxGlobWork bounds one :matches: the steps of the matcher, which without
// a bound are the product of the two lengths.
const maxGlobWork = 1 << 20

// glob matches value against a :matches pattern (RFC 5228 2.7.1): "*" is
// any run of characters, "?" is one character, a backslash makes the next
// character ordinary. It returns the parts of value the wildcards matched,
// in their order, each "*" as short as it can be (RFC 5229 3.2). The
// strings are compared as given: the caller folds case where the
// comparator does, and takes the matched parts from the original by their
// offsets.
func glob(pattern, value string) (spans [][2]int, ok bool) {
	spans, ok, _ = globWork(pattern, value, maxGlobWork)
	return spans, ok
}

// globWork is glob with a bound on the steps of the matcher, at most
// maxGlobWork, and returns how many it took. A match that would take more
// is no match.
func globWork(pattern, value string, limit int) (spans [][2]int, ok bool, work int) {
	limit = min(limit, maxGlobWork)
	// The pattern as elements: literal runs, "?" and "*".
	type elem struct {
		kind byte // 'l', '?', '*'
		lit  string
	}
	var elems []elem
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			elems = append(elems, elem{'l', lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case c == '\\' && i+1 < len(pattern):
			i++
			lit.WriteByte(pattern[i])
		case c == '*':
			flush()
			// Every star stands for itself, also one after another:
			// "**" gives two match variables (RFC 5229 3.2).
			elems = append(elems, elem{'*', ""})
		case c == '?':
			flush()
			elems = append(elems, elem{'?', ""})
		default:
			lit.WriteByte(c)
		}
	}
	flush()

	spans = make([][2]int, 0, 4)
	// match tries elems[e:] at value[pos:]. Each star first takes nothing
	// and grows only when the rest does not fit: the shortest match.
	var match func(e, pos int) bool
	match = func(e, pos int) bool {
		for e < len(elems) {
			if work++; work > limit {
				return false
			}
			el := elems[e]
			switch el.kind {
			case 'l':
				if !strings.HasPrefix(value[pos:], el.lit) {
					return false
				}
				pos += len(el.lit)
			case '?':
				if pos >= len(value) {
					return false
				}
				_, size := utf8.DecodeRuneInString(value[pos:])
				spans = append(spans, [2]int{pos, pos + size})
				pos += size
			case '*':
				if e == len(elems)-1 {
					spans = append(spans, [2]int{pos, len(value)})
					return true
				}
				mark := len(spans)
				for end := pos; end <= len(value); {
					spans = append(spans[:mark], [2]int{pos, end})
					if match(e+1, end) {
						return true
					}
					if work > limit || end == len(value) {
						break
					}
					_, size := utf8.DecodeRuneInString(value[end:])
					end += size
				}
				spans = spans[:mark]
				return false
			}
			e++
		}
		return pos == len(value)
	}
	if !match(0, 0) {
		return nil, false, work
	}
	return spans, true, work
}

// matcher is the match type and comparator of one test.
type matcher struct {
	match      string
	relation   string
	comparator string
	// spent, when set, is where the cost of every comparison is added, in
	// the units of maxWork (run.go).
	spent *int64
}

// globStepCost is what one step of the :matches matcher costs, in the
// units of maxWork: about as much as comparing this many octets.
const globStepCost = 16

func (m matcher) charge(n int) {
	if m.spent != nil {
		*m.spent += int64(n)
	}
}

// over reports whether the run has spent what it may.
func (m matcher) over() bool { return m.spent != nil && *m.spent > maxWork }

// one reports whether a value matches a key. For :matches it also returns
// the match variables: the whole value, then what each wildcard matched.
func (m matcher) one(value, key string) (bool, []string) {
	m.charge(len(value) + len(key) + 1)
	switch m.match {
	case "is":
		return compare(m.comparator, value, key) == 0, nil
	case "contains":
		if m.comparator == "i;ascii-casemap" {
			return strings.Contains(foldASCII(value), foldASCII(key)), nil
		}
		return strings.Contains(value, key), nil
	case "matches":
		v, k := value, key
		if m.comparator == "i;ascii-casemap" {
			v, k = foldASCII(value), foldASCII(key)
		}
		limit := maxGlobWork
		if m.spent != nil {
			limit = int(max(maxWork-*m.spent, 0)/globStepCost) + 1
		}
		spans, ok, work := globWork(k, v, limit)
		m.charge(work * globStepCost)
		if !ok {
			return false, nil
		}
		vars := []string{value}
		for _, s := range spans {
			vars = append(vars, value[s[0]:s[1]])
		}
		return true, vars
	case "value":
		return relate(m.relation, compare(m.comparator, value, key)), nil
	}
	return false, nil
}

// any reports whether one of the values matches one of the keys (RFC 5228
// 2.7: a test is true if any combination is). With :count it compares the
// number of values with the keys instead (RFC 5231 4.2).
func (m matcher) any(values, keys []string) (bool, []string) {
	if m.match == "count" {
		n := strconv.Itoa(len(values))
		for _, key := range keys {
			if relate(m.relation, compare(m.comparator, n, key)) {
				return true, nil
			}
		}
		return false, nil
	}
	for _, v := range values {
		for _, k := range keys {
			if m.over() {
				return false, nil
			}
			if ok, vars := m.one(v, k); ok {
				return true, vars
			}
		}
	}
	return false, nil
}
