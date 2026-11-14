package smtp

import "testing"

// RFC 8621 7: an smtpReply starts with a reply code. In the lab the
// reasons robinmail made up itself did not ("connecting to mx.postfix.test
// (10.77.25.11) failed: i/o timeout"), nor did what it took from reports
// without a Diagnostic-Code.
func TestStatusReply(t *testing.T) {
	for _, tc := range []struct{ status, text, want string }{
		{"4.4.1", "connecting to mx.postfix.test (10.77.25.11) failed: i/o timeout", "451 4.4.1 connecting to mx.postfix.test (10.77.25.11) failed: i/o timeout"},
		{"5.1.2", "no such domain", "550 5.1.2 no such domain"},
		{"2.0.0", "delivered", "250 2.0.0 delivered"},
		{"", "something", "550 5.0.0 something"},
		{"5.0.0", "", "550 5.0.0"},
		// A reply is left alone, whatever the status says.
		{"5.1.1", "550 5.1.1 No such user", "550 5.1.1 No such user"},
		{"4.2.0", "450 Greylisted", "450 Greylisted"},
		{"5.0.0", "552 5.3.4 The message is larger than the server accepts", "552 5.3.4 The message is larger than the server accepts"},
		// Not replies: a number that is no code, a code without text.
		{"5.0.0", "999 problems", "550 5.0.0 999 problems"},
		{"5.0.0", "5500 mailboxes", "550 5.0.0 5500 mailboxes"},
	} {
		if got := StatusReply(tc.status, tc.text); got != tc.want {
			t.Errorf("StatusReply(%q, %q) = %q, want %q", tc.status, tc.text, got, tc.want)
		}
	}
}
