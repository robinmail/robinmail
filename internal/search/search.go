// Package search is the part of full-text search that is not SQL: how text
// becomes tokens, how a query string becomes terms, and how a snippet with
// the matches marked is cut from a text.
//
// The index itself is an FTS5 table in rqlite (internal/store). Its
// tokenizer is SQLite's unicode61 with diacritics removed; Tokens here
// follows the same rule closely enough: a token is a run of letters and
// digits, compared without case and without diacritics. Where the two
// differ for some character, a query still finds what the index holds,
// because the query is given to FTS5 as text and tokenized there; only
// the marks of a snippet can then be off.
package search

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Limits on a query. Text is untrusted and a query is run by SQLite.
const (
	MaxTerms      = 32
	MaxTermTokens = 32
	maxTokenRunes = 64
)

// Token is one word of a text: Text is what is compared, Start and End are
// its place in the original, in bytes.
type Token struct {
	Text       string
	Start, End int
}

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

// fold makes a word comparable: lower case, no diacritics.
func fold(word string) string {
	ascii := true
	for i := 0; i < len(word); i++ {
		if word[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(word)
	}
	var b strings.Builder
	for _, r := range norm.NFD.String(word) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Tokens cuts text into its words. A combining mark stays with the letter
// it follows.
func Tokens(text string) []Token {
	var out []Token
	start := -1
	for i, r := range text {
		in := isTokenRune(r) || (start >= 0 && unicode.Is(unicode.Mn, r))
		if in && start < 0 {
			start = i
		}
		if !in && start >= 0 {
			out = appendToken(out, text, start, i)
			start = -1
		}
	}
	if start >= 0 {
		out = appendToken(out, text, start, len(text))
	}
	return out
}

func appendToken(out []Token, text string, start, end int) []Token {
	word := text[start:end]
	if utf8.RuneCountInString(word) > maxTokenRunes {
		// Cut the way a search for the same long word is cut.
		n := 0
		for i := range word {
			if n == maxTokenRunes {
				word = word[:i]
				break
			}
			n++
		}
	}
	if f := fold(word); f != "" {
		out = append(out, Token{Text: f, Start: start, End: end})
	}
	return out
}

// Term is one thing a text must contain: these words, one after the other.
type Term []string

// Phrase is the whole of s as one term: what IMAP's SEARCH BODY means by a
// string, at the grain of words.
func Phrase(s string) Term {
	var t Term
	for _, tok := range Tokens(s) {
		if len(t) == MaxTermTokens {
			break
		}
		t = append(t, tok.Text)
	}
	return t
}

// ParseQuery reads what a person typed into a search box (RFC 8621 4.4.1,
// the text conditions): words that must all be there, and "quoted text"
// that must be there as it is written. A word with punctuation inside,
// such as an address, is the phrase of its parts. What has no letter or
// digit in it is dropped; a query of only that has no terms.
func ParseQuery(s string) []Term {
	var terms []Term
	add := func(piece string) {
		if t := Phrase(piece); len(t) > 0 && len(terms) < MaxTerms {
			terms = append(terms, t)
		}
	}
	for s != "" {
		q := strings.IndexByte(s, '"')
		if q < 0 {
			for _, w := range strings.Fields(s) {
				add(w)
			}
			break
		}
		for _, w := range strings.Fields(s[:q]) {
			add(w)
		}
		s = s[q+1:]
		end := strings.IndexByte(s, '"')
		if end < 0 {
			end = len(s)
			add(s)
			break
		}
		add(s[:end])
		s = s[end+1:]
	}
	return terms
}

// FTS is the term as an FTS5 phrase. The tokens hold only letters and
// digits, so nothing in them can end the string.
func (t Term) FTS() string {
	return `"` + strings.ReplaceAll(strings.Join(t, " "), `"`, "") + `"`
}

// FTSQuery is all terms, each required, as an FTS5 expression; empty
// without terms.
func FTSQuery(terms []Term) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		if len(t) > 0 {
			parts = append(parts, t.FTS())
		}
	}
	return strings.Join(parts, " AND ")
}

// span is a match: tokens [from, to) of a text.
type span struct{ from, to int }

func matches(tokens []Token, terms []Term) []span {
	var out []span
	for i := range tokens {
		for _, t := range terms {
			if len(t) == 0 || i+len(t) > len(tokens) {
				continue
			}
			ok := true
			for k, w := range t {
				if tokens[i+k].Text != w {
					ok = false
					break
				}
			}
			if ok {
				out = append(out, span{i, i + len(t)})
			}
		}
	}
	return out
}

// Contains reports whether text has every term.
func Contains(text string, terms []Term) bool {
	tokens := Tokens(text)
	for _, t := range terms {
		if len(matches(tokens, []Term{t})) == 0 {
			return false
		}
	}
	return len(terms) > 0
}

// Highlight returns text as HTML with every match of a term between <mark>
// and </mark> (RFC 8621 5), and whether there was one. With max > 0 the
// result shows at most about max characters of the text, starting a little
// before the first match, with an ellipsis where it was cut.
func Highlight(text string, terms []Term, max int) (string, bool) {
	text = strings.ToValidUTF8(text, "�")
	tokens := Tokens(text)
	found := matches(tokens, terms)
	if len(found) == 0 {
		return "", false
	}
	// marked[i] says byte i is inside a match.
	type edge struct{ start, end int }
	var edges []edge
	for _, m := range found {
		e := edge{tokens[m.from].Start, tokens[m.to-1].End}
		if n := len(edges); n > 0 && e.start <= edges[n-1].end {
			if e.end > edges[n-1].end {
				edges[n-1].end = e.end
			}
			continue
		}
		edges = append(edges, e)
	}

	from, to := 0, len(text)
	if max > 0 && utf8.RuneCountInString(text) > max {
		// Start up to a fifth of the room before the first match, at the
		// start of a word.
		from = edges[0].start
		lead := max / 5
		for lead > 0 && from > 0 {
			_, size := utf8.DecodeLastRuneInString(text[:from])
			from -= size
			lead--
		}
		for from > 0 && from < edges[0].start {
			r, _ := utf8.DecodeLastRuneInString(text[:from])
			if !isTokenRune(r) {
				break
			}
			_, size := utf8.DecodeRuneInString(text[from:])
			from += size
		}
		to = from
		for n := 0; n < max && to < len(text); n++ {
			_, size := utf8.DecodeRuneInString(text[to:])
			to += size
		}
	}

	var b strings.Builder
	if from > 0 {
		b.WriteString("…")
	}
	pos := from
	for _, e := range edges {
		if e.start >= to {
			break
		}
		if e.end <= from {
			continue
		}
		start, end := e.start, e.end
		if start < from {
			start = from
		}
		if end > to {
			end = to
		}
		b.WriteString(html.EscapeString(text[pos:start]))
		b.WriteString("<mark>")
		b.WriteString(html.EscapeString(text[start:end]))
		b.WriteString("</mark>")
		pos = end
	}
	b.WriteString(html.EscapeString(text[pos:to]))
	if to < len(text) {
		b.WriteString("…")
	}
	return b.String(), true
}

// headerMarker is the token that stands for a header field's name in the
// index: letters and digits only, and nothing a message would contain by
// accident.
func headerMarker(name string) string {
	const hexdigits = "0123456789abcdef"
	var b strings.Builder
	b.WriteString("zqh")
	for i := 0; i < len(name) && i < 40; i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(hexdigits[c>>4])
		b.WriteByte(hexdigits[c&15])
	}
	return b.String()
}

// Limits on what of a header section is indexed.
const (
	maxHeaderFields = 200
	maxHeaderTokens = 40
)

// HeaderDoc is the text that is indexed for the header fields of a message,
// so that "has a field X" and "field X contains these words" (the header
// condition of RFC 8621 4.4.1) are phrases in it: every word of a field's
// value is written after a marker for the field's name. "X-Foo: bar baz"
// becomes "M bar M baz", with M the marker; a field without words is one M.
// A search for "bar baz" in X-Foo is the phrase "M bar M baz", which no
// other field can satisfy.
type HeaderDoc struct {
	b strings.Builder
	n int
}

// Add indexes one field. value is the decoded text of the field.
func (d *HeaderDoc) Add(name, value string) {
	if d.n >= maxHeaderFields || name == "" {
		return
	}
	d.n++
	m := headerMarker(name)
	tokens := Tokens(value)
	if len(tokens) > maxHeaderTokens {
		tokens = tokens[:maxHeaderTokens]
	}
	if len(tokens) == 0 {
		d.b.WriteString(m)
		d.b.WriteByte(' ')
	}
	for _, t := range tokens {
		d.b.WriteString(m)
		d.b.WriteByte(' ')
		d.b.WriteString(t.Text)
		d.b.WriteByte(' ')
	}
	// A break that no phrase crosses: a marker of no field.
	d.b.WriteString("zqhend ")
}

func (d *HeaderDoc) String() string { return d.b.String() }

// HeaderFTS is the FTS5 phrase for "the field name is there" (value empty
// or without words) or "the field name has these words in a row".
func HeaderFTS(name, value string) string {
	m := headerMarker(name)
	t := Phrase(value)
	if len(t) > maxHeaderTokens {
		t = t[:maxHeaderTokens]
	}
	if len(t) == 0 {
		return `"` + m + `"`
	}
	var b strings.Builder
	b.WriteByte('"')
	for i, w := range t {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(m)
		b.WriteByte(' ')
		b.WriteString(w)
	}
	b.WriteByte('"')
	return b.String()
}
