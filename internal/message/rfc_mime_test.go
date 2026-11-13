package message

import (
	"fmt"
	"strings"
	"testing"
)

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// RFC 2045 5.1: the two examples that "are completely equivalent", and the
// rules around them: type, subtype and parameter names are
// case-insensitive, a quoted value's quotes are not part of it, comments
// are allowed.
func TestRFC2045ContentType(t *testing.T) {
	for _, v := range []string{
		` text/plain; charset=us-ascii (Plain text)`,
		` text/plain; charset="us-ascii"`,
		` TEXT/Plain; CHARSET=us-ascii`,
		" text/plain;\r\n charset=us-ascii",
		` text / plain (the type) ; charset = "us-ascii"`,
	} {
		typ, params := parseParams(v)
		if typ != "text/plain" || params["charset"] != "us-ascii" || len(params) != 1 {
			t.Errorf("parseParams(%q) = %q %q", v, typ, params)
		}
	}
	// 5.1: "the string 'a;b' ... must be quoted".
	_, params := parseParams(` application/x-stuff; name="a;b=c"; other=1`)
	if params["name"] != "a;b=c" || params["other"] != "1" {
		t.Errorf("quoted value: %q", params)
	}
}

// RFC 2045 5.2: a message without Content-Type is "text/plain;
// charset=us-ascii". RFC 8621 4.1.4 on charset: null for a part with a
// Content-Type that is not text, the implicit us-ascii for text without the
// parameter and for a part without the field.
func TestRFC2045Defaults(t *testing.T) {
	for _, tc := range []struct{ header, typ, charset string }{
		{"", "text/plain", "us-ascii"},
		{"Content-Type: text/html\r\n", "text/html", "us-ascii"},
		{"Content-Type: text/plain; charset=ISO-8859-1\r\n", "text/plain", "iso-8859-1"},
		{"Content-Type: image/png\r\n", "image/png", "<nil>"},
		{"Content-Type: application/json; charset=utf-8\r\n", "application/json", "utf-8"},
		// Not "type/subtype": as if the field were not there.
		{"Content-Type: nonsense\r\n", "text/plain", "us-ascii"},
		{"Content-Type: /\r\n", "text/plain", "us-ascii"},
	} {
		p := Parse([]byte(tc.header + "\r\nbody")).Root
		if p.Type != tc.typ || str(p.Charset) != tc.charset {
			t.Errorf("%q: type %q charset %q", tc.header, p.Type, str(p.Charset))
		}
	}
}

// RFC 2045 6.7, quoted-printable. The rules by number, and the example of
// the soft line break.
func TestRFC2045QuotedPrintable(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// The example of rule 5.
		{"soft line breaks", "Now's the time =\r\nfor all folk to come=\r\n to the aid of their country.",
			"Now's the time for all folk to come to the aid of their country."},
		// (1) "=" and two hex digits is an octet; "=3D" is "=".
		{"rule 1", "a=3Db =E9", "a=b \xe9"},
		// (1) "Uppercase letters must be used", but (2) note: "a robust
		// implementation might choose to recognize" lower case.
		{"lowercase hex", "=e9", "\xe9"},
		// (3) white space at the end of a line is padding and is deleted.
		{"rule 3", "text   \r\nnext\t\r\n", "text\r\nnext\r\n"},
		// (3) an encoded space at the end of a line stays.
		{"encoded trailing space", "text=20\r\n", "text \r\n"},
		// (4) a line break is a line break.
		{"rule 4", "one\r\ntwo\r\n", "one\r\ntwo\r\n"},
		// (2) note: an "=" followed by something else is "to include the
		// '=' character and the following character in the decoded data".
		{"invalid sequence", "a=zzb = c", "a=zzb = c"},
		{"equals at the very end", "abc=", "abc"},
	} {
		if got := string(decodeQuotedPrintable([]byte(tc.in))); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// RFC 2045 6.8, base64, with the test vectors of RFC 4648 10. "Any
// characters outside of the base64 alphabet are to be ignored."
func TestRFC2045Base64(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"Zg==", "f"}, {"Zm8=", "fo"}, {"Zm9v", "foo"}, {"Zm9vYg==", "foob"},
		{"Zm9vYmE=", "fooba"}, {"Zm9vYmFy", "foobar"},
		{"Zm9v\r\nYmFy\r\n", "foobar"},
		{"Zm 9v*Ym\tFy", "foobar"},
		{"Zm9vYg", "foob"}, // padding left off
	} {
		got, ok := decodeBase64([]byte(tc.in))
		if string(got) != tc.want || !ok {
			t.Errorf("decodeBase64(%q) = %q, %v", tc.in, got, ok)
		}
	}
	if got, ok := decodeBase64([]byte("Zm9vY")); string(got) != "foo" || ok {
		t.Errorf("one dangling character: %q, %v", got, ok)
	}
}

// RFC 2045 6.1: Content-Transfer-Encoding values are case-insensitive;
// 6.4: an unknown one is treated like binary data.
func TestRFC2045TransferEncoding(t *testing.T) {
	for _, tc := range []struct{ enc, body, want string }{
		{"BASE64", "aGk=", "hi"},
		{"Quoted-Printable", "h=69", "hi"},
		{"7bit", "h=69", "h=69"},
		{"8bit", "h\xe9", "h\xe9"},
		{"binary", "\x00\x01", "\x00\x01"},
		{"x-unknown", "aGk=", "aGk="},
		{"base64 (comment)", "aGk=", "hi"},
	} {
		p := Parse([]byte("Content-Transfer-Encoding: " + tc.enc + "\r\n\r\n" + tc.body)).Root
		if string(p.Content()) != tc.want || p.Size != int64(len(tc.want)) {
			t.Errorf("%s: content %q size %d", tc.enc, p.Content(), p.Size)
		}
	}
}

// RFC 2046 5.1.1: the example message, with a preamble and an epilogue to
// ignore, a part without any header, and the rule about which line break
// belongs to the boundary.
const rfc2046Multipart = `
From: Nathaniel Borenstein <nsb@bellcore.com>
To: Ned Freed <ned@innosoft.com>
Date: Sun, 21 Mar 1993 23:56:48 -0800 (PST)
Subject: Sample message
MIME-Version: 1.0
Content-type: multipart/mixed; boundary="simple boundary"

This is the preamble.  It is to be ignored, though it
is a handy place for composition agents to include an
explanatory note to non-MIME conformant readers.

--simple boundary

This is implicitly typed plain US-ASCII text.
It does NOT end with a linebreak.
--simple boundary
Content-type: text/plain; charset=us-ascii

This is explicitly typed plain US-ASCII text.
It DOES end with a linebreak.

--simple boundary--

This is the epilogue.  It is also to be ignored.
`

func TestRFC2046MultipartExample(t *testing.T) {
	m := Parse([]byte(crlf(rfc2046Multipart)))
	if m.Root.Type != "multipart/mixed" || m.Root.PartID != "" || len(m.Root.SubParts) != 2 || len(m.Leaves) != 2 {
		t.Fatalf("structure: %s", dump(m.Root))
	}
	one, two := m.Root.SubParts[0], m.Root.SubParts[1]
	if got := string(one.Content()); got != "This is implicitly typed plain US-ASCII text.\r\nIt does NOT end with a linebreak." {
		t.Errorf("part 1: %q", got)
	}
	if got := string(two.Content()); got != "This is explicitly typed plain US-ASCII text.\r\nIt DOES end with a linebreak.\r\n" {
		t.Errorf("part 2: %q", got)
	}
	if one.Type != "text/plain" || str(one.Charset) != "us-ascii" || len(one.Headers) != 0 || one.PartID != "1" || two.PartID != "2" {
		t.Errorf("part 1 properties: %s", dump(one))
	}
	if m.Part("2") != two || m.Part("3") != nil || m.Part("") != nil || m.Part("01") != nil {
		t.Error("Part lookup")
	}
	d, ok := Date(header(t, crlf(rfc2046Multipart), "Date"))
	if !ok || d.Year() != 1993 {
		t.Errorf("date with a comment: %v %v", d, ok)
	}
}

// RFC 2046 5.1.1: the rules of the boundary.
func TestRFC2046Boundaries(t *testing.T) {
	parts := func(body, boundary string) []string {
		var out []string
		for _, p := range splitMultipart([]byte(body), boundary) {
			out = append(out, string(p))
		}
		return out
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	// "The boundary may be followed by zero or more characters of linear
	// whitespace."
	check("transport padding", parts("--b  \t\r\none\r\n--b-- \r\n", "b"), "one")
	// "The boundary delimiter MUST occur at the beginning of a line."
	check("not at the start of a line", parts("--b\r\nxx --b\r\n--b--\r\n", "b"), "xx --b")
	// A line that only starts like the delimiter is content: the nested
	// multiparts of the RFC's own text use boundaries that share a prefix.
	check("a longer boundary", parts("--b\r\none\r\n--bc\r\n--b--\r\n", "b"), "one\r\n--bc")
	// "the CRLF preceding the boundary delimiter line is conceptually
	// attached to the boundary", so a part can be empty or end without one.
	check("empty part", parts("--b\r\n--b\r\n\r\n--b--", "b"), "", "")
	// No closing delimiter: the last part runs to the end.
	check("unterminated", parts("--b\r\none\r\n--b\r\ntwo", "b"), "one", "two")
	check("no delimiter at all", parts("just text", "b"))
	check("bare LF", parts("pre\n--b\none\n--b--\npost", "b"), "one")
	// Boundaries are compared as they are: case matters.
	check("case", parts("--B\r\none\r\n--b--", "b"))
}

// RFC 2046 5.1.5: in a multipart/digest the default type of a part is
// message/rfc822. Its example, shortened to two messages.
func TestRFC2046Digest(t *testing.T) {
	raw := crlf(`
From: Moderator-Address
To: Recipient-List
Date: Mon, 22 Mar 1994 13:34:51 +0000
Subject: Internet Digest, volume 42
MIME-Version: 1.0
Content-Type: multipart/mixed;
              boundary="---- main boundary ----"

------ main boundary ----

  ...Introductory text or table of contents...

------ main boundary ----
Content-Type: multipart/digest;
              boundary="---- next message ----"

------ next message ----

From: someone-else
Date: Fri, 26 Mar 1993 12:59:38 -0800 (PST)
Subject: my opinion

  ...body goes here ...

------ next message ----

From: someone-else-again
Date: Fri, 26 Mar 1993 12:59:38 -0800 (PST)
Subject: my different opinion

  ... another body goes here ...

------ next message ------

------ main boundary ------
`)
	m := Parse([]byte(raw))
	got := dump(m.Root)
	want := "multipart/mixed[1:text/plain multipart/digest[2:message/rfc822 3:message/rfc822]]"
	if got != want {
		t.Fatalf("structure %s, want %s", got, want)
	}
	// A message/rfc822 part is a leaf; its content is the message.
	inner := Parse(m.Part("3").Content())
	if v, _ := Get(inner.Headers, "Subject"); Text(v) != "my different opinion" {
		t.Errorf("inner subject %q", v)
	}
}

// RFC 2046 5.1.4: the multipart/alternative example, and with it RFC 8621
// 4.1.4: of the alternatives, text/plain is the text body and text/html
// would be the HTML body. Here the richer alternative is not HTML, so both
// bodies fall back to the plain part and the other one is an attachment.
func TestRFC2046Alternative(t *testing.T) {
	raw := crlf(`
From: Nathaniel Borenstein <nsb@bellcore.com>
To: Ned Freed <ned@innosoft.com>
Date: Mon, 22 Mar 1993 09:41:09 -0800 (PST)
Subject: Formatted text mail
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary=boundary42

--boundary42
Content-Type: text/plain; charset=us-ascii

  ... plain text version of message goes here ...

--boundary42
Content-Type: text/enriched

  ... RFC 1896 text/enriched version of same message
      goes here ...

--boundary42
Content-Type: application/x-whatever

  ... fanciest version of same message goes here ...

--boundary42--
`)
	m := Parse([]byte(raw))
	text, html, att := m.Bodies()
	if ids(text) != "1" || ids(html) != "1" || ids(att) != "2,3" {
		t.Errorf("text %s html %s attachments %s", ids(text), ids(html), ids(att))
	}
	if got := m.Preview(); got != "... plain text version of message goes here ..." {
		t.Errorf("preview %q", got)
	}
}

// RFC 2231 3: parameter value continuations. Its example, which "is
// semantically identical to" the single long value.
func TestRFC2231Continuations(t *testing.T) {
	typ, params := parseParams(" message/external-body; access-type=URL;\r\n" +
		" URL*0=\"ftp://\";\r\n" +
		" URL*1=\"cs.utk.edu/pub/moore/bulk-mailer/bulk-mailer.tar\"")
	if typ != "message/external-body" || params["access-type"] != "URL" ||
		params["url"] != "ftp://cs.utk.edu/pub/moore/bulk-mailer/bulk-mailer.tar" {
		t.Errorf("%q %q", typ, params)
	}
	// "the ordering of the sections is not significant": the numbers are.
	_, params = parseParams(` x/y; n*1="two"; n*0="one "; n*2=" three"`)
	if params["n"] != "one two three" {
		t.Errorf("out of order: %q", params)
	}
}

// RFC 2231 4: charset and language. Its example.
func TestRFC2231Extended(t *testing.T) {
	_, params := parseParams(" application/x-stuff;\r\n title*=us-ascii'en-us'This%20is%20%2A%2A%2Afun%2A%2A%2A")
	if params["title"] != "This is ***fun***" {
		t.Errorf("%q", params)
	}
	// "the language field ... may be omitted", and so may the charset.
	_, params = parseParams(` x/y; a*=utf-8''caf%C3%A9; b*=''plain%20text; c*=iso-8859-1'fr'caf%E9`)
	if params["a"] != "café" || params["b"] != "plain text" || params["c"] != "café" {
		t.Errorf("%q", params)
	}
}

// RFC 2231 4.1: both together. Its example: extended and plain sections
// mixed, the charset given once, in the first section.
func TestRFC2231Combined(t *testing.T) {
	_, params := parseParams(" application/x-stuff;\r\n" +
		" title*0*=us-ascii'en'This%20is%20even%20more%20;\r\n" +
		" title*1*=%2A%2A%2Afun%2A%2A%2A%20;\r\n" +
		" title*2=\"isn't it!\"")
	if params["title"] != "This is even more ***fun*** isn't it!" {
		t.Errorf("%q", params)
	}
}

// RFC 8621 4.1.4, name: "the decoded filename parameter of the
// Content-Disposition header field ..., or (for compatibility with
// existing systems) if not present, then it's the decoded name parameter
// of the Content-Type header field". Decoded means RFC 2231 and RFC 2047.
func TestRFC8621PartName(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"Content-Disposition: attachment; filename=report.pdf\r\n", "report.pdf"},
		{"Content-Type: application/pdf; name=\"old name.pdf\"\r\n", "old name.pdf"},
		{"Content-Type: application/pdf; name=type.pdf\r\nContent-Disposition: attachment; filename=disposition.pdf\r\n", "disposition.pdf"},
		{"Content-Disposition: attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf\r\n", "résumé.pdf"},
		{"Content-Disposition: attachment;\r\n filename*0*=UTF-8''r%C3%A9;\r\n filename*1*=sum%C3%A9.pdf\r\n", "résumé.pdf"},
		// The RFC 2231 value wins over the fallback for old readers.
		{"Content-Disposition: attachment; filename=\"resume.pdf\"; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf\r\n", "résumé.pdf"},
		// RFC 2047 in a parameter: not allowed, but what most mail does.
		{"Content-Disposition: attachment; filename=\"=?UTF-8?B?csOpc3Vtw6kucGRm?=\"\r\n", "résumé.pdf"},
		{"Content-Type: text/plain\r\n", "<nil>"},
	} {
		p := Parse([]byte(tc.header + "\r\nx")).Root
		if str(p.Name) != tc.want {
			t.Errorf("%q: name %q, want %q", tc.header, str(p.Name), tc.want)
		}
	}
}

// RFC 8621 4.1.4: disposition, cid, language and location.
func TestRFC8621PartProperties(t *testing.T) {
	p := Parse([]byte("Content-Type: image/png\r\nContent-Disposition: INLINE; filename=a.png\r\n" +
		"Content-ID: <part1.abc@example.test>\r\nContent-Language: en, de-CH (Swiss German)\r\n" +
		"Content-Location: http://example.test/a.png\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw==")).Root
	if str(p.Disposition) != "inline" || str(p.CID) != "part1.abc@example.test" ||
		strings.Join(p.Language, "|") != "en|de-CH" || str(p.Location) != "http://example.test/a.png" || p.Size != 4 {
		t.Errorf("%s size %d language %q", dump(p), p.Size, p.Language)
	}
	p = Parse([]byte("Content-Type: text/plain\r\n\r\nx")).Root
	if p.Disposition != nil || p.CID != nil || p.Language != nil || p.Location != nil || p.Name != nil {
		t.Errorf("absent fields are not null: %s", dump(p))
	}
}

// part builds a leaf for the structure tests.
func part(typ, disposition, name string) *Part {
	p := &Part{Type: typ}
	if disposition != "" {
		p.Disposition = &disposition
	}
	if name != "" {
		p.Name = &name
	}
	return p
}

func multi(sub string, parts ...*Part) *Part {
	return &Part{Type: "multipart/" + sub, SubParts: parts}
}

func ids(parts []*Part) string {
	var out []string
	for _, p := range parts {
		out = append(out, p.PartID)
	}
	return strings.Join(out, ",")
}

// dump shows a body structure on one line.
func dump(p *Part) string {
	if p.IsMultipart() {
		var subs []string
		for _, c := range p.SubParts {
			subs = append(subs, dump(c))
		}
		return p.Type + "[" + strings.Join(subs, " ") + "]"
	}
	return p.PartID + ":" + p.Type
}

// RFC 8621 4.1.4: the example structure, "a message with both text and
// HTML versions that has gone through a list software manager that
// attaches a header and footer", and the lists the RFC says it flattens
// to.
func TestRFC8621BodyFlattening(t *testing.T) {
	A := part("text/plain", "inline", "")
	B := part("text/plain", "inline", "")
	C := part("image/jpeg", "inline", "")
	D := part("text/plain", "inline", "")
	E := part("text/html", "", "")
	F := part("image/jpeg", "", "")
	G := part("image/jpeg", "attachment", "")
	H := part("application/x-excel", "", "")
	J := part("message/rfc822", "", "")
	K := part("text/plain", "inline", "")
	for id, p := range map[string]*Part{"A": A, "B": B, "C": C, "D": D, "E": E, "F": F, "G": G, "H": H, "J": J, "K": K} {
		p.PartID = id
	}
	root := multi("mixed",
		A,
		multi("mixed",
			multi("alternative",
				multi("mixed", B, C, D),
				multi("related", E, F)),
			G, H, J),
		K)
	m := &Message{Root: root}
	text, html, att := m.Bodies()
	if ids(text) != "A,B,C,D,K" {
		t.Errorf("textBody %s, want A,B,C,D,K", ids(text))
	}
	if ids(html) != "A,E,K" {
		t.Errorf("htmlBody %s, want A,E,K", ids(html))
	}
	if ids(att) != "C,F,G,H,J" {
		t.Errorf("attachments %s, want C,F,G,H,J", ids(att))
	}
	if !m.HasAttachment() {
		t.Error("hasAttachment is false")
	}
}

// RFC 8621 4.1.4, the cases its algorithm spells out.
func TestRFC8621BodyCases(t *testing.T) {
	var walk func(p *Part, n *int)
	walk = func(p *Part, n *int) {
		if p.IsMultipart() {
			for _, c := range p.SubParts {
				walk(c, n)
			}
			return
		}
		*n++
		p.PartID = fmt.Sprint(*n)
	}
	for _, tc := range []struct {
		name            string
		root            *Part
		text, html, att string
		hasAttachment   bool
	}{
		{"plain only", part("text/plain", "", ""), "1", "1", "", false},
		{"html only", part("text/html", "", ""), "1", "1", "", false},
		{"alternative", multi("alternative", part("text/plain", "", ""), part("text/html", "", "")), "1", "2", "", false},
		// "Found HTML part only": it is also the text body.
		{"alternative with html only", multi("alternative", part("text/html", "", "")), "1", "1", "", false},
		{"text and a file", multi("mixed", part("text/plain", "", ""), part("application/pdf", "attachment", "a.pdf")),
			"1", "1", "2", true},
		// A text part with a file name that is not first is an attachment.
		{"named text", multi("mixed", part("text/plain", "", ""), part("text/plain", "", "notes.txt")), "1", "1", "2", true},
		// ... but the first part is the body whatever it is called.
		{"named first", multi("mixed", part("text/plain", "", "body.txt"), part("text/plain", "", "")), "1,2", "1,2", "", false},
		{"disposition attachment", multi("mixed", part("text/plain", "attachment", "")), "", "", "1", true},
		// In multipart/related only the first part is the body; the rest
		// are its resources.
		{"related", multi("related", part("text/html", "", ""), part("image/png", "inline", "")), "1", "1", "2", false},
		// An inline image between text parts is part of both bodies and
		// not an attachment.
		{"inline image", multi("mixed", part("text/plain", "", ""), part("image/png", "inline", ""), part("text/plain", "", "")),
			"1,2,3", "1,2,3", "", false},
		{"signed", multi("signed", part("text/plain", "", ""), part("application/pgp-signature", "", "")), "1", "1", "2", false},
		{"empty multipart", multi("mixed"), "", "", "", false},
	} {
		n := 0
		walk(tc.root, &n)
		m := &Message{Root: tc.root}
		text, html, att := m.Bodies()
		if ids(text) != tc.text || ids(html) != tc.html || ids(att) != tc.att || m.HasAttachment() != tc.hasAttachment {
			t.Errorf("%s: text %q html %q attachments %q hasAttachment %v", tc.name, ids(text), ids(html), ids(att), m.HasAttachment())
		}
	}
}

// RFC 8621 4.1.4, EmailBodyValue: the text after the transfer encoding and
// the charset are undone, with CRLF as LF; isEncodingProblem for a charset
// we do not know or bytes that are not valid in it; isTruncated when cut
// to maxBodyValueBytes, and "MUST ensure the truncation results in valid
// UTF-8".
func TestRFC8621BodyValue(t *testing.T) {
	p := Parse([]byte("Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"caf=E9\r\nline two\r\n")).Root
	v, problem, truncated := p.BodyValue(0)
	if v != "café\nline two\n" || problem || truncated {
		t.Errorf("%q %v %v", v, problem, truncated)
	}
	// "é" is two bytes: a cut at 4 would split it.
	v, _, truncated = p.BodyValue(4)
	if v != "caf" || !truncated {
		t.Errorf("truncated: %q %v", v, truncated)
	}
	v, _, truncated = p.BodyValue(5)
	if v != "café" || !truncated {
		t.Errorf("truncated at 5: %q %v", v, truncated)
	}

	p = Parse([]byte("Content-Type: text/plain; charset=x-klingon\r\n\r\nplain")).Root
	if v, problem, _ := p.BodyValue(0); v != "plain" || !problem {
		t.Errorf("unknown charset: %q %v", v, problem)
	}
	p = Parse([]byte("Content-Type: text/plain; charset=utf-8\r\n\r\nbad \xff byte")).Root
	if v, problem, _ := p.BodyValue(0); v != "bad \xef\xbf\xbd byte" || !problem {
		t.Errorf("invalid utf-8: %q %v", v, problem)
	}
	// 8-bit bytes in a part that claims us-ascii.
	p = Parse([]byte("\r\ncaf\xe9")).Root
	if _, problem, _ := p.BodyValue(0); !problem {
		t.Error("8-bit us-ascii is no encoding problem")
	}
}

// RFC 8621 4.1.4, preview: plain text, at most 256 characters; made from
// the HTML when there is no plain text.
func TestRFC8621Preview(t *testing.T) {
	m := Parse([]byte("Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><head><title>T</title><style>p{color:red}</style></head><body><script>alert(1)</script>" +
		"<p>Hello&nbsp;<b>world</b></p><p>second &amp; last</p></body></html>"))
	if got := m.Preview(); got != "Hello world second & last" {
		t.Errorf("html preview %q", got)
	}
	m = Parse([]byte("\r\n" + strings.Repeat("word ", 200)))
	if n := len([]rune(m.Preview())); n > PreviewLen || n < PreviewLen-5 {
		t.Errorf("preview of %d characters", n)
	}
	if got := Parse([]byte("Content-Type: image/png\r\n\r\nxx")).Preview(); got != "" {
		t.Errorf("image preview %q", got)
	}
}

// A real-world shaped message through the whole parser: alternative body
// inside mixed, an attachment with an encoded name.
func TestParseWhole(t *testing.T) {
	raw := crlf(`
From: =?UTF-8?Q?Ren=C3=A9e?= <renee@example.test>
To: you@example.test
Subject: =?UTF-8?B?UmFwcG9ydCDigJMgUTM=?=
Message-ID: <m1@example.test>
Date: Fri, 13 Nov 2026 10:00:00 +0100
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Hoi,=20
het rapport staat in de bijlage.

--inner
Content-Type: text/html; charset=utf-8

<p>Hoi,<br>het rapport staat in de <b>bijlage</b>.</p>
--inner--
--outer
Content-Type: application/pdf
Content-Disposition: attachment; filename*=UTF-8''rapport%20Q3.pdf
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--outer--
`)
	m := Parse([]byte(raw))
	if got := dump(m.Root); got != "multipart/mixed[multipart/alternative[1:text/plain 2:text/html] 3:application/pdf]" {
		t.Fatalf("structure %s", got)
	}
	text, html, att := m.Bodies()
	if ids(text) != "1" || ids(html) != "2" || ids(att) != "3" || !m.HasAttachment() {
		t.Errorf("text %s html %s attachments %s", ids(text), ids(html), ids(att))
	}
	if got := Text(header(t, raw, "Subject")); got != "Rapport – Q3" {
		t.Errorf("subject %q", got)
	}
	if got := m.Preview(); got != "Hoi, het rapport staat in de bijlage." {
		t.Errorf("preview %q", got)
	}
	pdf := m.Part("3")
	if str(pdf.Name) != "rapport Q3.pdf" || string(pdf.Content()) != "%PDF-1.4\n" || pdf.Size != 9 || pdf.Charset != nil {
		t.Errorf("attachment: %s name %q content %q", dump(pdf), str(pdf.Name), pdf.Content())
	}
}

// Hostile input: the limits of the package.
func TestLimits(t *testing.T) {
	// Multiparts nested deeper than MaxDepth: the structure stops there.
	var b strings.Builder
	const depth = MaxDepth + 50
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=b%d\r\n\r\n--b%d\r\n", i, i)
	}
	b.WriteString("\r\ndeep\r\n")
	m := Parse([]byte(b.String()))
	got := 0
	for p := m.Root; p.IsMultipart() && len(p.SubParts) > 0; p = p.SubParts[0] {
		got++
	}
	if got != MaxDepth {
		t.Errorf("depth %d, want %d", got, MaxDepth)
	}

	// More parts than MaxParts: the rest is dropped.
	b.Reset()
	b.WriteString("Content-Type: multipart/mixed; boundary=b\r\n\r\n")
	for i := 0; i < MaxParts+500; i++ {
		b.WriteString("--b\r\n\r\nx\r\n")
	}
	b.WriteString("--b--\r\n")
	m = Parse([]byte(b.String()))
	if n := len(m.Root.SubParts); n != MaxParts-1 {
		t.Errorf("%d parts, want %d", n, MaxParts-1)
	}

	// More header fields than MaxHeaderFields: the rest is ignored, the
	// body is still found.
	b.Reset()
	for i := 0; i < MaxHeaderFields+100; i++ {
		fmt.Fprintf(&b, "X-%d: v\r\n", i)
	}
	b.WriteString("\r\nbody")
	m = Parse([]byte(b.String()))
	if len(m.Headers) != MaxHeaderFields || string(m.Root.Content()) != "body" {
		t.Errorf("%d fields, body %q", len(m.Headers), m.Root.Content())
	}

	// More addresses than MaxAddresses.
	if n := len(Addresses(strings.Repeat("a@x.test,", MaxAddresses+10))); n != MaxAddresses {
		t.Errorf("%d addresses", n)
	}
	// Comments nested without end, quotes never closed: nothing hangs.
	Addresses(strings.Repeat("(", 100000))
	Addresses(strings.Repeat(`"\`, 100000))
	MessageIDs(strings.Repeat("<(", 100000))
	parseParams("a/b" + strings.Repeat(`; x*0*="`, 10000))
	stripComments(strings.Repeat("(", 1<<20))
	parseParams("a/b;" + strings.Repeat("(;", 1<<19))
	Addresses(strings.Repeat("[", 1<<20))
	URLs(strings.Repeat("(<", 1<<19))
	DecodeWords(strings.Repeat("=?utf-8?q?", 1<<17))
	DecodeWords(strings.Repeat("=?", 1<<19))
}
