package message

// The notifications Postfix 3.10, Exim 4.98 and OpenSMTPD 7.6 sent to a
// robinmail user in the interoperability lab (contrib/lab), as they
// arrived: testdata/dsn-*.eml. Each reports ghost@late.<peer>.test, an
// address the peer took with 250 and could not deliver to.

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"strings"
	"testing"
)

// peerFile reads a captured notification. The files are kept with LF line
// ends, which is what git makes of them; on the wire they had CRLF.
func peerFile(t *testing.T, peer string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/dsn-" + peer + ".eml")
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n", "\r\n"))
}

func peerDSN(t *testing.T, peer string) (*Message, *DSN) {
	t.Helper()
	m := Parse(peerFile(t, peer))
	d, ok := ParseDSN(m)
	if !ok {
		t.Fatalf("the notification of %s is not read as a DSN (root %s)", peer, m.Root.Type)
	}
	return m, d
}

func TestPeerNotifications(t *testing.T) {
	for _, tc := range []struct {
		peer, root, mta, status, diagnostic string
	}{
		// RFC 3464 to the letter; the diagnostic has Postfix's own type.
		{"postfix", "multipart/report", "mx.postfix.test", "5.1.1", `unknown user: "ghost"`},
		// A report, but Exim gives no Diagnostic-Code for a failure of
		// its own and 5.0.0 for everything.
		{"exim", "multipart/report", "mx.exim.test", "5.0.0", ""},
		// Not a report at all by its type: see ParseDSN.
		{"opensmtpd", "multipart/mixed", "mx.opensmtpd.test", "5.3.0", ""},
	} {
		t.Run(tc.peer, func(t *testing.T) {
			m, d := peerDSN(t, tc.peer)
			if m.Root.Type != tc.root {
				t.Errorf("root %s, want %s: the test data changed?", m.Root.Type, tc.root)
			}
			if d.ReportingMTA != tc.mta || len(d.Recipients) != 1 {
				t.Fatalf("%+v", d)
			}
			r := d.Recipients[0]
			if r.Address != "ghost@late."+tc.peer+".test" || r.Action != "failed" || r.Status != tc.status || r.Diagnostic != tc.diagnostic {
				t.Errorf("recipient %+v", r)
			}
			// The Message-ID of what was sent, which is how the report
			// finds its submission.
			if !strings.HasSuffix(d.OriginalMessageID, "@mx.robin.test") {
				t.Errorf("original Message-ID %q", d.OriginalMessageID)
			}
		})
	}
}

// What the leniency for OpenSMTPD must not take for a notification.
func TestNotANotification(t *testing.T) {
	raw := peerFile(t, "opensmtpd")
	// The same message, forwarded by a person as an attachment.
	forwarded := "From: a@b.test\r\nSubject: Fwd: look at this bounce\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=fwd\r\n\r\n--fwd\r\nContent-Type: text/plain\r\n\r\nsee below\r\n" +
		"--fwd\r\nContent-Type: message/rfc822\r\n\r\n" + string(raw) + "\r\n--fwd--\r\n"
	if _, ok := ParseDSN(Parse([]byte(forwarded))); ok {
		t.Error("a forwarded notification is read as one")
	}
	// A status part that is not multipart at the top.
	single := "From: a@b.test\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; x.test\r\n\r\nFinal-Recipient: rfc822; a@b.test\r\nAction: failed\r\nStatus: 5.0.0\r\n"
	if _, ok := ParseDSN(Parse([]byte(single))); ok {
		t.Error("a bare message/delivery-status is read as a notification")
	}
	// A multipart/report of another kind with a status part in it.
	other := strings.Replace(string(raw), "multipart/mixed;", "multipart/report; report-type=disposition-notification;", 1)
	if _, ok := ParseDSN(Parse([]byte(other))); ok {
		t.Error("a report of another type is read as a delivery status notification")
	}
}

// A text that is the whole body and has no line end at its end keeps it
// that way: the message must end in CRLF, so the text goes out as
// quoted-printable with a soft line break at the end (RFC 2045 6.7 rule
// 5), and no line is longer than 76. Before, such a text gained a line
// end; Fastmail's JMAP-TestSuite reads a value back and compares.
func TestBuildKeepsTextWithoutAFinalLineEnd(t *testing.T) {
	for _, text := range []string{
		"this is a text part",
		"two\r\nlines",
		strings.Repeat("x", 75), strings.Repeat("x", 76), strings.Repeat("x", 77),
		strings.Repeat("x", 73) + "=", strings.Repeat("x", 74) + "=", strings.Repeat("x", 75) + "=",
		strings.Repeat("x", 151), strings.Repeat("x", 152), strings.Repeat("é", 40),
		"ends in a space ", "=", ".",
	} {
		raw := Build([]Header{Field("Subject", "x")}, &BuildPart{Type: "text/plain", Body: []byte(text)})
		if !bytes.HasSuffix(raw, []byte("\r\n")) {
			t.Errorf("%q: the message does not end in CRLF", text)
		}
		for _, line := range strings.Split(string(raw), "\r\n") {
			if len(line) > 76 {
				t.Errorf("%q: a line of %d: %q", text, len(line), line)
			}
		}
		m := Parse(raw)
		if got := string(m.Leaves[0].Content()); got != text {
			t.Errorf("our parser reads %q back as %q", text, got)
		}
		// And a parser that is not ours.
		msg, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		var r io.Reader = msg.Body
		switch msg.Header.Get("Content-Transfer-Encoding") {
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, r)
		default:
			t.Errorf("%q: transfer encoding %q", text, msg.Header.Get("Content-Transfer-Encoding"))
		}
		if got, _ := io.ReadAll(r); string(got) != text {
			t.Errorf("the standard library reads %q back as %q", text, got)
		}
	}
	// A text that ends in a line end needs none of this.
	raw := Build(nil, &BuildPart{Type: "text/plain", Body: []byte("plain\r\n")})
	if strings.Contains(string(raw), "quoted-printable") || !strings.HasSuffix(string(raw), "\r\n\r\nplain\r\n") {
		t.Errorf("a text with a line end:\n%s", raw)
	}
	// Nor does a part inside a multipart: the delimiter's line break is
	// not its content.
	raw = Build(nil, &BuildPart{Type: "multipart/mixed", SubParts: []*BuildPart{{Type: "text/plain", Body: []byte("inside")}}})
	if m := Parse(raw); string(m.Leaves[0].Content()) != "inside" || strings.Contains(string(raw), "quoted-printable") {
		t.Errorf("inside a multipart:\n%s", raw)
	}
}
