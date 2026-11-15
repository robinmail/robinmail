// Package ical reads and writes the text format that iCalendar (RFC 5545)
// and vCard (RFC 6350, and version 3.0 of RFC 2426) share: content lines,
// folded at 75 octets, grouped into components between BEGIN and END.
//
// It knows the syntax, not the meaning: a property is a name, parameters
// and its value as written, escapes included. TextValue and the other
// helpers interpret a value when the caller knows its type. Unknown
// properties and parameters survive a parse and an encode unchanged, which
// is what a server that stores what clients send has to promise.
//
// Input is hostile: Parse bounds the depth of nesting, the number of
// properties and the length of a line, and never panics (FuzzParse).
package ical

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits of Parse.
const (
	MaxDepth = 8
	// MaxProps is how many properties and components one input may have
	// in all.
	MaxProps = 20_000
	// MaxLine is the longest logical (unfolded) line.
	MaxLine = 1 << 20
)

var (
	ErrSyntax = errors.New("ical: syntax error")
	ErrLimit  = errors.New("ical: input exceeds a limit")
)

// Param is a property parameter. A parameter may have several values
// (RFC 5545 3.2: "MEMBER=a,b"); they are kept apart, unquoted and with
// the caret escapes of RFC 6868 undone.
type Param struct {
	Name   string // upper case
	Values []string
}

// Prop is one content line.
type Prop struct {
	Group  string // vCard only (RFC 6350 3.3); as written
	Name   string // upper case
	Params []Param
	// Value is the value as written, escapes included, unfolded.
	Value string
}

// Component is BEGIN:NAME ... END:NAME with its properties in order.
type Component struct {
	Name     string // upper case
	Props    []Prop
	Children []*Component
}

// Param returns the first value of a parameter, or "".
func (p *Prop) Param(name string) string {
	for _, x := range p.Params {
		if x.Name == name && len(x.Values) > 0 {
			return x.Values[0]
		}
	}
	return ""
}

// ParamValues returns all values of a parameter, over all its occurrences.
func (p *Prop) ParamValues(name string) []string {
	var out []string
	for _, x := range p.Params {
		if x.Name == name {
			out = append(out, x.Values...)
		}
	}
	return out
}

// HasParam reports whether the parameter is present.
func (p *Prop) HasParam(name string) bool {
	for _, x := range p.Params {
		if x.Name == name {
			return true
		}
	}
	return false
}

// SetParam replaces a parameter; no values removes it.
func (p *Prop) SetParam(name string, values ...string) {
	name = strings.ToUpper(name)
	out := p.Params[:0:0]
	done := false
	for _, x := range p.Params {
		if x.Name == name {
			if !done && len(values) > 0 {
				out = append(out, Param{Name: name, Values: values})
				done = true
			}
			continue
		}
		out = append(out, x)
	}
	if !done && len(values) > 0 {
		out = append(out, Param{Name: name, Values: values})
	}
	p.Params = out
}

// Text is the value read as TEXT (RFC 5545 3.3.11, RFC 6350 3.4).
func (p *Prop) Text() string { return Unescape(p.Value) }

// Get returns the first property of a name, or nil.
func (c *Component) Get(name string) *Prop {
	for i := range c.Props {
		if c.Props[i].Name == name {
			return &c.Props[i]
		}
	}
	return nil
}

// All returns every property of a name.
func (c *Component) All(name string) []*Prop {
	var out []*Prop
	for i := range c.Props {
		if c.Props[i].Name == name {
			out = append(out, &c.Props[i])
		}
	}
	return out
}

// Text is the TEXT value of the first property of a name, or "".
func (c *Component) Text(name string) string {
	if p := c.Get(name); p != nil {
		return p.Text()
	}
	return ""
}

// Value is the raw value of the first property of a name, or "".
func (c *Component) Value(name string) string {
	if p := c.Get(name); p != nil {
		return p.Value
	}
	return ""
}

// Remove drops every property of the given names.
func (c *Component) Remove(names ...string) {
	out := c.Props[:0]
	for _, p := range c.Props {
		drop := false
		for _, n := range names {
			if p.Name == n {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, p)
		}
	}
	c.Props = out
}

// Set replaces all properties of p's name by p, at the place of the first.
func (c *Component) Set(p Prop) {
	p.Name = strings.ToUpper(p.Name)
	for i := range c.Props {
		if c.Props[i].Name == p.Name {
			c.Props[i] = p
			rest := c.Props[:i+1]
			for _, q := range c.Props[i+1:] {
				if q.Name != p.Name {
					rest = append(rest, q)
				}
			}
			c.Props = rest
			return
		}
	}
	c.Props = append(c.Props, p)
}

// SetText sets a property to a TEXT value.
func (c *Component) SetText(name, text string) {
	c.Set(Prop{Name: name, Value: Escape(text)})
}

// Add appends a property.
func (c *Component) Add(p Prop) {
	p.Name = strings.ToUpper(p.Name)
	c.Props = append(c.Props, p)
}

// Sub returns the child components of a name.
func (c *Component) Sub(name string) []*Component {
	var out []*Component
	for _, x := range c.Children {
		if x.Name == name {
			out = append(out, x)
		}
	}
	return out
}

// Clone copies a component deeply.
func (c *Component) Clone() *Component {
	n := &Component{Name: c.Name, Props: make([]Prop, len(c.Props))}
	for i, p := range c.Props {
		q := p
		q.Params = make([]Param, len(p.Params))
		for j, x := range p.Params {
			q.Params[j] = Param{Name: x.Name, Values: append([]string(nil), x.Values...)}
		}
		n.Props[i] = q
	}
	for _, x := range c.Children {
		n.Children = append(n.Children, x.Clone())
	}
	return n
}

// Parse reads every top-level component of the input: one VCALENDAR, or
// any number of VCARDs. Lines end in CRLF or LF; a line that starts with a
// space or a tab continues the one before (RFC 5545 3.1). Blank lines are
// skipped. Text outside a component is an error.
func Parse(data []byte) ([]*Component, error) {
	if !utf8.Valid(data) {
		// RFC 5545 3.1.4 and RFC 6350 3.1: the text is UTF-8. Latin-1
		// from old programs is taken byte by byte, which keeps it readable.
		data = latin1(data)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	lines, err := unfold(data)
	if err != nil {
		return nil, err
	}
	var top []*Component
	var stack []*Component
	count := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if count++; count > MaxProps {
			return nil, ErrLimit
		}
		p, err := parseLine(line)
		if err != nil {
			return nil, err
		}
		switch p.Name {
		case "BEGIN":
			if len(stack) >= MaxDepth {
				return nil, ErrLimit
			}
			name := strings.ToUpper(strings.TrimSpace(p.Value))
			if !validName(name) {
				return nil, fmt.Errorf("%w: BEGIN without a component name", ErrSyntax)
			}
			c := &Component{Name: name}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, c)
			} else {
				top = append(top, c)
			}
			stack = append(stack, c)
		case "END":
			name := strings.ToUpper(strings.TrimSpace(p.Value))
			if len(stack) == 0 || stack[len(stack)-1].Name != name {
				return nil, fmt.Errorf("%w: END:%s does not close the open component", ErrSyntax, name)
			}
			stack = stack[:len(stack)-1]
		default:
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: a property outside a component", ErrSyntax)
			}
			c := stack[len(stack)-1]
			c.Props = append(c.Props, p)
		}
	}
	if len(stack) > 0 {
		return nil, fmt.Errorf("%w: %s is not closed", ErrSyntax, stack[0].Name)
	}
	if len(top) == 0 {
		return nil, fmt.Errorf("%w: no component", ErrSyntax)
	}
	return top, nil
}

// ParseOne reads input that must hold exactly one top-level component of
// the given name.
func ParseOne(data []byte, name string) (*Component, error) {
	all, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if len(all) != 1 || all[0].Name != name {
		return nil, fmt.Errorf("%w: expected one %s", ErrSyntax, name)
	}
	return all[0], nil
}

func latin1(b []byte) []byte {
	var out bytes.Buffer
	for _, c := range b {
		out.WriteRune(rune(c))
	}
	return out.Bytes()
}

// unfold joins continuation lines.
func unfold(data []byte) ([]string, error) {
	var lines []string
	var cur strings.Builder
	have := false
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var line []byte
		if i < 0 {
			line, data = data, nil
		} else {
			line, data = data[:i], data[i+1:]
		}
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			if !have {
				// A continuation of nothing: RFC 5545 has no such thing,
				// but some writers put a space before the first line.
				line = bytes.TrimLeft(line, " \t")
			} else {
				line = line[1:]
			}
			if cur.Len()+len(line) > MaxLine {
				return nil, ErrLimit
			}
			cur.Write(line)
			have = true
			continue
		}
		if have {
			lines = append(lines, cur.String())
			cur.Reset()
		}
		if len(line) > MaxLine {
			return nil, ErrLimit
		}
		cur.Write(line)
		have = true
	}
	if have {
		lines = append(lines, cur.String())
	}
	return lines, nil
}

// validName is the "name" of RFC 5545 3.1: letters, digits and "-" (iana
// and x- names alike).
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// parseLine reads one content line:
//
//	contentline = [group "."] name *(";" param) ":" value
func parseLine(line string) (Prop, error) {
	var p Prop
	i := 0
	for i < len(line) && line[i] != ';' && line[i] != ':' {
		i++
	}
	name := line[:i]
	if g, n, ok := strings.Cut(name, "."); ok {
		p.Group, name = g, n
	}
	if !validName(name) {
		return p, fmt.Errorf("%w: bad property name %q", ErrSyntax, truncate(line))
	}
	p.Name = strings.ToUpper(name)
	for i < len(line) && line[i] == ';' {
		i++
		j := i
		for j < len(line) && line[j] != '=' && line[j] != ';' && line[j] != ':' {
			j++
		}
		pname := strings.ToUpper(line[i:j])
		if !validName(pname) {
			return p, fmt.Errorf("%w: bad parameter name in %q", ErrSyntax, truncate(line))
		}
		param := Param{Name: pname}
		if j >= len(line) || line[j] != '=' {
			// vCard 2.1 writes a bare value ("TEL;HOME:...") meaning
			// TYPE=HOME. Keep it as TYPE.
			param = Param{Name: "TYPE", Values: []string{strings.ToLower(pname)}}
			p.Params = append(p.Params, param)
			i = j
			continue
		}
		j++ // '='
		for {
			var v string
			if j < len(line) && line[j] == '"' {
				k := strings.IndexByte(line[j+1:], '"')
				if k < 0 {
					return p, fmt.Errorf("%w: unterminated quoted parameter value", ErrSyntax)
				}
				v = line[j+1 : j+1+k]
				j = j + 1 + k + 1
			} else {
				k := j
				for k < len(line) && line[k] != ',' && line[k] != ';' && line[k] != ':' {
					k++
				}
				v = line[j:k]
				j = k
			}
			param.Values = append(param.Values, caretDecode(v))
			if j < len(line) && line[j] == ',' {
				j++
				continue
			}
			break
		}
		p.Params = append(p.Params, param)
		i = j
	}
	if i >= len(line) || line[i] != ':' {
		return p, fmt.Errorf("%w: no ':' in %q", ErrSyntax, truncate(line))
	}
	p.Value = line[i+1:]
	return p, nil
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// caretDecode undoes RFC 6868: ^n is a newline, ^^ a caret, ^' a double
// quote; any other caret stays.
func caretDecode(s string) string {
	if !strings.Contains(s, "^") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '^' && i+1 < len(s) {
			switch s[i+1] {
			case 'n', 'N':
				b.WriteByte('\n')
				i++
				continue
			case '^':
				b.WriteByte('^')
				i++
				continue
			case '\'':
				b.WriteByte('"')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func caretEncode(s string) string {
	if !strings.ContainsAny(s, "^\n\"") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '^':
			b.WriteString("^^")
		case '\n':
			b.WriteString("^n")
		case '"':
			b.WriteString("^'")
		case '\r':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Unescape reads a TEXT value: "\\" "\;" "\," "\n" "\N" (RFC 5545 3.3.11).
// An unknown escape keeps its character.
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Escape writes a TEXT value.
func Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case ';':
			b.WriteString(`\;`)
		case ',':
			b.WriteString(`\,`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// SplitRaw splits a raw value at sep where sep is not escaped. It is how a
// structured value (vCard N and ADR with ";", CATEGORIES with ",") is
// taken apart; the parts are still escaped.
func SplitRaw(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// TextList reads a value of several TEXT values separated by commas.
func TextList(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := SplitRaw(raw, ',')
	for i, p := range parts {
		parts[i] = Unescape(p)
	}
	return parts
}

// JoinText writes several TEXT values separated by sep.
func JoinText(values []string, sep string) string {
	esc := make([]string, len(values))
	for i, v := range values {
		esc[i] = Escape(v)
	}
	return strings.Join(esc, sep)
}

// Encode writes a component as content lines folded at 75 octets, CRLF.
func Encode(c *Component) []byte {
	var b bytes.Buffer
	encode(&b, c)
	return b.Bytes()
}

// EncodeAll writes several components one after another.
func EncodeAll(cs []*Component) []byte {
	var b bytes.Buffer
	for _, c := range cs {
		encode(&b, c)
	}
	return b.Bytes()
}

func encode(b *bytes.Buffer, c *Component) {
	writeLine(b, "BEGIN:"+c.Name)
	for _, p := range c.Props {
		writeLine(b, p.line())
	}
	for _, x := range c.Children {
		encode(b, x)
	}
	writeLine(b, "END:"+c.Name)
}

func (p *Prop) line() string {
	var b strings.Builder
	if p.Group != "" {
		b.WriteString(p.Group)
		b.WriteByte('.')
	}
	b.WriteString(p.Name)
	for _, x := range p.Params {
		b.WriteByte(';')
		b.WriteString(x.Name)
		b.WriteByte('=')
		for i, v := range x.Values {
			if i > 0 {
				b.WriteByte(',')
			}
			v = caretEncode(v)
			if strings.ContainsAny(v, ";:,") || strings.TrimSpace(v) != v {
				b.WriteByte('"')
				b.WriteString(v)
				b.WriteByte('"')
			} else {
				b.WriteString(v)
			}
		}
	}
	b.WriteByte(':')
	// A raw value never holds a line break: a parsed one cannot, and an
	// escaped one has "\n". A caller that put one there gets it escaped
	// rather than a broken file.
	v := strings.ReplaceAll(strings.ReplaceAll(p.Value, "\r", ""), "\n", `\n`)
	b.WriteString(v)
	return b.String()
}

// writeLine folds at 75 octets without splitting a UTF-8 sequence (RFC
// 5545 3.1: "Lines of text SHOULD NOT be longer than 75 octets").
func writeLine(b *bytes.Buffer, s string) {
	const limit = 75
	first := true
	for len(s) > 0 {
		n := limit
		if !first {
			n = limit - 1
		}
		if len(s) <= n {
			if !first {
				b.WriteByte(' ')
			}
			b.WriteString(s)
			break
		}
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if !first {
			b.WriteByte(' ')
		}
		b.WriteString(s[:cut])
		b.WriteString("\r\n")
		s = s[cut:]
		first = false
	}
	b.WriteString("\r\n")
}
