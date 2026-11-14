package smtp

// Tests from Thunderbird 153 in the interoperability lab (contrib/lab,
// internal/labtest): what it sends on a submission port, replayed from
// robinmail's trace.

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// withdrawable is an Authenticator with one account whose login can be
// withdrawn, as revoking an app password does.
type withdrawable struct {
	mu       sync.Mutex
	password string
	watchers []chan struct{}
	stopped  int
}

func (w *withdrawable) Authenticate(ctx context.Context, ip net.IP, username, password string) (string, error) {
	user, _, _, err := w.AuthenticateSession(ctx, ip, username, password)
	return user, err
}

func (w *withdrawable) AuthenticateSession(_ context.Context, _ net.IP, username, password string) (string, <-chan struct{}, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if username != "tb@robin.test" || w.password == "" || password != w.password {
		return "", nil, nil, ErrAuthFailed
	}
	ch := make(chan struct{})
	w.watchers = append(w.watchers, ch)
	return username, ch, func() { w.mu.Lock(); w.stopped++; w.mu.Unlock() }, nil
}

// withdraw makes the password invalid and tells the open sessions.
func (w *withdrawable) withdraw() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.password = ""
	for _, ch := range w.watchers {
		close(ch)
	}
	w.watchers = nil
}

// Thunderbird keeps its submission connection open after a message, with
// no QUIT, and sends the next message on it without a new AUTH: in the lab
// it sent mail with the account password after the account had moved to
// app passwords, and would have with a revoked app password. RFC 4954 is
// silent on how long an authentication holds; here it holds as long as
// the login does. A session whose login is withdrawn is told 421 and
// closed, at once when it waits for a command. The client then connects
// again and is refused at AUTH with 535, which is where it asks its user
// for a password.
func TestThunderbirdSessionEndsWhenLoginWithdrawn(t *testing.T) {
	h := &owner{}
	auth := &withdrawable{password: "app-password"}
	addr := serveWith(t, &Server{Handler: h, Hostname: "mx.robin.test", TLSConfig: testTLSConfig(t), Auth: auth})
	plain := b64("\x00tb@robin.test\x00app-password")

	// The session as Thunderbird ran it (on 465, TLS from the start; here
	// after STARTTLS).
	c := secure(t, addr)
	c.cmd("EHLO [10.77.25.7]", "250")
	c.cmd("AUTH PLAIN "+plain, "235")
	c.cmd("MAIL FROM:<tb@robin.test> BODY=8BITMIME SIZE=448", "250")
	c.cmd("RCPT TO:<other@robin.test>", "250")
	c.cmd("DATA", "354")
	c.cmd("Subject: With the account password\r\n\r\nSent.\r\n.", "250")
	if env, _ := h.last(t); env.AuthUser != "tb@robin.test" {
		t.Fatalf("delivered as %q", env.AuthUser)
	}
	// No QUIT: the connection waits for the next message. The login goes.
	auth.withdraw()
	if got := c.expect("421"); !strings.HasPrefix(got, "421 4.7.0 mx.robin.test This login was withdrawn") {
		t.Errorf("a waiting session whose login was withdrawn is told %q", got)
	}
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.r.ReadByte(); err != io.EOF {
		t.Errorf("the connection stays open after the 421: %v", err)
	}

	// The next message: a new connection, refused at AUTH.
	c = secure(t, addr)
	c.cmd("EHLO [10.77.25.7]", "250")
	c.cmd("AUTH PLAIN "+plain, "535 5.7.8")
	c.cmd("MAIL FROM:<tb@robin.test> BODY=8BITMIME SIZE=415", "530")

	// A session that is in a transaction when the login goes gets no
	// further.
	auth.mu.Lock()
	auth.password = "second"
	auth.mu.Unlock()
	c = secure(t, addr)
	c.cmd("EHLO [10.77.25.7]", "250")
	c.cmd("AUTH PLAIN "+b64("\x00tb@robin.test\x00second"), "235")
	c.cmd("MAIL FROM:<tb@robin.test> BODY=8BITMIME SIZE=415", "250")
	before := delivered(&h.recorder)
	auth.withdraw()
	c.expect("421 4.7.0")
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := c.r.ReadByte(); err != io.EOF {
		t.Errorf("after the 421 the server still answers: %q %v", b, err)
	}
	if n := delivered(&h.recorder); n != before {
		t.Errorf("%d messages were delivered after the login was withdrawn", n-before)
	}
	// Every session's watch was ended with the session.
	waitUntil(t, func() bool { auth.mu.Lock(); defer auth.mu.Unlock(); return auth.stopped == 2 })
}

// A login that stands is not disturbed: Thunderbird's second and third
// message go out on the connection of the first.
func TestThunderbirdReusesItsSubmissionConnection(t *testing.T) {
	h := &owner{}
	auth := &withdrawable{password: "app-password"}
	addr := serveWith(t, &Server{Handler: h, Hostname: "mx.robin.test", TLSConfig: testTLSConfig(t), Auth: auth})
	c := secure(t, addr)
	c.cmd("EHLO [10.77.25.7]", "250")
	c.cmd("AUTH PLAIN "+b64("\x00tb@robin.test\x00app-password"), "235")
	for i := 0; i < 3; i++ {
		c.cmd("MAIL FROM:<tb@robin.test> BODY=8BITMIME SIZE=448", "250")
		c.cmd("RCPT TO:<other@robin.test>", "250")
		c.cmd("DATA", "354")
		c.cmd("Subject: one of three\r\n\r\nSent.\r\n.", "250")
	}
	if n := delivered(&h.recorder); n != 3 {
		t.Errorf("%d messages delivered", n)
	}
	c.cmd("QUIT", "221")
}

func delivered(h *recorder) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
