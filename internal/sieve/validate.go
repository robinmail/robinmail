package sieve

import (
	"strings"
)

// Parse reads a script and checks it: its syntax (RFC 5228 8), that
// "require" comes first and names only what this implementation has, that
// every command and test exists, was required where it is an extension,
// and has the arguments it takes. A script that parses will run.
func Parse(src []byte) (*Script, error) {
	nodes, err := parseSyntax(src)
	if err != nil {
		return nil, err
	}
	s := &Script{req: map[string]bool{}}
	// RFC 5228 3.2: require commands come before any other command.
	body := nodes
	for len(body) > 0 && body[0].name == "require" {
		n := body[0]
		if len(n.args) != 1 || !n.args[0].isStrings() || len(n.tests) > 0 || n.hasBlock {
			return nil, errAt(n.line, "require takes one list of strings")
		}
		for _, name := range n.args[0].strings() {
			if !knownExtension(name) {
				return nil, errAt(n.line, "the extension %q is not available", name)
			}
			if !s.req[name] {
				s.req[name] = true
				s.Requires = append(s.Requires, name)
			}
		}
		body = body[1:]
	}
	cmds, verr := s.block(body, true)
	if verr != nil {
		return nil, verr
	}
	s.cmds = cmds
	return s, nil
}

func (s *Script) needs(line int, ext, what string) *Error {
	if !s.req[ext] {
		return errAt(line, "%s needs require %q", what, ext)
	}
	return nil
}

func (s *Script) block(nodes []*node, top bool) ([]*command, *Error) {
	var out []*command
	for i, n := range nodes {
		c := &command{name: n.name, line: n.line}
		switch n.name {
		case "require":
			return nil, errAt(n.line, "require must come before every other command")
		case "if", "elsif":
			if n.name == "elsif" && (i == 0 || (nodes[i-1].name != "if" && nodes[i-1].name != "elsif")) {
				return nil, errAt(n.line, "elsif without an if before it")
			}
			if len(n.args) > 0 || len(n.tests) != 1 || n.list || !n.hasBlock {
				return nil, errAt(n.line, "%s takes one test and a block", n.name)
			}
			t, err := s.test(n.tests[0])
			if err != nil {
				return nil, err
			}
			c.test = t
		case "else":
			if i == 0 || (nodes[i-1].name != "if" && nodes[i-1].name != "elsif") {
				return nil, errAt(n.line, "else without an if before it")
			}
			if len(n.args) > 0 || len(n.tests) > 0 || !n.hasBlock {
				return nil, errAt(n.line, "else takes a block and nothing else")
			}
		default:
			if n.hasBlock || len(n.tests) > 0 {
				return nil, errAt(n.line, "%q takes no test and no block", n.name)
			}
			if err := s.action(n, c); err != nil {
				return nil, err
			}
		}
		if n.hasBlock {
			block, err := s.block(n.block, false)
			if err != nil {
				return nil, err
			}
			c.block = block
		}
		out = append(out, c)
	}
	return out, nil
}

// args walks the arguments of a command or test: tags first, in any
// order, then the positional ones.
type args struct {
	n    *node
	what string
	i    int
}

// tag returns the next argument if it is a tag, and moves past it.
func (a *args) tag() (string, bool) {
	if a.i < len(a.n.args) && a.n.args[a.i].kind == tokTag {
		a.i++
		return a.n.args[a.i-1].text, true
	}
	return "", false
}

func (a *args) str(what string) (string, *Error) {
	if a.i >= len(a.n.args) || a.n.args[a.i].kind != tokString {
		return "", errAt(a.n.line, "%s: expected %s, a string", a.what, what)
	}
	a.i++
	return a.n.args[a.i-1].text, nil
}

func (a *args) list(what string) ([]string, *Error) {
	if a.i >= len(a.n.args) || !a.n.args[a.i].isStrings() {
		return nil, errAt(a.n.line, "%s: expected %s, a string or a list of strings", a.what, what)
	}
	a.i++
	return a.n.args[a.i-1].strings(), nil
}

func (a *args) number(what string) (int64, *Error) {
	if a.i >= len(a.n.args) || a.n.args[a.i].kind != tokNumber {
		return 0, errAt(a.n.line, "%s: expected %s, a number", a.what, what)
	}
	a.i++
	return a.n.args[a.i-1].num, nil
}

func (a *args) end() *Error {
	if a.i < len(a.n.args) {
		return errAt(a.n.args[a.i].line, "%s: too many arguments", a.what)
	}
	return nil
}

func (a *args) bad(tag string) *Error {
	return errAt(a.n.line, "%s does not take :%s", a.what, tag)
}

var setModifiers = map[string]int{ // RFC 5229 4.1: the precedence of each
	"lower": 40, "upper": 40, "lowerfirst": 30, "upperfirst": 30, "quotewildcard": 20, "length": 10,
}

func (s *Script) action(n *node, c *command) *Error {
	a := &args{n: n, what: n.name}
	switch n.name {
	case "stop", "discard":
		return a.end()
	case "keep", "fileinto":
		if n.name == "fileinto" {
			if err := s.needs(n.line, "fileinto", "fileinto"); err != nil {
				return err
			}
		}
		for {
			tag, ok := a.tag()
			if !ok {
				break
			}
			switch {
			case tag == "flags":
				if err := s.needs(n.line, "imap4flags", ":flags"); err != nil {
					return err
				}
				flags, err := a.list("the flags")
				if err != nil {
					return err
				}
				c.flags, c.hasFlags = flags, true
			case tag == "copy" && n.name == "fileinto":
				if err := s.needs(n.line, "copy", ":copy"); err != nil {
					return err
				}
				c.copy = true
			case tag == "create" && n.name == "fileinto":
				if err := s.needs(n.line, "mailbox", ":create"); err != nil {
					return err
				}
				c.create = true
			default:
				return a.bad(tag)
			}
		}
		if n.name == "fileinto" {
			var err *Error
			if c.str, err = a.str("the mailbox"); err != nil {
				return err
			}
		}
		return a.end()
	case "redirect":
		for {
			tag, ok := a.tag()
			if !ok {
				break
			}
			if tag != "copy" {
				return a.bad(tag)
			}
			if err := s.needs(n.line, "copy", ":copy"); err != nil {
				return err
			}
			c.copy = true
		}
		var err *Error
		if c.str, err = a.str("the address"); err != nil {
			return err
		}
		if !(s.usesVariables() && hasVariable(c.str)) && !validAddress(c.str) {
			return errAt(n.line, "redirect: %q is not an address", c.str)
		}
		return a.end()
	case "reject", "ereject":
		if err := s.needs(n.line, n.name, n.name); err != nil {
			return err
		}
		var err *Error
		if c.str, err = a.str("the reason"); err != nil {
			return err
		}
		return a.end()
	case "setflag", "addflag", "removeflag":
		if err := s.needs(n.line, "imap4flags", n.name); err != nil {
			return err
		}
		first, err := a.list("the flags")
		if err != nil {
			return err
		}
		if a.i < len(n.args) {
			// Two arguments: the first is the variable that holds the flags.
			if n.args[0].kind != tokString {
				return errAt(n.line, "%s: the variable is one string", n.name)
			}
			if err := s.needs(n.line, "variables", "a flag variable"); err != nil {
				return err
			}
			if !validVariableName(first[0]) {
				return errAt(n.line, "%s: %q is not a variable name", n.name, first[0])
			}
			c.variable = strings.ToLower(first[0])
			if first, err = a.list("the flags"); err != nil {
				return err
			}
		}
		c.flags = first
		return a.end()
	case "set":
		if err := s.needs(n.line, "variables", "set"); err != nil {
			return err
		}
		seen := map[int]bool{}
		for {
			tag, ok := a.tag()
			if !ok {
				break
			}
			prec, known := setModifiers[tag]
			if !known {
				return a.bad(tag)
			}
			// RFC 5229 4.1: two modifiers of the same precedence are an error.
			if seen[prec] {
				return errAt(n.line, "set: :%s cannot be combined with another modifier of its kind", tag)
			}
			seen[prec] = true
			c.modifiers = append(c.modifiers, tag)
		}
		// Applied from the highest precedence down.
		for i := 1; i < len(c.modifiers); i++ {
			for j := i; j > 0 && setModifiers[c.modifiers[j]] > setModifiers[c.modifiers[j-1]]; j-- {
				c.modifiers[j], c.modifiers[j-1] = c.modifiers[j-1], c.modifiers[j]
			}
		}
		name, err := a.str("the variable")
		if err != nil {
			return err
		}
		if !validVariableName(name) || isDigits(name) {
			return errAt(n.line, "set: %q is not a variable name", name)
		}
		c.variable = strings.ToLower(name)
		if c.str, err = a.str("the value"); err != nil {
			return err
		}
		return a.end()
	case "vacation":
		if err := s.needs(n.line, "vacation", "vacation"); err != nil {
			return err
		}
		v := &vacationArgs{}
		for {
			tag, ok := a.tag()
			if !ok {
				break
			}
			var err *Error
			switch tag {
			case "days":
				v.days, err = a.number("the days")
				v.hasDays = true
			case "subject":
				v.subject, err = a.str("the subject")
				v.hasSubj = true
			case "from":
				if v.from, err = a.str("the address"); err == nil && !(s.usesVariables() && hasVariable(v.from)) && !validFrom(v.from) {
					err = errAt(n.line, "vacation: :from %q has no address", v.from)
				}
			case "addresses":
				v.addresses, err = a.list("the addresses")
			case "mime":
				v.mime = true
			case "handle":
				v.handle, err = a.str("the handle")
				v.hasHandle = true
			default:
				return a.bad(tag)
			}
			if err != nil {
				return err
			}
		}
		var err *Error
		if v.reason, err = a.str("the reason"); err != nil {
			return err
		}
		c.vacation = v
		return a.end()
	}
	return errAt(n.line, "unknown command %q", n.name)
}

var (
	matchTypes   = map[string]bool{"is": true, "contains": true, "matches": true}
	relations    = map[string]bool{"gt": true, "ge": true, "lt": true, "le": true, "eq": true, "ne": true}
	addressParts = map[string]string{"all": "", "localpart": "", "domain": "", "user": "subaddress", "detail": "subaddress"}
	comparators  = map[string]string{"i;ascii-casemap": "", "i;octet": "", "i;ascii-numeric": "comparator-i;ascii-numeric"}
	dateParts    = map[string]bool{"year": true, "month": true, "day": true, "date": true, "julian": true, "hour": true,
		"minute": true, "second": true, "time": true, "iso8601": true, "std11": true, "zone": true, "weekday": true}
)

func isAddressPart(tag string) bool {
	_, ok := addressParts[tag]
	return ok
}

// options reads the tagged arguments of a test: match type, comparator,
// and whatever of address part, :index, the body transforms and the date
// zones the test allows.
func (s *Script) options(a *args, t *test, allow string) *Error {
	n := a.n
	t.match, t.comparator, t.part, t.transform = "is", "i;ascii-casemap", "all", "text"
	seenMatch, seenIndex := false, false
	for {
		tag, ok := a.tag()
		if !ok {
			break
		}
		var err *Error
		switch {
		case matchTypes[tag]:
			if seenMatch {
				return errAt(n.line, "%s: more than one match type", n.name)
			}
			t.match, seenMatch = tag, true
		case tag == "count" || tag == "value":
			if seenMatch {
				return errAt(n.line, "%s: more than one match type", n.name)
			}
			if err := s.needs(n.line, "relational", ":"+tag); err != nil {
				return err
			}
			t.match, seenMatch = tag, true
			if t.relation, err = a.str("the relation"); err != nil {
				return err
			}
			if t.relation = strings.ToLower(t.relation); !relations[t.relation] {
				return errAt(n.line, "%s: %q is not a relation (gt, ge, lt, le, eq, ne)", n.name, t.relation)
			}
		case tag == "comparator":
			if t.comparator, err = a.str("the comparator"); err != nil {
				return err
			}
			ext, known := comparators[t.comparator]
			if !known {
				return errAt(n.line, "%s: the comparator %q is not available", n.name, t.comparator)
			}
			if ext != "" {
				if err := s.needs(n.line, ext, "the comparator "+t.comparator); err != nil {
					return err
				}
			}
		case isAddressPart(tag):
			if !strings.Contains(allow, "part") {
				return a.bad(tag)
			}
			if ext := addressParts[tag]; ext != "" {
				if err := s.needs(n.line, ext, ":"+tag); err != nil {
					return err
				}
			}
			t.part = tag
		case tag == "index" && strings.Contains(allow, "index"):
			if err := s.needs(n.line, "index", ":index"); err != nil {
				return err
			}
			if t.index, err = a.number("the index"); err != nil {
				return err
			}
			if t.index < 1 {
				return errAt(n.line, "%s: an index starts at 1", n.name)
			}
			seenIndex = true
		case tag == "last" && strings.Contains(allow, "index"):
			t.last = true
		case (tag == "raw" || tag == "text") && strings.Contains(allow, "body"):
			t.transform = tag
		case tag == "content" && strings.Contains(allow, "body"):
			t.transform = tag
			if t.contentTypes, err = a.list("the content types"); err != nil {
				return err
			}
		case tag == "zone" && strings.Contains(allow, "zone"):
			if t.zone, err = a.str("the zone"); err != nil {
				return err
			}
			if _, ok := parseZone(t.zone); !ok {
				return errAt(n.line, "%s: %q is not a time zone like +0100", n.name, t.zone)
			}
			if t.originalZone {
				return errAt(n.line, "%s: :zone and :originalzone together", n.name)
			}
		case tag == "originalzone" && strings.Contains(allow, "original"):
			if t.zone != "" {
				return errAt(n.line, "%s: :zone and :originalzone together", n.name)
			}
			t.originalZone = true
		default:
			return a.bad(tag)
		}
	}
	if t.last && !seenIndex {
		return errAt(n.line, "%s: :last without :index", n.name)
	}
	// RFC 5228 2.7.3: a match type the comparator cannot do is an error.
	if t.comparator == "i;ascii-numeric" && (t.match == "contains" || t.match == "matches") {
		return errAt(n.line, "%s: i;ascii-numeric has no :%s", n.name, t.match)
	}
	return nil
}

func (s *Script) headerNames(n *node, names []string) *Error {
	for _, name := range names {
		if !(s.usesVariables() && hasVariable(name)) && !validHeaderName(name) {
			return errAt(n.line, "%s: %q cannot be the name of a header field", n.name, name)
		}
	}
	return nil
}

func (s *Script) test(n *node) (*test, *Error) {
	t := &test{name: n.name, line: n.line}
	a := &args{n: n, what: n.name}
	if n.hasBlock {
		return nil, errAt(n.line, "a test has no block")
	}
	// Only allof, anyof and not have tests of their own.
	switch n.name {
	case "allof", "anyof":
		if len(n.args) > 0 || !n.list || len(n.tests) == 0 {
			return nil, errAt(n.line, "%s takes a list of tests in parentheses", n.name)
		}
	case "not":
		if len(n.args) > 0 || n.list || len(n.tests) != 1 {
			return nil, errAt(n.line, "not takes one test")
		}
	default:
		if len(n.tests) > 0 {
			return nil, errAt(n.line, "%q takes no test", n.name)
		}
	}
	for _, kid := range n.tests {
		k, err := s.test(kid)
		if err != nil {
			return nil, err
		}
		t.kids = append(t.kids, k)
	}

	var err *Error
	switch n.name {
	case "allof", "anyof", "not":
		return t, nil
	case "true", "false":
		return t, a.end()
	case "exists":
		if t.headers, err = a.list("the header names"); err != nil {
			return nil, err
		}
		if err := s.headerNames(n, t.headers); err != nil {
			return nil, err
		}
	case "size":
		tag, ok := a.tag()
		if !ok || (tag != "over" && tag != "under") {
			return nil, errAt(n.line, "size takes :over or :under and a number")
		}
		t.over = tag == "over"
		if t.limit, err = a.number("the size"); err != nil {
			return nil, err
		}
	case "header", "address":
		allow := "index"
		if n.name == "address" {
			allow = "index part"
		}
		if err := s.options(a, t, allow); err != nil {
			return nil, err
		}
		if t.headers, err = a.list("the header names"); err != nil {
			return nil, err
		}
		if err := s.headerNames(n, t.headers); err != nil {
			return nil, err
		}
		if t.keys, err = a.list("the keys"); err != nil {
			return nil, err
		}
	case "envelope":
		if err := s.needs(n.line, "envelope", "envelope"); err != nil {
			return nil, err
		}
		if err := s.options(a, t, "part"); err != nil {
			return nil, err
		}
		if t.headers, err = a.list("the envelope parts"); err != nil {
			return nil, err
		}
		for i, part := range t.headers {
			// RFC 5228 5.4: "from" and "to"; anything else is an error.
			if t.headers[i] = strings.ToLower(part); t.headers[i] != "from" && t.headers[i] != "to" {
				return nil, errAt(n.line, "envelope: %q is not a part of the envelope (from, to)", part)
			}
		}
		if t.keys, err = a.list("the keys"); err != nil {
			return nil, err
		}
	case "body":
		if err := s.needs(n.line, "body", "body"); err != nil {
			return nil, err
		}
		if err := s.options(a, t, "body"); err != nil {
			return nil, err
		}
		if t.keys, err = a.list("the keys"); err != nil {
			return nil, err
		}
	case "string":
		if err := s.needs(n.line, "variables", "string"); err != nil {
			return nil, err
		}
		if err := s.options(a, t, ""); err != nil {
			return nil, err
		}
		if t.sources, err = a.list("the strings"); err != nil {
			return nil, err
		}
		if t.keys, err = a.list("the keys"); err != nil {
			return nil, err
		}
	case "hasflag":
		if err := s.needs(n.line, "imap4flags", "hasflag"); err != nil {
			return nil, err
		}
		if err := s.options(a, t, ""); err != nil {
			return nil, err
		}
		if t.keys, err = a.list("the flags"); err != nil {
			return nil, err
		}
		if a.i < len(n.args) {
			// Two lists: the first names variables.
			if err := s.needs(n.line, "variables", "a flag variable"); err != nil {
				return nil, err
			}
			t.sources = t.keys
			for i, name := range t.sources {
				if !validVariableName(name) {
					return nil, errAt(n.line, "hasflag: %q is not a variable name", name)
				}
				t.sources[i] = strings.ToLower(name)
			}
			if t.keys, err = a.list("the flags"); err != nil {
				return nil, err
			}
		}
	case "mailboxexists":
		if err := s.needs(n.line, "mailbox", "mailboxexists"); err != nil {
			return nil, err
		}
		if t.keys, err = a.list("the mailbox names"); err != nil {
			return nil, err
		}
	case "date", "currentdate":
		if err := s.needs(n.line, "date", n.name); err != nil {
			return nil, err
		}
		allow := "zone"
		if n.name == "date" {
			allow = "zone original index"
		}
		if err := s.options(a, t, allow); err != nil {
			return nil, err
		}
		if n.name == "date" {
			name, err := a.str("the header name")
			if err != nil {
				return nil, err
			}
			t.headers = []string{name}
			if err := s.headerNames(n, t.headers); err != nil {
				return nil, err
			}
		}
		if t.datePart, err = a.str("the date part"); err != nil {
			return nil, err
		}
		if t.datePart = strings.ToLower(t.datePart); !dateParts[t.datePart] {
			return nil, errAt(n.line, "%s: %q is not a date part", n.name, t.datePart)
		}
		if t.keys, err = a.list("the keys"); err != nil {
			return nil, err
		}
	default:
		return nil, errAt(n.line, "unknown test %q", n.name)
	}
	return t, a.end()
}

// validVariableName reports whether s names a variable (RFC 5229 3): an
// identifier, or digits for a match variable. Namespaces are not supported.
func validVariableName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if isDigits(s) {
		return true
	}
	if !isAlpha(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isAlpha(s[i]) && !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

// validAddress reports whether s can be where a message is redirected to:
// one address of RFC 5321, without a name or brackets (RFC 5228 2.4.2.3).
func validAddress(s string) bool {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || len(s) > 254 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c == 0x7f || strings.IndexByte("<>(),;:\"[]\\", c) >= 0 && i > at {
			return false
		}
	}
	return !strings.ContainsAny(s[:at], " <>") && strings.Count(s[at:], "@") == 1
}

// validFrom reports whether a vacation :from has an address in it: it may
// be an address, or a name with the address in angle brackets.
func validFrom(s string) bool {
	if open := strings.LastIndexByte(s, '<'); open >= 0 && strings.HasSuffix(strings.TrimSpace(s), ">") {
		return validAddress(strings.TrimSuffix(strings.TrimSpace(s[open+1:]), ">"))
	}
	return validAddress(strings.TrimSpace(s))
}

// parseZone reads "+hhmm" or "-hhmm" as seconds east of UTC.
func parseZone(s string) (int, bool) {
	if len(s) != 5 || (s[0] != '+' && s[0] != '-') || !isDigits(s[1:]) {
		return 0, false
	}
	h, m := int(s[1]-'0')*10+int(s[2]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, false
	}
	secs := h*3600 + m*60
	if s[0] == '-' {
		secs = -secs
	}
	return secs, true
}
