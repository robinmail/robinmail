package message

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These tests are written from the RFC text, not from the code. Each names
// the section it checks and uses the RFC's own examples.

func crlf(s string) string { return strings.ReplaceAll(strings.TrimPrefix(s, "\n"), "\n", "\r\n") }

// js shows a value the way JMAP would send it, which makes nil names and
// empty lists visible in a failure.
func js(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func wantJSON(t *testing.T, name string, got any, want string) {
	t.Helper()
	if g := js(got); g != want {
		t.Errorf("%s:\n got %s\nwant %s", name, g, want)
	}
}

func header(t *testing.T, raw, name string) string {
	t.Helper()
	v, ok := Get(ParseHeader([]byte(raw)), name)
	if !ok {
		t.Fatalf("no %s field", name)
	}
	return v
}

// RFC 5322 2.2 and 2.2.3: a field is a name, a colon and a body; a body may
// be folded over lines that start with white space. RFC 8621 4.1.2.1: the
// Raw form is the octets after the colon up to the terminating CRLF.
func TestRFC5322HeaderFields(t *testing.T) {
	raw := crlf(`
Subject: This is a test
To: one@example.test,
 two@example.test
X-Empty:
Subject: second

body
`)
	fields, off := parseHeaders([]byte(raw))
	want := []Header{
		{"Subject", " This is a test"},
		{"To", " one@example.test,\r\n two@example.test"},
		{"X-Empty", ""},
		{"Subject", " second"},
	}
	if !reflect.DeepEqual(fields, want) {
		t.Errorf("fields:\n got %q\nwant %q", fields, want)
	}
	if got := raw[off:]; got != "body\r\n" {
		t.Errorf("body %q", got)
	}
	// RFC 8621 4.1.3: for a field that should be there once, the last one counts.
	if v, _ := Get(fields, "subject"); v != " second" {
		t.Errorf("Get returned %q", v)
	}
	if all := All(fields, "SUBJECT"); len(all) != 2 {
		t.Errorf("All returned %q", all)
	}

	// 2.1: "the header section ... is followed by an empty line", but a
	// message may also have no body at all.
	fields, off = parseHeaders([]byte("A: 1\r\nB: 2"))
	if len(fields) != 2 || off != 10 {
		t.Errorf("no body: %q, offset %d", fields, off)
	}
	// Bare LF line ends, as files on disk have.
	fields, off = parseHeaders([]byte("A: 1\n b\nB: 2\n\nbody"))
	if len(fields) != 2 || fields[0].Value != " 1\n b" || string("A: 1\n b\nB: 2\n\nbody"[off:]) != "body" {
		t.Errorf("bare LF: %q, offset %d", fields, off)
	}
	// Not a header at all: everything is body.
	fields, off = parseHeaders([]byte("just some text\r\n"))
	if len(fields) != 0 || off != 0 {
		t.Errorf("no header: %q, offset %d", fields, off)
	}
}

// RFC 5322 appendix A.1.1, "A Message from One Person to Another with
// Simple Addressing".
const rfc5322A11 = `
From: John Doe <jdoe@machine.example>
To: Mary Smith <mary@example.net>
Subject: Saying Hello
Date: Fri, 21 Nov 1997 09:55:06 -0600
Message-ID: <1234@local.machine.example>

This is a message just to say hello.
So, "Hello".
`

func TestRFC5322AppendixA11(t *testing.T) {
	raw := crlf(rfc5322A11)
	wantJSON(t, "From", Addresses(header(t, raw, "From")), `[{"name":"John Doe","email":"jdoe@machine.example"}]`)
	wantJSON(t, "To", Addresses(header(t, raw, "To")), `[{"name":"Mary Smith","email":"mary@example.net"}]`)
	if got := Text(header(t, raw, "Subject")); got != "Saying Hello" {
		t.Errorf("Subject %q", got)
	}
	wantJSON(t, "Message-ID", MessageIDs(header(t, raw, "Message-ID")), `["1234@local.machine.example"]`)
	d, ok := Date(header(t, raw, "Date"))
	if !ok || d.Format(time.RFC3339) != "1997-11-21T09:55:06-06:00" {
		t.Errorf("Date %v %v", d, ok)
	}
	m := Parse([]byte(raw))
	if got, _ := m.Root.Text(); got != "This is a message just to say hello.\nSo, \"Hello\".\n" {
		t.Errorf("body %q", got)
	}
}

// RFC 5322 appendix A.1.2, "Different Types of Mailboxes": a display name
// that needs quotes, a list with and without names, an angle address with
// no name at all, and a quoted string with quoted pairs.
func TestRFC5322AppendixA12(t *testing.T) {
	raw := crlf(`
From: "Joe Q. Public" <john.q.public@example.com>
To: Mary Smith <mary@x.test>, jdoe@example.org, Who? <one@y.test>
Cc: <boss@nil.test>, "Giant; \"Big\" Box" <sysservices@example.net>
Date: Tue, 1 Jul 2003 10:52:37 +0200
Message-ID: <5678.21-Nov-1997@example.com>

Hi everyone.
`)
	wantJSON(t, "From", Addresses(header(t, raw, "From")), `[{"name":"Joe Q. Public","email":"john.q.public@example.com"}]`)
	wantJSON(t, "To", Addresses(header(t, raw, "To")),
		`[{"name":"Mary Smith","email":"mary@x.test"},{"name":null,"email":"jdoe@example.org"},{"name":"Who?","email":"one@y.test"}]`)
	wantJSON(t, "Cc", Addresses(header(t, raw, "Cc")),
		`[{"name":null,"email":"boss@nil.test"},{"name":"Giant; \"Big\" Box","email":"sysservices@example.net"}]`)
	d, _ := Date(header(t, raw, "Date"))
	if d.Format(time.RFC3339) != "2003-07-01T10:52:37+02:00" {
		t.Errorf("Date %v", d)
	}
}

// RFC 5322 appendix A.1.3, "Group Addresses", read as RFC 8621 4.1.2.4
// wants: a group keeps its name and members, and an empty group stays.
func TestRFC5322AppendixA13(t *testing.T) {
	raw := crlf(`
From: Pete <pete@silly.example>
To: A Group:Ed Jones <c@a.test>,joe@where.test,John <jdoe@one.test>;
Cc: Undisclosed recipients:;
Date: Thu, 13 Feb 1969 23:32:54 -0330
Message-ID: <testabcd.1234@silly.example>

Testing.
`)
	wantJSON(t, "To grouped", GroupedAddresses(header(t, raw, "To")),
		`[{"name":"A Group","addresses":[{"name":"Ed Jones","email":"c@a.test"},{"name":null,"email":"joe@where.test"},{"name":"John","email":"jdoe@one.test"}]}]`)
	wantJSON(t, "To flat", Addresses(header(t, raw, "To")),
		`[{"name":"Ed Jones","email":"c@a.test"},{"name":null,"email":"joe@where.test"},{"name":"John","email":"jdoe@one.test"}]`)
	wantJSON(t, "Cc grouped", GroupedAddresses(header(t, raw, "Cc")), `[{"name":"Undisclosed recipients","addresses":[]}]`)
	wantJSON(t, "Cc flat", Addresses(header(t, raw, "Cc")), `[]`)
	d, _ := Date(header(t, raw, "Date"))
	if d.Format(time.RFC3339) != "1969-02-13T23:32:54-03:30" {
		t.Errorf("Date %v", d)
	}
}

// RFC 8621 4.1.2.4: "Any mailboxes that are not part of a group ... are
// grouped together under a null name", one group per consecutive run.
func TestRFC8621GroupedAddresses(t *testing.T) {
	// The example of RFC 8621 4.1.2.3 and 4.1.2.4.
	v := ` "James Smythe" <james@example.com>, Friends:
  jane@example.com, =?UTF-8?Q?John_Sm=C3=AEth?= <john@example.com>;`
	wantJSON(t, "asAddresses", Addresses(v),
		`[{"name":"James Smythe","email":"james@example.com"},{"name":null,"email":"jane@example.com"},{"name":"John Smîth","email":"john@example.com"}]`)
	wantJSON(t, "asGroupedAddresses", GroupedAddresses(v),
		`[{"name":null,"addresses":[{"name":"James Smythe","email":"james@example.com"}]},`+
			`{"name":"Friends","addresses":[{"name":null,"email":"jane@example.com"},{"name":"John Smîth","email":"john@example.com"}]}]`)

	wantJSON(t, "runs around a group", GroupedAddresses("a@x.test, b@x.test, G: c@x.test;, d@x.test"),
		`[{"name":null,"addresses":[{"name":null,"email":"a@x.test"},{"name":null,"email":"b@x.test"}]},`+
			`{"name":"G","addresses":[{"name":null,"email":"c@x.test"}]},`+
			`{"name":null,"addresses":[{"name":null,"email":"d@x.test"}]}]`)
	wantJSON(t, "empty", GroupedAddresses(""), `[]`)
}

// RFC 5322 appendix A.2, "Reply Messages": the identification fields of
// 3.6.4.
func TestRFC5322AppendixA2(t *testing.T) {
	raw := crlf(`
To: "Mary Smith: Personal Account" <smith@home.example>
From: John Doe <jdoe@machine.example>
Subject: Re: Saying Hello
Date: Fri, 21 Nov 1997 11:00:00 -0600
Message-ID: <abcd.1234@local.machine.test>
In-Reply-To: <3456@example.net>
References: <1234@local.machine.example> <3456@example.net>

This is a reply to your reply.
`)
	wantJSON(t, "To", Addresses(header(t, raw, "To")), `[{"name":"Mary Smith: Personal Account","email":"smith@home.example"}]`)
	wantJSON(t, "Message-ID", MessageIDs(header(t, raw, "Message-ID")), `["abcd.1234@local.machine.test"]`)
	wantJSON(t, "In-Reply-To", MessageIDs(header(t, raw, "In-Reply-To")), `["3456@example.net"]`)
	wantJSON(t, "References", MessageIDs(header(t, raw, "References")), `["1234@local.machine.example","3456@example.net"]`)
}

// RFC 5322 appendix A.5, "White Space, Comments, and Other Oddities":
// comments and folding white space between any two tokens.
func TestRFC5322AppendixA5(t *testing.T) {
	raw := crlf(`
From: Pete(A nice \) chap) <pete(his account)@silly.test(his host)>
To:A Group(Some people)
     :Chris Jones <c@(Chris's host.)public.example>,
         joe@example.org,
  John <jdoe@one.test> (my dear friend); (the end of the group)
Cc:(Empty list)(start)Hidden recipients  :(nobody(that I know))  ;
Date: Thu,
      13
        Feb
          1969
      23:32
               -0330 (Newfoundland Time)
Message-ID:              <testabcd.1234@silly.test>

Testing.
`)
	wantJSON(t, "From", Addresses(header(t, raw, "From")), `[{"name":"Pete","email":"pete@silly.test"}]`)
	wantJSON(t, "To", GroupedAddresses(header(t, raw, "To")),
		`[{"name":"A Group","addresses":[{"name":"Chris Jones","email":"c@public.example"},{"name":null,"email":"joe@example.org"},{"name":"John","email":"jdoe@one.test"}]}]`)
	wantJSON(t, "Cc", GroupedAddresses(header(t, raw, "Cc")), `[{"name":"Hidden recipients","addresses":[]}]`)
	d, ok := Date(header(t, raw, "Date"))
	if !ok || d.Format(time.RFC3339) != "1969-02-13T23:32:00-03:30" {
		t.Errorf("Date %v %v", d, ok)
	}
	wantJSON(t, "Message-ID", MessageIDs(header(t, raw, "Message-ID")), `["testabcd.1234@silly.test"]`)
}

// RFC 5322 appendix A.6: the obsolete forms of section 4, which "MUST be
// accepted and parsed by conformant receivers".
func TestRFC5322AppendixA6(t *testing.T) {
	// A.6.1: a display name with an unquoted ".", a route in the angle
	// address, an empty list member, white space around the dot.
	raw := crlf(`
From: Joe Q. Public <john.q.public@example.com>
To: Mary Smith <@node.test:mary@example.net>, , jdoe@test  . example
Date: Tue, 1 Jul 2003 10:52:37 +0200
Message-ID: <5678.21-Nov-1997@example.com>

Hi everyone.
`)
	wantJSON(t, "A.6.1 From", Addresses(header(t, raw, "From")), `[{"name":"Joe Q. Public","email":"john.q.public@example.com"}]`)
	wantJSON(t, "A.6.1 To", Addresses(header(t, raw, "To")),
		`[{"name":"Mary Smith","email":"mary@example.net"},{"name":null,"email":"jdoe@test.example"}]`)

	// A.6.2: two-digit year, no day of week, an alphabetic zone.
	d, ok := Date(" 21 Nov 97 09:55:06 GMT")
	if !ok || d.UTC().Format(time.RFC3339) != "1997-11-21T09:55:06Z" {
		t.Errorf("A.6.2 date %v %v", d, ok)
	}

	// A.6.3: white space before the colon of a field, comments and white
	// space inside the time and the domain, a fold that is only white space.
	raw = crlf(`
From  : John Doe <jdoe@machine(comment).  example>
To    : Mary Smith
__
          <mary@example.net>
Subject     : Saying Hello
Date  : Fri, 21 Nov 1997 09(comment):   55  :  06 -0600
Message-ID  : <1234   @   local(blah)  .machine .example>

This is a message just to say hello.
`)
	raw = strings.Replace(raw, "__", "  ", 1)
	fields := ParseHeader([]byte(raw))
	if len(fields) != 5 || fields[0].Name != "From" || fields[4].Name != "Message-ID" {
		t.Fatalf("A.6.3 fields: %q", fields)
	}
	wantJSON(t, "A.6.3 From", Addresses(header(t, raw, "From")), `[{"name":"John Doe","email":"jdoe@machine.example"}]`)
	wantJSON(t, "A.6.3 To", Addresses(header(t, raw, "To")), `[{"name":"Mary Smith","email":"mary@example.net"}]`)
	if got := Text(header(t, raw, "Subject")); got != "Saying Hello" {
		t.Errorf("A.6.3 Subject %q", got)
	}
	d, ok = Date(header(t, raw, "Date"))
	if !ok || d.Format(time.RFC3339) != "1997-11-21T09:55:06-06:00" {
		t.Errorf("A.6.3 Date %v %v", d, ok)
	}
}

// Deviation, kept on purpose: the obsolete msg-id of RFC 5322 4.5.4 allows
// white space and comments inside the angle brackets (A.6.3's Message-ID).
// We skip such an id instead of repairing it. An id decides which thread a
// message joins, and no software of the last decades writes one this way;
// what does show up between brackets with spaces in it is not an id.
func TestDeviationObsoleteMessageID(t *testing.T) {
	if got := MessageIDs(" <1234   @   local(blah)  .machine .example>"); got != nil {
		t.Errorf("got %q, want none", got)
	}
}

// RFC 8621 4.1.2.2: Text is unfolded, loses leading spaces, has encoded
// words decoded and is normalised to NFC.
func TestRFC8621TextForm(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{" Saying Hello", "Saying Hello"},
		{" a folded\r\n subject", "a folded subject"},
		{"   three spaces", "three spaces"},
		{" =?UTF-8?Q?caf=C3=A9?=", "café"},
		// e followed by a combining acute accent becomes one character.
		{" cafe\xcc\x81", "caf\xc3\xa9"},
		{" bad \xff byte", "bad \xef\xbf\xbd byte"},
	} {
		if got := Text(tc.in); got != tc.want {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// RFC 8621 4.1.2.3: without a display name, a comment after the addr-spec
// is the name.
func TestRFC8621AddressNameFromComment(t *testing.T) {
	wantJSON(t, "comment after bare address", Addresses(" jdoe@example.test (John Doe)"),
		`[{"name":"John Doe","email":"jdoe@example.test"}]`)
	wantJSON(t, "comment after angle address", Addresses(" <jdoe@example.test> (John Doe)"),
		`[{"name":"John Doe","email":"jdoe@example.test"}]`)
	wantJSON(t, "display name wins", Addresses(" Johnny <jdoe@example.test> (John Doe)"),
		`[{"name":"Johnny","email":"jdoe@example.test"}]`)
	wantJSON(t, "quoted local part", Addresses(` "john doe"@example.test`),
		`[{"name":null,"email":"\"john doe\"@example.test"}]`)
	wantJSON(t, "domain literal", Addresses(` jdoe@[192.0.2.1]`), `[{"name":null,"email":"jdoe@[192.0.2.1]"}]`)
}

// RFC 8621 4.1.2.7 with the header of RFC 2369 3.2: URLs between angle
// brackets, comments skipped.
func TestRFC8621URLsForm(t *testing.T) {
	wantJSON(t, "List-Unsubscribe", URLs(" (Use this command to get off the list)\r\n     <mailto:list-manager@host.com?body=unsubscribe%20list>"),
		`["mailto:list-manager@host.com?body=unsubscribe%20list"]`)
	wantJSON(t, "two", URLs(" <http://www.host.com/list.cgi?cmd=unsub&lst=list>,\r\n     <mailto:list-request@host.com?subject=unsubscribe>"),
		`["http://www.host.com/list.cgi?cmd=unsub&lst=list","mailto:list-request@host.com?subject=unsubscribe"]`)
	if got := URLs(" NO (posting not allowed on this list)"); got != nil {
		t.Errorf("no URL: %q", got)
	}
}

// RFC 8621 4.1.2: which form is allowed for which field.
func TestRFC8621FormAllowed(t *testing.T) {
	for _, tc := range []struct {
		name, form string
		want       bool
	}{
		{"Subject", FormText, true},
		{"Subject", FormAddresses, false},
		{"From", FormAddresses, true},
		{"from", FormGroupedAddresses, true},
		{"From", FormText, false},
		{"Message-ID", FormMessageIDs, true},
		{"References", FormDate, false},
		{"Date", FormDate, true},
		{"List-Unsubscribe", FormURLs, true},
		{"List-Id", FormText, true},
		{"Received", FormText, false},
		{"Received", FormRaw, true},
		// Not defined in RFC 5322 or RFC 2369: any form.
		{"X-Custom", FormAddresses, true},
		{"X-Custom", FormDate, true},
		{"X-Custom", "asNonsense", false},
	} {
		if got := FormAllowed(tc.name, tc.form); got != tc.want {
			t.Errorf("FormAllowed(%s, %s) = %v", tc.name, tc.form, got)
		}
	}
}

// RFC 2047 8, "Examples": the same header as its example, and what a
// reader shows for each field.
func TestRFC2047Examples(t *testing.T) {
	raw := crlf(`
From: =?US-ASCII?Q?Keith_Moore?= <moore@cs.utk.edu>
To: =?ISO-8859-1?Q?Keld_J=F8rn_Simonsen?= <keld@dkuug.dk>
CC: =?ISO-8859-1?Q?Andr=E9?= Pirard <PIRARD@vm1.ulg.ac.be>
Subject: =?ISO-8859-1?B?SWYgeW91IGNhbiByZWFkIHRoaXMgeW8=?=
 =?ISO-8859-2?B?dSB1bmRlcnN0YW5kIHRoZSBleGFtcGxlLg==?=

`)
	wantJSON(t, "From", Addresses(header(t, raw, "From")), `[{"name":"Keith Moore","email":"moore@cs.utk.edu"}]`)
	wantJSON(t, "To", Addresses(header(t, raw, "To")), `[{"name":"Keld Jørn Simonsen","email":"keld@dkuug.dk"}]`)
	wantJSON(t, "CC", Addresses(header(t, raw, "Cc")), `[{"name":"André Pirard","email":"PIRARD@vm1.ulg.ac.be"}]`)
	if got := Text(header(t, raw, "Subject")); got != "If you can read this you understand the example." {
		t.Errorf("Subject %q", got)
	}

	raw = crlf(`
From: =?ISO-8859-1?Q?Olle_J=E4rnefors?= <ojarnef@admin.kth.se>
To: ietf-822@dimacs.rutgers.edu, ojarnef@admin.kth.se
Subject: Time for ISO 10646?

`)
	wantJSON(t, "From 2", Addresses(header(t, raw, "From")), `[{"name":"Olle Järnefors","email":"ojarnef@admin.kth.se"}]`)

	// "From: Nathaniel Borenstein <nsb@thumper.bellcore.com>
	//        (=?iso-8859-8?b?7eXs+SDv4SDp7Oj08A==?=)": an encoded word in a
	// comment. The name is the phrase; the comment decodes to Hebrew text.
	if got := DecodeWords("(=?iso-8859-8?b?7eXs+SDv4SDp7Oj08A==?=)"); got != "(םולש ןב ילטפנ)" {
		t.Errorf("iso-8859-8 comment: %q", got)
	}
	wantJSON(t, "name from an encoded comment", Addresses(" nsb@thumper.bellcore.com (=?iso-8859-8?b?7eXs+SDv4SDp7Oj08A==?=)"),
		`[{"name":"םולש ןב ילטפנ","email":"nsb@thumper.bellcore.com"}]`)
}

// RFC 2047 8: the table of encoded forms and how each is displayed. White
// space between two encoded words is not shown (6.2); white space between
// an encoded word and other text is.
func TestRFC2047DisplayTable(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"(=?ISO-8859-1?Q?a?=)", "(a)"},
		{"(=?ISO-8859-1?Q?a?= b)", "(a b)"},
		{"(=?ISO-8859-1?Q?a?= =?ISO-8859-1?Q?b?=)", "(ab)"},
		{"(=?ISO-8859-1?Q?a?=  =?ISO-8859-1?Q?b?=)", "(ab)"},
		{"(=?ISO-8859-1?Q?a?=\r\n    =?ISO-8859-1?Q?b?=)", "(ab)"},
		{"(=?ISO-8859-1?Q?a_b?=)", "(a b)"},
		{"(=?ISO-8859-1?Q?a?= =?ISO-8859-2?Q?_b?=)", "(a b)"},
	} {
		if got := DecodeWords(tc.in); got != tc.want {
			t.Errorf("DecodeWords(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// RFC 2047 2, 4.1, 4.2 and 6.3.
func TestRFC2047Rules(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// 2: charset and encoding are case-independent.
		{"case", "=?iso-8859-1?q?=E9?= =?ISO-8859-1?b?6Q==?=", "éé"},
		// 4.2 (2): "=" followed by two hex digits; upper case should be
		// used, lower case is accepted.
		{"lowercase hex", "=?ISO-8859-1?Q?=e9?=", "é"},
		// 4.2 (2): "_" is always a space, in any charset.
		{"underscore", "=?UTF-8?Q?a_b?=", "a b"},
		// 6.3: an unknown charset is displayed as it is.
		{"unknown charset", "=?X-UNKNOWN-CS?Q?abc?=", "=?X-UNKNOWN-CS?Q?abc?="},
		// 2: only "B" and "Q" exist.
		{"unknown encoding", "=?UTF-8?X?abc?=", "=?UTF-8?X?abc?="},
		// 2: no white space inside an encoded word.
		{"space inside", "=?UTF-8?Q?a b?=", "=?UTF-8?Q?a b?="},
		{"unterminated", "=?UTF-8?Q?abc", "=?UTF-8?Q?abc"},
		{"plain text", "nothing = to ? see", "nothing = to ? see"},
		// RFC 2231 5: a language after the charset.
		{"language", "=?US-ASCII*EN?Q?Keith_Moore?=", "Keith Moore"},
	} {
		if got := DecodeWords(tc.in); got != tc.want {
			t.Errorf("%s: DecodeWords(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// Deviations from RFC 2047, kept on purpose; see DecodeWords.
func TestDeviationEncodedWords(t *testing.T) {
	// 5 (3): an encoded word "MUST NOT appear within a quoted-string".
	// Outlook and others put names there, and the reader wants the name.
	wantJSON(t, "in a quoted string", Addresses(` "=?UTF-8?Q?J=C3=BCrgen?=" <j@example.test>`),
		`[{"name":"Jürgen","email":"j@example.test"}]`)
	// 5 (3): "each encoded-word MUST represent an integral number of
	// characters". Senders split a character over two words anyway.
	if got := DecodeWords("=?UTF-8?B?w6k=?= =?UTF-8?Q?=C3?= =?UTF-8?Q?=A9?="); got != "éé" {
		t.Errorf("split character: %q", got)
	}
	// 2: an encoded word is at most 75 characters. Longer ones decode.
	long := "=?UTF-8?Q?" + strings.Repeat("a", 200) + "?="
	if got := DecodeWords(long); got != strings.Repeat("a", 200) {
		t.Errorf("long word not decoded")
	}
}

// Charsets through golang.org/x/text, by the labels mail uses.
func TestCharsets(t *testing.T) {
	for _, tc := range []struct {
		label string
		in    []byte
		want  string
	}{
		{"iso-8859-1", []byte{0xe9}, "é"},
		{"ISO-8859-15", []byte{0xa4}, "€"},
		{"windows-1252", []byte{0x80}, "€"},
		{"koi8-r", []byte{0xf0, 0xd2, 0xc9, 0xd7, 0xc5, 0xd4}, "Привет"},
		{"iso-2022-jp", []byte("\x1b$B$3$s$K$A$O\x1b(B"), "こんにちは"},
		{"shift_jis", []byte{0x82, 0xb1, 0x82, 0xf1}, "こん"},
		{"gb2312", []byte{0xc4, 0xe3, 0xba, 0xc3}, "你好"},
		{"big5", []byte{0xa7, 0x41, 0xa6, 0x6e}, "你好"},
		{"euc-kr", []byte{0xbe, 0xc8, 0xb3, 0xe7}, "안녕"},
		{"utf-8", []byte("h\xc3\xa9"), "hé"},
		{"US-ASCII", []byte("plain"), "plain"},
	} {
		got, known := decodeCharset(tc.in, tc.label)
		if !known || got != tc.want {
			t.Errorf("%s: %q, known %v, want %q", tc.label, got, known, tc.want)
		}
	}
	if _, known := decodeCharset([]byte("x"), "x-no-such-charset"); known {
		t.Error("an unknown charset was known")
	}
}
