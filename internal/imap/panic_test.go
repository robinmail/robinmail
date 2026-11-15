package imap

import (
	"context"
	"io"
	"testing"
	"time"
)

// panicBackend panics on the login of one name, as a bug reachable from a
// command would.
type panicBackend struct{ *memStore }

func (b panicBackend) Login(ctx context.Context, ip, name, password string) (User, error) {
	if name == "boom" {
		var m map[string]int
		m[name]++ // a nil map write: a runtime panic
	}
	return b.memStore.Login(ctx, ip, name, password)
}

// A panic in one session ends that session and nothing else: before, it
// ended the process, and with it SMTP and the web for everybody.
func TestPanicEndsOnlyTheSession(t *testing.T) {
	e := newEnv(t, func(s *Server) {
		s.MaxConnections, s.MaxConnsPerIP = 2, 2
	})
	e.srv.Backend = panicBackend{e.st}
	other := e.login() // a session that must survive
	for range 3 {
		c := e.dial()
		c.write("a LOGIN boom x\r\n")
		c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if b, err := io.ReadAll(c.br); err != nil || len(b) != 0 {
			t.Fatalf("after the panic: %q %v, want the connection closed", b, err)
		}
	}
	// The panicked sessions gave their places back (two allowed), and the
	// other one still answers.
	other.ok("NOOP")
	c := e.login()
	c.ok("NOOP")
	e.srv.mu.Lock()
	n := len(e.srv.conns)
	e.srv.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d sessions counted, want 2", n)
	}
}
