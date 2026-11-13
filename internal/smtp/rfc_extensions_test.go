package smtp

// Tests written from the RFCs of the SMTP extensions the server announces:
// STARTTLS (RFC 3207), SIZE (RFC 1870), 8BITMIME (RFC 6152), PIPELINING
// (RFC 2920), SMTPUTF8 (RFC 6531) and ENHANCEDSTATUSCODES (RFC 2034).

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"
)

// keywords returns the EHLO keywords of a reply, upper case, with their
// parameters.
func keywords(reply string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(strings.TrimSuffix(reply, "\r\n"), "\r\n")
	for _, l := range lines[1:] {
		k, v, _ := strings.Cut(l[4:], " ")
		out[strings.ToUpper(k)] = v
	}
	return out
}

func (c *client) startTLS() {
	c.t.Helper()
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		c.t.Fatal(err)
	}
	c.conn, c.r = tc, bufio.NewReader(tc)
}

// RFC 3207 2 to 4: the keyword is STARTTLS without parameters; the command
// takes none ("501 Syntax error (no parameters allowed)") and is answered
// with 220. The dialogue is the RFC's usage example (section 5).
func TestRFC3207_5_UsageExample(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, Hostname: "mail.imc.org", TLSConfig: testTLSConfig(t)}))
	before := keywords(c.cmd("EHLO mail.example.com", "250"))
	if v, ok := before["STARTTLS"]; !ok || v != "" {
		t.Errorf("EHLO keywords %v: want STARTTLS without a parameter", before)
	}
	c.cmd("STARTTLS now", "501")
	c.cmd("STARTTLS", "220")
	c.startTLS()
	// 4.2: "A server MUST NOT return the STARTTLS extension in response to
	// an EHLO command received after a TLS handshake has completed."
	after := keywords(c.cmd("EHLO mail.example.com", "250"))
	if _, ok := after["STARTTLS"]; ok {
		t.Errorf("STARTTLS offered inside TLS")
	}
	// The other extensions are still there, as in the example.
	for k := range before {
		if _, ok := after[k]; !ok && k != "STARTTLS" {
			t.Errorf("%s gone after TLS", k)
		}
	}
	// 4.2: "A client MUST NOT attempt to start a TLS session if a TLS
	// session is already active." A server that sees it refuses.
	if got := code(c.any("STARTTLS")); got[0] != '5' {
		t.Errorf("STARTTLS inside TLS: %s", got)
	}
	c.cmd("MAIL FROM:<a@example.com>", "250")
	c.cmd("RCPT TO:<b@imc.org>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.TLS == nil || env.TLS.Version < tls.VersionTLS12 {
		t.Errorf("the envelope does not record the TLS session: %+v", env.TLS)
	}
}

// RFC 3207 4.2: "Upon completion of the TLS handshake, the SMTP protocol is
// reset to the initial state (the state in SMTP after a server issues a 220
// service ready greeting). The server MUST discard any knowledge obtained
// from the client, such as the argument to the EHLO command."
func TestRFC3207_4_2_StateReset(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, TLSConfig: testTLSConfig(t)}))
	c.cmd("EHLO before-tls.example.com", "250")
	c.cmd("MAIL FROM:<plain@example.com>", "250")
	c.cmd("RCPT TO:<b@foo.com>", "250")
	c.cmd("STARTTLS", "220")
	c.startTLS()
	// No EHLO yet: as right after the greeting, MAIL is out of sequence.
	c.cmd("MAIL FROM:<a@example.com>", "503")
	// The transaction opened in plaintext is gone, also after EHLO.
	c.cmd("RCPT TO:<c@foo.com>", "503")
	c.cmd("DATA", "503")
	c.cmd("EHLO after-tls.example.com", "250")
	c.cmd("RCPT TO:<c@foo.com>", "503")
	c.cmd("MAIL FROM:<a@example.com>", "250")
	c.cmd("RCPT TO:<c@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	env, _ := h.last(t)
	if env.Helo != "after-tls.example.com" || env.From != "a@example.com" || strings.Join(env.Rcpts, " ") != "c@foo.com" {
		t.Errorf("knowledge from before TLS survived: %+v", env)
	}
}

// RFC 3207 4: "A publicly-referenced SMTP server MUST NOT require use of
// the STARTTLS extension in order to deliver mail locally."
func TestRFC3207_4_TLSNotRequired(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, TLSConfig: testTLSConfig(t)}))
	c.cmd("EHLO mail.example.com", "250")
	c.cmd("MAIL FROM:<a@example.com>", "250")
	c.cmd("RCPT TO:<b@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.TLS != nil {
		t.Error("a plaintext delivery claims TLS")
	}
}

// RFC 3207 4: "If the SMTP client is using pipelining as defined in RFC
// 2920, the STARTTLS command must be the last command in a group." What a
// client, or someone in the middle, sends behind STARTTLS in plaintext must
// never run inside the TLS session (CVE-2011-0411). The RFC names no reply
// for this; we answer 554 and close.
func TestRFC3207_4_NoPipeliningAcrossSTARTTLS(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, TLSConfig: testTLSConfig(t)}))
	c.cmd("EHLO mail.example.com", "250")
	c.send("STARTTLS\r\nMAIL FROM:<injected@example.com>\r\nRCPT TO:<b@foo.com>\r\n")
	c.expect("554")
	if _, err := c.r.ReadByte(); err == nil {
		t.Error("the connection stayed open")
	}
	if len(h.rcpts) != 0 {
		t.Errorf("the injected commands ran: %v", h.rcpts)
	}
}

// RFC 3207 4, without a certificate: the keyword is not offered and the
// command is not available.
func TestRFC3207_NotOfferedWithoutCertificate(t *testing.T) {
	c := dial(t, serveWith(t, &Server{Handler: &recorder{}}))
	if _, ok := keywords(c.cmd("EHLO mail.example.com", "250"))["STARTTLS"]; ok {
		t.Error("STARTTLS offered without a certificate")
	}
	if got := code(c.any("STARTTLS")); got != "502" && got != "500" && got != "454" {
		t.Errorf("STARTTLS without a certificate: %s", got)
	}
	c.cmd("NOOP", "250")
}

// RFC 1870 3: the EHLO keyword is SIZE with "a decimal number indicating
// the fixed maximum message size in bytes that the server will accept";
// size-param ::= [1*DIGIT].
func TestRFC1870_3_Keyword(t *testing.T) {
	c := dial(t, serveWith(t, &Server{Handler: &recorder{}, MaxMessageBytes: 1000000}))
	if got := keywords(c.cmd("EHLO ymir.claremont.edu", "250"))["SIZE"]; got != "1000000" {
		t.Errorf("SIZE parameter %q, want 1000000", got)
	}
}

// RFC 1870 6 and 6.1: MAIL FROM takes SIZE=size-value, size-value ::=
// 1*20DIGIT. A declared size over the fixed maximum gets 552; "only one
// SIZE parameter may be specified in a single MAIL command". The first
// command is the one from the RFC's example (section 8), against the
// example's limit.
func TestRFC1870_6_ExtendedMAIL(t *testing.T) {
	c := dial(t, serveWith(t, &Server{Handler: &recorder{}, MaxMessageBytes: 1000000}))
	c.cmd("EHLO ymir.claremont.edu", "250")
	for _, tc := range []struct{ params, code string }{
		{"SIZE=500000", "250"},
		{"SIZE=1000000", "250"}, // the maximum itself
		{"SIZE=0", "250"},
		{"SIZE=00000000000000500000", "250"}, // 20 digits
		{"size=500000", "250"},               // keywords are case-insensitive
		{"SIZE=1000001", "552"},
		{"SIZE=99999999999999999999", "552"},  // 20 digits, beyond 64 bits
		{"SIZE=000000000000000500000", "501"}, // 21 digits
		{"SIZE=", "501"},
		{"SIZE", "501"},
		{"SIZE=-1", "501"},
		{"SIZE=+1", "501"},
		{"SIZE=1e3", "501"},
		{"SIZE=500000 SIZE=500000", "501"},
	} {
		if got := code(c.any("MAIL FROM:<ned@thor.innosoft.com> " + tc.params)); got != tc.code {
			t.Errorf("MAIL with %s: %s, want %s", tc.params, got, tc.code)
		}
		// 6.2: after a 552 the client sends RSET or QUIT; no transaction
		// was opened.
		if tc.code != "250" {
			c.cmd("RCPT TO:<ned@innosoft.com>", "503")
		}
		c.cmd("RSET", "250")
	}
}

// RFC 1870 6.3 and 4: the declared size is an estimate. A message that
// turns out larger than the maximum is rejected with 552 after the data, a
// message within it is accepted whatever was declared, and the size counts
// "the message header, body, and the CR-LF sequences between lines, but not
// the SMTP DATA command's terminating dot or doubled quoting dots". "Servers
// MUST NOT use the SIZE parameter to determine end of content."
func TestRFC1870_6_3_ActualSize(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, MaxMessageBytes: 1000}))
	c.cmd("EHLO ymir.claremont.edu", "250")
	send := func(declared int, msg, want string) {
		t.Helper()
		c.cmd(fmt.Sprintf("MAIL FROM:<ned@thor.innosoft.com> SIZE=%d", declared), "250")
		c.cmd("RCPT TO:<ned@innosoft.com>", "250")
		c.cmd("DATA", "354")
		c.send(stuff(msg))
		c.expect(want)
		c.cmd("NOOP", "250") // still in sync
	}
	// Exactly the maximum, every line starting with a period: the doubled
	// periods and the terminator do not count.
	exact := strings.Repeat("."+strings.Repeat("x", 97)+"\r\n", 10)
	send(1000, exact, "250")
	if _, got := h.last(t); string(got) != exact {
		t.Errorf("delivered %d octets, want 1000", len(got))
	}
	// One octet more: too big, though declared small.
	send(10, exact[:98]+"y"+exact[98:], "552")
	// Declared smaller than it is, but within the maximum: accepted, and
	// read to its real end, not to the declared size.
	send(5, "0123456789\r\n", "250")
	if _, got := h.last(t); string(got) != "0123456789\r\n" {
		t.Errorf("delivered %q", got)
	}
	if h.count() != 2 {
		t.Errorf("%d messages delivered, want 2", h.count())
	}
}

// RFC 6152 2 and 3: the EHLO keyword is 8BITMIME without parameters; MAIL
// takes BODY=7BIT or BODY=8BITMIME, once. The dialogue is the RFC's usage
// example (section 4), and "the server SMTP must deliver or relay the
// content in such a way as to preserve all bits in each octet".
func TestRFC6152_4_UsageExample(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, Hostname: "dbc.mtview.ca.us"}))
	if v, ok := keywords(c.cmd("EHLO ymir.claremont.edu", "250"))["8BITMIME"]; !ok || v != "" {
		t.Errorf("8BITMIME not offered, or with a parameter %q", v)
	}
	c.cmd("MAIL FROM:<ned@ymir.claremont.edu> BODY=8BITMIME", "250")
	c.cmd("RCPT TO:<mrose@dbc.mtview.ca.us>", "250")
	c.cmd("DATA", "354")
	// Every octet value except CR and LF, which only occur as CRLF.
	var all []byte
	for i := 0; i < 256; i++ {
		if i != '\r' && i != '\n' {
			all = append(all, byte(i))
		}
	}
	msg := "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		string(all[:100]) + "\r\n" + string(all[100:]) + "\r\n"
	c.send(stuff(msg))
	c.expect("250")
	if _, got := h.last(t); string(got) != msg {
		t.Errorf("octets changed in transit:\n got %q\nwant %q", got, msg)
	}

	for params, want := range map[string]string{
		"BODY=7BIT":               "250",
		"body=8bitmime":           "250", // keyword and value are case-insensitive
		"BODY=8BITMIME SIZE=100":  "250",
		"BODY=BINARYMIME":         "501", // RFC 3030, not offered
		"BODY=8BIT":               "501",
		"BODY=":                   "501",
		"BODY":                    "501",
		"BODY=7BIT BODY=8BITMIME": "501", // "Only one BODY parameter may be used"
	} {
		if got := code(c.any("MAIL FROM:<ned@ymir.claremont.edu> " + params)); got != want {
			t.Errorf("MAIL with %s: %s, want %s", params, got, want)
		}
		c.cmd("RSET", "250")
	}
}

// RFC 6152 3: "the usual SMTP data-stuffing algorithm applies", and line
// lengths stay limited, so 8BITMIME is not a way to send binary data. A
// lone CR or LF in 8-bit content is still refused (see the deviation test
// for RFC 5321 2.3.8).
func TestRFC6152_3_NotBinary(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h}))
	c.cmd("EHLO ymir.claremont.edu", "250")
	c.cmd("MAIL FROM:<ned@ymir.claremont.edu> BODY=8BITMIME", "250")
	c.cmd("RCPT TO:<mrose@dbc.mtview.ca.us>", "250")
	c.cmd("DATA", "354")
	c.send("\xff\xfe\n\xfd\r\n.\r\n")
	c.expect("550")
	if h.count() != 0 {
		t.Error("content with a bare LF was delivered")
	}
}

// RFC 2920 4, first example: a whole transaction in two groups. The server
// answers every command, in order (3.2 (1)).
func TestRFC2920_4_Example(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h, Hostname: "innosoft.com"}))
	if v, ok := keywords(c.cmd("EHLO dbc.mtview.ca.us", "250"))["PIPELINING"]; !ok || v != "" {
		t.Errorf("PIPELINING not offered, or with a parameter %q", v)
	}
	c.send("MAIL FROM:<mrose@dbc.mtview.ca.us>\r\n" +
		"RCPT TO:<ned@innosoft.com>\r\n" +
		"RCPT TO:<dan@innosoft.com>\r\n" +
		"RCPT TO:<kvc@innosoft.com>\r\n" +
		"DATA\r\n")
	for _, want := range []string{"250", "250", "250", "250", "354"} {
		c.expect(want)
	}
	c.send("message\r\n.\r\nQUIT\r\n")
	c.expect("250")
	c.expect("221")
	env, msg := h.last(t)
	if strings.Join(env.Rcpts, " ") != "ned@innosoft.com dan@innosoft.com kvc@innosoft.com" || string(msg) != "message\r\n" {
		t.Errorf("delivered %q to %v", msg, env.Rcpts)
	}
}

// RFC 2920 4, second example: all recipients are rejected. 3.2 (3): the
// server "SHOULD issue a positive response to the DATA command if and only
// if one or more valid RCPT TO addresses have been previously received", so
// DATA fails and the client sends no message.
func TestRFC2920_4_ExampleAllRejected(t *testing.T) {
	h := &recorder{rcpt: func(to string) error {
		return &Error{550, "5.7.1", "remote mail to <" + to + "> not allowed"}
	}}
	c := dial(t, serveWith(t, &Server{Handler: h, Hostname: "innosoft.com"}))
	c.cmd("EHLO dbc.mtview.ca.us", "250")
	c.send("MAIL FROM:<mrose@dbc.mtview.ca.us>\r\n" +
		"RCPT TO:<nsb@thumper.bellcore.com>\r\n" +
		"RCPT TO:<galvin@tis.com>\r\n" +
		"DATA\r\n")
	for _, want := range []string{"250", "550", "550", "554"} {
		c.expect(want)
	}
	c.cmd("QUIT", "221")
	if h.count() != 0 {
		t.Error("a message without recipients was delivered")
	}
}

// RFC 2920 3.1: "The actual transfer of message content is explicitly
// allowed to be the first 'command' in a group. That is, a RSET/MAIL FROM
// sequence used to initiate a new message transaction can be placed in the
// same group as the final transfer of the headers and body of the previous
// message." 3.2 (9): the server "MUST NOT flush or otherwise lose the
// contents of the TCP input buffer under any circumstances whatsoever",
// also not after a command that failed.
func TestRFC2920_3_GroupsAndInputBuffer(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h}))
	c.cmd("EHLO dbc.mtview.ca.us", "250")
	c.send("MAIL FROM:<mrose@dbc.mtview.ca.us>\r\nRCPT TO:<ned@innosoft.com>\r\nDATA\r\n")
	c.expect("250")
	c.expect("250")
	c.expect("354")
	c.send("first\r\n.\r\n" +
		"RSET\r\n" +
		"MAIL FROM:<mrose@dbc.mtview.ca.us>\r\n" +
		"RCPT TO:not-a-path\r\n" + // fails; what follows is still read
		"FROB\r\n" +
		"RCPT TO:<dan@innosoft.com>\r\n" +
		"DATA\r\n")
	for _, want := range []string{"250", "250", "250", "501", "500", "250", "354"} {
		c.expect(want)
	}
	c.send("second\r\n.\r\nNOOP\r\n")
	c.expect("250")
	c.expect("250")
	if h.count() != 2 {
		t.Fatalf("%d messages delivered, want 2", h.count())
	}
	if env, msg := h.last(t); string(msg) != "second\r\n" || env.Rcpts[0] != "dan@innosoft.com" {
		t.Errorf("second message %q to %v", msg, env.Rcpts)
	}
}

// RFC 2920 3.2 (5) to (7): the server "MUST NOT buffer responses to EHLO,
// DATA, VRFY, EXPN, TURN, QUIT, and NOOP", nor to unrecognized commands, and
// "MUST send all pending responses immediately whenever the local TCP input
// buffer is emptied". Here the client sends a command plus the first half
// of the next one and then waits: the reply to the first has to come.
func TestRFC2920_3_2_ResponsesNotHeldBack(t *testing.T) {
	c := dial(t, serveWith(t, &Server{Handler: &recorder{}}))
	c.cmd("EHLO dbc.mtview.ca.us", "250")
	c.conn.SetDeadline(time.Now().Add(2 * time.Second))
	for _, tc := range []struct{ first, code string }{
		{"NOOP", "250"},
		{"VRFY mrose", "252"},
		{"EXPN list", "502"},
		{"FROB", "500"},
		{"EHLO dbc.mtview.ca.us", "250"},
		// Replies to MAIL and RCPT may be buffered for a group, but not
		// once there is no complete command left to read.
		{"MAIL FROM:<mrose@dbc.mtview.ca.us>", "250"},
		{"RCPT TO:<ned@innosoft.com>", "250"},
		{"RSET", "250"},
	} {
		c.send(tc.first + "\r\nNO")
		c.expect(tc.code) // fails on the deadline if the reply is held back
		c.send("OP\r\n")
		c.expect("250")
	}
}

// RFC 6531 3.1 and 3.6: the EHLO keyword is SMTPUTF8 without parameters,
// and a server that offers it also offers 8BITMIME.
func TestRFC6531_3_Keyword(t *testing.T) {
	c := dial(t, serveWith(t, &Server{Handler: &recorder{}}))
	kw := keywords(c.cmd("EHLO example.com", "250"))
	if v, ok := kw["SMTPUTF8"]; !ok || v != "" {
		t.Errorf("SMTPUTF8 not offered, or with a parameter %q", v)
	}
	if _, ok := kw["8BITMIME"]; !ok {
		t.Error("SMTPUTF8 without 8BITMIME")
	}
}

// RFC 6531 3.2 to 3.4: with the SMTPUTF8 parameter on MAIL, UTF-8 is
// accepted wherever RFC 5321 has a mailbox: in atoms, in quoted strings and
// as U-labels in domains. "The actual parsing of the <local-part> and the
// delimiters used are unchanged." The parameter takes no value.
func TestRFC6531_3_3_InternationalizedAddresses(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h}))
	c.cmd("EHLO example.com", "250")
	c.cmd("MAIL FROM:<δοκιμή@παράδειγμα.δοκιμή> SMTPUTF8", "250")
	for _, to := range []string{
		"用户@例子.广告",
		"Pelé@example.com",
		"user@bücher.example",
		"user@xn--bcher-kva.example", // an A-label needs no extension
		`"josé garcía"@example.com`,
		"аб.вг@example.com",
	} {
		c.cmd("RCPT TO:<"+to+">", "250")
	}
	// Still not mailboxes: the grammar's delimiters did not change.
	for _, to := range []string{"Pelé", "Pe lé@example.com", "Pelé.@example.com", "Pelé@-bücher.example", "Pelé@exa\xffmple.com"} {
		c.cmd("RCPT TO:<"+to+">", "501")
	}
	c.cmd("DATA", "354")
	c.send("Subject: δοκιμή\r\n\r\nx\r\n.\r\n")
	c.expect("250")
	env, _ := h.last(t)
	if !env.UTF8 || env.From != "δοκιμή@παράδειγμα.δοκιμή" || len(env.Rcpts) != 6 || env.Rcpts[0] != "用户@例子.广告" {
		t.Errorf("envelope %+v", env)
	}

	// 3.4: "If this parameter is provided, it MUST not accept a value."
	c.cmd("MAIL FROM:<user@example.com> SMTPUTF8=1", "501")
	c.cmd("MAIL FROM:<user@example.com> SMTPUTF8=", "501")
	// It may be given for a message whose envelope is ASCII (3.6).
	c.cmd("MAIL FROM:<user@example.com> smtputf8 BODY=8BITMIME", "250")
	c.cmd("RCPT TO:<user@example.com>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); !env.UTF8 {
		t.Error("SMTPUTF8 not recorded for an ASCII envelope")
	}
}

// RFC 6531 3.5: without the SMTPUTF8 parameter a non-ASCII address is
// refused: "When messages are rejected because the RCPT command requires an
// ASCII address, the reply-code 553 is returned ... When messages are
// rejected because the MAIL command requires an ASCII address, the
// reply-code 550 is returned", with enhanced status code X.6.7.
func TestRFC6531_3_5_ReplyCodes(t *testing.T) {
	h := &recorder{}
	c := dial(t, serveWith(t, &Server{Handler: h}))
	c.cmd("EHLO example.com", "250")
	if reply := c.cmd("MAIL FROM:<Pelé@example.com>", "550"); !strings.HasPrefix(reply, "550 5.6.7 ") {
		t.Errorf("MAIL with a non-ASCII address and no SMTPUTF8: %q", reply)
	}
	if reply := c.cmd("MAIL FROM:<user@bücher.example>", "550"); !strings.HasPrefix(reply, "550 5.6.7 ") {
		t.Errorf("MAIL with a U-label and no SMTPUTF8: %q", reply)
	}
	c.cmd("MAIL FROM:<user@example.com>", "250")
	if reply := c.cmd("RCPT TO:<Pelé@example.com>", "553"); !strings.HasPrefix(reply, "553 5.6.7 ") {
		t.Errorf("RCPT with a non-ASCII address and no SMTPUTF8: %q", reply)
	}
	// The transaction goes on for recipients that are ASCII.
	c.cmd("RCPT TO:<user@example.com>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.UTF8 || len(env.Rcpts) != 1 {
		t.Errorf("envelope %+v", env)
	}
	if len(h.rcpts) != 1 {
		t.Errorf("the handler was asked about %v", h.rcpts)
	}
}

// statusCode is RFC 2034 4's status-code at the start of a reply's text:
// class "." subject "." detail, followed by a space.
var statusCode = regexp.MustCompile(`^[245]\.[0-9]{1,3}\.[0-9]{1,3} `)

// RFC 2034 3 and 4: the keyword is ENHANCEDSTATUSCODES without parameters.
// "These codes must appear in all 2xx, 4xx, and 5xx response lines other
// than initial greeting and any response to HELO or EHLO. Note that 3xx
// responses are NOT included in this list." The class agrees with the reply
// code, and "when responses are continued across multiple lines the same
// status code must appear at the beginning of the text in each line".
func TestRFC2034_4_StatusCodes(t *testing.T) {
	c := tourServer(t)
	steps := tour(t, c, c.expect("220"))
	checked := 0
	for _, step := range steps {
		cmd, reply := step[0], step[1]
		verb, _, _ := strings.Cut(cmd, " ")
		lines := strings.Split(strings.TrimSuffix(reply, "\r\n"), "\r\n")
		first := ""
		for _, l := range lines {
			text := ""
			if len(l) > 4 {
				text = l[4:]
			}
			sc := statusCode.FindString(text)
			switch {
			case verb == "(connect)" || (verb == "EHLO" || verb == "HELO") && l[0] == '2':
				// Exempt. (An error reply to EHLO carries a code; the
				// RFC does not forbid that.)
				continue
			case l[0] == '3':
				if sc != "" {
					t.Errorf("%s: 3xx reply with a status code: %q", cmd, l)
				}
				continue
			case sc == "":
				t.Errorf("%s: reply line without a status code: %q", cmd, l)
				continue
			case sc[0] != l[0]:
				t.Errorf("%s: status code class does not agree with the reply code: %q", cmd, l)
			}
			if first == "" {
				first = sc
			} else if sc != first {
				t.Errorf("%s: status codes differ within one reply: %q", cmd, reply)
			}
			checked++
		}
	}
	if checked < 30 {
		t.Errorf("only %d reply lines checked", checked)
	}
}

// RFC 2034 5: "a compliant server includes these codes in the responses it
// sends regardless of whether or not the client expects them": also after
// HELO, and in what it says before a session exists.
func TestRFC2034_5_WithoutEHLO(t *testing.T) {
	h := &recorder{}
	srv := &Server{Handler: h, MaxConnsPerIP: 2}
	addr := serveWith(t, srv)
	c := dial(t, addr)
	c.cmd("HELO ymir.claremont.edu", "250")
	for _, tc := range []struct{ cmd, code string }{
		{"MAIL FROM:<ned@ymir.claremont.edu>", "250"}, {"RCPT TO:<mrose@dbc.mtview.ca.us>", "250"},
		{"RSET", "250"}, {"NOOP", "250"}, {"FROB", "500"}, {"QUIT", "221"},
	} {
		if reply := c.cmd(tc.cmd, tc.code); !statusCode.MatchString(reply[4:]) {
			t.Errorf("%s after HELO: no status code in %q", tc.cmd, reply)
		}
	}
	// Refused for the per-address limit: a 421 with a 4.X.X code.
	dial(t, addr)
	dial(t, addr)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if !strings.HasPrefix(line, "421 4.") || !statusCode.MatchString(line[4:]) {
		t.Errorf("refused connection got %q", line)
	}
}

// RFC 2034 6, the usage example: the status codes the RFC's server gives
// for an accepted sender (2.1.0), an accepted recipient (2.1.5), an unknown
// mailbox (5.1.1) and a refused relay (5.7.1).
//
// Deviation: the example answers the relay attempt with 551. That code
// means "user not local; please try <forward-path>" (RFC 5321 3.4), and we
// have no path to offer, so the handler says 550 with the same status code.
func TestRFC2034_6_UsageExample(t *testing.T) {
	h := &recorder{rcpt: func(to string) error {
		switch to {
		case "nosuchuser@dbc.mtview.ca.us":
			return &Error{550, "5.1.1", `Mailbox "nosuchuser" does not exist`}
		case "remoteuser@isi.edu":
			return &Error{550, "5.7.1", "Forwarding to remote hosts disabled"}
		}
		return nil
	}}
	c := dial(t, serveWith(t, &Server{Handler: h, Hostname: "dbc.mtview.ca.us"}))
	if v, ok := keywords(c.cmd("EHLO ymir.claremont.edu", "250"))["ENHANCEDSTATUSCODES"]; !ok || v != "" {
		t.Errorf("ENHANCEDSTATUSCODES not offered, or with a parameter %q", v)
	}
	for _, tc := range []struct{ cmd, want string }{
		{"MAIL FROM:<ned@ymir.claremont.edu>", "250 2.1.0 "},
		{"RCPT TO:<mrose@dbc.mtview.ca.us>", "250 2.1.5 "},
		{"RCPT TO:<nosuchuser@dbc.mtview.ca.us>", "550 5.1.1 "},
		{"RCPT TO:<remoteuser@isi.edu>", "550 5.7.1 "},
		{"DATA", "354 "},
	} {
		if reply := c.any(tc.cmd); !strings.HasPrefix(reply, tc.want) {
			t.Errorf("%s: %q, want %q", tc.cmd, reply, tc.want)
		}
	}
	c.send("x\r\n.\r\n")
	if reply := c.expect("250"); !strings.HasPrefix(reply, "250 2.") {
		t.Errorf("after the data: %q", reply)
	}
	if reply := c.cmd("QUIT", "221"); !strings.HasPrefix(reply, "221 2.") {
		t.Errorf("QUIT: %q", reply)
	}
}
