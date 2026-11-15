package queue

import (
	"net"
	"strings"
	"testing"

	"github.com/robinmail/robinmail/internal/store"
)

// Where mail for a domain goes is the domain's to say, in DNS. It may not
// say "to a machine on your own network": an MX host whose address is
// loopback, private or link-local is not connected to, unless the
// operator allows it (a lab on a private network).
func TestNoDeliveryToAddressesThatAreNotPublic(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.10", "172.16.0.9", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::25", "fe80::1", "::ffff:10.0.0.1"} {
		t.Run(ip, func(t *testing.T) {
			x := newWorld(t)
			inside := startMX(t, "mx.inside.test", x.ca.issue(t, false, "mx.inside.test"))
			x.dns.mu.Lock()
			x.dns.mx["evil.test"] = []*net.MX{{Host: "mx.inside.test.", Pref: 10}}
			x.dns.ip["mx.inside.test"] = []string{ip}
			x.dns.mu.Unlock()
			x.net.route(net.JoinHostPort(ip, "25"), inside.addr)

			sub := x.send("to the inside", "anna@evil.test")
			x.run()
			if envs, _ := inside.got(); len(envs) != 0 || len(x.net.dialed()) != 0 {
				t.Fatalf("connected to %v and delivered %d messages", x.net.dialed(), len(envs))
			}
			got := x.sub(sub.ID)
			if got.Queue != store.QueueDone || states(got) != "anna=failed" || !strings.HasPrefix(got.Recipients[0].Status, "5.") {
				t.Errorf("submission %s, queue %q, status %q: want a permanent failure", states(got), got.Queue, got.Recipients[0].Status)
			}
		})
	}
}

// With a public and a private address the public one is used.
func TestPrivateAddressesAreSkipped(t *testing.T) {
	x := newWorld(t)
	b := startMX(t, "mx.b.test", x.ca.issue(t, false, "mx.b.test"))
	public := x.domainMX("b.test", b, 10)
	x.dns.mu.Lock()
	x.dns.ip["mx.b.test"] = []string{"10.0.0.1", public}
	x.dns.mu.Unlock()
	x.net.route("10.0.0.1:25", b.addr)
	sub := x.send("hello", "anna@b.test")
	x.run()
	if d := x.net.dialed(); len(d) != 1 || d[0] != public+":25" {
		t.Errorf("dialed %v", d)
	}
	if got := x.sub(sub.ID); states(got) != "anna=delivered" {
		t.Errorf("submission %s", states(got))
	}
}

// The operator of a lab says so, and a route is the operator's word too.
func TestPrivateAddressesWhenAllowed(t *testing.T) {
	x := newWorld(t)
	x.w.AllowPrivate = true
	b := startMX(t, "mx.b.test", x.ca.issue(t, false, "mx.b.test"))
	x.dns.mu.Lock()
	x.dns.mx["b.test"] = []*net.MX{{Host: "mx.b.test.", Pref: 10}}
	x.dns.ip["mx.b.test"] = []string{"10.0.0.1"}
	x.dns.mu.Unlock()
	x.net.route("10.0.0.1:25", b.addr)
	sub := x.send("hello", "anna@b.test")
	x.run()
	if got := x.sub(sub.ID); states(got) != "anna=delivered" {
		t.Errorf("allowed: submission %s", states(got))
	}

	y := newWorld(t)
	c := startMX(t, "mx.c.test", y.ca.issue(t, false, "mx.c.test"))
	y.w.Routes = map[string]string{"c.test": "10.0.0.2:2525"}
	y.net.route("10.0.0.2:2525", c.addr)
	sub = y.send("hello", "anna@c.test")
	y.run()
	if got := y.sub(sub.ID); states(got) != "anna=delivered" {
		t.Errorf("routed: submission %s", states(got))
	}
}

// The HTTP requests that go where another domain says (its MTA-STS policy,
// the https address for its TLS reports) do not connect to an address
// that is not public either, whatever name it was reached under.
func TestOutboundHTTPConnectsToPublicAddressesOnly(t *testing.T) {
	connected := make(chan struct{}, 4)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			connected <- struct{}{}
			c.Close()
		}
	}()
	// "localhost" is a name, as an attacker's own name for 127.0.0.1 would be.
	_, port, _ := net.SplitHostPort(l.Addr().String())
	for _, host := range []string{"127.0.0.1", "localhost"} {
		_, err = OutboundHTTP(false).Get("https://" + net.JoinHostPort(host, port) + "/.well-known/mta-sts.txt")
		if err == nil || !strings.Contains(err.Error(), "not a public address") {
			t.Errorf("%s: %v, want a refusal to connect", host, err)
		}
	}
	select {
	case <-connected:
		t.Error("a connection was made to 127.0.0.1")
	default:
	}
	// The lab's client does connect (and then fails on what is no TLS).
	if _, err = OutboundHTTP(true).Get("https://" + l.Addr().String() + "/"); err == nil || strings.Contains(err.Error(), "not a public address") {
		t.Errorf("allowed: %v", err)
	}
	select {
	case <-connected:
	default:
		t.Error("the client that may did not connect")
	}
}
