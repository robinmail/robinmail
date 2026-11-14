package message

import (
	"bytes"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// RFC 3464 appendix E.1, "simple DSN", with a message in the place where
// the RFC says "[original message goes here]".
const rfc3464Simple = "Date: Thu, 7 Jul 1994 17:16:05 -0400\r\n" +
	"From: Mail Delivery Subsystem <MAILER-DAEMON@CS.UTK.EDU>\r\n" +
	"Message-Id: <199407072116.RAA14128@CS.UTK.EDU>\r\n" +
	"Subject: Returned mail: Cannot send message for 5 days\r\n" +
	"To: <owner-info-mime@cs.utk.edu>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/report; report-type=delivery-status;\r\n" +
	"      boundary=\"RAA14128.773615765/CS.UTK.EDU\"\r\n" +
	"\r\n" +
	"--RAA14128.773615765/CS.UTK.EDU\r\n" +
	"\r\n" +
	"The original message was received at Sat, 2 Jul 1994 17:10:28 -0400\r\n" +
	"from root@localhost\r\n" +
	"\r\n" +
	"   ----- The following addresses had delivery problems -----\r\n" +
	"<louisl@larry.slip.umd.edu>  (unrecoverable error)\r\n" +
	"\r\n" +
	"--RAA14128.773615765/CS.UTK.EDU\r\n" +
	"content-type: message/delivery-status\r\n" +
	"\r\n" +
	"Reporting-MTA: dns; cs.utk.edu\r\n" +
	"\r\n" +
	"Original-Recipient: rfc822;louisl@larry.slip.umd.edu\r\n" +
	"Final-Recipient: rfc822;louisl@larry.slip.umd.edu\r\n" +
	"Action: failed\r\n" +
	"Status: 4.0.0\r\n" +
	"Diagnostic-Code: smtp; 426 connection timed out\r\n" +
	"Last-Attempt-Date: Thu, 7 Jul 1994 17:15:49 -0400\r\n" +
	"\r\n" +
	"--RAA14128.773615765/CS.UTK.EDU\r\n" +
	"content-type: message/rfc822\r\n" +
	"\r\n" +
	"From: owner-info-mime@cs.utk.edu\r\n" +
	"To: louisl@larry.slip.umd.edu\r\n" +
	"Message-ID: <original.1994@cs.utk.edu>\r\n" +
	"Subject: info-mime digest\r\n" +
	"\r\n" +
	"The digest.\r\n" +
	"\r\n" +
	"--RAA14128.773615765/CS.UTK.EDU--\r\n"

// RFC 3464 2.2 and 2.3: the fields of a delivery status, read from the
// RFC's own example.
func TestRFC3464ParseExample(t *testing.T) {
	d, ok := ParseDSN(Parse([]byte(rfc3464Simple)))
	if !ok {
		t.Fatal("the example of appendix E.1 is not read as a DSN")
	}
	if d.ReportingMTA != "cs.utk.edu" || d.OriginalMessageID != "original.1994@cs.utk.edu" || len(d.Recipients) != 1 {
		t.Fatalf("%+v", d)
	}
	r := d.Recipients[0]
	want := time.Date(1994, 7, 7, 17, 15, 49, 0, time.FixedZone("", -4*3600))
	if r.Address != "louisl@larry.slip.umd.edu" || r.Action != "failed" || r.Status != "4.0.0" ||
		r.Diagnostic != "426 connection timed out" || !r.LastAttempt.Equal(want) {
		t.Errorf("recipient %+v", r)
	}

	// Appendix E.2: several recipients with different outcomes, comments
	// in the fields, and a Remote-MTA.
	multi := strings.Replace(rfc3464Simple,
		"Original-Recipient: rfc822;louisl@larry.slip.umd.edu\r\nFinal-Recipient: rfc822;louisl@larry.slip.umd.edu\r\nAction: failed\r\nStatus: 4.0.0\r\nDiagnostic-Code: smtp; 426 connection timed out\r\nLast-Attempt-Date: Thu, 7 Jul 1994 17:15:49 -0400\r\n",
		"Final-Recipient: RFC822; arathib@vnet.ibm.com\r\nAction: failed\r\nStatus: 5.0.0 (permanent failure)\r\nDiagnostic-Code: smtp;  550 'arathib@vnet.IBM.COM' is not a\r\n registered gateway user\r\nRemote-MTA: dns; vnet.ibm.com\r\n"+
			"\r\nFinal-Recipient: RFC822; johnh@hpnjld.njd.hp.com\r\nAction: delayed\r\nStatus: 4.0.0 (hpnjld.njd.jp.com: host name lookup failure)\r\n"+
			"\r\nFinal-Recipient: RFC822; wsnell@sdcc13.ucsd.edu\r\nAction: failed\r\nStatus: 5.0.0\r\nDiagnostic-Code: smtp; 550 user unknown\r\nRemote-MTA: dns; sdcc13.ucsd.edu\r\n", 1)
	d, ok = ParseDSN(Parse([]byte(multi)))
	if !ok || len(d.Recipients) != 3 {
		t.Fatalf("appendix E.2: %+v", d)
	}
	if r := d.Recipients[0]; r.Address != "arathib@vnet.ibm.com" || r.Status != "5.0.0" || r.RemoteMTA != "vnet.ibm.com" ||
		r.Diagnostic != "550 'arathib@vnet.IBM.COM' is not a registered gateway user" {
		t.Errorf("first recipient %+v", r)
	}
	if r := d.Recipients[1]; r.Action != "delayed" || r.Status != "4.0.0" {
		t.Errorf("second recipient %+v", r)
	}

	// What is not a DSN.
	for name, raw := range map[string]string{
		"plain mail":          "From: a@b.test\r\nSubject: Undelivered Mail Returned to Sender\r\n\r\nYour message bounced.\r\n",
		"another report type": strings.Replace(rfc3464Simple, "report-type=delivery-status", "report-type=disposition-notification", 1),
		"no status part":      strings.Replace(rfc3464Simple, "content-type: message/delivery-status", "content-type: text/plain", 1),
		"no recipient":        strings.Replace(rfc3464Simple, "Final-Recipient:", "X-Final-Recipient:", 1),
	} {
		if _, ok := ParseDSN(Parse([]byte(raw))); ok {
			t.Errorf("%s is read as a DSN", name)
		}
	}
}

// RFC 3464 and RFC 6522: what BuildDSN writes is a multipart/report with
// the three parts in order, the fields the RFC requires, and reads back.
func TestRFC3464Build(t *testing.T) {
	original := []byte("From: Mike <mike@a.test>\r\nTo: nobody@b.test, zoe@c.test\r\nBcc: secret@d.test\r\nSubject: Offerte\r\n" +
		"Message-ID: <sent-1@a.test>\r\n\r\nThe body, which is not returned.\r\n")
	arrival := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	now := arrival.Add(5 * 24 * time.Hour)
	name := "Mail Delivery System"
	raw := BuildDSN(DSNMessage{
		From: Address{Name: &name, Email: "MAILER-DAEMON@mail.a.test"}, To: "mike@a.test",
		MessageID: "dsn-1@mail.a.test", Date: now, Original: original,
	}, DSN{ReportingMTA: "mail.a.test", ArrivalDate: arrival, Recipients: []DSNRecipient{
		{Address: "nobody@b.test", Action: "failed", Status: "5.1.1", RemoteMTA: "mx.b.test",
			Diagnostic: "550 5.1.1 No such user (try <postmaster>)", LastAttempt: now},
		{Address: "zoe@c.test", Action: "failed", Status: "4.4.7", Diagnostic: "delivery was tried for 120h0m0s without success", LastAttempt: now},
	}})
	lines(t, raw)

	sm, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// RFC 6522 3: the type, and the report-type parameter.
	mt, params, err := mime.ParseMediaType(sm.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/report" || params["report-type"] != "delivery-status" {
		t.Errorf("Content-Type %q", sm.Header.Get("Content-Type"))
	}
	// RFC 3464 2: addressed to the sender of the message; RFC 3834 5.
	if sm.Header.Get("To") != "mike@a.test" || sm.Header.Get("Auto-Submitted") != "auto-replied" ||
		sm.Header.Get("In-Reply-To") != "<sent-1@a.test>" || sm.Header.Get("References") != "<sent-1@a.test>" {
		t.Errorf("header: %v", sm.Header)
	}
	if from, _ := mail.ParseAddress(sm.Header.Get("From")); from == nil || from.Address != "MAILER-DAEMON@mail.a.test" {
		t.Errorf("From %q", sm.Header.Get("From"))
	}

	m := Parse(raw)
	// RFC 6522 3: a human-readable part, then the machine-readable one,
	// then, optionally, the message or its header.
	if len(m.Leaves) != 3 || m.Leaves[0].Type != "text/plain" || m.Leaves[1].Type != "message/delivery-status" || m.Leaves[2].Type != "text/rfc822-headers" {
		t.Fatalf("parts: %d", len(m.Leaves))
	}
	human := string(m.Leaves[0].Content())
	for _, want := range []string{"mail.a.test", "<nobody@b.test>: failed (5.1.1)", "550 5.1.1 No such user", "Subject: Offerte"} {
		if !strings.Contains(human, want) {
			t.Errorf("the text for people lacks %q:\n%s", want, human)
		}
	}
	status := string(m.Leaves[1].Content())
	// 2.2: Reporting-MTA is required; 2.3: Final-Recipient, Action and
	// Status are, per recipient; groups are separated by an empty line.
	for _, want := range []string{
		"Reporting-MTA: dns; mail.a.test\r\nArrival-Date: Sat, 14 Nov 2026 09:00:00 +0000\r\n\r\n",
		"Final-Recipient: rfc822; nobody@b.test\r\nAction: failed\r\nStatus: 5.1.1\r\nRemote-MTA: dns; mx.b.test\r\n" +
			"Diagnostic-Code: smtp; 550 5.1.1 No such user [try <postmaster>]\r\n",
		"\r\n\r\nFinal-Recipient: rfc822; zoe@c.test\r\nAction: failed\r\nStatus: 4.4.7\r\n",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("the delivery status lacks %q:\n%s", want, status)
		}
	}
	returned := string(m.Leaves[2].Content())
	if !strings.Contains(returned, "Message-ID: <sent-1@a.test>") || strings.Contains(returned, "secret@d.test") || strings.Contains(returned, "not returned") {
		t.Errorf("returned header:\n%s", returned)
	}

	d, ok := ParseDSN(m)
	if !ok || d.ReportingMTA != "mail.a.test" || !d.ArrivalDate.Equal(arrival) || d.OriginalMessageID != "sent-1@a.test" || len(d.Recipients) != 2 {
		t.Fatalf("read back: %+v", d)
	}
	if r := d.Recipients[0]; r.Address != "nobody@b.test" || r.Action != "failed" || r.Status != "5.1.1" || r.RemoteMTA != "mx.b.test" ||
		r.Diagnostic != "550 5.1.1 No such user [try <postmaster>]" || !r.LastAttempt.Equal(now) {
		t.Errorf("recipient read back: %+v", r)
	}

	// RFC 6533 3: an address that is not ASCII has the type utf-8.
	raw = BuildDSN(DSNMessage{From: Address{Email: "MAILER-DAEMON@a.test"}, To: "mike@a.test", MessageID: "2@a.test", Date: now, Original: original},
		DSN{ReportingMTA: "a.test", Recipients: []DSNRecipient{{Address: "zoë@c.test", Action: "failed", Status: "5.1.1"}}})
	if !bytes.Contains(raw, []byte("Final-Recipient: utf-8; zo")) {
		t.Errorf("no utf-8 address type:\n%s", raw)
	}
}

// RFC 3463 2: status-code = class "." subject "." detail; class is 2, 4 or
// 5; subject and detail are one to three digits.
func TestRFC3463StatusCode(t *testing.T) {
	for _, s := range []string{"2.0.0", "4.4.7", "5.1.1", "5.1.10", "4.999.999", "5.7.26"} {
		if !ValidStatus(s) {
			t.Errorf("%s is refused", s)
		}
	}
	for _, s := range []string{"", "3.0.0", "1.2.3", "5.1", "5.1.1.1", "5.1000.1", "5..1", "5.1.x", "5.1.1 ", "x.1.1"} {
		if ValidStatus(s) {
			t.Errorf("%q is accepted", s)
		}
	}
}

func FuzzParseDSN(f *testing.F) {
	f.Add([]byte(rfc3464Simple))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if d, ok := ParseDSN(Parse(raw)); ok && len(d.Recipients) == 0 {
			t.Fatal("a DSN without recipients")
		}
	})
}
