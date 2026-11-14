// Package sieve reads and runs Sieve scripts: the mail filtering language
// of RFC 5228, with the extensions fileinto, envelope, body (RFC 5173),
// variables (RFC 5229), vacation (RFC 5230), relational (RFC 5231),
// imap4flags (RFC 5232), subaddress (RFC 5233), copy (RFC 3894), mailbox
// (RFC 5490), reject and ereject (RFC 5429), date and index (RFC 5260) and
// the comparator i;ascii-numeric.
//
// A script is somebody's text and a message is somebody else's: both are
// untrusted. Parse bounds the size, the nesting and the number of commands
// of a script; Run bounds the work one message costs and the number of
// actions it can cause. Nothing here touches a mailbox or sends mail: Run
// returns what the script wants done, and the caller does it.
package sieve

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on a script.
const (
	// MaxScriptSize is the largest script Parse reads, in octets.
	MaxScriptSize = 64 << 10
	// maxNesting is how deep blocks and tests may nest. RFC 5228 2.10.5
	// asks for at least 15 levels of blocks.
	maxNesting = 32
	// maxCommands is how many commands and tests a script may have.
	maxCommands = 5000
	// maxListItems is how many strings a string list may have.
	maxListItems = 1000
)

// Error is a script that cannot be used, with the line the fault is on.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

func errAt(line int, format string, args ...any) *Error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// Kinds of token.
const (
	tokEOF = iota
	tokIdent
	tokTag    // ":name", without the colon
	tokNumber // value in num
	tokString // value in text
	tokPunct  // one of [ ] ( ) { } , ;
)

type token struct {
	kind int
	text string
	num  int64
	line int
}

type lexer struct {
	src  string
	pos  int
	line int
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// skip passes white space and comments (RFC 5228 2.3 and 8.1).
func (l *lexer) skip() *Error {
	for l.pos < len(l.src) {
		switch c := l.src[l.pos]; {
		case c == '\n':
			l.line++
			l.pos++
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '/' && strings.HasPrefix(l.src[l.pos:], "/*"):
			end := strings.Index(l.src[l.pos+2:], "*/")
			if end < 0 {
				return errAt(l.line, "a comment that starts with /* is not closed")
			}
			l.line += strings.Count(l.src[l.pos:l.pos+2+end], "\n")
			l.pos += end + 4
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) next() (token, *Error) {
	if err := l.skip(); err != nil {
		return token{}, err
	}
	t := token{line: l.line}
	if l.pos >= len(l.src) {
		return t, nil
	}
	c := l.src[l.pos]
	switch {
	case strings.IndexByte("[](){},;", c) >= 0:
		l.pos++
		t.kind, t.text = tokPunct, string(c)
	case c == '"':
		s, err := l.quoted()
		if err != nil {
			return t, err
		}
		t.kind, t.text = tokString, s
	case c == ':':
		l.pos++
		start := l.pos
		for l.pos < len(l.src) && (isAlpha(l.src[l.pos]) || (l.pos > start && isDigit(l.src[l.pos]))) {
			l.pos++
		}
		if l.pos == start {
			return t, errAt(l.line, "a colon without a tag name")
		}
		t.kind, t.text = tokTag, strings.ToLower(l.src[start:l.pos])
	case isDigit(c):
		start := l.pos
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
		n, err := strconv.ParseInt(l.src[start:l.pos], 10, 64)
		if err != nil {
			return t, errAt(l.line, "the number %s is too large", l.src[start:l.pos])
		}
		if l.pos < len(l.src) {
			shift := map[byte]uint{'k': 10, 'm': 20, 'g': 30}[l.src[l.pos]|0x20]
			if shift > 0 {
				l.pos++
				if n > (1<<62)>>shift {
					return t, errAt(l.line, "the number %s is too large", l.src[start:l.pos])
				}
				n <<= shift
			}
		}
		// A letter or digit straight after a number is no token of the grammar.
		if l.pos < len(l.src) && (isAlpha(l.src[l.pos]) || isDigit(l.src[l.pos])) {
			return t, errAt(l.line, "%q after a number", l.src[l.pos])
		}
		t.kind, t.num = tokNumber, n
	case isAlpha(c):
		start := l.pos
		for l.pos < len(l.src) && (isAlpha(l.src[l.pos]) || isDigit(l.src[l.pos])) {
			l.pos++
		}
		word := l.src[start:l.pos]
		if strings.EqualFold(word, "text") && l.pos < len(l.src) && l.src[l.pos] == ':' {
			s, err := l.multiline()
			if err != nil {
				return t, err
			}
			t.kind, t.text = tokString, s
			break
		}
		t.kind, t.text = tokIdent, strings.ToLower(word)
	default:
		return t, errAt(l.line, "unexpected character %q", c)
	}
	return t, nil
}

// quoted reads a quoted string (RFC 5228 2.4.2): a backslash keeps the
// next character as it is, whatever it is.
func (l *lexer) quoted() (string, *Error) {
	start := l.line
	l.pos++ // the opening quote
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '"':
			l.pos++
			return checkString(b.String(), start)
		case '\\':
			l.pos++
			if l.pos >= len(l.src) {
				return "", errAt(start, "a string is not closed")
			}
			c = l.src[l.pos]
		case '\n':
			l.line++
		}
		b.WriteByte(c)
		l.pos++
	}
	return "", errAt(start, "a string is not closed")
}

// multiline reads a "text:" string (RFC 5228 2.4.2): the lines up to one
// that is a single dot. A line that starts with a dot has one more dot in
// front of it.
func (l *lexer) multiline() (string, *Error) {
	start := l.line
	l.pos++ // the colon
	for l.pos < len(l.src) && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t') {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '#' {
		for l.pos < len(l.src) && l.src[l.pos] != '\n' {
			l.pos++
		}
	}
	if l.pos < len(l.src) && l.src[l.pos] == '\r' {
		l.pos++
	}
	if l.pos >= len(l.src) || l.src[l.pos] != '\n' {
		return "", errAt(start, "only a comment may follow \"text:\" on its line")
	}
	l.pos++
	l.line++
	var b strings.Builder
	for l.pos < len(l.src) {
		end := strings.IndexByte(l.src[l.pos:], '\n')
		var line string
		if end < 0 {
			line = l.src[l.pos:]
			l.pos = len(l.src)
		} else {
			line = l.src[l.pos : l.pos+end]
			l.pos += end + 1
			l.line++
		}
		bare := strings.TrimSuffix(line, "\r")
		if bare == "." {
			return checkString(b.String(), start)
		}
		if end < 0 {
			break
		}
		if strings.HasPrefix(bare, ".") {
			bare = bare[1:]
		}
		b.WriteString(bare)
		b.WriteString("\r\n")
	}
	return "", errAt(start, "a \"text:\" string is not closed by a line with a single dot")
}

// checkString holds strings to what RFC 5228 2.4.2 allows: UTF-8 without
// NUL.
func checkString(s string, line int) (string, *Error) {
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		return "", errAt(line, "a string must be UTF-8 without NUL")
	}
	return s, nil
}

// arg is one argument of a command or test.
type arg struct {
	kind int // tokTag, tokNumber, tokString (one string) or tokPunct (a string list)
	text string
	num  int64
	list []string
	line int
}

// strings is the argument as a string list; a single string is a list of
// one (RFC 5228 2.4.2.1).
func (a arg) strings() []string {
	if a.kind == tokString {
		return []string{a.text}
	}
	return a.list
}

func (a arg) isStrings() bool { return a.kind == tokString || a.kind == tokPunct }

// node is a command or a test as written: a name, its arguments, the tests
// in its argument and, for a command, its block.
type node struct {
	name  string
	args  []arg
	tests []*node
	// list says the tests stood in parentheses.
	list     bool
	block    []*node
	hasBlock bool
	line     int
}

type parser struct {
	lex   lexer
	tok   token
	count int
}

func (p *parser) advance() *Error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) is(punct string) bool { return p.tok.kind == tokPunct && p.tok.text == punct }

// parseSyntax reads a script by the grammar of RFC 5228 8.2. It knows no
// command: that is validate's part.
func parseSyntax(src []byte) ([]*node, *Error) {
	if len(src) > MaxScriptSize {
		return nil, errAt(1, "the script is larger than %d octets", MaxScriptSize)
	}
	if !utf8.Valid(src) {
		return nil, errAt(1, "the script is not UTF-8")
	}
	p := &parser{lex: lexer{src: string(src), line: 1}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	cmds, err := p.commands(0)
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokEOF {
		return nil, errAt(p.tok.line, "unexpected %s", p.describe())
	}
	return cmds, nil
}

func (p *parser) describe() string {
	switch p.tok.kind {
	case tokEOF:
		return "end of the script"
	case tokNumber:
		return "number"
	case tokString:
		return "string"
	case tokTag:
		return ":" + p.tok.text
	}
	return strconv.Quote(p.tok.text)
}

func (p *parser) commands(depth int) ([]*node, *Error) {
	var out []*node
	for p.tok.kind == tokIdent {
		cmd, err := p.command(depth)
		if err != nil {
			return nil, err
		}
		out = append(out, cmd)
	}
	return out, nil
}

func (p *parser) command(depth int) (*node, *Error) {
	n := &node{name: p.tok.text, line: p.tok.line}
	if p.count++; p.count > maxCommands {
		return nil, errAt(n.line, "the script has more than %d commands and tests", maxCommands)
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.arguments(n, depth); err != nil {
		return nil, err
	}
	switch {
	case p.is(";"):
		return n, p.advance()
	case p.is("{"):
		if depth >= maxNesting {
			return nil, errAt(p.tok.line, "blocks are nested more than %d deep", maxNesting)
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		block, err := p.commands(depth + 1)
		if err != nil {
			return nil, err
		}
		if !p.is("}") {
			return nil, errAt(p.tok.line, "expected a command or \"}\", found %s", p.describe())
		}
		n.block, n.hasBlock = block, true
		return n, p.advance()
	}
	return nil, errAt(p.tok.line, "expected \";\" or \"{\" after %q, found %s", n.name, p.describe())
}

func (p *parser) arguments(n *node, depth int) *Error {
	for {
		a := arg{kind: p.tok.kind, text: p.tok.text, num: p.tok.num, line: p.tok.line}
		switch {
		case p.tok.kind == tokTag || p.tok.kind == tokNumber || p.tok.kind == tokString:
			if err := p.advance(); err != nil {
				return err
			}
		case p.is("["):
			if err := p.advance(); err != nil {
				return err
			}
			a.kind, a.text = tokPunct, ""
			for {
				if p.tok.kind != tokString {
					return errAt(p.tok.line, "expected a string in a list, found %s", p.describe())
				}
				if a.list = append(a.list, p.tok.text); len(a.list) > maxListItems {
					return errAt(p.tok.line, "a list has more than %d strings", maxListItems)
				}
				if err := p.advance(); err != nil {
					return err
				}
				if p.is("]") {
					break
				}
				if !p.is(",") {
					return errAt(p.tok.line, "expected \",\" or \"]\" in a list, found %s", p.describe())
				}
				if err := p.advance(); err != nil {
					return err
				}
			}
			if err := p.advance(); err != nil {
				return err
			}
		default:
			return p.tests(n, depth)
		}
		n.args = append(n.args, a)
	}
}

// tests reads the test or the list of tests that may end the arguments.
func (p *parser) tests(n *node, depth int) *Error {
	if depth >= maxNesting {
		return errAt(p.tok.line, "tests are nested more than %d deep", maxNesting)
	}
	switch {
	case p.tok.kind == tokIdent:
		t, err := p.test(depth + 1)
		if err != nil {
			return err
		}
		n.tests = []*node{t}
	case p.is("("):
		n.list = true
		if err := p.advance(); err != nil {
			return err
		}
		for {
			if p.tok.kind != tokIdent {
				return errAt(p.tok.line, "expected a test, found %s", p.describe())
			}
			t, err := p.test(depth + 1)
			if err != nil {
				return err
			}
			n.tests = append(n.tests, t)
			if p.is(")") {
				return p.advance()
			}
			if !p.is(",") {
				return errAt(p.tok.line, "expected \",\" or \")\" in a list of tests, found %s", p.describe())
			}
			if err := p.advance(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) test(depth int) (*node, *Error) {
	n := &node{name: p.tok.text, line: p.tok.line}
	if p.count++; p.count > maxCommands {
		return nil, errAt(n.line, "the script has more than %d commands and tests", maxCommands)
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return n, p.arguments(n, depth)
}
