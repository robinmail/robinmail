package delivery

// Tests written from RFC 5321 4.4, "Trace Information": the Return-Path and
// Received lines added at final delivery.

import (
	"context"
	"crypto/tls"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/smtp"
	"github.com/robinmail/robinmail/internal/store"
)

// The grammar of RFC 5321 4.4 as regular expressions, for a line whose
// folding has been undone (CRLF plus white space is FWS, here one space).
//
//	Time-stamp-line = "Received:" FWS Stamp <CRLF>
//	Stamp           = From-domain By-domain Opt-info [CFWS] ";" FWS date-time
//	From-domain     = "FROM" FWS Extended-Domain
//	By-domain       = CFWS "BY" FWS Extended-Domain
//	Extended-Domain = Domain / ( Domain FWS "(" TCP-info ")" ) /
//	                  ( address-literal FWS "(" TCP-info ")" )
//	TCP-info        = address-literal / ( Domain FWS address-literal )
//	Opt-info        = [Via] [With] [ID] [For] [Additional-Registered-Clauses]
//	With            = CFWS "WITH" FWS Protocol
//	ID              = CFWS "ID" FWS ( Atom / msg-id )
//	For             = CFWS "FOR" FWS ( Path / Mailbox )
const (
	reDomain  = `[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*`
	reLiteral = `\[(?:[0-9]{1,3}(?:\.[0-9]{1,3}){3}|IPv6:[0-9A-Fa-f:.]+)\]`
	reTCPInfo = `(?:` + reLiteral + `|` + reDomain + ` ` + reLiteral + `)`
	reExtDom  = `(?:` + reDomain + ` \(` + reTCPInfo + `\)|` + reLiteral + ` \(` + reTCPInfo + `\)|` + reDomain + `)`
	// A comment (RFC 5322 3.2.2) without nesting or quoted pairs, which is
	// all we write: ctext is printable ASCII except "(", ")" and "\".
	reComment = `\((?:[!-'*-\[\]-~]| )*\)`
	reCFWS    = `(?: |` + reComment + `)+`
	reAtom    = "[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+"
	// date-time of RFC 5322 3.3 without the obsolete forms, which 4.4
	// prohibits: four-digit year, numeric zone.
	reDate = `(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun), [0-9]{1,2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) [0-9]{4} ` +
		`[0-9]{2}:[0-9]{2}:[0-9]{2} [+-][0-9]{4}`
)

var stamp = regexp.MustCompile(`^Received: from (` + reExtDom + `)` +
	reCFWS + `by (` + reExtDom + `)` +
	`(?:` + reCFWS + `with (` + reAtom + `))?` +
	`(?:` + reCFWS + `id (` + reAtom + `))?` +
	`(?:` + reCFWS + `for (<[^<> ]+>))?` +
	`(?:` + reCFWS + `)?; (` + reDate + `)$`)

// unfold undoes header folding (RFC 5322 2.2.3) and drops the final CRLF.
func unfold(line string) string {
	return strings.ReplaceAll(strings.TrimSuffix(line, "\r\n"), "\r\n\t", " ")
}

// headerLines splits a stored message into its header fields, each with its
// continuation lines.
func headerLines(t *testing.T, raw string) []string {
	t.Helper()
	head, _, ok := strings.Cut(raw, "\r\n\r\n")
	if !ok {
		t.Fatalf("no end of header in %q", raw)
	}
	var out []string
	for _, l := range strings.SplitAfter(head+"\r\n", "\r\n") {
		switch {
		case l == "":
		case (l[0] == ' ' || l[0] == '\t') && len(out) > 0:
			out[len(out)-1] += l
		default:
			out = append(out, l)
		}
	}
	return out
}

// deliverOne delivers msg with env and returns the stored header fields.
func deliverOne(t *testing.T, env *smtp.Envelope, msg string) []string {
	t.Helper()
	d, st := setup(t, "")
	st.accounts["anna@example.test"] = "acct-anna"
	if err := d.Deliver(context.Background(), env, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := read(t, st, store.RoleInbox)
	if len(got) != 1 {
		t.Fatalf("%d copies for mike, want 1", len(got))
	}
	return headerLines(t, got[0])
}

// RFC 5321 4.4: the Received line follows the Time-stamp-line grammar; "the
// FROM clause ... SHOULD contain both (1) the name of the source host as
// presented in the EHLO command and (2) an address literal containing the IP
// address of the source, determined from the TCP connection"; the WITH
// protocol is a registered name (RFC 3848, RFC 6531 4); FOR has exactly one
// path.
func TestRFC5321_4_4_TimeStampLine(t *testing.T) {
	tlsState := &tls.ConnectionState{Version: tls.VersionTLS13, CipherSuite: tls.TLS_AES_128_GCM_SHA256}
	for _, tc := range []struct {
		name     string
		env      smtp.Envelope
		from, by string
		with     string
		forPath  string
	}{
		{"confirmed PTR name",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", Rcpts: []string{"mike@example.test"}},
			"bar.com (mail.sender.test [192.0.2.7])", "mx.example.test", "ESMTP", "<mike@example.test>"},
		{"no PTR name",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.9"), Helo: "bar.com", Rcpts: []string{"mike@example.test"}},
			"bar.com ([192.0.2.9])", "mx.example.test", "ESMTP", "<mike@example.test>"},
		// 4.1.3: an IPv6 address literal carries the tag "IPv6:".
		{"IPv6",
			smtp.Envelope{RemoteIP: net.ParseIP("2001:db8::25"), Helo: "bar.com", Rcpts: []string{"mike@example.test"}},
			"bar.com ([IPv6:2001:db8::25])", "mx.example.test", "ESMTP", "<mike@example.test>"},
		{"IPv4 over an IPv6 socket",
			smtp.Envelope{RemoteIP: net.ParseIP("::ffff:192.0.2.9"), Helo: "bar.com", Rcpts: []string{"mike@example.test"}},
			"bar.com ([192.0.2.9])", "mx.example.test", "ESMTP", "<mike@example.test>"},
		{"EHLO with an address literal",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.9"), Helo: "[192.0.2.9]", Rcpts: []string{"mike@example.test"}},
			"[192.0.2.9] ([192.0.2.9])", "mx.example.test", "ESMTP", "<mike@example.test>"},
		// The EHLO name is the client's word. One that is not a Domain or
		// address literal cannot go into the FROM clause as it is.
		{"EHLO name with an underscore",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.9"), Helo: "my_host", Rcpts: []string{"mike@example.test"}},
			"[192.0.2.9] ([192.0.2.9])", "mx.example.test", "ESMTP", "<mike@example.test>"},
		{"EHLO name that tries to forge the rest of the line",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.9"), Helo: "bar.com)(by.mx.bank.test", Rcpts: []string{"mike@example.test"}},
			"[192.0.2.9] ([192.0.2.9])", "mx.example.test", "ESMTP", "<mike@example.test>"},
		// RFC 3848: ESMTPS is ESMTP with STARTTLS.
		{"TLS",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", TLS: tlsState, Rcpts: []string{"mike@example.test"}},
			"bar.com (mail.sender.test [192.0.2.7])", "mx.example.test", "ESMTPS", "<mike@example.test>"},
		// RFC 6531 3.7.3 and 4: "The protocol value of the 'WITH' clause
		// when this extension is used is one of the SMTPUTF8 values."
		{"SMTPUTF8",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", UTF8: true, Rcpts: []string{"mike@example.test"}},
			"bar.com (mail.sender.test [192.0.2.7])", "mx.example.test", "UTF8SMTP", "<mike@example.test>"},
		{"SMTPUTF8 and TLS",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", UTF8: true, TLS: tlsState, Rcpts: []string{"mike@example.test"}},
			"bar.com (mail.sender.test [192.0.2.7])", "mx.example.test", "UTF8SMTPS", "<mike@example.test>"},
		// "If the FOR clause appears, it MUST contain exactly one <path>
		// entry, even when multiple RCPT commands have been given." With
		// several recipients we leave it out (7.2: it would disclose
		// them to each other).
		{"two recipients",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", Rcpts: []string{"mike@example.test", "anna@example.test"}},
			"bar.com (mail.sender.test [192.0.2.7])", "mx.example.test", "ESMTP", ""},
		// Two addresses of one account are one delivery.
		{"one account twice",
			smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", Rcpts: []string{"mike@example.test", "mike+x@example.test"}},
			"bar.com (mail.sender.test [192.0.2.7])", "mx.example.test", "ESMTP", "<mike@example.test>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.env.From = "Smith@bar.com"
			fields := deliverOne(t, &tc.env, "Subject: hi\r\n\r\nbody\r\n")
			if len(fields) < 2 || !strings.HasPrefix(fields[1], "Received:") {
				t.Fatalf("header %q", fields)
			}
			for _, l := range strings.Split(strings.TrimSuffix(fields[1], "\r\n"), "\r\n") {
				// RFC 5322 2.1.1: a line SHOULD be at most 78 characters.
				if len(l) > 78 {
					t.Errorf("line of %d characters: %q", len(l), l)
				}
			}
			line := unfold(fields[1])
			m := stamp.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("not a Time-stamp-line: %q", line)
			}
			if m[1] != tc.from || m[2] != tc.by || m[3] != tc.with || m[5] != tc.forPath {
				t.Errorf("from %q by %q with %q for %q\nwant from %q by %q with %q for %q\nin %q",
					m[1], m[2], m[3], m[5], tc.from, tc.by, tc.with, tc.forPath, line)
			}
			if m[4] == "" {
				t.Errorf("no ID clause in %q", line)
			}
			// "SHOULD use explicit offsets in the dates (e.g., -0800),
			// rather than time zone names of any type."
			when, err := time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", m[6])
			if err != nil || !when.Equal(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)) {
				t.Errorf("date %q: %v", m[6], err)
			}
			if (tc.env.TLS != nil) != strings.Contains(line, "(TLS 1.3 TLS_AES_128_GCM_SHA256)") {
				t.Errorf("TLS comment in %q", line)
			}
		})
	}
}

// RFC 5321 4.4: "When the delivery SMTP server makes the 'final delivery' of
// a message, it inserts a return-path line at the beginning of the mail
// data ... The return-path line preserves the information in the
// <reverse-path> from the MAIL command." And: "the final mail data will
// begin with a return path line, followed by one or more time stamp lines."
//
//	Return-path-line = "Return-Path:" FWS Reverse-path <CRLF>
func TestRFC5321_4_4_ReturnPath(t *testing.T) {
	for from, want := range map[string]string{
		"Smith@bar.com":            "Return-Path: <Smith@bar.com>\r\n", // 2.4: case preserved
		"":                         "Return-Path: <>\r\n",              // the null reverse-path of a notification
		`"Joe\,Smith"@bar.com`:     "Return-Path: <\"Joe\\,Smith\"@bar.com>\r\n",
		"bounce+a=b.test@bar.com":  "Return-Path: <bounce+a=b.test@bar.com>\r\n",
		"user@[IPv6:2001:db8::25]": "Return-Path: <user@[IPv6:2001:db8::25]>\r\n",
	} {
		env := &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", From: from, Rcpts: []string{"mike@example.test"}}
		fields := deliverOne(t, env, "Received: from a.test by b.test; Thu, 21 May 1998 05:33:29 -0700\r\nSubject: hi\r\n\r\nbody\r\n")
		if fields[0] != want {
			t.Errorf("reverse-path %q: first line %q, want %q", from, fields[0], want)
		}
		if !strings.HasPrefix(fields[1], "Received: from bar.com ") || !strings.HasPrefix(fields[2], "Received: from a.test ") {
			t.Errorf("reverse-path %q: the Return-Path is not followed by the time stamp lines: %q", from, fields)
		}
	}
}

// RFC 6531 3.7.3: with SMTPUTF8 the reverse-path in the Return-Path line may
// have non-ASCII characters, as it was given. It is not MIME-encoded: an
// encoded word is not an address.
func TestRFC6531_3_7_3_ReturnPath(t *testing.T) {
	env := &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", UTF8: true,
		From: "δοκιμή@παράδειγμα.δοκιμή", Rcpts: []string{"mike@example.test"}}
	fields := deliverOne(t, env, "Subject: hi\r\n\r\nbody\r\n")
	if fields[0] != "Return-Path: <δοκιμή@παράδειγμα.δοκιμή>\r\n" {
		t.Errorf("first line %q", fields[0])
	}
}

// RFC 5321 4.4: "exactly one return path SHOULD be present when the message
// is delivered", and "SMTP servers making final delivery MAY remove
// Return-path header fields before adding their own." We do: a Return-Path
// that came with the message is not the envelope's, and a client reading
// the wrong one would send bounces to whoever the sender named.
func TestRFC5321_4_4_OneReturnPath(t *testing.T) {
	env := &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "bar.com", From: "Smith@bar.com", Rcpts: []string{"mike@example.test"}}
	fields := deliverOne(t, env, "Return-Path: <victim@bank.test>\r\nSubject: hi\r\nreturn-path:\r\n\t<other@bank.test>\r\n\r\nbody\r\n")
	n := 0
	for _, f := range fields {
		if name, _, _ := strings.Cut(f, ":"); strings.EqualFold(name, "Return-Path") {
			n++
		}
	}
	if n != 1 || fields[0] != "Return-Path: <Smith@bar.com>\r\n" {
		t.Errorf("%d Return-Path lines: %q", n, fields)
	}
}

// RFC 5321 4.4: "An Internet mail program MUST NOT change or delete a
// Received: line that was previously added to the message header section.
// SMTP servers MUST prepend Received lines to messages; they MUST NOT change
// the order of existing lines or insert Received lines in any other
// location." The rest of the header and the body pass through untouched
// too (4.5.2: "All characters are to be delivered to the recipient's
// mailbox").
func TestRFC5321_4_4_ExistingTraceUntouched(t *testing.T) {
	// The message of appendix D.3, step 2, with a second hop added and a
	// header field between the Received lines.
	msg := "Received: from bar.com by foo.com ; Thu, 21 May 1998\r\n    05:33:29 -0700\r\n" +
		"X-Between: kept in place\r\n" +
		"Received:  FROM  older.example  BY bar.com\tWITH smtp;\r\n\t Thu, 21 May 1998 05:33:22 -0700\r\n" +
		"Date: Thu, 21 May 1998 05:33:22 -0700\r\n" +
		"From: John Q. Public <JQP@bar.com>\r\n" +
		"Subject:  The Next Meeting of the Board\r\n" +
		"To: Jones@xyz.com\r\n" +
		"\r\n" +
		"Bill:\r\nThe next meeting of the board of directors will be\r\non Tuesday.\r\n                        John.\r\n"
	d, st := setup(t, "")
	env := &smtp.Envelope{RemoteIP: net.ParseIP("192.0.2.7"), Helo: "foo.com", From: "JQP@bar.com", Rcpts: []string{"mike@example.test"}}
	if err := d.Deliver(context.Background(), env, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	got := read(t, st, store.RoleInbox)[0]
	if !strings.HasSuffix(got, msg) {
		t.Fatalf("the original message is not the end of the stored one:\n%q", got)
	}
	added := headerLines(t, strings.TrimSuffix(got, msg)+"\r\n")
	if len(added) != 2 || !strings.HasPrefix(added[0], "Return-Path: ") || !strings.HasPrefix(added[1], "Received: from foo.com ") {
		t.Errorf("added in front: %q", added)
	}
}
