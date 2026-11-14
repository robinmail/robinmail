package imap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Reading commands: RFC 3501 section 9 (formal syntax) and section 4 (data
// formats), RFC 9051 section 9, RFC 7888 (non-synchronizing literals).
//
// A command is read whole before it is looked at: its lines and the
// literals between them. What may be read is bounded before it is read.

// Limits on what a client may send.
const (
	// maxLine is the longest line of a command, literals not counted.
	// RFC 7162 4 asks for at least 8192; clients send long UID sets.
	maxLine = 64 << 10
	// maxLiteral is the largest literal that is not a message.
	maxLiteral = 64 << 10
	// maxCommandText is all lines and small literals of one command.
	maxCommandText = 256 << 10
	// maxListItems bounds the items of one parenthesised list or set.
	maxListItems   = 10_000
	maxSearchDepth = 32
)

// A parseError is a command that is not IMAP: the answer is BAD.
type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

func bad(format string, a ...any) error { return &parseError{fmt.Sprintf(format, a...)} }

// errLineTooLong and errLiteralTooLarge end the reading of a command.
var (
	errLineTooLong = errors.New("line too long")
	errTooLarge    = errors.New("literal too large")
)

// part is a piece of a command: a stretch of text, or a literal.
type part struct {
	text string
	lit  bool
	// binary marks a literal8 (RFC 3516), "~{n}".
	binary bool
}

// parser reads the arguments of one command.
type parser struct {
	parts []part
	i     int // the part being read
	pos   int // offset in its text
	// utf8 allows UTF-8 in quoted strings (RFC 6855, RFC 9051).
	utf8 bool
}

// literalStart recognises "{n}", "{n+}" (RFC 7888) and "~{n}" (RFC 3516) at
// the end of a line.
func literalStart(line string) (size int64, sync, binary, ok bool) {
	if !strings.HasSuffix(line, "}") {
		return 0, false, false, false
	}
	open := strings.LastIndexByte(line, '{')
	if open < 0 {
		return 0, false, false, false
	}
	digits := line[open+1 : len(line)-1]
	sync = true
	if strings.HasSuffix(digits, "+") {
		sync, digits = false, digits[:len(digits)-1]
	}
	if digits == "" || len(digits) > 12 || strings.TrimLeft(digits, "0123456789") != "" {
		return 0, false, false, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false, false, false
	}
	binary = open > 0 && line[open-1] == '~'
	return n, sync, binary, true
}

// readLine reads one line without its line end. A line ends in CRLF; a
// bare LF is taken too.
func readLine(br *bufio.Reader, max int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return "", errLineTooLong
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	line = line[:len(line)-1]
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return string(line), nil
}

// commandReader reads commands from a connection.
type commandReader struct {
	br *bufio.Reader
	// continuation is called before a synchronizing literal is read: the
	// client waits for it (RFC 3501 4.3).
	continuation func() error
	// literalLimit says how large the literal that follows text may be.
	// It gets the parts read so far.
	literalLimit func(parts []part) int64
}

// read reads one command: lines and literals until a line that does not
// end in a literal. On errTooLarge the oversized literal was not read: for
// a synchronizing one the client has not sent it; for a non-synchronizing
// one skip says how many octets, and the rest of the command, are still
// to come.
func (r *commandReader) read() (parts []part, skip int64, err error) {
	total := 0
	for {
		line, err := readLine(r.br, maxLine)
		if err != nil {
			return parts, 0, err
		}
		total += len(line)
		if total > maxCommandText {
			return parts, 0, errLineTooLong
		}
		size, sync, binary, isLit := literalStart(line)
		if !isLit {
			return append(parts, part{text: line}), 0, nil
		}
		text := line[:strings.LastIndexByte(line, '{')]
		if binary {
			text = text[:len(text)-1]
		}
		parts = append(parts, part{text: text})
		limit := int64(maxLiteral)
		if r.literalLimit != nil {
			limit = r.literalLimit(parts)
		}
		if size > limit {
			if sync {
				return parts, 0, errTooLarge
			}
			return parts, size, errTooLarge
		}
		if sync {
			if err := r.continuation(); err != nil {
				return parts, 0, err
			}
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(r.br, buf); err != nil {
			return parts, 0, err
		}
		if size <= maxLiteral {
			total += int(size)
		}
		parts = append(parts, part{text: string(buf), lit: true, binary: binary})
	}
}

func (p *parser) cur() string {
	if p.i >= len(p.parts) || p.parts[p.i].lit {
		return ""
	}
	return p.parts[p.i].text[p.pos:]
}

// peek is the next character of the text, 0 at its end.
func (p *parser) peek() byte {
	if s := p.cur(); s != "" {
		return s[0]
	}
	return 0
}

// atEnd reports whether the command has nothing more.
func (p *parser) atEnd() bool {
	return p.cur() == "" && p.i >= len(p.parts)-1 && !p.literalNext()
}

// literalNext reports whether the next thing is a literal.
func (p *parser) literalNext() bool {
	return p.i < len(p.parts) && !p.parts[p.i].lit && p.pos == len(p.parts[p.i].text) &&
		p.i+1 < len(p.parts) && p.parts[p.i+1].lit
}

func (p *parser) end() error {
	if !p.atEnd() {
		return bad("unexpected text after the command: %q", clip(p.cur(), 40))
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// space reads the one space between arguments.
func (p *parser) space() error {
	if p.peek() != ' ' {
		return bad("a space is missing")
	}
	p.pos++
	return nil
}

// skip reads c if it is next.
func (p *parser) skip(c byte) bool {
	if p.peek() == c {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expect(c byte) error {
	if !p.skip(c) {
		return bad("%q is missing", string(c))
	}
	return nil
}

// atomChar: any CHAR except atom-specials (RFC 3501 9: "(" ")" "{" SP CTL
// list-wildcards quoted-specials resp-specials).
func atomChar(c byte) bool {
	return c > ' ' && c < 0x7f && strings.IndexByte(`(){%*"\]`, c) < 0
}

func (p *parser) takeWhile(ok func(byte) bool) string {
	s := p.cur()
	n := 0
	for n < len(s) && ok(s[n]) {
		n++
	}
	p.pos += n
	return s[:n]
}

// atom reads an atom.
func (p *parser) atom() (string, error) {
	s := p.takeWhile(atomChar)
	if s == "" {
		return "", bad("an atom is missing")
	}
	return s, nil
}

// word reads an atom and returns it in upper case: a command or item name.
func (p *parser) word() (string, error) {
	s, err := p.atom()
	return strings.ToUpper(s), err
}

// literal reads the literal that is next.
func (p *parser) literal() (string, bool) {
	if !p.literalNext() {
		return "", false
	}
	s := p.parts[p.i+1].text
	p.i += 2
	p.pos = 0
	return s, true
}

// quoted reads a quoted string, which starts at the current position.
func (p *parser) quoted() (string, error) {
	s := p.cur()
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			p.pos += i + 1
			out := b.String()
			if !p.utf8 && !isASCII(out) {
				return "", bad("8-bit characters in a quoted string")
			}
			if !utf8.ValidString(out) {
				return "", bad("a quoted string that is not UTF-8")
			}
			return out, nil
		case c == '\\':
			i++
			if i >= len(s) || (s[i] != '"' && s[i] != '\\') {
				return "", bad("a backslash in a quoted string quotes only \" and \\")
			}
			b.WriteByte(s[i])
		case c == '\r' || c == '\n' || c == 0:
			return "", bad("a quoted string cannot hold a line end")
		default:
			b.WriteByte(c)
		}
	}
	return "", bad("a quoted string does not end")
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// str reads a string: quoted or literal.
func (p *parser) str() (string, error) {
	if s, ok := p.literal(); ok {
		return s, nil
	}
	if p.peek() == '"' {
		return p.quoted()
	}
	return "", bad("a string is missing")
}

// astring reads an atom, a quoted string or a literal. An atom here may
// hold "]" (ASTRING-CHAR).
func (p *parser) astring() (string, error) {
	if p.peek() == '"' || p.literalNext() {
		return p.str()
	}
	s := p.takeWhile(func(c byte) bool { return atomChar(c) || c == ']' })
	if s == "" {
		return "", bad("a string is missing")
	}
	return s, nil
}

// listMailbox reads a mailbox name with wildcards (RFC 3501 9,
// list-mailbox).
func (p *parser) listMailbox() (string, error) {
	if p.peek() == '"' || p.literalNext() {
		return p.str()
	}
	s := p.takeWhile(func(c byte) bool { return atomChar(c) || c == ']' || c == '%' || c == '*' })
	if s == "" {
		return "", bad("a mailbox name is missing")
	}
	return s, nil
}

// number reads a number of at most 32 bits.
func (p *parser) number() (uint32, error) {
	n, err := p.number64()
	if err != nil {
		return 0, err
	}
	if n > 1<<32-1 {
		return 0, bad("a number is too large")
	}
	return uint32(n), nil
}

// number64 reads a number of at most 63 bits (RFC 7162 mod-sequence).
func (p *parser) number64() (int64, error) {
	s := p.takeWhile(func(c byte) bool { return c >= '0' && c <= '9' })
	if s == "" {
		return 0, bad("a number is missing")
	}
	if len(s) > 19 {
		return 0, bad("a number is too large")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, bad("a number is too large")
	}
	return n, nil
}

// star is "*" in a sequence set: the highest number in use. 0 is free
// for it: no message has that number or UID.
const star = 0

// seqRange is one range of a sequence set, Lo <= Hi after resolve.
type seqRange struct{ Lo, Hi uint32 }

// seqSet is a sequence set (RFC 3501 9, sequence-set). Saved is "$", the
// result of the last SEARCH with SAVE (RFC 5182).
type seqSet struct {
	Ranges []seqRange
	Saved  bool
}

func (p *parser) seqNumber() (uint32, error) {
	if p.skip('*') {
		return star, nil
	}
	n, err := p.number()
	if err != nil {
		return 0, err
	}
	if n == star {
		return 0, bad("0 is not a message number")
	}
	return n, nil
}

// seqSet reads a sequence set.
func (p *parser) seqSet() (seqSet, error) {
	if p.skip('$') {
		return seqSet{Saved: true}, nil
	}
	var set seqSet
	for {
		lo, err := p.seqNumber()
		if err != nil {
			return set, err
		}
		hi := lo
		if p.skip(':') {
			if hi, err = p.seqNumber(); err != nil {
				return set, err
			}
		}
		set.Ranges = append(set.Ranges, seqRange{lo, hi})
		if len(set.Ranges) > maxListItems {
			return set, bad("too many ranges in a sequence set")
		}
		if !p.skip(',') {
			return set, nil
		}
	}
}

// resolve gives the ranges with "*" replaced by max, the highest number in
// use, each with Lo <= Hi. RFC 3501 9: "n:m" and "m:n" are the same, and
// "*" is the highest number, so "5:*" with 3 messages is 3:5.
func (s seqSet) resolve(max uint32) []seqRange {
	out := make([]seqRange, 0, len(s.Ranges))
	for _, r := range s.Ranges {
		if r.Lo == star {
			r.Lo = max
		}
		if r.Hi == star {
			r.Hi = max
		}
		if r.Lo > r.Hi {
			r.Lo, r.Hi = r.Hi, r.Lo
		}
		out = append(out, r)
	}
	return out
}

func inRanges(ranges []seqRange, n uint32) bool {
	for _, r := range ranges {
		if n >= r.Lo && n <= r.Hi {
			return true
		}
	}
	return false
}

// flagList reads "(" flags ")" or, where the syntax allows it, flags
// without parentheses.
func (p *parser) flagList(bare bool) ([]string, error) {
	paren := p.skip('(')
	if !paren && !bare {
		return nil, bad("a flag list is missing")
	}
	var flags []string
	for {
		if paren && p.skip(')') {
			return flags, nil
		}
		if len(flags) > 0 {
			if !paren && p.peek() != ' ' {
				return flags, nil
			}
			if err := p.space(); err != nil {
				return nil, err
			}
		}
		f, err := p.flag()
		if err != nil {
			return nil, err
		}
		flags = append(flags, f)
		if len(flags) > maxListItems {
			return nil, bad("too many flags")
		}
	}
}

// flag reads one flag: "\" atom, or an atom (a keyword).
func (p *parser) flag() (string, error) {
	prefix := ""
	if p.skip('\\') {
		prefix = `\`
	}
	a, err := p.atom()
	if err != nil {
		return "", bad("a flag is missing")
	}
	return prefix + a, nil
}

var months = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// date reads a date as SEARCH has it (RFC 3501 9, date): d-Mon-yyyy,
// quoted or not.
func (p *parser) date() (time.Time, error) {
	var s string
	var err error
	if p.peek() == '"' {
		s, err = p.quoted()
	} else {
		s, err = p.atom()
	}
	if err != nil {
		return time.Time{}, bad("a date is missing")
	}
	for _, layout := range []string{"2-Jan-2006", "02-Jan-2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, bad("not a date: %q", clip(s, 30))
}

// dateTime reads the date-time of APPEND (RFC 3501 9, date-time):
// "dd-Mon-yyyy hh:mm:ss +zzzz" in quotes, the day padded with a space or
// a zero.
func (p *parser) dateTime() (time.Time, error) {
	if p.peek() != '"' {
		return time.Time{}, bad("a date-time is missing")
	}
	s, err := p.quoted()
	if err != nil {
		return time.Time{}, err
	}
	for _, layout := range []string{"_2-Jan-2006 15:04:05 -0700", "02-Jan-2006 15:04:05 -0700", "2-Jan-2006 15:04:05 -0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, bad("not a date-time: %q", clip(s, 40))
}

// formatDateTime writes INTERNALDATE.
func formatDateTime(t time.Time) string {
	return t.Format("02-Jan-2006 15:04:05 -0700")
}

// quote writes a string for a response: quoted when it can be, a literal
// otherwise. With utf8, UTF-8 may be quoted (RFC 6855 3, RFC 9051 4.3).
func quote(s string, utf8ok bool) string {
	ok := len(s) <= 1000
	for i := 0; ok && i < len(s); i++ {
		c := s[i]
		if c == '\r' || c == '\n' || c == 0 || (c >= 0x80 && !utf8ok) {
			ok = false
		}
	}
	if ok && utf8ok && !utf8.ValidString(s) {
		ok = false
	}
	if !ok {
		return "{" + strconv.Itoa(len(s)) + "}\r\n" + s
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

// nquote is quote, with NIL for a missing value.
func nquote(s *string, utf8ok bool) string {
	if s == nil {
		return "NIL"
	}
	return quote(*s, utf8ok)
}

// validTag: 1*<any ASTRING-CHAR except "+"> (RFC 3501 9).
func validTag(tag string) bool {
	if tag == "" || len(tag) > 100 {
		return false
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if !(atomChar(c) || c == ']') || c == '+' {
			return false
		}
	}
	return true
}
