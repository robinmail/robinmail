package queue

import (
	"context"
	"net"
	"strings"
	"testing"
)

// panicDNS panics on the MX lookup of one domain, as a bug reachable
// from a recipient domain or a remote server would.
type panicDNS struct{ *testDNS }

func (d panicDNS) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	if strings.HasPrefix(name, "boom.") {
		var m map[string]int
		m[name]++ // a nil map write: a runtime panic
	}
	return d.testDNS.LookupMX(ctx, name)
}

// A panic in one delivery attempt ends that attempt and nothing else:
// before, it ended the process. The message stays queued.
func TestPanicEndsOnlyTheAttempt(t *testing.T) {
	x := newWorld(t)
	x.w.Resolver = panicDNS{x.dns}
	b := startMX(t, "mx.b.test", nil)
	x.domainMX("b.test", b, 10)
	boom := x.send("boom", "anna@boom.test")
	x.send("fine", "anna@b.test")
	x.run()
	if envs, _ := b.got(); len(envs) != 1 {
		t.Fatalf("%d of 1 arrived", len(envs))
	}
	if s := x.sub(boom.ID); s.Done() {
		t.Fatalf("the message whose attempt panicked is done: %+v", s)
	}
}
