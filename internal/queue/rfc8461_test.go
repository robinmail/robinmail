package queue

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/store"
)

// RFC 8461 3.1: the TXT record. The example is "v=STSv1; id=20160831085700Z;".
func TestRFC8461_3_1_Record(t *testing.T) {
	for _, tc := range []struct {
		records []string
		id      string
		ok      bool
	}{
		{[]string{"v=STSv1; id=20160831085700Z;"}, "20160831085700Z", true},
		{[]string{"v=STSv1;id=1"}, "1", true},
		{[]string{"v=STSv1; id=abc; ext=1"}, "abc", true}, // unknown fields are ignored
		{[]string{"some other TXT", "v=STSv1; id=7"}, "7", true},
		// "If the number of resulting records is not one, or if the
		// resulting record is syntactically invalid, senders MUST assume
		// the recipient domain does not have an available MTA-STS Policy".
		{[]string{"v=STSv1; id=1", "v=STSv1; id=2"}, "", false},
		{[]string{"v=STSv2; id=1"}, "", false},
		{[]string{"id=1; v=STSv1"}, "", false}, // the version comes first
		{[]string{"v=STSv1"}, "", false},
		{[]string{"v=STSv1; id="}, "", false},
		{[]string{"v=STSv1; id=has-dash"}, "", false}, // id is 1*32(ALPHA / DIGIT)
		{[]string{"v=STSv1; id=" + strings.Repeat("a", 33)}, "", false},
		{nil, "", false},
	} {
		if id, ok := stsRecordID(tc.records); id != tc.id || ok != tc.ok {
			t.Errorf("%q: %q %v, want %q %v", tc.records, id, ok, tc.id, tc.ok)
		}
	}
}

// RFC 8461 3.2: the policy file. The example of the section.
func TestRFC8461_3_2_Policy(t *testing.T) {
	example := "version: STSv1\r\nmode: enforce\r\nmx: mail.example.com\r\nmx: *.example.net\r\nmx: backupmx.example.com\r\nmax_age: 604800\r\n"
	mode, mx, age, err := parseSTSPolicy([]byte(example))
	if err != nil || mode != "enforce" || strings.Join(mx, " ") != "mail.example.com *.example.net backupmx.example.com" || age != 604800*time.Second {
		t.Errorf("the example: %q %v %v %v", mode, mx, age, err)
	}
	// LF alone ends a line too, and fields we do not know are ignored.
	if mode, mx, _, err := parseSTSPolicy([]byte("version: STSv1\nmode: testing\nfuture: yes\nmx: a.example\nmax_age: 60\n")); err != nil || mode != "testing" || len(mx) != 1 {
		t.Errorf("LF and an unknown field: %q %v %v", mode, mx, err)
	}
	// "max_age: [...] maximum value of 31557600".
	if _, _, age, _ := parseSTSPolicy([]byte("version: STSv1\nmode: none\nmax_age: 99999999999\n")); age != 31557600*time.Second {
		t.Errorf("max_age is not capped: %v", age)
	}
	// Mode none needs no mx.
	if mode, mx, _, err := parseSTSPolicy([]byte("version: STSv1\nmode: none\nmax_age: 86400\n")); err != nil || mode != "none" || len(mx) != 0 {
		t.Errorf("mode none: %q %v %v", mode, mx, err)
	}
	for name, body := range map[string]string{
		"no version":    "mode: enforce\nmx: a.example\nmax_age: 60\n",
		"wrong version": "version: STSv2\nmode: enforce\nmx: a.example\nmax_age: 60\n",
		"unknown mode":  "version: STSv1\nmode: strict\nmx: a.example\nmax_age: 60\n",
		"no max_age":    "version: STSv1\nmode: enforce\nmx: a.example\n",
		"bad max_age":   "version: STSv1\nmode: enforce\nmx: a.example\nmax_age: soon\n",
		"no mx":         "version: STSv1\nmode: enforce\nmax_age: 60\n",
		"not a policy":  "<html><body>404</body></html>\n",
		"empty":         "",
	} {
		if _, _, _, err := parseSTSPolicy([]byte(body)); err == nil {
			t.Errorf("%s is accepted", name)
		}
	}
}

// RFC 8461 4.1: matching an MX host against the patterns. A wildcard
// stands for exactly one label, at the left.
func TestRFC8461_4_1_MXMatching(t *testing.T) {
	patterns := []string{"mail.example.com", "*.example.net", "backupmx.example.com"}
	for host, want := range map[string]bool{
		"mail.example.com":      true,
		"MAIL.Example.COM.":     true,
		"mx1.example.net":       true,
		"example.net":           false, // "*.example.com" does not match "example.com"
		"a.b.example.net":       false, // one label only
		"evil.example.com":      false,
		"mail.example.com.evil": false,
		"xexample.net":          false,
	} {
		if got := stsMatch(patterns, host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}

// stsStore is the part of Store the policy code uses, in memory.
type stsStore struct {
	Store
	mu     sync.Mutex
	saved  map[string]store.STSPolicy
	writes int
}

func (s *stsStore) STSPolicy(_ context.Context, domain string) (store.STSPolicy, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.saved[domain]
	return p, ok, nil
}

func (s *stsStore) SaveSTSPolicy(_ context.Context, p store.STSPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved[p.Domain] = p
	s.writes++
	return nil
}

// policyHost serves a policy file for a domain over HTTPS, with a
// certificate of the test CA for mta-sts.<domain>, and returns a client
// that reaches it under that name.
type policyHost struct {
	mu     sync.Mutex
	body   string
	status int
	ctype  string
	hits   int
	paths  []string
	down   bool
}

func (p *policyHost) set(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f()
}

func servePolicy(t *testing.T, ca *testCA, certNames ...string) (*policyHost, *http.Client) {
	p := &policyHost{status: 200, ctype: "text/plain"}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.hits++
		p.paths = append(p.paths, r.Host+r.URL.Path)
		if p.status/100 == 3 {
			http.Redirect(w, r, "https://elsewhere.example/.well-known/mta-sts.txt", p.status)
			return
		}
		w.Header().Set("Content-Type", p.ctype)
		w.WriteHeader(p.status)
		w.Write([]byte(p.body))
	}))
	srv.TLS = ca.issue(t, false, certNames...)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	hc := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: ca.pool},
			DisableKeepAlives: true, // every fetch dials, so "down" is seen
			// Every name leads to the test server; nothing leaves.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				p.mu.Lock()
				down := p.down
				p.mu.Unlock()
				if down {
					return nil, &net.OpError{Op: "dial", Err: net.UnknownNetworkError("down")}
				}
				var d net.Dialer
				return d.DialContext(ctx, network, srv.Listener.Addr().String())
			},
		},
		CheckRedirect: defaultSTSClient.CheckRedirect,
	}
	return p, hc
}

// RFC 8461 3.3, 5 and 5.1: finding, fetching, caching and refreshing a
// policy.
func TestRFC8461PolicyDiscovery(t *testing.T) {
	ca := newCA(t)
	dns := newDNS()
	st := &stsStore{saved: map[string]store.STSPolicy{}}
	host, hc := servePolicy(t, ca, "mta-sts.example.com")
	clock := time.Now()
	w := &Worker{Hostname: "mail.example.org", Resolver: dns, Store: st, HTTP: hc, Now: func() time.Time { return clock }}
	const enforce = "version: STSv1\r\nmode: enforce\r\nmx: mail.example.com\r\nmax_age: 3600\r\n"

	// No TXT record: no policy, and nothing is fetched.
	if p := w.policy(ctx, "example.com"); p != nil || host.hits != 0 {
		t.Fatalf("a policy without a TXT record: %+v, %d fetches", p, host.hits)
	}
	// 3.3: the policy is at https://mta-sts.<domain>/.well-known/mta-sts.txt.
	dns.txt["_mta-sts.example.com"] = []string{"v=STSv1; id=1"}
	host.body = enforce
	p := w.policy(ctx, "example.com")
	if p == nil || p.Mode != "enforce" || p.MX[0] != "mail.example.com" || p.ID != "1" || !p.ExpiresAt.Equal(clock.Add(time.Hour)) {
		t.Fatalf("policy %+v", p)
	}
	if host.hits != 1 || host.paths[0] != "mta-sts.example.com/.well-known/mta-sts.txt" {
		t.Errorf("fetched %v", host.paths)
	}
	// Cached: the same id means no new fetch.
	for i := 0; i < 3; i++ {
		w.policy(ctx, "example.com")
	}
	if host.hits != 1 || st.writes != 1 {
		t.Errorf("%d fetches and %d writes for an unchanged id", host.hits, st.writes)
	}
	// 3.1: a new id means the policy changed: fetch again.
	dns.txt["_mta-sts.example.com"] = []string{"v=STSv1; id=2"}
	host.set(func() { host.body = strings.Replace(enforce, "enforce", "testing", 1) })
	if p := w.policy(ctx, "example.com"); p == nil || p.Mode != "testing" || p.ID != "2" || host.hits != 2 {
		t.Errorf("after a new id: %+v, %d fetches", p, host.hits)
	}

	// 5.1 and 10.2: an attacker who can break DNS or the fetch must not
	// be able to switch a policy off. The cached one keeps applying
	// until it expires.
	dns.txt["_mta-sts.example.com"] = []string{"v=STSv1; id=3"}
	host.set(func() { host.down = true })
	if p := w.policy(ctx, "example.com"); p == nil || p.ID != "2" {
		t.Errorf("fetch fails: %+v, want the cached policy", p)
	}
	delete(dns.txt, "_mta-sts.example.com")
	if p := w.policy(ctx, "example.com"); p == nil || p.ID != "2" {
		t.Errorf("TXT record gone: %+v, want the cached policy", p)
	}
	dns.broken["_mta-sts.example.com"] = true
	if p := w.policy(ctx, "example.com"); p == nil || p.ID != "2" {
		t.Errorf("TXT lookup fails: %+v, want the cached policy", p)
	}
	clock = clock.Add(2 * time.Hour)
	if p := w.policy(ctx, "example.com"); p != nil {
		t.Errorf("an expired policy still applies: %+v", p)
	}
	dns.broken["_mta-sts.example.com"] = false
	host.set(func() { host.down = false })

	// What is not a policy leaves the domain without one (3.3).
	dns.txt["_mta-sts.example.com"] = []string{"v=STSv1; id=4"}
	for name, change := range map[string]func(){
		"HTTP 404":                          func() { host.status = 404 },
		"a redirect, which is not followed": func() { host.status = 302 },
		"text/html":                         func() { host.ctype = "text/html" },
		"an invalid policy":                 func() { host.body = "version: STSv1\nmode: enforce\n" },
		"a huge body":                       func() { host.body = enforce + strings.Repeat("x: y\n", 20000) },
	} {
		host.set(func() { host.status, host.ctype, host.body = 200, "text/plain", enforce; change() })
		if p := w.policy(ctx, "example.com"); p != nil {
			t.Errorf("%s gives a policy: %+v", name, p)
		}
	}
	// "text/plain" may have parameters.
	host.set(func() { host.status, host.ctype, host.body = 200, "text/plain; charset=utf-8", enforce })
	if p := w.policy(ctx, "example.com"); p == nil || p.ID != "4" {
		t.Errorf("text/plain with a charset: %+v", p)
	}
	// 5: mode none is no policy, but it is cached like one.
	dns.txt["_mta-sts.example.com"] = []string{"v=STSv1; id=5"}
	host.set(func() { host.body = "version: STSv1\nmode: none\nmax_age: 3600\n" })
	before := host.hits
	if p := w.policy(ctx, "example.com"); p != nil {
		t.Errorf("mode none: %+v", p)
	}
	w.policy(ctx, "example.com")
	if host.hits != before+1 {
		t.Errorf("mode none is fetched every time: %d fetches", host.hits-before)
	}

	// 3.3: the certificate must be valid for the policy host. One for
	// another name gives no policy.
	_, wrong := servePolicy(t, ca, "www.example.com")
	w2 := &Worker{Hostname: "mail.example.org", Resolver: dns, Store: &stsStore{saved: map[string]store.STSPolicy{}}, HTTP: wrong}
	dns.txt["_mta-sts.other.example"] = []string{"v=STSv1; id=1"}
	if p := w2.policy(ctx, "other.example"); p != nil {
		t.Errorf("a policy from a host with the wrong certificate: %+v", p)
	}
}
