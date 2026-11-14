package message

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// Tests for writing messages. What Build and the Format functions write is
// read back three ways: by our own parser, by Go's net/mail, and by Go's
// mime and mime/multipart. Three readers that agree on what we wrote is
// the best check there is short of other people's mail clients.

func sp(s string) *string { return &s }

// lines checks RFC 5322 2.1.1 on what we wrote: CRLF only, no line over
// 998 octets, and how many are over the 78 that lines "SHOULD" keep to.
func lines(t *testing.T, raw []byte) (over78 int) {
	t.Helper()
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) || bytes.ContainsRune(bytes.ReplaceAll(raw, []byte("\r\n"), nil), '\r') {
		t.Errorf("a bare CR or LF in:\n%q", raw)
	}
	for _, l := range bytes.Split(raw, []byte("\r\n")) {
		if len(l) > 998 {
			t.Errorf("a line of %d octets", len(l))
		}
		if len(l) > 78 {
			over78++
		}
	}
	return over78
}

// RFC 5322 2.2.3 and 3.6.5: an unstructured field folds at white space,
// and unfolding gives the text back.
func TestRFC5322FoldingText(t *testing.T) {
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 8)
	for _, text := range []string{
		"Saying Hello",
		"",
		long,
		"two  spaces and a trailing one ",
		strings.Repeat("x", 300), // one word longer than a line: it cannot fold
	} {
		v := FormatText("Subject", text)
		raw := []byte("Subject:" + v + "\r\n")
		if !ValidRaw(v) {
			t.Errorf("%q is not a valid field value", v)
		}
		if n := lines(t, raw); n > 0 && !strings.Contains(text, strings.Repeat("x", 100)) {
			t.Errorf("%d lines over 78 for %q:\n%s", n, text, raw)
		}
		if got := Text(v); got != strings.TrimLeft(text, " ") {
			t.Errorf("our parser reads %q, want %q", got, text)
		}
		m, err := mail.ReadMessage(bytes.NewReader(append(raw, "\r\n"...)))
		if err != nil {
			t.Fatal(err)
		}
		if got := m.Header.Get("Subject"); got != strings.TrimSpace(text) {
			t.Errorf("net/mail reads %q, want %q", got, text)
		}
	}
}

// RFC 2047 2 and 5: an encoded word is at most 75 characters, a line with
// one at most 76, a character is never split over two words, and words
// next to each other join without the white space between them (6.2).
func TestRFC2047EncodedWords(t *testing.T) {
	dec := new(mime.WordDecoder)
	for _, text := range []string{
		"Größe", "Keld Jørn Simonsen", "André Pirard",
		"日本語のメールの件名はとても長くなることがあります。本当に長いです。",
		"Re: [lijst] “Vergadering” over het €-budget van volgend jaar, met bijlagen en nog wat woorden",
		"a=?b?c",              // looks like the start of an encoded word
		"tab\there",           // a control character
		"😀 emoji at the start", // four-byte characters
		strings.Repeat("é", 200),
	} {
		v := FormatText("Subject", text)
		raw := "Subject:" + v
		for _, l := range strings.Split(raw, "\r\n") {
			if len(l) > 76 {
				t.Errorf("%q: line of %d characters with an encoded word: %q", text, len(l), l)
			}
			for _, w := range strings.Fields(strings.TrimPrefix(l, "Subject:")) {
				if strings.HasPrefix(w, "=?") {
					if len(w) > 75 {
						t.Errorf("encoded word of %d characters: %q", len(w), w)
					}
					// Each word decodes on its own: no split character.
					if _, err := dec.Decode(w); err != nil {
						t.Errorf("%q does not decode alone: %v", w, err)
					}
				}
			}
		}
		if got := Text(v); got != text {
			t.Errorf("our parser reads %q, want %q", got, text)
		}
		got, err := dec.DecodeHeader(strings.ReplaceAll(v, "\r\n", ""))
		if err != nil || strings.TrimSpace(got) != text {
			t.Errorf("mime reads %q (%v), want %q", got, err, text)
		}
	}
	// RFC 2047 8, the examples: we read them the same as what we write.
	for encoded, want := range map[string]string{
		"=?ISO-8859-1?Q?Andr=E9?= Pirard <PIRARD@vm1.ulg.ac.be>": "André Pirard",
		"=?US-ASCII?Q?Keith_Moore?= <moore@cs.utk.edu>":          "Keith Moore",
	} {
		a := Addresses(encoded)
		if len(a) != 1 || a[0].Name == nil || *a[0].Name != want {
			t.Errorf("%s: %+v", encoded, a)
		}
	}
}

// RFC 5322 3.4 and RFC 2047 5 (3): display names as atoms, as a quoted
// string when they have specials, as encoded words when not ASCII; and an
// encoded word never inside a quoted string.
func TestRFC5322Addresses(t *testing.T) {
	list := []Address{
		{Name: sp("Joe Q. Public"), Email: "john.q.public@example.com"}, // RFC 5322 A.1.2
		{Name: sp("Mary Smith"), Email: "mary@x.test"},
		{Email: "jdoe@example.org"},
		{Name: sp("Giant; \"Big\" Box"), Email: "sysservices@example.net"},
		{Name: sp("Keld Jørn Simonsen"), Email: "keld@dkuug.dk"},
		{Name: sp("山田 太郎"), Email: "taro@example.jp"},
		{Name: sp(""), Email: "noname@example.com"},
		{Name: sp("O'Neil, Pat"), Email: "pat@example.com"},
	}
	for i := 0; i < 20; i++ {
		list = append(list, Address{Name: sp("Person Number"), Email: "person" + string(rune('a'+i)) + "@a-rather-long-domain-name.example"})
	}
	v := FormatAddresses("To", list)
	raw := []byte("To:" + v + "\r\n")
	if n := lines(t, raw); n > 0 {
		t.Errorf("%d lines over 78:\n%s", n, raw)
	}
	if strings.Contains(v, `"=?`) {
		t.Errorf("an encoded word inside a quoted string: %s", v)
	}
	if !strings.Contains(v, `"Joe Q. Public" <john.q.public@example.com>`) || !strings.Contains(v, "Mary Smith <mary@x.test>") {
		t.Errorf("names not written as the RFC's examples have them: %s", v)
	}
	got := Addresses(v)
	if len(got) != len(list) {
		t.Fatalf("our parser reads %d addresses, want %d", len(got), len(list))
	}
	std, err := mail.ParseAddressList(strings.ReplaceAll(v, "\r\n", ""))
	if err != nil || len(std) != len(list) {
		t.Fatalf("net/mail: %v, %d addresses", err, len(std))
	}
	for i, want := range list {
		name := ""
		if want.Name != nil {
			name = *want.Name
		}
		gotName := ""
		if got[i].Name != nil {
			gotName = *got[i].Name
		}
		if got[i].Email != want.Email || gotName != name {
			t.Errorf("our parser: %d is %q <%s>, want %q <%s>", i, gotName, got[i].Email, name, want.Email)
		}
		if std[i].Address != want.Email || std[i].Name != name {
			t.Errorf("net/mail: %d is %q <%s>, want %q <%s>", i, std[i].Name, std[i].Address, name, want.Email)
		}
	}

	// RFC 5322 3.4, groups; the example of appendix A.1.3.
	groups := []Group{
		{Name: sp("A Group"), Addresses: []Address{{Name: sp("Ed Jones"), Email: "c@a.test"}, {Email: "joe@where.test"}, {Name: sp("John"), Email: "jdoe@one.test"}}},
		{Addresses: []Address{{Email: "loose@example.com"}}},
		{Name: sp("Undisclosed recipients"), Addresses: []Address{}},
	}
	gv := FormatGroups("To", groups)
	if want := " A Group: Ed Jones <c@a.test>, joe@where.test, John <jdoe@one.test>;,\r\n loose@example.com, Undisclosed recipients: ;"; gv != want {
		t.Errorf("groups written as %q, want %q", gv, want)
	}
	back := GroupedAddresses(gv)
	if len(back) != 3 || *back[0].Name != "A Group" || len(back[0].Addresses) != 3 || back[1].Name != nil ||
		back[1].Addresses[0].Email != "loose@example.com" || *back[2].Name != "Undisclosed recipients" || len(back[2].Addresses) != 0 {
		t.Errorf("groups read back as %+v", back)
	}
	// A local part that is not a dot-atom is quoted (3.4.1).
	if v := FormatAddresses("To", []Address{{Email: "john smith@example.com"}, {Email: "a..b@example.com"}}); v != ` johnsmith@example.com, "a..b"@example.com` {
		t.Errorf("odd local parts: %q", v)
	}
}

// RFC 5322 3.3 and 3.6.4: dates and message ids.
func TestRFC5322DateAndIDs(t *testing.T) {
	// The date of appendix A.1.1.
	when := time.Date(1997, 11, 21, 9, 55, 6, 0, time.FixedZone("", -6*3600))
	if v := FormatDate(when); v != " Fri, 21 Nov 1997 09:55:06 -0600" {
		t.Errorf("date %q", v)
	}
	if got, ok := Date(FormatDate(when)); !ok || !got.Equal(when) {
		t.Errorf("date read back as %v", got)
	}
	ids := []string{"1234@local.machine.example", "3456@example.net"}
	for i := 0; i < 30; i++ {
		ids = append(ids, strings.Repeat("r", 30)+string(rune('a'+i%26))+"@refs.example")
	}
	v := FormatMessageIDs("References", ids)
	if n := lines(t, []byte("References:"+v)); n > 0 {
		t.Errorf("%d lines over 78", n)
	}
	got := MessageIDs(v)
	if strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("ids read back as %v", got)
	}
	if id := NewMessageID("mail.example.test"); len(MessageIDs("<"+id+">")) != 1 || !strings.HasSuffix(id, "@mail.example.test") {
		t.Errorf("new message id %q", id)
	}
	if NewMessageID("h") == NewMessageID("h") {
		t.Error("two message ids are the same")
	}
	// A value that would end the field early is not written (2.2).
	for _, bad := range []string{" a\r\nBcc: x@y", " a\nb", " a\rb", " nul\x00", " " + strings.Repeat("x", 1000), " end\r\n"} {
		if ValidRaw(bad) {
			t.Errorf("%q passes as a field value", bad)
		}
	}
	if !ValidRaw(" folded\r\n here") {
		t.Error("proper folding is refused")
	}
}

// RFC 2231 3 and 4: a parameter value that is not ASCII, or long, is
// written as name*=utf-8''... and in numbered sections; RFC 2045 5.1: a
// token as it is, other ASCII quoted.
func TestRFC2231Parameters(t *testing.T) {
	for _, name := range []string{
		"report.pdf",
		"my report (final).pdf",
		"Größenvergleich für Übungsblätter.pdf",
		"日本語のファイル名.txt",
		strings.Repeat("long-name-", 20) + ".txt",
		`quote"and\backslash.txt`,
		"€uro 100% 'quoted' *star*.txt",
	} {
		raw := Build([]Header{Field("Subject", "x")}, &BuildPart{Type: "application/pdf", Name: name, Disposition: "attachment", Body: []byte{1, 2, 3}})
		if n := lines(t, raw); n > 0 {
			t.Errorf("%q: %d lines over 78:\n%s", name, n, raw)
		}
		m := Parse(raw)
		if m.Root.Name == nil || *m.Root.Name != name {
			t.Errorf("our parser reads the name %v, want %q\n%s", m.Root.Name, name, raw)
		}
		sm, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"Content-Disposition", "Content-Type"} {
			_, params, err := mime.ParseMediaType(sm.Header.Get(field))
			key := map[string]string{"Content-Disposition": "filename", "Content-Type": "name"}[field]
			if err != nil || params[key] != name {
				t.Errorf("mime reads %s %s=%q (%v), want %q", field, key, params[key], err, name)
			}
		}
	}
	// The example of RFC 2231 4.1 reads as its text says.
	if _, p := parseParams("application/x-stuff; title*=us-ascii'en-us'This%20is%20%2A%2A%2Afun%2A%2A%2A"); p["title"] != "This is ***fun***" {
		t.Errorf("RFC 2231 4.1 example: %q", p["title"])
	}
}

// RFC 2045 6: which Content-Transfer-Encoding a body gets, and that it
// decodes to the content.
func TestRFC2045ChoosingTransferEncoding(t *testing.T) {
	bin := make([]byte, 5000)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	for _, tc := range []struct {
		name, typ string
		body      []byte
		want      string // the encoding; "" is 7bit, which is the default and not written
	}{
		{"short ASCII lines", "text/plain", []byte("Hello,\r\n\r\nSee you.\r\n"), ""},
		{"LF line ends", "text/plain", []byte("Hello,\n\nSee you.\n"), ""},
		{"empty", "text/plain", nil, ""},
		{"a line over 78", "text/plain", []byte(strings.Repeat("word ", 40) + "\r\n"), "quoted-printable"},
		{"some non-ASCII", "text/plain", []byte("Beste Zoë,\r\n\r\nTot morgen in het café.\r\n"), "quoted-printable"},
		{"mostly non-ASCII", "text/plain", []byte("こんにちは。お元気ですか。\r\n"), "base64"},
		{"an equals sign", "text/plain", []byte("a = b\r\n"), ""},
		{"trailing spaces and From", "text/plain", []byte("From here   \r\n.\r\n"), ""},
		{"html", "text/html", []byte("<p>Caf&eacute; " + strings.Repeat("x", 200) + "</p>"), "quoted-printable"},
		{"binary", "application/octet-stream", bin, "base64"},
		{"image", "image/png", []byte("\x89PNG\r\n\x1a\n"), "base64"},
		{"ASCII that is not text", "application/json", []byte(`{"a":1}`), "base64"},
	} {
		raw := Build([]Header{Field("From", "a@b.test")}, &BuildPart{Type: tc.typ, Body: tc.body})
		lines(t, raw)
		for _, c := range raw {
			if c >= 0x80 {
				t.Errorf("%s: an 8-bit octet on the wire", tc.name)
				break
			}
		}
		m := Parse(raw)
		enc, _ := Get(m.Headers, "Content-Transfer-Encoding")
		if strings.TrimSpace(enc) != tc.want {
			t.Errorf("%s: encoding %q, want %q", tc.name, strings.TrimSpace(enc), tc.want)
		}
		want := tc.body
		if strings.HasPrefix(tc.typ, "text/") {
			want = []byte(strings.ReplaceAll(strings.ReplaceAll(string(tc.body), "\r\n", "\n"), "\n", "\r\n"))
			if len(want) > 0 && !bytes.HasSuffix(want, []byte("\r\n")) {
				want = append(want, "\r\n"...)
			}
		}
		got := m.Root.Content()
		if strings.HasPrefix(tc.typ, "text/") && len(got) > 0 && !bytes.HasSuffix(got, []byte("\r\n")) {
			got = append(bytes.Clone(got), "\r\n"...)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: our parser reads %q, want %q", tc.name, clipTest(got), clipTest(want))
		}
		// Go's own decoders agree.
		sm, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		var r io.Reader = sm.Body
		switch tc.want {
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		case "base64":
			r = newBase64Reader(r)
		}
		std, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if strings.HasPrefix(tc.typ, "text/") && len(std) > 0 && !bytes.HasSuffix(std, []byte("\r\n")) {
			std = append(std, "\r\n"...)
		}
		if !bytes.Equal(std, want) {
			t.Errorf("%s: Go reads %q, want %q", tc.name, clipTest(std), clipTest(want))
		}
	}
	// RFC 2046 5.2.1: an attached message is never base64 or
	// quoted-printable. With 8-bit content it is 8bit.
	inner := []byte("From: a@b.test\r\nSubject: caf\xc3\xa9\r\n\r\nbody\r\n")
	raw := Build(nil, &BuildPart{Type: "multipart/mixed", SubParts: []*BuildPart{{Type: "message/rfc822", Body: inner}}})
	m := Parse(raw)
	if enc, _ := Get(m.Leaves[0].Headers, "Content-Transfer-Encoding"); strings.TrimSpace(enc) != "8bit" || !bytes.Equal(m.Leaves[0].Content(), inner) {
		t.Errorf("attached message: encoding %q, content %q", enc, m.Leaves[0].Content())
	}
}

func clipTest(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "..."
	}
	return string(b)
}

// TestBuildStructure writes the usual tree of a mail with a text and an
// HTML version, an inline image and an attachment, and reads it back.
func TestBuildStructure(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nnot really a picture")
	pdf := bytes.Repeat([]byte("%PDF-1.7 \xff\xfe"), 100)
	root := &BuildPart{Type: "multipart/mixed", SubParts: []*BuildPart{
		{Type: "multipart/alternative", SubParts: []*BuildPart{
			{Type: "text/plain", Body: []byte("Hallo Zoë,\n\nzie de bijlage.\n")},
			{Type: "multipart/related", Params: map[string]string{"type": "text/html"}, SubParts: []*BuildPart{
				{Type: "text/html", Body: []byte(`<p>Hallo Zoë, <img src="cid:logo@robinmail.test"></p>`)},
				{Type: "image/png", Name: "logo.png", Disposition: "inline", CID: "logo@robinmail.test", Body: png},
			}},
		}},
		{Type: "application/pdf", Name: "Offerte één.pdf", Disposition: "attachment", Body: pdf, Language: []string{"nl"}, Location: "https://example.test/offerte.pdf"},
	}}
	headers := []Header{
		{"Date", FormatDate(time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC))},
		{"From", FormatAddresses("From", []Address{{Name: sp("Mike"), Email: "mike@a.test"}})},
		{"To", FormatAddresses("To", []Address{{Name: sp("Zoë"), Email: "zoe@b.test"}})},
		{"Subject", FormatText("Subject", "Offerte één")},
		{"Message-ID", FormatMessageIDs("Message-ID", []string{"1@a.test"})},
	}
	raw := Build(headers, root)
	lines(t, raw)

	m := Parse(raw)
	if got, _ := Get(m.Headers, "MIME-Version"); strings.TrimSpace(got) != "1.0" {
		t.Errorf("MIME-Version %q", got) // RFC 2045 4
	}
	var shape func(p *Part) string
	shape = func(p *Part) string {
		if !p.IsMultipart() {
			return p.Type
		}
		var subs []string
		for _, sp := range p.SubParts {
			subs = append(subs, shape(sp))
		}
		return p.Type + "[" + strings.Join(subs, " ") + "]"
	}
	if got, want := shape(m.Root), "multipart/mixed[multipart/alternative[text/plain multipart/related[text/html image/png]] application/pdf]"; got != want {
		t.Errorf("structure %s, want %s", got, want)
	}
	text, html, attachments := m.Bodies()
	if len(text) != 1 || text[0].Type != "text/plain" || len(html) != 1 || html[0].Type != "text/html" {
		t.Errorf("textBody %v htmlBody %v", text, html)
	}
	if len(attachments) != 2 || *attachments[0].CID != "logo@robinmail.test" || *attachments[0].Disposition != "inline" ||
		!bytes.Equal(attachments[0].Content(), png) || *attachments[1].Name != "Offerte één.pdf" || !bytes.Equal(attachments[1].Content(), pdf) ||
		attachments[1].Language[0] != "nl" || *attachments[1].Location != "https://example.test/offerte.pdf" {
		t.Errorf("attachments read back wrong: %+v", attachments)
	}
	if !m.HasAttachment() {
		t.Error("hasAttachment is false")
	}
	if s, _ := text[0].Text(); s != "Hallo Zoë,\n\nzie de bijlage.\n" {
		t.Errorf("text body %q", s)
	}

	// The same through net/mail and mime/multipart.
	sm, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if subj, _ := new(mime.WordDecoder).DecodeHeader(sm.Header.Get("Subject")); subj != "Offerte één" {
		t.Errorf("net/mail subject %q", subj)
	}
	var walk func(header string, body io.Reader) string
	walk = func(header string, body io.Reader) string {
		mt, params, err := mime.ParseMediaType(header)
		if err != nil {
			t.Fatalf("Content-Type %q: %v", header, err)
		}
		if !strings.HasPrefix(mt, "multipart/") {
			io.Copy(io.Discard, body)
			return mt
		}
		mr := multipart.NewReader(body, params["boundary"])
		var subs []string
		for {
			p, err := mr.NextRawPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("multipart: %v", err)
			}
			if mt2 := p.Header.Get("Content-Type"); strings.HasPrefix(mt2, "application/pdf") {
				data, _ := io.ReadAll(newBase64Reader(p))
				if !bytes.Equal(data, pdf) {
					t.Errorf("mime/multipart reads %d bytes of PDF, want %d", len(data), len(pdf))
				}
				if _, dp, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition")); dp["filename"] != "Offerte één.pdf" {
					t.Errorf("mime/multipart file name %q", dp["filename"])
				}
			}
			subs = append(subs, walk(p.Header.Get("Content-Type"), p))
		}
		return mt + "[" + strings.Join(subs, " ") + "]"
	}
	if got, want := walk(sm.Header.Get("Content-Type"), sm.Body), shape(m.Root); got != want {
		t.Errorf("mime/multipart sees %s, our parser %s", got, want)
	}

	// A boundary never appears in the content (RFC 2046 5.1.1).
	tricky := Build(nil, &BuildPart{Type: "multipart/mixed", SubParts: []*BuildPart{{Type: "text/plain", Body: []byte("--=_boundary\r\n--\r\n")}}})
	if got := Parse(tricky).Leaves; len(got) != 1 || string(got[0].Content()) != "--=_boundary\r\n--\r\n" {
		t.Errorf("content that looks like a delimiter: %q", got[0].Content())
	}
}

func FuzzBuildRoundTrip(f *testing.F) {
	f.Add("Subject", "héllo wörld", "body\n", "file näme.txt")
	f.Add("X-Long-Header-Name-That-Goes-On", strings.Repeat("=?", 40), "a=b\r\n.\r\n", "a\"b")
	f.Fuzz(func(t *testing.T, name, text, body, file string) {
		if !validHeaderName(name) || !strings.EqualFold(name, "subject") && strings.HasPrefix(strings.ToLower(name), "content-") {
			name = "Subject"
		}
		if strings.ContainsAny(text, "\x00") || strings.ContainsAny(file, "\x00\r\n") || len(file) > 200 {
			return
		}
		raw := Build([]Header{{name, FormatText(name, text)}}, &BuildPart{Type: "multipart/mixed", SubParts: []*BuildPart{
			{Type: "text/plain", Body: []byte(body)},
			{Type: "application/octet-stream", Name: file, Body: []byte(body)},
		}})
		for _, l := range bytes.Split(raw, []byte("\r\n")) {
			if len(l) > 998 {
				t.Fatalf("line of %d octets", len(l))
			}
		}
		m := Parse(raw)
		if len(m.Leaves) != 2 || !bytes.Equal(m.Leaves[1].Content(), []byte(body)) {
			t.Fatalf("attachment read back as %q, want %q", m.Leaves[1].Content(), body)
		}
		if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
			t.Fatalf("net/mail: %v", err)
		}
	})
}

func validHeaderName(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == ':' {
			return false
		}
	}
	return true
}
