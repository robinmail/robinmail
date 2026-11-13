// Package message reads a raw Internet message (RFC 5322 with MIME, RFC 2045
// to 2049) into the shape JMAP wants: the Email body structure and the
// parsed header forms of RFC 8621 section 4.1.
//
// Mail is untrusted. Nothing here fails: a message that does not parse is
// read as far as it goes. Every loop is bounded by the size of the input,
// and nesting depth, part count and header count have limits of their own.
package message

import (
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits for hostile input.
const (
	// MaxHeaderBytes is how much of a header section is read. Fields past
	// it are ignored; the body still starts where the section ends.
	MaxHeaderBytes = 1 << 20
	// MaxHeaderFields is how many fields of one header section are kept.
	MaxHeaderFields = 1000
	// MaxDepth is how deep multiparts may nest. A multipart below that is
	// kept without its parts.
	MaxDepth = 20
	// MaxParts is how many body parts a message may have. Parts past it
	// are dropped.
	MaxParts = 1000
	// MaxAddresses is how many addresses are read from one header field.
	MaxAddresses = 1000
	// MaxMessageIDLen is the longest message id that is kept.
	MaxMessageIDLen = 250
	// MaxParams is how many parameters are read from one header field.
	MaxParams = 64
)

// Header is one header field. Name is as written. Value is the Raw form of
// RFC 8621 4.1.2.1: everything after the colon up to the line break that
// ends the field, folding included.
type Header struct {
	Name  string
	Value string
}

// parseHeaders reads a header section and returns its fields and the offset
// of the body. The section ends at the first empty line, or at the first
// line that is neither a field nor a continuation: that line then starts the
// body. Lines may end in CRLF or a bare LF.
func parseHeaders(raw []byte) (fields []Header, body int) {
	type span struct{ name, start, end int } // name end, value start, value end
	var cur *span
	lineStart := 0
	flush := func() {
		if cur == nil {
			return
		}
		if len(fields) < MaxHeaderFields && cur.start <= MaxHeaderBytes {
			name := strings.TrimRight(string(raw[lineStartOf(raw, cur.name):cur.name]), " \t")
			fields = append(fields, Header{Name: name, Value: string(raw[cur.start:cur.end])})
		}
		cur = nil
	}
	for lineStart < len(raw) {
		end := lineStart
		for end < len(raw) && raw[end] != '\n' {
			end++
		}
		next := end
		if next < len(raw) {
			next++ // past the LF
		}
		lineEnd := end
		if lineEnd > lineStart && raw[lineEnd-1] == '\r' {
			lineEnd--
		}
		line := raw[lineStart:lineEnd]
		switch {
		case len(line) == 0:
			flush()
			return fields, next
		case line[0] == ' ' || line[0] == '\t':
			if cur == nil {
				// A continuation of nothing: the body starts here.
				return fields, lineStart
			}
			cur.end = lineEnd
		default:
			colon := fieldNameEnd(line)
			if colon < 0 {
				if lineStart == 0 && len(line) > 5 && string(line[:5]) == "From " {
					break // an mbox separator in front of the message
				}
				flush()
				return fields, lineStart
			}
			flush()
			cur = &span{name: lineStart + colon, start: lineStart + colon + 1, end: lineEnd}
		}
		lineStart = next
	}
	flush()
	return fields, len(raw)
}

func lineStartOf(raw []byte, i int) int {
	for i > 0 && raw[i-1] != '\n' {
		i--
	}
	return i
}

// fieldNameEnd returns the index of the colon that ends a field name, or -1
// if the line does not start with one. A name is printable ASCII without a
// colon (RFC 5322 2.2); white space before the colon is the obsolete syntax
// of 4.5.
func fieldNameEnd(line []byte) int {
	for i, c := range line {
		switch {
		case c == ':':
			if i == 0 {
				return -1
			}
			return i
		case c == ' ' || c == '\t':
			for j := i; j < len(line); j++ {
				if line[j] == ':' {
					return j
				}
				if line[j] != ' ' && line[j] != '\t' {
					return -1
				}
			}
			return -1
		case c < 33 || c > 126:
			return -1
		}
	}
	return -1
}

// Get returns the value of the last field with the name, as RFC 8621 4.1.3
// wants for a header that should be there once.
func Get(fields []Header, name string) (string, bool) {
	for i := len(fields) - 1; i >= 0; i-- {
		if strings.EqualFold(fields[i].Name, name) {
			return fields[i].Value, true
		}
	}
	return "", false
}

// All returns the values of every field with the name, in order.
func All(fields []Header, name string) []string {
	var out []string
	for _, f := range fields {
		if strings.EqualFold(f.Name, name) {
			out = append(out, f.Value)
		}
	}
	return out
}

// unfold removes the line breaks of folding (RFC 5322 2.2.3).
func unfold(v string) string {
	if !strings.ContainsAny(v, "\r\n") {
		return v
	}
	return strings.NewReplacer("\r\n", "", "\n", "", "\r", "").Replace(v)
}

// Text is the Text form of RFC 8621 4.1.2.2: unfolded, leading spaces
// removed, encoded words decoded, in Unicode Normalization Form C.
func Text(v string) string {
	s := DecodeWords(strings.TrimLeft(unfold(v), " \t"))
	return norm.NFC.String(strings.ToValidUTF8(s, "\uFFFD"))
}

// Address is one mailbox (RFC 8621 4.1.2.3). Name is nil without a display
// name.
type Address struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// Group is a named group of mailboxes, or a run of mailboxes outside any
// group, for which Name is nil (RFC 8621 4.1.2.4).
type Group struct {
	Name      *string   `json:"name"`
	Addresses []Address `json:"addresses"`
}

// Addresses is the Addresses form: every mailbox of an address-list, the
// ones in groups included, without the group names.
func Addresses(v string) []Address {
	out := []Address{}
	for _, g := range GroupedAddresses(v) {
		out = append(out, g.Addresses...)
	}
	return out
}

// GroupedAddresses is the GroupedAddresses form. It reads the address-list
// of RFC 5322 3.4, the obsolete forms of 4.4 included (routes, white space
// around "." and "@", empty list members), and never fails: what does not
// look like a mailbox is returned as written.
func GroupedAddresses(v string) []Group {
	var sk skipper
	s := unfold(v)
	groups := []Group{}
	inGroup := false
	noBracket := false // a "[" without "]" was seen
	count := 0
	add := func(a Address) {
		if count >= MaxAddresses {
			return
		}
		count++
		// Consecutive mailboxes outside a group share one group without a name.
		if n := len(groups); !inGroup && (n == 0 || groups[n-1].Name != nil) {
			groups = append(groups, Group{Addresses: []Address{}})
		}
		g := &groups[len(groups)-1]
		g.Addresses = append(g.Addresses, a)
	}

	type word struct{ raw, text string }
	var words []word
	comment := ""
	phrase := func() *string {
		parts := make([]string, len(words))
		for i, w := range words {
			parts[i] = w.text
		}
		return displayName(strings.Join(parts, " "))
	}
	flushBare := func() {
		if len(words) == 0 {
			comment = ""
			return
		}
		var b strings.Builder
		at := false
		for _, w := range words {
			at = at || strings.Contains(w.raw, "@")
		}
		for i, w := range words {
			if i > 0 && !at {
				b.WriteByte(' ')
			}
			b.WriteString(w.raw)
		}
		add(Address{Name: displayName(comment), Email: cleanEmail(b.String())})
		words, comment = nil, ""
	}

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case c == '(':
			end := sk.comment(s, i)
			if end < 0 {
				end = len(s)
				comment = unquotePairs(s[i+1:])
			} else {
				comment = unquotePairs(s[i+1 : end-1])
			}
			i = end
		case c == '"':
			end := sk.quoted(s, i)
			if end < 0 {
				words = append(words, word{s[i:], unquotePairs(s[i+1:])})
				i = len(s)
				break
			}
			words = append(words, word{s[i:end], unquotePairs(s[i+1 : end-1])})
			i = end
		case c == '<':
			end := i + 1
			for end < len(s) && s[end] != '>' {
				if s[end] == '"' {
					if q := sk.quoted(s, end); q > 0 {
						end = q
						continue
					}
				}
				end++
			}
			name := phrase()
			spec := s[i+1 : end]
			words, comment = nil, ""
			i = end
			if i < len(s) {
				i++ // the ">"
			}
			// Up to the next separator only a comment means something.
			for i < len(s) && s[i] != ',' && s[i] != ';' {
				switch s[i] {
				case '(':
					if e := sk.comment(s, i); e > 0 {
						comment = unquotePairs(s[i+1 : e-1])
						i = e
						continue
					}
				case '"':
					if e := sk.quoted(s, i); e > 0 {
						i = e
						continue
					}
				}
				i++
			}
			if name == nil {
				name = displayName(comment)
			}
			comment = ""
			add(Address{Name: name, Email: cleanEmail(spec)})
		case c == ',':
			flushBare()
			i++
		case c == ';':
			flushBare()
			inGroup = false
			i++
		case c == ':' && !inGroup:
			name := phrase()
			if name == nil {
				name = new(string)
			}
			groups = append(groups, Group{Name: name, Addresses: []Address{}})
			inGroup = true
			words, comment = nil, ""
			i++
		default:
			start := i
			for i < len(s) {
				c := s[i]
				if c == '[' && !noBracket {
					if end := strings.IndexByte(s[i:], ']'); end > 0 {
						i += end + 1
						continue
					}
					noBracket = true
				}
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' || strings.IndexByte(`()<>,;"`, c) >= 0 ||
					(c == ':' && !inGroup) {
					break
				}
				i++
			}
			if i == start {
				i++ // a ">" or ")" on its own
				break
			}
			words = append(words, word{s[start:i], s[start:i]})
		}
	}
	flushBare()
	return groups
}

// displayName turns a phrase or a comment into a name: encoded words
// decoded, white space collapsed. Empty is no name.
func displayName(s string) *string {
	s = strings.Join(strings.Fields(DecodeWords(s)), " ")
	s = norm.NFC.String(strings.ToValidUTF8(s, "\uFFFD"))
	if s == "" {
		return nil
	}
	return &s
}

// cleanEmail takes comments, white space and an obsolete route out of what
// stood between angle brackets, or of a bare addr-spec.
func cleanEmail(s string) string {
	var sk skipper
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '(':
			if end := sk.comment(s, i); end > 0 {
				i = end
				continue
			}
			i++
		case c == '"':
			if end := sk.quoted(s, i); end > 0 {
				b.WriteString(s[i:end])
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	out := b.String()
	// "<@a.example,@b.example:user@c.example>" (RFC 5322 4.4).
	if strings.HasPrefix(out, "@") {
		if colon := strings.IndexByte(out, ':'); colon > 0 {
			out = out[colon+1:]
		}
	}
	return strings.ToValidUTF8(out, "\uFFFD")
}

// unquotePairs removes the backslash of every quoted-pair.
func unquotePairs(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// MessageIDs is the MessageIds form (RFC 8621 4.1.2.5): the message
// identifiers in the value of a Message-ID, In-Reply-To or References
// field, without their angle brackets ("the angle bracket characters are
// not part of the msg-id", RFC 5322 3.6.4).
//
// Identifiers may be separated by CFWS, so comments are skipped, nested and
// with quoted pairs (3.2.2); a "<" inside a comment starts nothing. The
// obsolete syntax (4.5.4) also allows phrases between identifiers, as in
// "In-Reply-To: Your message of Friday <id>", so quoted strings are skipped
// the same way and other words are ignored.
//
// It is lenient about what is between the brackets: real identifiers do not
// all have the "@" the grammar asks for. Ids with white space or of absurd
// length are skipped.
func MessageIDs(v string) []string {
	var sk skipper
	var out []string
	for i := 0; i < len(v); {
		switch v[i] {
		case '(':
			if end := sk.comment(v, i); end > 0 {
				i = end
				continue
			}
		case '"':
			if end := sk.quoted(v, i); end > 0 {
				i = end
				continue
			}
		case '<':
			end := strings.IndexAny(v[i+1:], "<>")
			if end < 0 {
				return out
			}
			end += i + 1
			if v[end] == '<' {
				i = end
				continue
			}
			id := strings.ToValidUTF8(v[i+1:end], "")
			if id != "" && len(id) <= MaxMessageIDLen && !strings.ContainsFunc(id, func(r rune) bool {
				return unicode.IsSpace(r) || unicode.IsControl(r)
			}) {
				out = append(out, id)
			}
			i = end + 1
			continue
		}
		i++
	}
	return out
}

// skipComment returns the index after the comment that starts at v[i], or
// -1 if it never ends.
func skipComment(v string, i int) int {
	depth := 0
	for ; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// skipQuoted returns the index after the quoted string that starts at v[i],
// or -1 if it never ends.
func skipQuoted(v string, i int) int {
	for i++; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return -1
}

// skipper skips comments and quoted strings in one value. Once a comment
// or a quoted string turned out to have no end, it stops looking for one:
// otherwise a value of nothing but "(" costs a scan to the end for each.
type skipper struct{ noComment, noQuote bool }

func (k *skipper) comment(v string, i int) int {
	if k.noComment {
		return -1
	}
	end := skipComment(v, i)
	k.noComment = end < 0
	return end
}

func (k *skipper) quoted(v string, i int) int {
	if k.noQuote {
		return -1
	}
	end := skipQuoted(v, i)
	k.noQuote = end < 0
	return end
}

// stripComments removes comments outside quoted strings.
func stripComments(s string) string {
	var sk skipper
	if !strings.Contains(s, "(") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '(':
			if end := sk.comment(s, i); end > 0 {
				b.WriteByte(' ')
				i = end
				continue
			}
		case '"':
			if end := sk.quoted(s, i); end > 0 {
				b.WriteString(s[i:end])
				i = end
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// Date is the Date form (RFC 8621 4.1.2.6): the date-time of RFC 5322 3.3,
// obsolete forms included. ok is false if it does not parse.
func Date(v string) (t time.Time, ok bool) {
	if len(v) > 1000 {
		return time.Time{}, false
	}
	// Folding white space may stand between any two tokens, also around
	// the colons of the time (4.3); net/mail wants single spaces.
	s := strings.Join(strings.Fields(stripComments(unfold(v))), " ")
	s = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(s, " :", ":"), ": ", ":"), " ,", ",")
	t, err := mail.ParseDate(s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// URLs is the URLs form (RFC 8621 4.1.2.7): the URLs between angle brackets
// in a List-* field (RFC 2369 2), comments skipped. nil if there are none.
func URLs(v string) []string {
	var sk skipper
	s := unfold(v)
	var out []string
	for i := 0; i < len(s); {
		switch s[i] {
		case '(':
			if end := sk.comment(s, i); end > 0 {
				i = end
				continue
			}
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return out
			}
			// "any white space inside the angle brackets is ignored".
			u := strings.Join(strings.Fields(s[i+1:i+end]), "")
			if u != "" && utf8.ValidString(u) && !strings.Contains(u, "<") && len(out) < MaxAddresses {
				out = append(out, u)
			}
			i += end + 1
			continue
		}
		i++
	}
	return out
}

// Header forms, as in "header:Subject:asText".
const (
	FormRaw              = "asRaw"
	FormText             = "asText"
	FormAddresses        = "asAddresses"
	FormGroupedAddresses = "asGroupedAddresses"
	FormMessageIDs       = "asMessageIds"
	FormDate             = "asDate"
	FormURLs             = "asURLs"
)

// The fields RFC 8621 4.1.2 allows each form for. A field that RFC 5322 and
// RFC 2369 do not define may be read in any form.
var (
	textFields    = fieldSet("Subject", "Comments", "Keywords", "List-Id")
	addressFields = fieldSet("From", "Sender", "Reply-To", "To", "Cc", "Bcc",
		"Resent-From", "Resent-Sender", "Resent-Reply-To", "Resent-To", "Resent-Cc", "Resent-Bcc")
	idFields   = fieldSet("Message-ID", "In-Reply-To", "References", "Resent-Message-ID")
	dateFields = fieldSet("Date", "Resent-Date")
	urlFields  = fieldSet("List-Help", "List-Unsubscribe", "List-Subscribe", "List-Post", "List-Owner", "List-Archive")
	// The rest of RFC 5322: only readable as Raw.
	otherFields = fieldSet("Return-Path", "Received")
)

func fieldSet(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[strings.ToLower(n)] = true
	}
	return m
}

// FormAllowed reports whether a header field may be read in a form.
func FormAllowed(name, form string) bool {
	n := strings.ToLower(name)
	known := textFields[n] || addressFields[n] || idFields[n] || dateFields[n] || urlFields[n] || otherFields[n]
	switch form {
	case FormRaw:
		return true
	case FormText:
		return textFields[n] || !known
	case FormAddresses, FormGroupedAddresses:
		return addressFields[n] || !known
	case FormMessageIDs:
		return idFields[n] || !known
	case FormDate:
		return dateFields[n] || !known
	case FormURLs:
		return urlFields[n] || !known
	}
	return false
}
