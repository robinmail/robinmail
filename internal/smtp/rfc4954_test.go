package smtp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"
)

// Tests of AUTH, written from RFC 4954 (the SMTP extension), RFC 4616
// (PLAIN), RFC 3207 (what must wait for TLS), RFC 6409 and RFC 8314
// (submission, and TLS from the first byte on port 465).

// passwords is an Authenticator with fixed accounts.
type passwords map[string]string

func (p passwords) Authenticate(_ context.Context, _ net.IP, username, password string) (string, error) {
	if username == "down" {
		return "", &Error{Code: 454, Enhanced: "4.7.0", Msg: "Temporary authentication failure"}
	}
	if want, ok := p[username]; ok && want == password {
		return username + "@example.com", nil
	}
	return "", ErrAuthFailed
}

// owner is a handler that refuses a sender who is not the user.
type owner struct{ recorder }

func (o *owner) Mail(_ context.Context, env *Envelope) error {
	if env.From != "" && env.From != env.AuthUser {
		return &Error{Code: 550, Enhanced: "5.7.1", Msg: "Sender address is not yours"}
	}
	return nil
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func submission(t *testing.T) (*owner, string, *tls.Config) {
	h := &owner{}
	tc := testTLSConfig(t)
	addr := serveWith(t, &Server{Handler: h, Hostname: "smtp.example.com", TLSConfig: tc,
		Auth: passwords{"test": "1234", "tim": "tanstaaftanstaaf"}})
	return h, addr, tc
}

// secure connects, says EHLO, starts TLS and says EHLO again.
func secure(t *testing.T, addr string) *client {
	c := dial(t, addr)
	c.cmd("EHLO client.example.com", "250")
	c.cmd("STARTTLS", "220")
	c.startTLS()
	c.cmd("EHLO client.example.com", "250")
	return c
}

// RFC 4954 4.1, the example: AUTH is used after STARTTLS, with an initial
// response, and answered with "235 2.7.0 Authentication successful".
func TestRFC4954_4_1_Example(t *testing.T) {
	h, addr, _ := submission(t)
	c := dial(t, addr)
	// RFC 4954 4, "PLAIN [...] MUST NOT be advertised or used unless a
	// security layer is active" in spirit of RFC 4616 1 and RFC 8314 3:
	// before TLS there is no AUTH keyword.
	before := keywords(c.cmd("EHLO client.example.com", "250"))
	if _, ok := before["AUTH"]; ok {
		t.Errorf("AUTH is offered on an unencrypted connection: %v", before)
	}
	if _, ok := before["STARTTLS"]; !ok {
		t.Errorf("no STARTTLS: %v", before)
	}
	c.cmd("STARTTLS", "220")
	c.startTLS()
	after := keywords(c.cmd("EHLO client.example.com", "250"))
	if got := strings.Fields(after["AUTH"]); len(got) != 2 || got[0] != "PLAIN" || got[1] != "LOGIN" {
		t.Errorf("AUTH keyword %q, want the mechanisms PLAIN and LOGIN", after["AUTH"])
	}
	// The example's own line: "test" NUL "test" NUL "1234".
	if reply := c.cmd("AUTH PLAIN dGVzdAB0ZXN0ADEyMzQ=", "235"); reply != "235 2.7.0 Authentication successful\r\n" {
		t.Errorf("reply %q", reply)
	}
	c.cmd("MAIL FROM:<test@example.com>", "250")
	c.cmd("RCPT TO:<someone@elsewhere.example>", "250")
	c.cmd("DATA", "354")
	c.send("Subject: hi\r\n\r\nhello\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.AuthUser != "test@example.com" || env.TLS == nil {
		t.Errorf("envelope %+v", env)
	}
	// 4: "After an AUTH command has been successfully completed, no more
	// AUTH commands may be issued in the same session [...] with a 503".
	c.cmd("AUTH PLAIN dGVzdAB0ZXN0ADEyMzQ=", "503")
}

// RFC 4616 2 and 4: the PLAIN message is [authzid] NUL authcid NUL passwd.
// RFC 4954 4: without an initial response the server sends an empty
// challenge, "334 " and nothing else.
func TestRFC4616Plain(t *testing.T) {
	_, addr, _ := submission(t)
	// The example of RFC 4616 4: no authorization identity.
	c := secure(t, addr)
	c.cmd("AUTH PLAIN "+b64("\x00tim\x00tanstaaftanstaaf"), "235")

	c = secure(t, addr)
	if reply := c.cmd("AUTH PLAIN", "334"); reply != "334 \r\n" {
		t.Errorf("challenge %q, want an empty one", reply)
	}
	c.cmd(b64("\x00tim\x00tanstaaftanstaaf"), "235")

	for name, msg := range map[string]string{
		"wrong password":       "\x00tim\x00wrong",
		"unknown user":         "\x00nobody\x00tanstaaftanstaaf",
		"another authzid":      "admin\x00tim\x00tanstaaftanstaaf", // 4616 2: acting as someone else is refused
		"a control in the pwd": "\x00tim\x00tans\x01",
	} {
		c := secure(t, addr)
		if reply := c.cmd("AUTH PLAIN "+b64(msg), "535"); !strings.HasPrefix(reply, "535 5.7.8 ") {
			t.Errorf("%s: %q, want 535 5.7.8 (RFC 4954 6)", name, reply)
		}
		// 4: "the client may try another authentication".
		c.cmd("AUTH PLAIN "+b64("\x00tim\x00tanstaaftanstaaf"), "235")
	}
	for name, msg := range map[string]string{
		"two parts":      "tim\x00tanstaaftanstaaf",
		"four parts":     "\x00tim\x00a\x00b",
		"no authcid":     "\x00\x00tanstaaftanstaaf",
		"no password":    "\x00tim\x00",
		"nothing at all": "",
	} {
		c := secure(t, addr)
		c.cmd("AUTH PLAIN "+b64(msg)+map[bool]string{true: "="}[msg == ""], "501")
		_ = name
	}
}

// RFC 4954 4: what the exchange refuses, and with which code.
func TestRFC4954Errors(t *testing.T) {
	_, addr, _ := submission(t)
	c := dial(t, addr)
	c.cmd("EHLO client.example.com", "250")
	// RFC 4954 6 and RFC 5248: 538 5.7.11, "Encryption required for
	// requested authentication mechanism".
	if reply := c.cmd("AUTH PLAIN dGVzdAB0ZXN0ADEyMzQ=", "538"); !strings.HasPrefix(reply, "538 5.7.11 ") {
		t.Errorf("AUTH without TLS: %q", reply)
	}
	// RFC 3207 4: "530 Must issue a STARTTLS command first".
	if reply := c.cmd("MAIL FROM:<test@example.com>", "530"); !strings.Contains(reply, "STARTTLS") {
		t.Errorf("MAIL without TLS: %q", reply)
	}

	c = secure(t, addr)
	// RFC 4954 6: "530 5.7.0 Authentication required".
	if reply := c.cmd("MAIL FROM:<test@example.com>", "530"); reply != "530 5.7.0 Authentication required\r\n" {
		t.Errorf("MAIL without AUTH: %q", reply)
	}
	// 4: "If the requested authentication mechanism is invalid [...] the
	// server rejects the AUTH command with a 504 reply", 5.5.4.
	if reply := c.cmd("AUTH CRAM-MD5", "504"); !strings.HasPrefix(reply, "504 5.5.4 ") {
		t.Errorf("unknown mechanism: %q", reply)
	}
	// 4: "If the server cannot [BASE64] decode any client response, it
	// MUST reject the AUTH command with a 501 reply", 5.5.2.
	if reply := c.cmd("AUTH PLAIN not*base64", "501"); !strings.HasPrefix(reply, "501 5.5.2 ") {
		t.Errorf("bad base64: %q", reply)
	}
	// 4: "If the client wishes to cancel the authentication exchange, it
	// issues a line with a single '*'. [...] the server MUST reject the
	// AUTH command by sending a 501 reply."
	c.cmd("AUTH PLAIN", "334")
	c.cmd("*", "501")
	c.cmd("AUTH LOGIN", "334")
	c.cmd(b64("test"), "334")
	c.cmd("*", "501")
	// 6: 454 4.7.0 for a failure that is not the client's.
	if reply := c.cmd("AUTH PLAIN "+b64("\x00down\x00x"), "454"); !strings.HasPrefix(reply, "454 4.7.0 ") {
		t.Errorf("temporary failure: %q", reply)
	}
	// The session survives all of that.
	c.cmd("AUTH PLAIN dGVzdAB0ZXN0ADEyMzQ=", "235")
	// 4: "An AUTH command issued during a mail transaction MUST be
	// rejected with a 503 reply."
	c.cmd("MAIL FROM:<test@example.com>", "250")
	c.cmd("AUTH PLAIN dGVzdAB0ZXN0ADEyMzQ=", "503")
	c.cmd("RSET", "250")
	// 4.1.1.1 of RFC 5321 with RFC 3207 4.2: EHLO again does not log out,
	// and AUTH is not possible twice.
	c.cmd("EHLO client.example.com", "250")
	c.cmd("MAIL FROM:<test@example.com>", "250")

	// Guessing ends the session: failures count as errors.
	h := &owner{}
	addr2 := serveWith(t, &Server{Handler: h, TLSConfig: testTLSConfig(t), Auth: passwords{}, MaxErrors: 3})
	c = secure(t, addr2)
	c.cmd("AUTH PLAIN "+b64("\x00a\x00b"), "535")
	c.cmd("AUTH PLAIN "+b64("\x00a\x00c"), "535")
	c.cmd("AUTH PLAIN "+b64("\x00a\x00d"), "421")
}

// AUTH LOGIN is not in an RFC (draft-murchison-sasl-login): the server
// asks "Username:" and "Password:" in base64. Old clients use nothing
// else.
func TestAuthLogin(t *testing.T) {
	h, addr, _ := submission(t)
	c := secure(t, addr)
	if reply := c.cmd("AUTH LOGIN", "334"); reply != "334 VXNlcm5hbWU6\r\n" {
		t.Errorf("first prompt %q", reply)
	}
	if reply := c.cmd(b64("test"), "334"); reply != "334 UGFzc3dvcmQ6\r\n" {
		t.Errorf("second prompt %q", reply)
	}
	c.cmd(b64("1234"), "235")

	// With the user name as initial response.
	c = secure(t, addr)
	c.cmd("AUTH LOGIN "+b64("test"), "334")
	c.cmd(b64("wrong"), "535")
	c.cmd("AUTH login "+b64("test"), "334") // 4: mechanism names are case-insensitive
	c.cmd(b64("1234"), "235")

	// RFC 6409 6.1: the server "MAY" check that the sender is the user;
	// ours asks the handler (MailChecker).
	if reply := c.cmd("MAIL FROM:<someone-else@example.com>", "550"); !strings.HasPrefix(reply, "550 5.7.1 ") {
		t.Errorf("another sender: %q", reply)
	}
	// RFC 4954 5: the AUTH parameter of MAIL FROM is accepted from an
	// authenticated client.
	c.cmd("MAIL FROM:<test@example.com> AUTH=<>", "250")
	c.cmd("RCPT TO:<a@b.example>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.AuthUser != "test@example.com" {
		t.Errorf("AuthUser %q", env.AuthUser)
	}
}

// A server without Auth, port 25: no AUTH keyword, the command is 502, and
// the AUTH parameter of MAIL is unknown (RFC 5321 4.1.1.11: 555).
func TestNoAuthOnPort25(t *testing.T) {
	c := dial(t, serveWith(t, &Server{Handler: &recorder{}, TLSConfig: testTLSConfig(t)}))
	c.cmd("EHLO client.example.com", "250")
	c.cmd("STARTTLS", "220")
	c.startTLS()
	if kw := keywords(c.cmd("EHLO client.example.com", "250")); kw["AUTH"] != "" {
		t.Errorf("AUTH offered: %v", kw)
	}
	c.cmd("AUTH PLAIN dGVzdAB0ZXN0ADEyMzQ=", "502")
	c.cmd("MAIL FROM:<a@b.example> AUTH=<>", "555")
	c.cmd("MAIL FROM:<a@b.example>", "250")
}

// RFC 8314 3.3: on port 465 TLS starts with the connection ("Implicit
// TLS"). The greeting comes inside TLS, AUTH is offered at once, and
// STARTTLS is not.
func TestRFC8314ImplicitTLS(t *testing.T) {
	h := &owner{}
	tc := testTLSConfig(t)
	srv := &Server{Handler: h, Hostname: "smtp.example.com", TLSConfig: tc, Auth: passwords{"test": "1234"}}
	l, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	conn, err := tls.Dial("tcp", l.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	c := &client{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.expect("220")
	kw := keywords(c.cmd("EHLO client.example.com", "250"))
	if _, ok := kw["STARTTLS"]; ok || kw["AUTH"] != "PLAIN LOGIN" {
		t.Errorf("keywords on implicit TLS: %v", kw)
	}
	c.cmd("STARTTLS", "503")
	c.cmd("AUTH PLAIN dGVzdAB0ZXN0ADEyMzQ=", "235")
	c.cmd("MAIL FROM:<test@example.com>", "250")
	c.cmd("RCPT TO:<a@b.example>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.TLS == nil || env.AuthUser != "test@example.com" {
		t.Errorf("envelope %+v", env)
	}
	c.cmd("QUIT", "221")

	// A client that speaks plain SMTP to that port gets nothing.
	plain, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	plain.SetDeadline(time.Now().Add(2 * time.Second))
	plain.Write([]byte("EHLO client.example.com\r\n"))
	if line, _ := bufio.NewReader(plain).ReadString('\n'); strings.HasPrefix(line, "220") || strings.HasPrefix(line, "250") {
		t.Errorf("an SMTP reply without TLS on the implicit TLS port: %q", line)
	}
}
