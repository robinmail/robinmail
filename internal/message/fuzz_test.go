package message

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The fuzz tests check what must hold for any input: no panic, the limits,
// valid UTF-8 where text goes to a client, and nothing dangerous in a view.

func FuzzParse(f *testing.F) {
	f.Add([]byte(crlf(rfc5322A11)))
	f.Add([]byte(crlf(rfc2046Multipart)))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: multipart/alternative; boundary=c\r\n\r\n--c\r\n\r\nx\r\n--c--\r\n--b--"))
	f.Add([]byte("Content-Type: text/html; charset=iso-2022-jp\r\nContent-Transfer-Encoding: base64\r\n\r\nGyRCJDMbKEI="))
	f.Add([]byte("Content-Disposition: attachment; filename*0*=utf-8''a%20; filename*1=b\r\n\r\n"))
	f.Add([]byte("\x00\x01"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		m := Parse(raw)
		if len(m.Headers) > MaxHeaderFields {
			t.Fatalf("%d header fields", len(m.Headers))
		}
		count := 0
		var walk func(p *Part, depth int)
		walk = func(p *Part, depth int) {
			count++
			if depth > MaxDepth {
				t.Fatalf("depth %d", depth)
			}
			if p.IsMultipart() != (p.PartID == "") || (p.SubParts != nil) != p.IsMultipart() {
				t.Fatalf("part %q of type %q with %d subparts", p.PartID, p.Type, len(p.SubParts))
			}
			if !p.IsMultipart() {
				if m.Part(p.PartID) != p || p.Size != int64(len(p.Content())) {
					t.Fatalf("leaf %q", p.PartID)
				}
				value, _, _ := p.BodyValue(100)
				if !utf8.ValidString(value) || len(value) > 100 {
					t.Fatalf("body value %q", value)
				}
			}
			for _, s := range []*string{p.Name, p.Charset, p.Disposition, p.CID, p.Location} {
				if s != nil && !utf8.ValidString(*s) {
					t.Fatalf("invalid UTF-8 in a property: %q", *s)
				}
			}
			for _, c := range p.SubParts {
				walk(c, depth+1)
			}
		}
		walk(m.Root, 0)
		if count > MaxParts {
			t.Fatalf("%d parts", count)
		}
		text, html, att := m.Bodies()
		if len(text)+len(html)+len(att) > 3*len(m.Leaves) {
			t.Fatal("more body parts than leaves")
		}
		if p := m.Preview(); !utf8.ValidString(p) || utf8.RuneCountInString(p) > PreviewLen || strings.ContainsAny(p, "\r\n\t") {
			t.Fatalf("preview %q", p)
		}
		m.HasAttachment()
	})
}

func FuzzHeaderForms(f *testing.F) {
	f.Add(` "Giant; \"Big\" Box" <sysservices@example.net>, A Group:Ed Jones <c@a.test>,joe@where.test;`)
	f.Add(" =?ISO-8859-1?Q?Andr=E9?= Pirard <PIRARD@vm1.ulg.ac.be> (=?iso-8859-8?b?7eXs+SDv4SDp7Oj08A==?=)")
	f.Add(" <1234@local.machine.example> (comment <x@y>) \"<q@z>\" <3456@example.net>")
	f.Add(" Fri, 21 Nov 1997 09(comment):   55  :  06 -0600")
	f.Add(" <mailto:a@b?subject=x>, <http://c/d> (comment)")
	f.Add(" text/plain; charset*0*=utf-8''x; charset*1=\"y\" (c)")
	f.Fuzz(func(t *testing.T, v string) {
		for _, g := range GroupedAddresses(v) {
			if g.Name != nil && !utf8.ValidString(*g.Name) {
				t.Fatalf("group name %q", *g.Name)
			}
			for _, a := range g.Addresses {
				if !utf8.ValidString(a.Email) || (a.Name != nil && (!utf8.ValidString(*a.Name) || *a.Name == "")) {
					t.Fatalf("address %+v", a)
				}
			}
		}
		if n := len(Addresses(v)); n > MaxAddresses {
			t.Fatalf("%d addresses", n)
		}
		for _, id := range MessageIDs(v) {
			if id == "" || len(id) > MaxMessageIDLen || strings.ContainsAny(id, "<> \r\n") || !utf8.ValidString(id) {
				t.Fatalf("message id %q", id)
			}
		}
		if s := Text(v); !utf8.ValidString(s) {
			t.Fatalf("text %q", s)
		}
		for _, u := range URLs(v) {
			if !utf8.ValidString(u) || strings.ContainsAny(u, " <>") {
				t.Fatalf("url %q", u)
			}
		}
		Date(v)
		value, params := parseParams(v)
		if !utf8.ValidString(value) || len(params) > 2*MaxParams {
			t.Fatalf("params %q %q", value, params)
		}
		for k, p := range params {
			if !utf8.ValidString(p) || k == "" {
				t.Fatalf("param %q=%q", k, p)
			}
		}
	})
}

func FuzzSanitizeHTML(f *testing.F) {
	for _, v := range xssVectors {
		f.Add(v)
	}
	f.Add(`<table><tr><td style="color:red" align=center><a href="https://example.test">x</a><img src="cid:a"></td></tr></table>`)
	f.Fuzz(func(t *testing.T, src string) {
		for _, opt := range []ViewOptions{{}, {RemoteImages: true, CID: func(cid string) string { return cid }}} {
			v := SanitizeHTML(src, opt)
			if !utf8.ValidString(v.HTML) {
				t.Fatalf("invalid UTF-8 from %q", src)
			}
			if bad := dangerous(t, v.HTML, opt.RemoteImages); len(bad) > 0 {
				t.Fatalf("%q\n  became %q\n  which has %q", src, v.HTML, bad)
			}
			// Sanitising is stable: what comes out goes through unchanged
			// in meaning, so a second pass finds nothing new to remove.
			again := SanitizeHTML(v.HTML, opt)
			if bad := dangerous(t, again.HTML, opt.RemoteImages); len(bad) > 0 {
				t.Fatalf("second pass of %q has %q", src, bad)
			}
		}
	})
}
