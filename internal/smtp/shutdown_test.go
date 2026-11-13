package smtp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// blockingHandler holds every delivery until release is closed.
type blockingHandler struct {
	started chan struct{}
	release chan struct{}
}

func (h *blockingHandler) Rcpt(context.Context, *Envelope, string) error { return nil }

func (h *blockingHandler) Deliver(context.Context, *Envelope, []byte) error {
	close(h.started)
	<-h.release
	return nil
}

// Shutdown tells a waiting session 421 at once, lets a session that is
// delivering finish and answer first, and refuses new connections
// (RFC 5321 3.8: a server that shuts down sends 421 and closes).
func TestGracefulShutdown(t *testing.T) {
	h := &blockingHandler{started: make(chan struct{}), release: make(chan struct{})}
	s := &Server{Hostname: "mx.test", Handler: h}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(l) }()
	addr := l.Addr().String()

	idle := dial(t, addr)
	idle.cmd("EHLO client.test", "250")
	half := dial(t, addr) // in a transaction, but waiting for a command
	half.cmd("EHLO client.test", "250")
	half.cmd("MAIL FROM:<a@sender.test>", "250")
	busy := dial(t, addr)
	busy.cmd("EHLO client.test", "250")
	busy.cmd("MAIL FROM:<a@sender.test>", "250")
	busy.cmd("RCPT TO:<b@example.test>", "250")
	busy.cmd("DATA", "354")
	busy.send("Subject: x\r\n\r\nbody\r\n.\r\n")
	<-h.started

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- s.Shutdown(ctx)
	}()

	for name, c := range map[string]*client{"idle": idle, "mid-transaction": half} {
		if got := c.expect("421"); !strings.HasPrefix(got, "421 4.3.2 mx.test ") {
			t.Errorf("%s session got %q", name, got)
		}
		if _, err := c.r.ReadByte(); err == nil {
			t.Errorf("%s session stayed open after 421", name)
		}
	}
	if err := <-served; err != ErrServerClosed {
		t.Errorf("Serve returned %v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Error("still accepting connections")
	}
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned %v while a delivery was running", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The running delivery completes and is acknowledged; only then 421.
	close(h.release)
	busy.expect("250")
	busy.expect("421")
	if err := <-done; err != nil {
		t.Errorf("Shutdown = %v, want nil: every session ended by itself", err)
	}
}

// A session that will not end is cut off when the context is done.
func TestShutdownDeadline(t *testing.T) {
	h := &blockingHandler{started: make(chan struct{}), release: make(chan struct{})}
	s := &Server{Hostname: "mx.test", Handler: h}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	c := dial(t, l.Addr().String())
	c.cmd("EHLO client.test", "250")
	c.cmd("MAIL FROM:<a@sender.test>", "250")
	c.cmd("RCPT TO:<b@example.test>", "250")
	c.cmd("DATA", "354")
	c.send("half a message\r\n") // and then nothing

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); err != context.DeadlineExceeded {
		t.Errorf("Shutdown = %v, want deadline exceeded", err)
	}
	if _, err := c.r.ReadByte(); err == nil {
		t.Error("connection still open after the deadline")
	}
}

// RFC 5321 4.5.3.2.7: a server that gives up waiting for a command says so
// with 421 before it closes (3.8).
func TestCommandTimeout(t *testing.T) {
	s := &Server{Hostname: "mx.test", Handler: &fakeHandler{}, CommandTimeout: 100 * time.Millisecond}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	defer s.Shutdown(context.Background())
	c := dial(t, l.Addr().String())
	if got := c.expect("421"); !strings.HasPrefix(got, "421 4.4.2 mx.test ") {
		t.Errorf("after the command timeout: %q", got)
	}
}
