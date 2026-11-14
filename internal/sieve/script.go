package sieve

import (
	"sort"
	"strings"
)

// Extensions are the capabilities a script may name in "require", sorted.
// The base language of RFC 5228 needs none.
var Extensions = []string{
	"body", "comparator-i;ascii-casemap", "comparator-i;ascii-numeric", "comparator-i;octet", "copy", "date",
	"envelope", "ereject", "fileinto", "imap4flags", "index", "mailbox", "reject", "relational", "subaddress",
	"vacation", "variables",
}

func knownExtension(name string) bool {
	i := sort.SearchStrings(Extensions, name)
	return i < len(Extensions) && Extensions[i] == name
}

// Script is a script that parsed and is valid: every command and test is
// known, required, and has the arguments it takes.
type Script struct {
	cmds []*command
	// Requires are the capabilities the script names, in its order.
	Requires []string
	req      map[string]bool
}

// command is one command, with its arguments read.
type command struct {
	name string
	line int
	// if, elsif: the test; if, elsif, else: the block.
	test  *test
	block []*command
	// fileinto: the mailbox; redirect: the address; reject, ereject: the
	// reason; set: the value.
	str string
	// keep, fileinto: :flags; setflag, addflag, removeflag: the flags.
	flags    []string
	hasFlags bool
	copy     bool // fileinto, redirect: :copy
	create   bool // fileinto: :create
	// set: the variable and the modifiers, in the order they apply;
	// setflag and friends: the variable that holds the flags, if any.
	variable  string
	modifiers []string
	vacation  *vacationArgs
}

// vacationArgs are the arguments of a vacation command (RFC 5230 4).
type vacationArgs struct {
	days      int64
	hasDays   bool
	subject   string
	hasSubj   bool
	from      string
	addresses []string
	mime      bool
	handle    string
	hasHandle bool
	reason    string
}

// test is one test, with its arguments read.
type test struct {
	name string
	line int
	kids []*test // allof, anyof, not

	// How strings are compared.
	match      string // "is", "contains", "matches", "count", "value"
	relation   string // for count and value: gt, ge, lt, le, eq, ne
	comparator string // "i;ascii-casemap", "i;octet", "i;ascii-numeric"
	part       string // address part: all, localpart, domain, user, detail

	headers []string // header, address, exists, date: field names; envelope: parts
	keys    []string
	// :index (RFC 5260 6): the n-th field of the name, from the end with last.
	index int64
	last  bool

	over  bool // size
	limit int64

	transform    string // body: raw, text, content
	contentTypes []string

	zone         string // date: "+hhmm", or "" for the local zone
	originalZone bool
	datePart     string

	sources []string // string: the strings looked in; hasflag: the variables
}

// usesVariables reports whether strings are subject to variable
// substitution (RFC 5229 3).
func (s *Script) usesVariables() bool { return s.req["variables"] }

// validHeaderName reports whether s can name a header field (RFC 5322
// 2.2: printable ASCII without the colon).
func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == ':' {
			return false
		}
	}
	return true
}

// hasVariable reports whether a string may be changed by substitution.
func hasVariable(s string) bool { return strings.Contains(s, "${") }
