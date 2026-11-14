package smtp

// Tests from the interoperability lab (contrib/lab, internal/labtest):
// what Postfix 3.10, Exim 4.98 and OpenSMTPD 7.6 really send, replayed
// here from robinmail's trace of those sessions, so the behaviour they
// depend on is kept without Docker.

import (
	"strings"
	"testing"
)

// Deviation from RFC 5321 4.1.2 ("the underscore character is not
// permitted" in a Domain): a sender whose domain has an underscore is
// accepted. In the lab Postfix and OpenSMTPD both send mail from
// <alice@under_score.postfix.test> and both accept such a sender; only
// Exim refuses it, with 501 as the RFC has it. Mail from hosts with such
// names exists, the name is only ever looked up, and refusing it loses
// wanted mail. A recipient stays strict: it must be one of our domains.
func TestRFC5321_4_1_2_Deviation_UnderscoreInSenderDomain(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO mx.postfix.test", "250")
	// As Postfix sent it.
	c.cmd("MAIL FROM:<alice@under_score.postfix.test> SIZE=372", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.cmd("Subject: x\r\n\r\nx\r\n.", "250")
	if env, _ := h.last(t); env.From != "alice@under_score.postfix.test" {
		t.Errorf("sender %q", env.From)
	}
	for _, ok := range []string{"<a@_dmarc.example.com>", "<a@mail_1.example.com>", "<a@x_.example.com>"} {
		c.cmd("MAIL FROM:"+ok, "250")
		c.cmd("RSET", "250")
	}
	// Still not a domain, underscore or not.
	for _, bad := range []string{"<a@under_score..test>", "<a@-under_score.test>", "<a@under_score.test.>", "<a@under score.test>"} {
		c.cmd("MAIL FROM:"+bad, "501")
	}
	// The recipient and the source route keep the RFC's grammar.
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@foo_bar.com>", "501")
	c.cmd("RSET", "250")
	c.cmd("MAIL FROM:<@host_a.int:Smith@bar.com>", "501")
	if ValidDomain("under_score.test") {
		t.Error("ValidDomain takes an underscore")
	}
	// What a user may send to: a sender with such a domain can be
	// answered ("invalidRecipients" in the lab, before).
	if !ValidMailbox("alice@under_score.postfix.test") || ValidMailbox("alice@under score.test") {
		t.Error("ValidMailbox: an underscore in the domain of a recipient we send to")
	}
}

// The transactions of the three peers as the lab recorded them. They
// differ in what RFC 2920, 1870, 6152 and 6531 make optional:
//
//	             pipelines  SIZE=  BODY=8BITMIME  SMTPUTF8
//	Postfix      yes        yes    yes            yes
//	Exim         yes        yes    no             only for an address
//	OpenSMTPD    no         no     no             no
//
// Exim and OpenSMTPD send 8-bit bodies and UTF-8 headers without saying
// so. RFC 6152 3 wants BODY=8BITMIME for 8-bit content, but a server
// cannot tell before DATA, and refusing afterwards would refuse most mail
// from both: the content is taken as it comes.
func TestPeerTransactions(t *testing.T) {
	const eightBit = "Subject: Gr\xc3\xbc\xc3\x9fe\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\nGr\xc3\xbc\xc3\x9fe, \xe6\x97\xa5\xe6\x9c\xac\xe8\xaa\x9e\r\n"
	for _, tc := range []struct {
		name      string
		peer      string
		pipelined bool
		mail      string
		utf8      bool
	}{
		{"postfix", "postfix", true, "MAIL FROM:<alice@postfix.test> SIZE=501 BODY=8BITMIME SMTPUTF8", true},
		{"postfix, UTF-8 sender", "postfix", true, "MAIL FROM:<j\xc3\xb6rg@postfix.test> SIZE=467 BODY=8BITMIME SMTPUTF8", true},
		{"exim", "exim", true, "MAIL FROM:<alice@exim.test> SIZE=1594", false},
		{"exim, UTF-8 sender", "exim", true, "MAIL FROM:<j\xc3\xb6rg@exim.test> SIZE=1589 SMTPUTF8", true},
		{"opensmtpd", "opensmtpd", false, "MAIL FROM:<alice@opensmtpd.test>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := fooCom(t)
			c.cmd("EHLO mx."+tc.peer+".test", "250")
			if tc.pipelined {
				// One write, as both do after EHLO said PIPELINING.
				c.send(tc.mail + "\r\nRCPT TO:<Jones@foo.com>\r\nRCPT TO:<Green@foo.com>\r\nDATA\r\n")
				c.expect("250")
				c.expect("250")
				c.expect("550")
				c.expect("354")
			} else {
				c.cmd(tc.mail, "250")
				c.cmd("RCPT TO:<Jones@foo.com>", "250")
				c.cmd("RCPT TO:<Green@foo.com>", "550")
				c.cmd("DATA", "354")
			}
			c.cmd(eightBit+".", "250")
			c.cmd("QUIT", "221")
			env, msg := h.last(t)
			if string(msg) != eightBit {
				t.Errorf("message %q", msg)
			}
			if env.UTF8 != tc.utf8 || len(env.Rcpts) != 1 {
				t.Errorf("envelope %+v", env)
			}
		})
	}
}

// Exim offers a message although its declared size is over the limit of
// our EHLO, and takes the 552 at MAIL (RFC 1870 6.1); Postfix does not
// offer it at all. OpenSMTPD declares nothing and sends the whole
// message: the 552 comes after the data, which is read to its end so the
// session stays usable (RFC 1870 6.2).
func TestPeerOverTheSizeLimit(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, MaxMessageBytes: 2000}))
	if !strings.Contains(c.cmd("EHLO mx.exim.test", "250"), "SIZE 2000") {
		t.Fatal("no SIZE 2000 in EHLO")
	}
	// Exim.
	c.send("MAIL FROM:<alice@exim.test> SIZE=2269397\r\nRCPT TO:<Jones@foo.com>\r\nDATA\r\n")
	if rep := c.expect("552"); !strings.Contains(rep, "5.3.4") {
		t.Errorf("MAIL: %q", rep)
	}
	c.expect("503")
	c.expect("503")
	// OpenSMTPD, on the same connection.
	c.cmd("RSET", "250")
	c.cmd("MAIL FROM:<alice@opensmtpd.test>", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send("Subject: big\r\n\r\n" + strings.Repeat(strings.Repeat("x", 78)+"\r\n", 100) + ".\r\n")
	if rep := c.expect("552"); !strings.Contains(rep, "5.3.4") {
		t.Errorf("after the data: %q", rep)
	}
	// And still in step.
	c.cmd("MAIL FROM:<alice@opensmtpd.test>", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.cmd("Subject: small\r\n\r\nx\r\n.", "250")
	if h.count() != 1 {
		t.Errorf("%d messages delivered, want 1", h.count())
	}
}
