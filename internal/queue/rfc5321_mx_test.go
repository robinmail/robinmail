package queue

import (
	"net"
	"strings"
	"testing"
	"time"
)

func hostsOf(t *testing.T, w *Worker, domain string) (string, *failure) {
	t.Helper()
	dest, fail := w.lookupMX(ctx, domain)
	return strings.Join(dest.hosts, " "), fail
}

// RFC 5321 5.1: how the hosts for a domain are found.
func TestRFC5321_5_1_MXLookup(t *testing.T) {
	dns := newDNS()
	w := &Worker{Hostname: "mail.example.org", Resolver: dns}

	// "the sender-SMTP MUST [...] try [...] in order of preference": the
	// lowest number first.
	dns.mx["foo.com"] = []*net.MX{{Host: "backup.foo.com.", Pref: 20}, {Host: "MX1.foo.com.", Pref: 10}, {Host: "last.foo.com.", Pref: 30}}
	if got, fail := hostsOf(t, w, "foo.com"); fail != nil || got != "mx1.foo.com backup.foo.com last.foo.com" {
		t.Errorf("order: %q %v", got, fail)
	}

	// "If there are multiple destinations with the same preference [...]
	// the sender-SMTP MUST randomize them to spread the load".
	dns.mx["even.com"] = []*net.MX{{Host: "a.even.com.", Pref: 10}, {Host: "b.even.com.", Pref: 10}, {Host: "c.even.com.", Pref: 10}, {Host: "z.even.com.", Pref: 20}}
	first := map[string]int{}
	for i := 0; i < 300; i++ {
		got, _ := hostsOf(t, w, "even.com")
		if !strings.HasSuffix(got, " z.even.com") {
			t.Fatalf("a less preferred host is not last: %q", got)
		}
		first[strings.Fields(got)[0]]++
	}
	if len(first) != 3 || first["a.even.com"] < 50 || first["b.even.com"] < 50 || first["c.even.com"] < 50 {
		t.Errorf("hosts of equal preference are not tried in random order: %v", first)
	}

	// "If an empty list of MXs is returned, the address is treated as if
	// it was associated with an implicit MX RR, with a preference of 0,
	// pointing to that host."
	dns.ip["nomx.com"] = []string{"192.0.2.9"}
	dest, fail := w.lookupMX(ctx, "nomx.com")
	if fail != nil || len(dest.hosts) != 1 || dest.hosts[0] != "nomx.com" || !dest.implicit {
		t.Errorf("implicit MX: %+v %v", dest, fail)
	}

	// "If MX records are present, but none of them are usable, or the
	// implicit MX is unusable, this situation MUST be reported as an
	// error." A lookup that fails for now is not "no MX": 4yz, try later.
	dns.broken["flaky.com"] = true
	if _, fail := hostsOf(t, w, "flaky.com"); fail == nil || fail.permanent || fail.status != "4.4.3" {
		t.Errorf("a failing lookup: %+v", fail)
	}

	// "If an MX record points to the sending host itself, [...] all MX
	// records with a preference greater than or equal to that of the
	// sending host MUST be discarded": the relay rule, and if nothing is
	// left the mail would loop.
	dns.mx["relayed.com"] = []*net.MX{{Host: "primary.relayed.com.", Pref: 5}, {Host: "mail.example.org.", Pref: 10},
		{Host: "peer.relayed.com.", Pref: 10}, {Host: "far.relayed.com.", Pref: 50}}
	if got, fail := hostsOf(t, w, "relayed.com"); fail != nil || got != "primary.relayed.com" {
		t.Errorf("we are a backup MX: %q %v", got, fail)
	}
	dns.mx["ours.com"] = []*net.MX{{Host: "mail.example.org.", Pref: 10}, {Host: "backup.ours.com.", Pref: 20}}
	if got, fail := hostsOf(t, w, "ours.com"); fail == nil || !fail.permanent || fail.status != "5.4.6" || got != "" {
		t.Errorf("we are the best MX of a domain we do not have: %q %+v", got, fail)
	}
}

// RFC 7505 3 and 4.2: a single MX with preference 0 and the target "." is
// a null MX: the domain accepts no mail, and "mail systems SHOULD NOT
// attempt to look up A or AAAA records". The status is 5.1.10.
func TestRFC7505NullMX(t *testing.T) {
	dns := newDNS()
	w := &Worker{Hostname: "mail.example.org", Resolver: dns}
	dns.mx["nomail.example.com"] = []*net.MX{{Host: ".", Pref: 0}}
	dns.ip["nomail.example.com"] = []string{"192.0.2.1"} // a web server lives here
	got, fail := hostsOf(t, w, "nomail.example.com")
	if fail == nil || !fail.permanent || fail.status != "5.1.10" || got != "" {
		t.Errorf("null MX: %q %+v", got, fail)
	}
	if dns.count("A ") != 0 {
		t.Errorf("addresses were looked up for a null MX: %v", dns.lookups)
	}
	// 3: "A domain that advertises a null MX MUST NOT advertise any other
	// MX RR." If one does anyway, the real hosts are used.
	dns.mx["confused.example.com"] = []*net.MX{{Host: ".", Pref: 0}, {Host: "mx.confused.example.com.", Pref: 10}}
	if got, fail := hostsOf(t, w, "confused.example.com"); fail != nil || got != "mx.confused.example.com" {
		t.Errorf("null MX next to a real one: %q %v", got, fail)
	}
}

// RFC 5321 5.2: a host with IPv6 and IPv4 addresses; every address is a
// candidate, in the order the resolver prefers them.
func TestRFC5321_5_2_Addresses(t *testing.T) {
	dns := newDNS()
	w := &Worker{Hostname: "mail.example.org", Resolver: dns}
	dns.ip["mx.dual.example"] = []string{"2001:db8::25", "192.0.2.25"}
	addrs, fail := w.addresses(ctx, "mx.dual.example")
	if fail != nil || len(addrs) != 2 || addrs[0].IP.To4() != nil || addrs[1].IP.To4() == nil {
		t.Errorf("addresses %v %v", addrs, fail)
	}
	if _, fail := w.addresses(ctx, "gone.example"); fail == nil || !fail.permanent {
		t.Errorf("a host without an address: %+v", fail)
	}
	dns.broken["flaky.example"] = true
	if _, fail := w.addresses(ctx, "flaky.example"); fail == nil || fail.permanent {
		t.Errorf("a failing lookup: %+v", fail)
	}
	dns.ip["many.example"] = []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4", "192.0.2.5", "192.0.2.6"}
	if addrs, _ := w.addresses(ctx, "many.example"); len(addrs) != maxAddrsPerHost {
		t.Errorf("%d addresses of one host are tried", len(addrs))
	}
	// A domain in the address is lower case ASCII for DNS: A-labels for an
	// internationalized name (RFC 5890), nothing for an address literal.
	for in, want := range map[string]string{"Example.COM": "example.com", "bücher.example": "xn--bcher-kva.example", "trailing.example.": "trailing.example"} {
		if got, ok := asciiDomain(in); !ok || got != want {
			t.Errorf("asciiDomain(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "[192.0.2.1]", "[IPv6:2001:db8::1]"} {
		if got, ok := asciiDomain(in); ok {
			t.Errorf("asciiDomain(%q) = %q", in, got)
		}
	}
}

// RFC 5321 4.5.4.1: "the retry interval SHOULD be at least 30 minutes;
// however, more sophisticated and variable strategies will be beneficial
// [...] the give-up time generally needs to be at least 4-5 days". Ours
// starts short, for the server that was down for a minute, and settles at
// four hours.
func TestBackoff(t *testing.T) {
	want := []time.Duration{5, 10, 20, 40, 80, 160, 240, 240, 240}
	total := time.Duration(0)
	for i, w := range want {
		if got := Backoff(i + 1); got != w*time.Minute {
			t.Errorf("after try %d: %v, want %v", i+1, got, w*time.Minute)
		}
	}
	tries := 0
	for total < 5*24*time.Hour {
		tries++
		total += Backoff(tries)
	}
	if tries < 20 || tries > 40 {
		t.Errorf("%d tries in five days", tries)
	}
}
