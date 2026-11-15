package search

import (
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func words(tokens []Token) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = t.Text
	}
	return out
}

func TestTokens(t *testing.T) {
	for in, want := range map[string][]string{
		"":                       {},
		"Hello, World!":          {"hello", "world"},
		"aaa_old x-foo":          {"aaa", "old", "x", "foo"},
		"bob@example.test":       {"bob", "example", "test"},
		"Café RÉSUMÉ Müller":     {"cafe", "resume", "muller"},
		"café":                  {"cafe"}, // a combining accent
		"日本語 text":               {"日本語", "text"},
		"don't 3.14 v2":          {"don", "t", "3", "14", "v2"},
		"  \t\r\n ":              {},
		"ΑΘΗΝΑ Ωμέγα":            {"αθηνα", "ωμεγα"},
		strings.Repeat("x", 100): {strings.Repeat("x", maxTokenRunes)},
		"a\xffb":                 {"a", "b"},
	} {
		got := words(Tokens(in))
		if len(got) == 0 {
			got = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Tokens(%q) = %q, want %q", in, got, want)
		}
	}
	// Offsets name the word in the original.
	text := "The Café is open"
	tok := Tokens(text)[1]
	if text[tok.Start:tok.End] != "Café" {
		t.Errorf("offsets: %q", text[tok.Start:tok.End])
	}
}

// RFC 8621 4.4.1: words are all required; text in double quotes is a phrase.
func TestParseQuery(t *testing.T) {
	for in, want := range map[string][]Term{
		"":                      nil,
		"banana":                {{"banana"}},
		"banana supply":         {{"banana"}, {"supply"}},
		`"banana supply" chain`: {{"banana", "supply"}, {"chain"}},
		`chain "banana supply"`: {{"chain"}, {"banana", "supply"}},
		`"unclosed phrase here`: {{"unclosed", "phrase", "here"}},
		`a"b c"d`:               {{"a"}, {"b", "c"}, {"d"}},
		"bob@example.test":      {{"bob", "example", "test"}},
		`"" !!! ...`:            nil,
		"AND OR NOT NEAR":       {{"and"}, {"or"}, {"not"}, {"near"}},
		`x* col:y ^z (w)`:       {{"x"}, {"col", "y"}, {"z"}, {"w"}},
	} {
		if got := ParseQuery(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseQuery(%q) = %q, want %q", in, got, want)
		}
	}
	many := ParseQuery(strings.Repeat("w ", 500))
	if len(many) != MaxTerms {
		t.Errorf("%d terms, want the limit %d", len(many), MaxTerms)
	}
	if got := Phrase(strings.Repeat("w ", 500)); len(got) != MaxTermTokens {
		t.Errorf("%d tokens in a phrase, want the limit %d", len(got), MaxTermTokens)
	}
}

func TestFTSQuery(t *testing.T) {
	if got := FTSQuery(ParseQuery(`"banana supply" chain`)); got != `"banana supply" AND "chain"` {
		t.Errorf("got %s", got)
	}
	if got := FTSQuery(nil); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestHighlight(t *testing.T) {
	for _, c := range []struct {
		text, query string
		max         int
		want        string
		ok          bool
	}{
		{"Meeting about bananas", "bananas", 0, "Meeting about <mark>bananas</mark>", true},
		{"Meeting about bananas", "banana", 0, "", false},
		{"A <b>bold</b> & brave Café", "cafe bold", 0, "A &lt;b&gt;<mark>bold</mark>&lt;/b&gt; &amp; brave <mark>Café</mark>", true},
		{"one two three two one", `"two three"`, 0, "one <mark>two three</mark> two one", true},
		{"one two three", `"one two" "two three"`, 0, "<mark>one two three</mark>", true},
		{"x x x", "x", 0, "<mark>x</mark> <mark>x</mark> <mark>x</mark>", true},
		{"", "x", 0, "", false},
		{"some text", "", 0, "", false},
	} {
		got, ok := Highlight(c.text, ParseQuery(c.query), c.max)
		if got != c.want || ok != c.ok {
			t.Errorf("Highlight(%q, %q) = %q %v, want %q %v", c.text, c.query, got, ok, c.want, c.ok)
		}
	}
	long := strings.Repeat("filler word ", 100) + "the needle is here " + strings.Repeat("more filler ", 100)
	got, ok := Highlight(long, ParseQuery("needle"), 60)
	if !ok || !strings.Contains(got, "<mark>needle</mark>") || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("cut: %q", got)
	}
	if n := utf8.RuneCountInString(strings.NewReplacer("<mark>", "", "</mark>", "", "…", "").Replace(got)); n > 60 {
		t.Errorf("cut to %d characters, want at most 60", n)
	}
	if !Contains("the banana supply chain", ParseQuery(`chain "banana supply"`)) || Contains("banana", ParseQuery("banana chain")) || Contains("x", nil) {
		t.Error("Contains")
	}
}

// The header condition: a value's words are found only behind the marker
// of their own field.
func TestHeaderDoc(t *testing.T) {
	var d HeaderDoc
	d.Add("X-Foo", "bar baz")
	d.Add("X-Empty", "")
	d.Add("X-Bar", "baz")
	doc := Tokens(d.String())
	has := func(name, value string) bool {
		q := strings.Trim(HeaderFTS(name, value), `"`)
		return len(matches(doc, []Term{strings.Fields(q)})) > 0
	}
	for _, c := range []struct {
		name, value string
		want        bool
	}{
		{"x-foo", "", true}, {"X-FOO", "bar", true}, {"x-foo", "bar baz", true}, {"x-foo", "baz", true},
		{"x-foo", "baz bar", false}, {"x-bar", "bar", false}, {"x-bar", "baz", true}, {"x-empty", "", true},
		{"x-empty", "bar", false}, {"x-nope", "", false}, {"x-fo", "", false}, {"x-foo", "baz baz", false},
	} {
		if got := has(c.name, c.value); got != c.want {
			t.Errorf("header %q %q: %v, want %v", c.name, c.value, got, c.want)
		}
	}
}

// What is built for FTS5 from any input is made of quoted strings of
// letters and digits, AND, and nothing else: no input can write a query.
func FuzzQuery(f *testing.F) {
	for _, s := range []string{"banana", `"a b" c`, `") OR (subject: x`, "a\x00b", `x" NEAR "y`, "\xff\xfe", `{body} : x*`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		terms := ParseQuery(s)
		if len(terms) > MaxTerms {
			t.Fatalf("%d terms", len(terms))
		}
		check := func(q string) {
			for _, part := range strings.Split(q, " AND ") {
				if q == "" {
					continue
				}
				if len(part) < 3 || part[0] != '"' || part[len(part)-1] != '"' {
					t.Fatalf("not a quoted string: %q in %q", part, q)
				}
				for _, r := range part[1 : len(part)-1] {
					// A mark that NFD splits off a letter and that is not
					// removed as a diacritic (a spacing mark, such as the
					// second half of Tamil AU) stays in the word. It is no
					// syntax of FTS5, inside quotes least of all.
					if r != ' ' && !isTokenRune(r) && !unicode.Is(unicode.M, r) {
						t.Fatalf("%q in %q", r, q)
					}
				}
			}
		}
		check(FTSQuery(terms))
		check(HeaderFTS(s, s))
		out, _ := Highlight(s, terms, 50)
		if !utf8.ValidString(out) {
			t.Fatalf("Highlight made invalid UTF-8: %q", out)
		}
		rest := strings.NewReplacer("<mark>", "", "</mark>", "").Replace(out)
		if strings.ContainsAny(rest, "<>") {
			t.Fatalf("unescaped markup: %q", out)
		}
		var d HeaderDoc
		d.Add(s, s)
		Tokens(d.String())
	})
}
