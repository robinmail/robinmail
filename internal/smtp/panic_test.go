package smtp

import (
	"context"
	"net"
	"strings"
	"testing"
)

// panicHandler panics on delivery, as a bug reachable from a message
// would.
type panicHandler struct{ fakeHandler }

func (h *panicHandler) Deliver(ctx context.Context, env *Envelope, msg []byte) error {
	if strings.Contains(string(msg), "boom") {
		var m map[string]int
		m["boom"]++ // a nil map write: a runtime panic
	}
	return h.fakeHandler.Deliver(ctx, env, msg)
}

// A panic in one session ends that session and nothing else: before, it
// ended the process, and with it IMAP and the web for everybody.
func TestPanicEndsOnlyTheSession(t *testing.T) {
	h := &panicHandler{}
	s := &Server{Hostname: "mx.test", Handler: h, MaxMessageBytes: 1 << 20, MaxConnsPerIP: 1, MaxConnections: 1}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Shutdown(context.Background()) })

	for i, body := range []string{"boom", "boom", "fine"} {
		r, err := sendOne(l.Addr().String(), []byte("Subject: "+body+"\r\n\r\n"+body+"\r\n.\r\n"))
		switch {
		case body == "boom" && err == nil:
			t.Fatalf("message %d: %q, want the connection closed", i, r)
		case body == "fine" && (err != nil || !strings.HasPrefix(r, "250")):
			// With one connection allowed in all and per address, this
			// also shows that the panicked sessions gave their places back.
			t.Fatalf("message %d after the panics: %q %v", i, r, err)
		}
	}
	waitFor(t, func() bool { return s.Budget.Used() == 0 })
	if len(h.msgs) != 1 {
		t.Fatalf("%d messages delivered", len(h.msgs))
	}
}
