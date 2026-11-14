package imap

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// More of RFC 3501: the commands of the selected state (6.4.4 to 6.4.8),
// the data of 7.4.2, and the syntax rules of sections 4 and 9.

func day1994(d int, month time.Month) time.Time { return time.Date(1994, month, d, 12, 0, 0, 0, time.UTC) }

// searchBox is a mailbox whose messages differ in what SEARCH looks at.
func searchBox(e *env) {
	msg := func(from, to, subject, date, body string, extra ...string) string {
		h := "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\nDate: " + date + "\r\n"
		for _, x := range extra {
			h += x + "\r\n"
		}
		return h + "\r\n" + body + "\r\n"
	}
	// 1: old, flagged, from Smith.
	e.st.addAt("INBOX", msg("John Smith <smith@example.org>", "fred@example.test", "budget", "Mon, 10 Jan 1994 10:00:00 +0000", "numbers"),
		day1994(10, time.January), "$flagged", "$seen")
	// 2: flagged, since February, not from Smith.
	e.st.addAt("INBOX", msg("Ann Jones <jones@example.org>", "fred@example.test", "lunch", "Wed, 2 Feb 1994 09:00:00 +0000", "sandwiches"),
		day1994(2, time.February), "$flagged")
	// 3: flagged, since February, from Smith.
	e.st.addAt("INBOX", msg("smith@example.org", "fred@example.test", "re: lunch", "Thu, 3 Feb 1994 09:00:00 +0000", "yes"),
		day1994(3, time.February), "$flagged", "$answered")
	// 4: not flagged; a custom header, a Cc, a keyword; a long body.
	e.st.addAt("INBOX", msg("Bob <bob@example.org>", "list@example.org", "minutes", "Fri, 4 Feb 1994 09:00:00 +0000",
		strings.Repeat("minutes of the meeting. ", 100), "Cc: fred@example.test", "X-Priority: 1 (Highest)"),
		day1994(4, time.February), "$seen", "work")
	// 5: a draft marked deleted, with an encoded subject and a base64 body
	// that holds the word "zanzibar"; a Bcc.
	e.st.addAt("INBOX", "From: fred@example.test\r\nTo: =?utf-8?q?J=C3=BCrgen?= <juergen@example.org>\r\nBcc: boss@example.org\r\n"+
		"Subject: =?utf-8?b?R3LDvMOfZSBhdXMgS8O2bG4=?=\r\nDate: Sat, 5 Feb 1994 23:30:00 -0800\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\nR3LDvMOfZSBhdXMgWmFuemliYXIK\r\n",
		day1994(6, time.February), "$draft", "$deleted")
}

// RFC 3501 6.4.4, the examples:
//
//	C: A282 SEARCH FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"
//	S: * SEARCH 2 84 882
//	C: A283 SEARCH TEXT "string not in mailbox"
//	S: * SEARCH
//	C: A284 SEARCH CHARSET UTF-8 TEXT {6}
//	C: XXXXXX
//	S: * SEARCH 43
//
// and every search key the section defines.
func TestRFC3501Search(t *testing.T) {
	e := newEnv(t)
	searchBox(e)
	c := e.selected("INBOX")
	c.expect(`SEARCH FLAGGED SINCE 1-Feb-1994 NOT FROM "Smith"`, "* SEARCH 2", "OK SEARCH completed")
	c.expect(`SEARCH TEXT "string not in mailbox"`, "* SEARCH", "OK SEARCH completed")
	// A literal, in UTF-8: "Grüße" is in the subject of message 5 as an
	// encoded word and in its body as base64.
	c.write("a1 SEARCH CHARSET UTF-8 TEXT {7}\r\n")
	if got := c.line(); got != "+ OK" {
		t.Fatalf("continuation: %q", got)
	}
	c.write("Grüße\r\n")
	wantLines(t, "a literal in UTF-8", c.until("a1"), "* SEARCH 5", "OK SEARCH completed")
	// "If the server does not support the specified [CHARSET], it MUST
	// return a tagged NO response ... SHOULD contain the BADCHARSET
	// response code".
	c.expect("SEARCH CHARSET KOI8-R ALL", "NO [BADCHARSET (US-ASCII UTF-8)] that charset is not supported")

	for _, tc := range []struct{ keys, want string }{
		{"ALL", "1 2 3 4 5"},
		{"ANSWERED", "3"}, {"UNANSWERED", "1 2 4 5"},
		{"DELETED", "5"}, {"UNDELETED", "1 2 3 4"},
		{"DRAFT", "5"}, {"UNDRAFT", "1 2 3 4"},
		{"FLAGGED", "1 2 3"}, {"UNFLAGGED", "4 5"},
		{"SEEN", "1 4"}, {"UNSEEN", "2 3 5"},
		{"KEYWORD work", "4"}, {"UNKEYWORD work", "1 2 3 5"}, {`KEYWORD \Seen`, "1 4"},
		// \Recent is not kept: nothing is NEW or RECENT, everything OLD.
		{"NEW", ""}, {"RECENT", ""}, {"OLD", "1 2 3 4 5"},
		{`FROM smith`, "1 3"}, {`FROM "John Smith"`, "1"}, {`TO juergen`, "5"}, {`CHARSET UTF-8 TO "Jürgen"`, "5"},
		{`CC fred`, "4"}, {`BCC boss`, "5"},
		{`SUBJECT LUNCH`, "2 3"}, {`SUBJECT "re: lunch"`, "3"},
		{`BODY sandwiches`, "2"}, {`BODY zanzibar`, "5"}, {`BODY lunch`, ""},
		{`TEXT lunch`, "2 3"}, {`TEXT example.org`, "1 2 3 4 5"},
		{`HEADER X-Priority highest`, "4"}, {`HEADER X-Priority ""`, "4"}, {`HEADER Cc ""`, "4"}, {`HEADER X-Nothing ""`, ""},
		{"BEFORE 3-Feb-1994", "1 2"}, {"ON 3-Feb-1994", "3"}, {"SINCE 3-Feb-1994", "3 4 5"}, {`ON "10-Jan-1994"`, "1"},
		// The Date field of message 5 says the 5th, its internal date the 6th.
		{"SENTBEFORE 3-Feb-1994", "1 2"}, {"SENTON 5-Feb-1994", ""}, {"SENTON 6-Feb-1994", "5"}, {"SENTSINCE 4-Feb-1994", "4 5"},
		{"LARGER 1000", "4"}, {"SMALLER 1000", "1 2 3 5"},
		{"1:2", "1 2"}, {"2,4:*", "2 4 5"}, {"*", "5"}, {"UID 3:*", "3 4 5"}, {"UID 7:*", "5"},
		{"NOT FLAGGED", "4 5"}, {"NOT NOT SEEN", "1 4"},
		{"OR SEEN DRAFT", "1 4 5"}, {"OR FROM smith OR TO list SUBJECT lunch", "1 2 3 4"},
		{"(FLAGGED UNSEEN)", "2 3"}, {"FLAGGED (OR SEEN ANSWERED)", "1 3"}, {"NOT (FLAGGED SEEN)", "2 3 4 5"},
		{"FLAGGED UNSEEN SINCE 3-Feb-1994", "3"},
	} {
		want := "* SEARCH"
		if tc.want != "" {
			want += " " + tc.want
		}
		c.expect("SEARCH "+tc.keys, want, "OK SEARCH completed")
	}

	// Syntax.
	for _, tc := range []struct{ keys, want string }{
		{"", "BAD a search key is missing"},
		{"COLOUR red", "BAD unknown search key COLOUR"},
		{"FROM", "BAD a space is missing"},
		{"SINCE yesterday", `BAD not a date: "yesterday"`},
		{"LARGER big", "BAD a number is missing"},
		{"OR SEEN", "BAD a space is missing"},
		{"(SEEN", `BAD ")" is missing`},
		{"0", "BAD 0 is not a message number"},
		{"9", "BAD a message number in the search is above the 5 messages of the mailbox"},
		{strings.Repeat("NOT ", 40) + "SEEN", "BAD the search is nested too deep"},
	} {
		c.expect("SEARCH "+tc.keys, tc.want)
	}
}

// What a search reads. Flags, sizes, dates and the remembered header
// fields cost no message; HEADER, BODY and TEXT open each candidate, and
// only the candidates the cheaper keys left.
func TestSearchOpensOnlyWhatItMust(t *testing.T) {
	e := newEnv(t)
	searchBox(e)
	c := e.selected("INBOX")
	opened := func(search string) int {
		e.st.mu.Lock()
		e.st.opened = 0
		e.st.mu.Unlock()
		c.ok(search)
		e.st.mu.Lock()
		defer e.st.mu.Unlock()
		return e.st.opened
	}
	for _, tc := range []struct {
		search string
		want   int
	}{
		{"SEARCH FLAGGED SINCE 1-Feb-1994 NOT FROM smith SUBJECT lunch SMALLER 5000", 0},
		{"SEARCH BODY sandwiches", 5},
		{"SEARCH UNSEEN FLAGGED BODY sandwiches", 2},
		{"SEARCH HEADER X-Priority 1", 5},
	} {
		if got := opened(tc.search); got != tc.want {
			t.Errorf("%s opened %d messages, want %d", tc.search, got, tc.want)
		}
	}
	// A header field that the store remembers cut short: a miss in what
	// is remembered is settled by the message.
	long := "Subject: " + strings.Repeat("padding ", 20) + "needle\r\n\r\nx\r\n"
	e.st.add("INBOX", long)
	c.ok("NOOP")
	c.expect("SEARCH SUBJECT needle", "* SEARCH 6", "OK SEARCH completed")
}

// The message of RFC 3501 6.4.5, where part numbers are explained:
//
//	HEADER     ([RFC-2822] header of the message)
//	TEXT       ([RFC-2822] text body of the message) MULTIPART/MIXED
//	1          TEXT/PLAIN
//	2          APPLICATION/OCTET-STREAM
//	3          MESSAGE/RFC822
//	3.HEADER   ([RFC-2822] header of the message)
//	3.TEXT     ([RFC-2822] text body of the message) MULTIPART/MIXED
//	3.1        TEXT/PLAIN
//	3.2        APPLICATION/OCTET-STREAM
//	4          MULTIPART/MIXED
//	4.1        IMAGE/GIF
//	4.1.MIME   ([MIME-IMB] header for the IMAGE/GIF)
//	4.2        MESSAGE/RFC822
//	4.2.HEADER ([RFC-2822] header of the message)
//	4.2.TEXT   ([RFC-2822] text body of the message) MULTIPART/MIXED
//	4.2.1      TEXT/PLAIN
//	4.2.2      MULTIPART/ALTERNATIVE
//	4.2.2.1    TEXT/PLAIN
//	4.2.2.2    TEXT/RICHTEXT
var (
	partsInner3 = "Subject: three\r\nContent-Type: multipart/mixed; boundary=b3\r\n\r\n" +
		"--b3\r\nContent-Type: text/plain\r\n\r\nthree one\r\n" +
		"--b3\r\nContent-Type: application/octet-stream\r\n\r\nthree two\r\n" +
		"--b3--\r\n"
	partsInner42 = "Subject: four two\r\nContent-Type: multipart/mixed; boundary=b42\r\n\r\n" +
		"--b42\r\nContent-Type: text/plain\r\n\r\nfour two one\r\n" +
		"--b42\r\nContent-Type: multipart/alternative; boundary=b422\r\n\r\n" +
		"--b422\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--b422\r\nContent-Type: text/richtext\r\n\r\n<bold>rich</bold>\r\n" +
		"--b422--\r\n" +
		"--b42--\r\n"
	partsHeader = "From: fred@example.test\r\nSubject: parts\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=top\r\n\r\n"
	partsText   = "preamble\r\n" +
		"--top\r\nContent-Type: text/plain\r\n\r\none\r\n" +
		"--top\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\ndHdv\r\n" +
		"--top\r\nContent-Type: message/rfc822\r\n\r\n" + partsInner3 +
		"--top\r\nContent-Type: multipart/mixed; boundary=b4\r\n\r\n" +
		"--b4\r\nContent-Type: image/gif\r\nContent-Transfer-Encoding: base64\r\n\r\nR0lGODlh\r\n" +
		"--b4\r\nContent-Type: message/rfc822\r\n\r\n" + partsInner42 +
		"--b4--\r\n" +
		"--top--\r\nepilogue\r\n"
	partsMessage = partsHeader + partsText
)

func literal(s string) string { return fmt.Sprintf("{%d}\r\n%s", len(s), s) }

func TestRFC3501FetchBodyParts(t *testing.T) {
	e := newEnv(t)
	e.st.add("INBOX", partsMessage, "$seen")
	c := e.selected("INBOX")
	for _, tc := range []struct{ section, want string }{
		{"", partsMessage},
		{"HEADER", partsHeader},
		{"TEXT", partsText},
		{"1", "one"},
		{"2", "dHdv"},
		{"3", partsInner3[:len(partsInner3)-2]},
		{"3.HEADER", "Subject: three\r\nContent-Type: multipart/mixed; boundary=b3\r\n\r\n"},
		{"3.TEXT", partsInner3[strings.Index(partsInner3, "--b3") : len(partsInner3)-2]},
		{"3.1", "three one"},
		{"3.2", "three two"},
		{"4.1", "R0lGODlh"},
		{"4.1.MIME", "Content-Type: image/gif\r\nContent-Transfer-Encoding: base64\r\n\r\n"},
		{"4.2.HEADER", "Subject: four two\r\nContent-Type: multipart/mixed; boundary=b42\r\n\r\n"},
		{"4.2.1", "four two one"},
		{"4.2.2.1", "plain"},
		{"4.2.2.2", "<bold>rich</bold>"},
		{"1.MIME", "Content-Type: text/plain\r\n\r\n"},
		{"3.MIME", "Content-Type: message/rfc822\r\n\r\n"},
		{"4.2.2.MIME", "Content-Type: multipart/alternative; boundary=b422\r\n\r\n"},
	} {
		c.expect("FETCH 1 BODY["+tc.section+"]", "* 1 FETCH (BODY["+tc.section+"] "+literal(tc.want)+")", "OK FETCH completed")
	}
	// 4 and 4.2.TEXT are multiparts: their body is what is between the
	// part's header and the delimiter that ends it.
	four := c.ok("FETCH 1 BODY[4]")[0]
	if !strings.Contains(four, "--b4\r\nContent-Type: image/gif") || !strings.HasSuffix(four, "--b4--)") {
		t.Errorf("BODY[4]: %q", four)
	}
	// A part that is not there: "BODY[5] NIL"; so is HEADER of a part
	// that is no message.
	for _, section := range []string{"5", "1.1", "3.3", "4.2.2.3", "1.HEADER", "2.TEXT", "4.1.HEADER"} {
		c.expect("FETCH 1 BODY["+section+"]", "* 1 FETCH (BODY["+section+"] NIL)", "OK FETCH completed")
	}

	// HEADER.FIELDS and HEADER.FIELDS.NOT: "Subsetting does not exclude
	// the [RFC-2822] delimiting blank line between the header and the
	// body; the blank line is included in all header fetches".
	c.expect("FETCH 1 BODY[HEADER.FIELDS (subject FROM)]",
		"* 1 FETCH (BODY[HEADER.FIELDS (subject FROM)] "+literal("From: fred@example.test\r\nSubject: parts\r\n\r\n")+")", "OK FETCH completed")
	c.expect("FETCH 1 BODY[HEADER.FIELDS.NOT (subject FROM)]",
		"* 1 FETCH (BODY[HEADER.FIELDS.NOT (subject FROM)] "+
			literal("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=top\r\n\r\n")+")", "OK FETCH completed")
	c.expect("FETCH 1 BODY[3.HEADER.FIELDS (Subject)]",
		"* 1 FETCH (BODY[3.HEADER.FIELDS (Subject)] "+literal("Subject: three\r\n\r\n")+")", "OK FETCH completed")
	c.expect("FETCH 1 BODY[HEADER.FIELDS (X-Nothing)]", "* 1 FETCH (BODY[HEADER.FIELDS (X-Nothing)] "+literal("\r\n")+")", "OK FETCH completed")

	// Partial fetches: "BODY[]<0.2048>" answers "BODY[]<0>"; a start
	// beyond the end gives an empty string; an end beyond it is cut.
	c.expect("FETCH 1 BODY[1]<0.2>", "* 1 FETCH (BODY[1]<0> {2}\r\non)", "OK FETCH completed")
	c.expect("FETCH 1 BODY[1]<1.200>", "* 1 FETCH (BODY[1]<1> {2}\r\nne)", "OK FETCH completed")
	c.expect("FETCH 1 BODY[1]<3.5>", "* 1 FETCH (BODY[1]<3> {0}\r\n)", "OK FETCH completed")
	c.expect("FETCH 1 BODY[HEADER]<6.4>", "* 1 FETCH (BODY[HEADER]<6> {4}\r\nfred)", "OK FETCH completed")
	c.expect("FETCH 1 BODY[1]<0.0>", "BAD a partial fetch of 0 octets")

	// RFC822, RFC822.HEADER and RFC822.TEXT are BODY[], BODY.PEEK[HEADER]
	// and BODY[TEXT] under older names.
	c.expect("FETCH 1 RFC822.HEADER", "* 1 FETCH (RFC822.HEADER "+literal(partsHeader)+")", "OK FETCH completed")
	c.expect("FETCH 1 RFC822.TEXT", "* 1 FETCH (RFC822.TEXT "+literal(partsText)+")", "OK FETCH completed")
	c.expect("FETCH 1 RFC822", "* 1 FETCH (RFC822 "+literal(partsMessage)+")", "OK FETCH completed")

	// The structure of the same message (7.4.2).
	// A message/rfc822 part has, after its size, the envelope and the
	// structure of the message it holds and its size in lines.
	inner3 := `("MESSAGE" "RFC822" NIL NIL NIL "7BIT" ` + fmt.Sprint(len(partsInner3)-2) +
		` (NIL "three" NIL NIL NIL NIL NIL NIL NIL NIL) (` + `("TEXT" "PLAIN" NIL NIL NIL "7BIT" 9 1)` +
		`("APPLICATION" "OCTET-STREAM" NIL NIL NIL "7BIT" 9) "MIXED") 12)`
	inner42 := `("MESSAGE" "RFC822" NIL NIL NIL "7BIT" ` + fmt.Sprint(len(partsInner42)-2) +
		` (NIL "four two" NIL NIL NIL NIL NIL NIL NIL NIL) (` + `("TEXT" "PLAIN" NIL NIL NIL "7BIT" 12 1)` +
		`(("TEXT" "PLAIN" NIL NIL NIL "7BIT" 5 1)("TEXT" "RICHTEXT" NIL NIL NIL "7BIT" 17 1) "ALTERNATIVE") "MIXED") 20)`
	c.expect("FETCH 1 BODY",
		`* 1 FETCH (BODY (("TEXT" "PLAIN" NIL NIL NIL "7BIT" 3 1)("APPLICATION" "OCTET-STREAM" NIL NIL NIL "BASE64" 4)`+
			inner3+`(("IMAGE" "GIF" NIL NIL NIL "BASE64" 8)`+inner42+` "MIXED") "MIXED"))`, "OK FETCH completed")
}

// RFC 3501 7.4.2, the examples of BODYSTRUCTURE:
//
//	("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 3028 92)
//
// (in the sample session) and, for a multipart,
//
//	(("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 1152 23)
//	 ("TEXT" "PLAIN" ("CHARSET" "US-ASCII" "NAME" "cc.diff")
//	  "<960723163407.20117h@cac.washington.edu>" "Compiler diff"
//	  "BASE64" 4554 73) "MIXED")
//
// The message below is built to have exactly those parts.
func TestRFC3501BodyStructure(t *testing.T) {
	part1 := strings.Repeat(strings.Repeat("a", 50)+"\r\n", 22) + "abcdef\r\n"
	part2 := strings.Repeat(strings.Repeat("Q", 60)+"\r\n", 72) + strings.Repeat("Q", 88) + "\r\n"
	if len(part1) != 1152 || len(part2) != 4554 {
		t.Fatalf("the parts are %d and %d octets", len(part1), len(part2))
	}
	msg := "From: fred@example.test\r\nMIME-Version: 1.0\r\nContent-Type: MULTIPART/MIXED; BOUNDARY=\"x\"\r\n\r\n" +
		"--x\r\nContent-Type: TEXT/PLAIN; CHARSET=US-ASCII\r\n\r\n" + part1 + "\r\n" +
		"--x\r\nContent-Type: TEXT/PLAIN; CHARSET=US-ASCII; NAME=\"cc.diff\"\r\n" +
		"Content-ID: <960723163407.20117h@cac.washington.edu>\r\nContent-Description: Compiler diff\r\n" +
		"Content-Transfer-Encoding: BASE64\r\n\r\n" + part2 + "\r\n" +
		"--x--\r\n"
	e := newEnv(t)
	e.st.add("INBOX", msg)
	c := e.selected("INBOX")
	c.expect("FETCH 1 BODY",
		`* 1 FETCH (BODY (("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 1152 23)`+
			`("TEXT" "PLAIN" ("CHARSET" "US-ASCII" "NAME" "cc.diff") "<960723163407.20117h@cac.washington.edu>" "Compiler diff" "BASE64" 4554 73) "MIXED"))`,
		"OK FETCH completed")
	// BODYSTRUCTURE adds the extension data: for a part MD5, disposition,
	// language and location; for a multipart the parameters first.
	c.expect("FETCH 1 BODYSTRUCTURE",
		`* 1 FETCH (BODYSTRUCTURE (("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 1152 23 NIL NIL NIL NIL)`+
			`("TEXT" "PLAIN" ("CHARSET" "US-ASCII" "NAME" "cc.diff") "<960723163407.20117h@cac.washington.edu>" "Compiler diff" "BASE64" 4554 73 NIL NIL NIL NIL)`+
			` "MIXED" ("BOUNDARY" "x") NIL NIL NIL))`,
		"OK FETCH completed")

	// Disposition, language, location and MD5.
	e.st.add("INBOX", "Content-Type: application/pdf; name=\"report.pdf\"\r\nContent-Disposition: attachment; filename=\"report.pdf\"; size=4\r\n"+
		"Content-Language: en, nl\r\nContent-Location: http://example.org/report.pdf\r\nContent-MD5: Q2hlY2sgSW50ZWdyaXR5IQ==\r\n"+
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERg==\r\n")
	c.ok("NOOP")
	c.expect("FETCH 2 BODYSTRUCTURE",
		`* 2 FETCH (BODYSTRUCTURE ("APPLICATION" "PDF" ("NAME" "report.pdf") NIL NIL "BASE64" 10 "Q2hlY2sgSW50ZWdyaXR5IQ==" `+
			`("attachment" ("FILENAME" "report.pdf" "SIZE" "4")) ("en" "nl") "http://example.org/report.pdf"))`,
		"OK FETCH completed")

	// A message without a Content-Type is text/plain in US-ASCII (RFC 2045
	// 5.2); one without a body or without the empty line still has a
	// structure.
	e.st.add("INBOX", "Subject: bare\r\n\r\nhello\r\n")
	e.st.add("INBOX", "Subject: header only\r\n")
	c.ok("NOOP")
	c.expect("FETCH 3:4 BODY",
		`* 3 FETCH (BODY ("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 7 1))`,
		`* 4 FETCH (BODY ("TEXT" "PLAIN" ("CHARSET" "US-ASCII") NIL NIL "7BIT" 0 0))`,
		"OK FETCH completed")
}

// RFC 3501 7.4.2, ENVELOPE: "The fields of the envelope structure are in
// the following order: date, subject, from, sender, reply-to, to, cc, bcc,
// in-reply-to, and message-id." "If the Sender or Reply-To lines are absent
// in the [RFC-2822] header, or are present but empty, the server sets the
// corresponding member of the envelope to be the same value as the from
// member". Group syntax: "a start of group ... has a NIL host name and the
// mailbox name field holds the group name phrase"; the end of a group is
// an address of four NILs.
func TestRFC3501Envelope(t *testing.T) {
	e := newEnv(t)
	e.st.add("INBOX", "Date: Wed, 17 Jul 1996 02:23:25 -0700 (PDT)\r\n"+
		"From: \"Terry Gray\" <gray@cac.washington.edu>\r\nSender: secretary@cac.washington.edu\r\n"+
		"Reply-To: Terry <reply@cac.washington.edu>, other@example.org\r\n"+
		"To: A Group: one@example.org, Two <two@example.org>;, three@example.org\r\n"+
		"Bcc: undisclosed-recipients:;\r\n"+
		"Subject: a subject that is\r\n folded over two lines\r\n"+
		"In-Reply-To: <parent@example.org>\r\nMessage-ID: <child@example.org>\r\n\r\nx\r\n")
	// Nothing but a From, with a name outside ASCII and a quote in it.
	e.st.add("INBOX", "From: =?utf-8?q?J=C3=BCrgen_=22the_boss=22?= <j@example.org>\r\n\r\nx\r\n")
	e.st.add("INBOX", "\r\nno header at all\r\n")
	c := e.selected("INBOX")
	c.expect("FETCH 1:3 ENVELOPE",
		`* 1 FETCH (ENVELOPE ("Wed, 17 Jul 1996 02:23:25 -0700 (PDT)" "a subject that is folded over two lines" `+
			`(("Terry Gray" NIL "gray" "cac.washington.edu")) `+
			`((NIL NIL "secretary" "cac.washington.edu")) `+
			`(("Terry" NIL "reply" "cac.washington.edu")(NIL NIL "other" "example.org")) `+
			`((NIL NIL "A Group" NIL)(NIL NIL "one" "example.org")("Two" NIL "two" "example.org")(NIL NIL NIL NIL)(NIL NIL "three" "example.org")) `+
			`NIL ((NIL NIL "undisclosed-recipients" NIL)(NIL NIL NIL NIL)) "<parent@example.org>" "<child@example.org>"))`,
		`* 2 FETCH (ENVELOPE (NIL NIL `+
			`(("=?utf-8?q?J=C3=BCrgen_\"the_boss\"?=" NIL "j" "example.org")) `+
			`(("=?utf-8?q?J=C3=BCrgen_\"the_boss\"?=" NIL "j" "example.org")) `+
			`(("=?utf-8?q?J=C3=BCrgen_\"the_boss\"?=" NIL "j" "example.org")) NIL NIL NIL NIL NIL))`,
		`* 3 FETCH (ENVELOPE (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL))`,
		"OK FETCH completed")
}

// RFC 3501 6.4.5, the example:
//
//	C: A654 FETCH 2:4 (FLAGS BODY[HEADER.FIELDS (DATE FROM)])
//	S: * 2 FETCH ....
//	S: * 3 FETCH ....
//	S: * 4 FETCH ....
//	S: A654 OK FETCH completed
//
// the macros ALL, FAST and FULL, and the rule for \Seen: BODY[...] sets
// it, BODY.PEEK[...] does not, and "if this causes the flags to change,
// they SHOULD be included as part of the FETCH responses".
func TestRFC3501Fetch(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 5)
	c := e.selected("INBOX")
	hdr := func(n int) string {
		return literal(fmt.Sprintf("Date: Mon, 7 Feb 1994 21:52:25 -0800\r\nFrom: Fred Foobar <foobar@Blurdybloop.COM>\r\n\r\n"))
	}
	c.expect("FETCH 2:4 (FLAGS BODY[HEADER.FIELDS (DATE FROM)])",
		"* 2 FETCH (FLAGS () BODY[HEADER.FIELDS (DATE FROM)] "+hdr(2)+")",
		"* 3 FETCH (FLAGS () BODY[HEADER.FIELDS (DATE FROM)] "+hdr(3)+")",
		"* 4 FETCH (FLAGS () BODY[HEADER.FIELDS (DATE FROM)] "+hdr(4)+")",
		// It was not .PEEK: the messages are seen now, and the client is told.
		`* 2 FETCH (FLAGS (\Seen))`, `* 3 FETCH (FLAGS (\Seen))`, `* 4 FETCH (FLAGS (\Seen))`,
		"OK FETCH completed")
	c.expect("FETCH 5 BODY.PEEK[TEXT]", "* 5 FETCH (BODY[TEXT] "+literal("Message 5.\r\n")+")", "OK FETCH completed")
	c.expect("FETCH 1,5 FLAGS", "* 1 FETCH (FLAGS ())", "* 5 FETCH (FLAGS ())", "OK FETCH completed")

	size := len(plain(1, "message 1"))
	c.expect("FETCH 1 FAST",
		fmt.Sprintf(`* 1 FETCH (FLAGS () INTERNALDATE "17-Jul-1996 02:44:25 -0700" RFC822.SIZE %d)`, size), "OK FETCH completed")
	all := c.ok("FETCH 1 ALL")
	if len(all) != 1 || !strings.HasPrefix(all[0], "* 1 FETCH (FLAGS () INTERNALDATE") || !strings.Contains(all[0], " ENVELOPE (") || strings.Contains(all[0], "BODY") {
		t.Errorf("ALL: %q", all)
	}
	full := c.ok("FETCH 1 FULL")
	if len(full) != 1 || !strings.Contains(full[0], " ENVELOPE (") || !strings.Contains(full[0], ` BODY ("TEXT" "PLAIN"`) {
		t.Errorf("FULL: %q", full)
	}
	c.expect("FETCH 1 (UID RFC822.SIZE)", fmt.Sprintf("* 1 FETCH (UID 1 RFC822.SIZE %d)", size), "OK FETCH completed")

	// Syntax.
	for _, tc := range []struct{ args, want string }{
		{"", "BAD a number is missing"},
		{"1", "BAD a space is missing"},
		{"1 COLOUR", `BAD unknown FETCH item "COLOUR"`},
		{"1 ()", "BAD FETCH needs an item"},
		{"1 (FLAGS", "BAD a space is missing"},
		{"1 BODY[NOPE]", `BAD unknown section "NOPE"`},
		{"1 BODY[MIME]", "BAD MIME needs a part number"},
		{"1 BODY[HEADER.FIELDS ()]", "BAD the list of header fields is empty"},
		{"1 BODY[1", `BAD "]" is missing`},
		{"0 FLAGS", "BAD 0 is not a message number"},
		{"6 FLAGS", "BAD there is no message 6: the mailbox has 5"},
		{"1:9 FLAGS", "BAD there is no message 9: the mailbox has 5"},
		{"a FLAGS", "BAD a number is missing"},
	} {
		c.expect("FETCH "+tc.args, tc.want)
	}
}

// RFC 3501 6.4.6, the example:
//
//	C: A003 STORE 2:4 +FLAGS (\Deleted)
//	S: * 2 FETCH (FLAGS (\Deleted \Seen))
//	S: * 3 FETCH (FLAGS (\Deleted))
//	S: * 4 FETCH (FLAGS (\Deleted \Flagged \Seen))
//	S: A003 OK STORE completed
//
// and FLAGS, -FLAGS and the .SILENT forms.
func TestRFC3501Store(t *testing.T) {
	e := newEnv(t)
	e.st.add("INBOX", plain(1, "one"))
	e.st.add("INBOX", plain(2, "two"), "$seen")
	e.st.add("INBOX", plain(3, "three"))
	e.st.add("INBOX", plain(4, "four"), "$flagged", "$seen")
	c := e.selected("INBOX")
	c.expect(`STORE 2:4 +FLAGS (\Deleted)`,
		`* 2 FETCH (FLAGS (\Deleted \Seen))`,
		`* 3 FETCH (FLAGS (\Deleted))`,
		`* 4 FETCH (FLAGS (\Flagged \Deleted \Seen))`,
		"OK STORE completed")
	// "FLAGS.SILENT ... Equivalent to FLAGS, but without returning a new value".
	c.expect(`STORE 2:4 -FLAGS.SILENT (\Deleted)`, "OK STORE completed")
	// Without .SILENT the flags come back also when nothing changed.
	c.expect(`STORE 2 -FLAGS (\Deleted)`, `* 2 FETCH (FLAGS (\Seen))`, "OK STORE completed")
	// FLAGS replaces; a keyword is a flag like any other; the list may
	// come without parentheses.
	c.expect(`STORE 1 FLAGS (\Answered $Forwarded Work)`,
		`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft $Forwarded work)`,
		`* 1 FETCH (FLAGS (\Answered $Forwarded work))`, "OK STORE completed")
	c.expect(`STORE 1 FLAGS \Draft`, `* 1 FETCH (FLAGS (\Draft))`, "OK STORE completed")
	c.expect(`STORE 1 FLAGS ()`, `* 1 FETCH (FLAGS ())`, "OK STORE completed")
	// \Recent cannot be changed by a client (2.3.2): it is ignored.
	c.expect(`STORE 1 +FLAGS (\Recent \Seen)`, `* 1 FETCH (FLAGS (\Seen))`, "OK STORE completed")
	// Another session sees it at its next command.
	o := e.selected("INBOX")
	c.ok(`STORE 3 +FLAGS.SILENT (\Flagged)`)
	o.expect("NOOP", `* 3 FETCH (FLAGS (\Flagged))`, "OK NOOP completed")

	for _, tc := range []struct{ args, want string }{
		{`1 +FLAGS (\Nonsense)`, `BAD \Nonsense is not a flag`},
		{`1 FLAGGS (\Seen)`, "BAD STORE changes FLAGS or FLAGS.SILENT"},
		{`1 +FLAGS`, "BAD a space is missing"},
		{`9 +FLAGS (\Seen)`, "BAD there is no message 9: the mailbox has 4"},
		{`1 +FLAGS (\Seen`, "BAD a space is missing"},
	} {
		c.expect("STORE "+tc.args, tc.want)
	}
}

// RFC 3501 6.4.7, the example:
//
//	C: A003 COPY 2:4 MEETING
//	S: A003 OK COPY completed
//
// "The flags and internal date of the message(s) SHOULD be preserved".
// "If the destination mailbox does not exist, a server SHOULD return an
// error ... [TRYCREATE]". With UIDPLUS the OK carries COPYUID.
func TestRFC3501Copy(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 5)
	e.st.setFlag("INBOX", 3, "$flagged", true)
	c := e.selected("INBOX")
	c.expect("COPY 2:4 MEETING", "NO [TRYCREATE] no such mailbox")
	c.ok("CREATE MEETING")
	c.expect("COPY 2:4 MEETING", "OK [COPYUID 3857529051 2:4 1:3] COPY completed")
	c.expect("STATUS MEETING (MESSAGES UIDNEXT)", "* STATUS MEETING (MESSAGES 3 UIDNEXT 4)", "OK STATUS completed")
	c.expect("STATUS INBOX (MESSAGES)", "* STATUS INBOX (MESSAGES 5)", "OK STATUS completed")
	c.ok("SELECT MEETING")
	c.expect("FETCH 1:* (UID FLAGS INTERNALDATE)",
		`* 1 FETCH (UID 1 FLAGS () INTERNALDATE "17-Jul-1996 02:44:25 -0700")`,
		`* 2 FETCH (UID 2 FLAGS (\Flagged) INTERNALDATE "17-Jul-1996 02:44:25 -0700")`,
		`* 3 FETCH (UID 3 FLAGS () INTERNALDATE "17-Jul-1996 02:44:25 -0700")`,
		"OK FETCH completed")
	c.expect("COPY 9 INBOX", "BAD there is no message 9: the mailbox has 3")
}

// RFC 3501 6.4.8, the example:
//
//	C: A999 UID FETCH 4827313:4828442 FLAGS
//	S: * 23 FETCH (FLAGS (\Seen) UID 4827313)
//	S: * 24 FETCH (FLAGS (\Seen) UID 4827943)
//	S: * 25 FETCH (FLAGS (\Seen) UID 4828442)
//	S: A999 OK UID FETCH completed
//
// "the number after the '*' in an untagged FETCH response is always a
// message sequence number, not a unique identifier". "A non-existent
// unique identifier is ignored without any error message generated".
// And section 9, sequence-set: "*" is the largest number in use, and "the
// order of range ends is not significant": with UIDs 1 to 8 present,
// "9:*" is 8:9 and names message 8.
func TestRFC3501UID(t *testing.T) {
	e := newEnv(t)
	e.fill("INBOX", 8)
	for _, uid := range []uint32{2, 5} {
		e.st.remove("INBOX", uid)
	}
	for _, uid := range []uint32{3, 4, 6} {
		e.st.setFlag("INBOX", uid, "$seen", true)
	}
	c := e.selected("INBOX")
	c.expect("UID FETCH 3:6 FLAGS",
		`* 2 FETCH (UID 3 FLAGS (\Seen))`, `* 3 FETCH (UID 4 FLAGS (\Seen))`, `* 4 FETCH (UID 6 FLAGS (\Seen))`,
		"OK UID FETCH completed")
	c.expect("UID FETCH 2,5,99 FLAGS", "OK UID FETCH completed")
	c.expect("UID FETCH 9:* UID", "* 6 FETCH (UID 8)", "OK UID FETCH completed")
	c.expect("UID FETCH *:7 UID", "* 5 FETCH (UID 7)", "* 6 FETCH (UID 8)", "OK UID FETCH completed")
	c.expect("UID SEARCH UNSEEN", "* SEARCH 1 7 8", "OK UID SEARCH completed")
	c.expect("UID SEARCH 2:3", "* SEARCH 3 4", "OK UID SEARCH completed")
	// A client that uses UIDs gets them in every FETCH response.
	c.expect(`UID STORE 1:3 +FLAGS (\Flagged)`,
		`* 1 FETCH (UID 1 FLAGS (\Flagged))`, `* 2 FETCH (UID 3 FLAGS (\Flagged \Seen))`, "OK UID STORE completed")
	c.ok("CREATE target")
	c.expect("UID COPY 6:8 target", "OK [COPYUID 3857529051 6:8 1:3] UID COPY completed")
	c.expect("UID COPY 99 target", "OK UID COPY completed, no messages")
	c.expect("UID NONSENSE 1", "BAD unknown command")
	c.expect("UID", "BAD unknown command")
}
