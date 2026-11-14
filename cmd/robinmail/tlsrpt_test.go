package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/config"
	"github.com/mikepage/robinmail/internal/queue"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/tlsrpt"
)

// TestMTASTSPolicyServed is phase 8's policy host: robinmail answers
// https://mta-sts.<domain>/.well-known/mta-sts.txt for every domain it
// has (RFC 8461 3.2 and 3.3), under that name and for nothing else, and
// "robinmail dns" prints the records that go with it.
func TestMTASTSPolicyServed(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	const host = "mail.example.test"
	certFile, keyFile, _ := selfSigned(t, t.TempDir(), host)
	c := newCLI(t, base, fmt.Sprintf(`"smtp":{"listen":"127.0.0.1:0"},"web":{"listen":"127.0.0.1:0"},"tls":{"cert_file":%q,"key_file":%q},
		"imap":{"listen_tls":"127.0.0.1:1993"},"submission":{"listen":"127.0.0.1:1587","listen_tls":"127.0.0.1:1465"},
		"mta_sts":{"mode":"enforce","max_age_seconds":604800,"mx":["mail.example.test","*.backup.example.test"]},"tls_rpt":{"disabled":true},`,
		certFile, keyFile))
	domain := "sts-" + randomHex() + ".test"
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)

	// The records. The id is taken from the policy, so it stays the same
	// until the policy changes (RFC 8461 3.1).
	const policy = "version: STSv1\r\nmode: enforce\r\nmx: mail.example.test\r\nmx: *.backup.example.test\r\nmax_age: 604800\r\n"
	records := c.ok("", "dns", domain)
	t.Logf("robinmail dns %s\n%s", domain, records)
	d := regexp.QuoteMeta(domain)
	c.expect(records,
		`(?m)^_mta-sts\.`+d+`\.\tTXT\t"v=STSv1; id=`+stsPolicyID(policy)+`"$`,
		`(?m)^mta-sts\.`+d+`\.\tCNAME\tmail\.example\.test\.$`,
		`(?m)^;   mode: enforce$`, `(?m)^;   mx: \*\.backup\.example\.test$`,
		`(?m)^_smtp\._tls\.`+d+`\.\tTXT\t"v=TLSRPTv1; rua=mailto:postmaster@`+d+`"$`,
		`(?m)^autoconfig\.`+d+`\.\tCNAME\tmail\.example\.test\.$`,
		// RFC 6186 3 and RFC 8314 5.1, with the ports of the config; "."
		// for what is not offered (RFC 6186 3.4).
		`(?m)^_imaps\._tcp\.`+d+`\.\tSRV\t0 1 1993 mail\.example\.test\.$`,
		`(?m)^_imap\._tcp\.`+d+`\.\tSRV\t0 0 0 \.$`,
		`(?m)^_submissions\._tcp\.`+d+`\.\tSRV\t0 1 1465 mail\.example\.test\.$`,
		`(?m)^_submission\._tcp\.`+d+`\.\tSRV\t0 1 1587 mail\.example\.test\.$`,
		`(?m)^_pop3\._tcp\.`+d+`\.\tSRV\t0 0 0 \.$`, `(?m)^_pop3s\._tcp\.`+d+`\.\tSRV\t0 0 0 \.$`)
	if again := c.ok("", "dns", domain); again != records {
		t.Error("the records differ between two runs")
	}
	if id := stsPolicyID(policy); !regexp.MustCompile(`^[a-z0-9]{1,32}$`).MatchString(id) {
		t.Errorf("the id %q is not 1 to 32 letters and digits (RFC 8461 3.1)", id)
	}

	old := ownDomainsFor
	ownDomainsFor = 0
	t.Cleanup(func() { ownDomainsFor = old })
	// The submission and IMAP ports of this config are fixed and may be
	// taken on this machine: serve without them.
	c2 := newCLI(t, base, fmt.Sprintf(`"smtp":{"listen":"127.0.0.1:0"},"web":{"listen":"127.0.0.1:0"},"tls":{"cert_file":%q,"key_file":%q},
		"mta_sts":{"mode":"enforce","max_age_seconds":604800,"mx":["mail.example.test","*.backup.example.test"]},"tls_rpt":{"disabled":true},`,
		certFile, keyFile))
	os.Rename(c.dir+"/master.key", c2.dir+"/master.key")
	srv := startServe(t, c2)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	// The certificate of the test is for the hostname only; whether the
	// certificate covers the policy host is the operator's (or ACME's)
	// part and not looked at here.
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, srv.web)
		},
	}}
	get := func(url string) (int, string, http.Header) {
		t.Helper()
		resp, err := hc.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), resp.Header
	}
	// RFC 8461 3.2: the policy file; 3.3: "at the well-known URI", media
	// type "text/plain".
	status, body, header := get("https://mta-sts." + domain + "/.well-known/mta-sts.txt")
	if status != 200 || body != policy || !strings.HasPrefix(header.Get("Content-Type"), "text/plain") {
		t.Errorf("the policy: %d %q %q", status, header.Get("Content-Type"), body)
	}
	// Nothing else lives under the name: no webmail, no login, no API.
	for _, path := range []string{"/", "/.well-known/jmap", "/auth/session", "/.well-known/mta-sts.txt/x"} {
		if status, _, _ := get("https://mta-sts." + domain + path); status != 404 {
			t.Errorf("mta-sts.%s%s: %d, want 404", domain, path, status)
		}
	}
	// A name that is not a domain of ours: 421, as for any foreign host.
	if status, _, _ := get("https://mta-sts.elsewhere-" + domain + "/.well-known/mta-sts.txt"); status != http.StatusMisdirectedRequest {
		t.Errorf("the policy host of a domain that is not ours: %d", status)
	}
	// The hostname itself has no policy file; a domain added while the
	// server runs is served without a restart.
	if status, _, _ := get("https://" + host + "/.well-known/mta-sts.txt"); status == 200 {
		t.Error("the policy is served under the hostname")
	}
	later := "later-" + domain
	c2.ok("", "domain", "add", later)
	if status, body, _ := get("https://mta-sts." + later + "/.well-known/mta-sts.txt"); status != 200 || body != policy {
		t.Errorf("a domain added later: %d %q", status, body)
	}
	req, _ := http.NewRequest("POST", "https://mta-sts."+domain+"/.well-known/mta-sts.txt", nil)
	if resp, err := hc.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the policy: %v %v", resp, err)
	}

	// Without mta_sts in the config there is no policy host.
	if _, ok := stsPolicy(config.MTASTS{}); ok {
		t.Error("a policy without a mode")
	}
	if text, _ := stsPolicy(config.MTASTS{Mode: "none", MaxAgeSeconds: 86400, MX: []string{"x.test"}}); text != "version: STSv1\r\nmode: none\r\nmax_age: 86400\r\n" {
		t.Errorf("mode none: %q", text) // RFC 8461 3.2: no mx needed with mode none
	}
}

// TestTLSReports is phase 8's TLS reporting (RFC 8460) from end to end on
// one server: counts of outbound sessions become the report of the day,
// which goes to the rua of the domain's record, by mail and over HTTPS;
// the mail arrives at a robinmail (this one), which keeps the report.
func TestTLSReports(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	in := startInstanceWith(t, base, "rpt", `"tls_rpt":{"disabled":true},`, func(*queue.Worker, *instance) {})
	postmaster := in.user("postmaster")
	cfg, err := config.Load(in.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := open(ctx, cfg, false)
	if err != nil {
		t.Fatal(err)
	}

	// Yesterday's sessions, as the queue worker would have counted them.
	reported, silent := "reported-"+randomHex()+".test", "silent-"+randomHex()+".test"
	yesterday := time.Now().Add(-24 * time.Hour)
	a.store.Now = func() time.Time { return yesterday }
	sts := `{"policy-type":"sts","policy-string":["version: STSv1","mode: testing","mx: mx.` + reported + `","max_age: 86400"],"policy-domain":"` + reported + `","mx-host":["mx.` + reported + `"]}`
	count := func(r store.TLSResult, n int) {
		for i := 0; i < n; i++ {
			if err := a.store.CountTLSResult(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	count(store.TLSResult{Domain: reported, Policy: sts}, 5)
	count(store.TLSResult{Domain: reported, Policy: sts, Result: tlsrpt.CertificateExpired, MX: "mx." + reported, IP: "192.0.2.9"}, 2)
	count(store.TLSResult{Domain: silent, Policy: `{"policy-type":"no-policy-found","policy-domain":"` + silent + `"}`}, 3)
	a.store.Now = time.Now

	// Where reports go: by mail to the postmaster of this server's domain,
	// and to an HTTPS endpoint.
	var mu sync.Mutex
	var posted []byte
	var postedType string
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		posted, postedType = body, r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer endpoint.Close()
	mailed := 0
	r := &tlsReporter{
		st: a.store, host: in.host, organization: "robinmail test",
		from: func(context.Context) string { return "postmaster@" + in.domain },
		lookupTXT: func(_ context.Context, name string) ([]string, error) {
			switch name {
			case "_smtp._tls." + reported:
				return []string{"v=TLSRPTv1; rua=mailto:" + postmaster + "," + endpoint.URL + "/v1/tlsrpt"}, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		},
		// The mail goes straight to the receiving server's port 25, as
		// the queue would deliver it.
		mail: func(_ context.Context, _, from, rcpt string, raw []byte) error {
			mailed++
			s := dialSMTP(t, in.smtp)
			s.cmd("EHLO "+in.host, 250)
			s.send(from, rcpt, string(raw), 250)
			s.cmd("QUIT", 221)
			return nil
		},
		post: endpoint.Client(), now: time.Now, log: slog.New(slog.DiscardHandler),
	}
	sent, err := r.run(ctx)
	if err != nil || sent != 1 || mailed != 1 {
		t.Fatalf("run: %d reports sent, %d mailed, %v", sent, mailed, err)
	}
	// Each report is made once, whoever asks again.
	if sent, err := r.run(ctx); err != nil || sent != 0 || mailed != 1 {
		t.Fatalf("a second run: %d sent, %v", sent, err)
	}

	// Over HTTPS (RFC 8460 5.4): the gzip of the JSON, with its media type.
	mu.Lock()
	rep, err := tlsrpt.Parse(posted)
	mu.Unlock()
	if err != nil || postedType != "application/tlsrpt+gzip" || len(posted) < 2 || posted[0] != 0x1f {
		t.Fatalf("the POST: %q, %v", postedType, err)
	}
	day := time.Unix(store.TLSDay(yesterday)*86400, 0).UTC()
	if rep.OrganizationName != "robinmail test" || rep.ContactInfo != "postmaster@"+in.domain ||
		rep.ReportID != day.Format("20060102")+"."+reported+"@"+in.host ||
		!rep.DateRange.Start.Equal(day) || !rep.DateRange.End.Equal(day.Add(24*time.Hour-time.Second)) {
		t.Errorf("the report: %+v", rep)
	}
	if len(rep.Policies) != 1 || rep.Policies[0].Policy.Type != "sts" || rep.Policies[0].Policy.Domain != reported ||
		rep.Policies[0].Summary != (tlsrpt.Summary{Successful: 5, Failed: 2}) || len(rep.Policies[0].FailureDetails) != 1 ||
		rep.Policies[0].FailureDetails[0] != (tlsrpt.FailureDetail{ResultType: "certificate-expired", ReceivingMXHostname: "mx." + reported,
			ReceivingIP: "192.0.2.9", FailedSessionCount: 2}) {
		t.Errorf("the policy of the report: %+v", rep.Policies)
	}

	// By mail: it is in the postmaster's Inbox as a message, and kept as
	// a report where the administrator finds it.
	// Lists cut long names short.
	short := regexp.QuoteMeta(reported[:20])
	in.expect(in.inbox(postmaster), `Report Domain: `+short)
	list := in.ok("", "tlsrpt", "received")
	in.expect(list, `robinmail test\s+`+short+`\S*\s+.*ok 5\s+failed 2`)
	id := regexp.MustCompile(`(?m)^(\d+)\s.*robinmail test\s+` + short).FindStringSubmatch(list)[1]
	shown := in.ok("", "tlsrpt", "show", id)
	in.expect(shown, `"report-id": "`+regexp.QuoteMeta(rep.ReportID)+`"`, `"policy-domain": "`+regexp.QuoteMeta(reported)+`"`, `"result-type": "certificate-expired"`)
	// The same report arriving again is not kept twice.
	s := dialSMTP(t, in.smtp)
	s.cmd("EHLO "+in.host, 250)
	s.send("postmaster@"+in.domain, postmaster, "TLS-Report-Domain: "+reported+"\r\nSubject: again "+randomHex()+"\r\nContent-Type: application/tlsrpt+json\r\n\r\n"+string(rep.JSON())+"\r\n", 250)
	if again := in.ok("", "tlsrpt", "received"); strings.Count(again, reported[:20]) != 1 {
		t.Errorf("the report is listed %d times:\n%s", strings.Count(again, reported[:20]), again)
	}

	// What the administrator sees of the sending side: the counts, and
	// what became of each day's report. The domain without a record got
	// none, and is not asked again.
	results := in.ok("", "tlsrpt", "results", "3")
	in.expect(results,
		regexp.QuoteMeta(reported)+`\s+sts\s+ok\s+5`,
		regexp.QuoteMeta(reported)+`\s+sts\s+certificate-expired\s+2\s+mx\.`+regexp.QuoteMeta(reported)+` 192\.0\.2\.9`,
		`report of \S+ for `+regexp.QuoteMeta(reported)+`: sent to mailto:`+regexp.QuoteMeta(postmaster)+`, https://`,
		`report of \S+ for `+regexp.QuoteMeta(silent)+`: the domain asks for no reports`)
	for _, bad := range [][]string{{"tlsrpt", "show", "999999999"}, {"tlsrpt", "show", "x"}, {"tlsrpt", "results", "0"}} {
		if out, err := in.do("", bad...); err == nil {
			t.Errorf("%v worked: %s", bad, out)
		}
	}
}
