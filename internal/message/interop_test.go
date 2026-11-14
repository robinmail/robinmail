package message

// The notifications Postfix 3.10, Exim 4.98 and OpenSMTPD 7.6 sent to a
// robinmail user in the interoperability lab (contrib/lab), as they
// arrived: testdata/dsn-*.eml. Each reports ghost@late.<peer>.test, an
// address the peer took with 250 and could not deliver to.

import (
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
