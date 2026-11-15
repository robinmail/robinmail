package queue

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/message"
	"github.com/robinmail/robinmail/internal/smtp"
	"github.com/robinmail/robinmail/internal/store"
)

// These tests run the Worker against a real store (rqlite), a test
// resolver and mail servers on 127.0.0.1. They need:
//
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 go test ./internal/queue

func states(sub store.Submission) string {
	var out []string
	for _, r := range sub.Recipients {
		out = append(out, r.Address[:strings.IndexByte(r.Address, '@')]+"="+r.State)
	}
	return strings.Join(out, " ")
}

// One message to two recipients of a domain is one connection and one
// transaction; the next message to that domain uses the connection again.
func TestDeliverAndReuseConnection(t *testing.T) {
	x := newWorld(t)
	b := startMX(t, "mx.b.test", x.ca.issue(t, false, "mx.b.test"))
	x.domainMX("b.test", b, 10)

	sub := x.send("first", "anna@b.test", "bob@b.test")
	if n := x.run(); n != 1 {
		t.Fatalf("worked on %d messages", n)
	}
	envs, msgs := b.got()
	if len(envs) != 1 || envs[0].From != x.acct.Address || strings.Join(envs[0].Rcpts, " ") != "anna@b.test bob@b.test" {
		t.Fatalf("the server got %+v", envs)
	}
	if envs[0].Helo != "mail."+x.domain {
		t.Errorf("EHLO %q, want our host name", envs[0].Helo)
	}
	// Opportunistic TLS: the server offers STARTTLS, so it is used.
	if envs[0].TLS == nil {
		t.Error("STARTTLS was offered and not used")
	}
	if !bytes.Contains(msgs[0], []byte("Subject: first\r\n")) {
		t.Errorf("message %q", msgs[0])
	}
	got := x.sub(sub.ID)
	if got.Queue != store.QueueDone || states(got) != "anna=delivered bob=delivered" || got.Tries != 1 || got.Blob != "" {
		t.Errorf("submission %+v", got)
	}
	if r := got.Recipients[0]; !strings.HasPrefix(r.Reply, "250 2.0.0 ") || r.Status != "2.0.0" || r.MTA != "mx.b.test" || r.Mailbox {
		t.Errorf("recipient %+v", r)
	}
	if len(x.inbox()) != 0 {
		t.Error("a bounce for delivered mail")
	}

	x.send("second", "carol@b.test")
	x.send("third", "dave@b.test")
	x.run()
	if envs, _ := b.got(); len(envs) != 3 {
		t.Fatalf("%d messages arrived, want 3", len(envs))
	}
	if n := b.connections(); n > 2 {
		t.Errorf("%d connections for three messages to one domain", n)
	}
	// Nothing is due any more.
	if n := x.run(); n != 0 {
		t.Errorf("worked on %d messages when all were done", n)
	}
	// A server that closed the idle connection: the next message opens
	// a new one instead of failing.
	x.w.Close()
	x.send("fourth", "erin@b.test")
	x.run()
	if envs, _ := b.got(); len(envs) != 4 {
		t.Errorf("after the pool was closed: %d messages", len(envs))
	}
}

// RFC 5321 5.1 and 5.2 in action: the best MX refuses the connection and
// the next one gets the mail; of a host with two addresses the second is
// used when the first does not answer.
func TestFallbackToNextMXAndAddress(t *testing.T) {
	x := newWorld(t)
	backup := startMX(t, "mx2.b.test", nil)
	x.dns.mx["b.test"] = []*net.MX{{Host: "mx1.b.test.", Pref: 10}, {Host: "mx2.b.test.", Pref: 20}}
	x.dns.ip["mx1.b.test"] = []string{"192.0.2.101"} // nobody listens there
	x.dns.ip["mx2.b.test"] = []string{"2001:db8::2", "192.0.2.102"}
	x.net.route("192.0.2.102:25", backup.addr) // only reachable over IPv4

	sub := x.send("fallback", "anna@b.test")
	x.run()
	if envs, _ := backup.got(); len(envs) != 1 {
		t.Fatalf("the backup MX got %d messages", len(envs))
	}
	want := []string{"192.0.2.101:25", "[2001:db8::2]:25", "192.0.2.102:25"}
	if got := x.net.dialed(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("connections tried: %v, want %v", got, want)
	}
	if got := x.sub(sub.ID); states(got) != "anna=delivered" || got.Recipients[0].MTA != "mx2.b.test" {
		t.Errorf("submission %+v", got)
	}
	// Without STARTTLS on offer the mail goes in the clear.
	if envs, _ := backup.got(); envs[0].TLS != nil {
		t.Error("TLS without STARTTLS")
	}
}

// A recipient the server refuses for good bounces at once; the others of
// the same message are delivered. The bounce is a DSN (RFC 3464) in the
// sender's Inbox.
func TestPermanentFailureBouncesAtOnce(t *testing.T) {
	x := newWorld(t)
	b := startMX(t, "mx.b.test", nil)
	b.rcpt = func(to string) error {
		if strings.HasPrefix(to, "nobody@") {
			return &smtp.Error{Code: 550, Enhanced: "5.1.1", Msg: "No such user here"}
		}
		return nil
	}
	x.domainMX("b.test", b, 10)
	sub := x.send("to two", "anna@b.test", "nobody@b.test")
	x.run()

	got := x.sub(sub.ID)
	if got.Queue != store.QueueDone || states(got) != "anna=delivered nobody=failed" {
		t.Fatalf("submission %+v", got)
	}
	if r := got.Recipients[1]; r.Reply != "550 5.1.1 No such user here" || r.Status != "5.1.1" || r.MTA != "mx.b.test" {
		t.Errorf("failed recipient %+v", r)
	}
	if envs, _ := b.got(); len(envs) != 1 || strings.Join(envs[0].Rcpts, " ") != "anna@b.test" {
		t.Errorf("the server got %+v", envs)
	}
	inbox := x.inbox()
	if len(inbox) != 1 {
		t.Fatalf("%d messages in the sender's Inbox, want the bounce", len(inbox))
	}
	bounce := inbox[0]
	t.Logf("the bounce:\n%s", strings.ReplaceAll(string(bounce), "\r\n", "\n"))
	// RFC 3464 2 and RFC 5321 4.5.5: a notification has the null
	// reverse-path, here as the Return-Path of the delivered message.
	if !bytes.HasPrefix(bounce, []byte("Return-Path: <>\r\n")) {
		t.Errorf("no null reverse-path: %q", bounce[:40])
	}
	m := message.Parse(bounce)
	dsn, ok := message.ParseDSN(m)
	if !ok || len(dsn.Recipients) != 1 {
		t.Fatalf("the bounce is not a DSN: %+v", dsn)
	}
	r := dsn.Recipients[0]
	if r.Address != "nobody@b.test" || r.Action != "failed" || r.Status != "5.1.1" || r.RemoteMTA != "mx.b.test" ||
		r.Diagnostic != "550 5.1.1 No such user here" || dsn.ReportingMTA != "mail."+x.domain || dsn.OriginalMessageID != got.MessageID {
		t.Errorf("DSN %+v, recipient %+v", dsn, r)
	}
	to, _ := message.Get(m.Headers, "To")
	if strings.TrimSpace(to) != x.acct.Address {
		t.Errorf("the bounce is addressed to %q", to)
	}
	// The delivered recipient is not in the report.
	if bytes.Contains(bounce, []byte("anna@b.test>: failed")) {
		t.Error("the bounce names a recipient that got the mail")
	}
	// It threads with the message it is about.
	if v, _ := message.Get(m.Headers, "References"); !strings.Contains(v, got.MessageID) {
		t.Errorf("References %q", v)
	}
}

// The exit of phase 5: a destination that refuses connections is retried
// on the backoff schedule and bounces when five days are over.
func TestRetryScheduleAndExpiry(t *testing.T) {
	x := newWorld(t)
	x.dns.mx["down.test"] = []*net.MX{{Host: "mx.down.test.", Pref: 10}}
	x.dns.ip["mx.down.test"] = []string{"192.0.2.50"} // refuses connections
	start := x.now()
	sub := x.send("into the void", "anna@down.test")

	var attempts []time.Duration
	for i := 0; i < 100; i++ {
		before := len(x.net.dialed())
		if n := x.run(); n == 1 {
			if len(x.net.dialed()) != before+1 {
				t.Fatalf("attempt %d made %d connections", len(attempts)+1, len(x.net.dialed())-before)
			}
			attempts = append(attempts, x.now().Sub(start))
		}
		got := x.sub(sub.ID)
		if got.Queue == store.QueueDone {
			break
		}
		// Nothing happens before the next try is due.
		x.advance(got.NextTry.Sub(x.now()) - time.Second)
		if n := x.run(); n != 0 {
			t.Fatalf("an attempt one second before it was due (after %d attempts)", len(attempts))
		}
		if r := got.Recipients[0]; r.State != store.RcptQueued || !strings.Contains(r.Reply, "connection refused") || r.Status != "4.4.1" ||
			!strings.HasPrefix(r.Reply, "451 4.4.1 ") { // a reply code first: RFC 8621 7, smtpReply
			t.Fatalf("recipient between attempts: %+v", r)
		}
		x.advance(time.Second)
	}
	t.Logf("attempts at: %v", attempts)
	// 5 minutes, then twice as long each time, up to 4 hours.
	wantGaps := []time.Duration{5, 10, 20, 40, 80, 160, 240, 240}
	for i, gap := range wantGaps {
		if got := attempts[i+1] - attempts[i]; got != gap*time.Minute {
			t.Errorf("between attempt %d and %d: %v, want %v", i+1, i+2, got, gap*time.Minute)
		}
	}
	last := attempts[len(attempts)-1]
	if last < 5*24*time.Hour || last > 5*24*time.Hour+4*time.Hour {
		t.Errorf("gave up after %v, want just over five days", last)
	}
	if len(attempts) < 25 || len(attempts) > 40 {
		t.Errorf("%d attempts in five days", len(attempts))
	}
	got := x.sub(sub.ID)
	if got.Queue != store.QueueDone || states(got) != "anna=failed" || got.Recipients[0].Status != "4.4.7" {
		t.Fatalf("after five days: %+v", got)
	}
	inbox := x.inbox()
	if len(inbox) != 1 {
		t.Fatalf("%d bounces", len(inbox))
	}
	dsn, ok := message.ParseDSN(message.Parse(inbox[0]))
	if !ok || dsn.Recipients[0].Action != "failed" || dsn.Recipients[0].Status != "4.4.7" ||
		!strings.Contains(dsn.Recipients[0].Diagnostic, "120h0m0s") || !strings.Contains(dsn.Recipients[0].Diagnostic, "connection refused") {
		t.Errorf("the bounce: %+v", dsn)
	}
	// Nothing more happens to it.
	x.advance(24 * time.Hour)
	if n := x.run(); n != 0 || len(x.inbox()) != 1 {
		t.Errorf("activity after the bounce")
	}
}

// A server that says "try later" keeps the message queued; when it takes
// the mail on a later try, there is no bounce.
func TestTemporaryFailureThenDelivery(t *testing.T) {
	x := newWorld(t)
	b := startMX(t, "mx.b.test", nil)
	var mu sync.Mutex
	grey := true
	b.rcpt = func(string) error {
		mu.Lock()
		defer mu.Unlock()
		if grey {
			return &smtp.Error{Code: 451, Enhanced: "4.7.1", Msg: "Greylisted, come back later"}
		}
		return nil
	}
	x.domainMX("b.test", b, 10)
	sub := x.send("greylisted", "anna@b.test")
	x.run()
	got := x.sub(sub.ID)
	if got.Queue != store.QueueActive || states(got) != "anna=queued" || got.Recipients[0].Reply != "451 4.7.1 Greylisted, come back later" ||
		!got.NextTry.Equal(x.now().Add(5*time.Minute)) {
		t.Fatalf("after a 451: %+v", got)
	}
	mu.Lock()
	grey = false
	mu.Unlock()
	x.advance(5 * time.Minute)
	x.run()
	if got := x.sub(sub.ID); got.Queue != store.QueueDone || states(got) != "anna=delivered" || got.Tries != 2 {
		t.Errorf("after the second try: %+v", got)
	}
	if len(x.inbox()) != 0 {
		t.Error("a bounce for mail that was delivered")
	}
}

// Domains that take no mail fail at once, each with its own status.
func TestDomainsWithoutMail(t *testing.T) {
	x := newWorld(t)
	x.dns.mx["nullmx.test"] = []*net.MX{{Host: ".", Pref: 0}}
	x.dns.mx["loop.test"] = []*net.MX{{Host: "mail." + x.domain + ".", Pref: 10}}
	sub := x.send("nowhere", "a@nullmx.test", "b@nxdomain.test", "c@loop.test", "d@[192.0.2.1]")
	x.run()
	got := x.sub(sub.ID)
	if got.Queue != store.QueueDone || states(got) != "a=failed b=failed c=failed d=failed" {
		t.Fatalf("submission %+v", got)
	}
	for i, want := range []string{"5.1.10", "5.1.2", "5.4.6", "5.1.2"} {
		if got.Recipients[i].Status != want {
			t.Errorf("%s: status %s (%s), want %s", got.Recipients[i].Address, got.Recipients[i].Status, got.Recipients[i].Reply, want)
		}
	}
	if len(x.net.dialed()) != 0 {
		t.Errorf("connections were tried: %v", x.net.dialed())
	}
	// One bounce for the four.
	inbox := x.inbox()
	if len(inbox) != 1 {
		t.Fatalf("%d bounces", len(inbox))
	}
	if dsn, _ := message.ParseDSN(message.Parse(inbox[0])); len(dsn.Recipients) != 4 || dsn.Recipients[0].RemoteMTA != "" {
		t.Errorf("the bounce: %+v", dsn)
	}
	// A DNS failure is not "no such domain": the message waits.
	x.dns.broken["flaky.test"] = true
	sub = x.send("dns down", "a@flaky.test")
	x.run()
	if got := x.sub(sub.ID); got.Queue != store.QueueActive || got.Recipients[0].Status != "4.4.3" {
		t.Errorf("DNS failure: %+v", got)
	}
}

// RFC 5321 6.1 and 4.5.5: nothing is sent in reply to a message with the
// null reverse-path, or bounces would bounce for ever.
func TestNoBounceForABounce(t *testing.T) {
	x := newWorld(t)
	b := startMX(t, "mx.b.test", nil)
	b.rcpt = func(string) error { return &smtp.Error{Code: 550, Enhanced: "5.1.1", Msg: "No such user"} }
	x.domainMX("b.test", b, 10)
	sub := x.sendFrom("", "a notification", "nobody@b.test")
	x.run()
	if got := x.sub(sub.ID); got.Queue != store.QueueDone || states(got) != "nobody=failed" {
		t.Fatalf("submission %+v", got)
	}
	if n := len(x.inbox()); n != 0 {
		t.Errorf("%d bounces for a message with the null reverse-path", n)
	}
}

// Recipients at our own domains are stored here, without DNS and SMTP.
func TestLocalRecipients(t *testing.T) {
	x := newWorld(t)
	x.local.domains["also-ours.test"] = true
	x.local.unknown["ghost@also-ours.test"] = true
	b := startMX(t, "mx.b.test", nil)
	x.domainMX("b.test", b, 10)
	sub := x.send("mixed", "colleague@"+x.domain, "anna@b.test", "ghost@also-ours.test")
	x.run()
	got := x.sub(sub.ID)
	if states(got) != "colleague=delivered anna=delivered ghost=failed" {
		t.Fatalf("submission %+v", got)
	}
	// Here "delivered" is known to mean "in the mailbox".
	if !got.Recipients[0].Mailbox || got.Recipients[1].Mailbox || got.Recipients[2].Status != "5.1.1" {
		t.Errorf("recipients %+v", got.Recipients)
	}
	if want := x.acct.Address + " -> colleague@" + x.domain; len(x.local.delivered) != 1 || x.local.delivered[0] != want {
		t.Errorf("local deliveries %v", x.local.delivered)
	}
	if x.dns.count("MX "+x.domain) != 0 || x.dns.count("MX also-ours.test") != 0 {
		t.Errorf("MX lookups for our own domains: %v", x.dns.lookups)
	}
	if envs, _ := b.got(); len(envs) != 1 {
		t.Errorf("the remote server got %d messages", len(envs))
	}
}

// A node that dies holding a claim: the message is neither lost nor sent
// twice. Nobody touches the row while the claim lasts; after that another
// node takes it and delivers once.
func TestNodeDiesHoldingAClaim(t *testing.T) {
	x := newWorld(t)
	b := startMX(t, "mx.b.test", nil)
	x.domainMX("b.test", b, 10)
	sub := x.send("claimed by a node that dies", "anna@b.test")

	// The node claims the row and is never heard of again.
	dead, err := x.st.ClaimSubmission(ctx, store.SubmissionRef{Account: x.acct.ID, ID: sub.ID}, "dead-node:1", x.now(), x.now().Add(2*time.Minute))
	if err != nil || dead.ClaimedBy != "dead-node:1" {
		t.Fatalf("claim: %+v %v", dead, err)
	}
	live := x.worker("n2")
	for _, wait := range []time.Duration{0, time.Minute, 59 * time.Second} {
		x.advance(wait)
		if n, _ := live.RunOnce(ctx); n != 0 {
			t.Fatalf("another node took the row %v into a claim of 2 minutes", x.now().Sub(dead.CreatedAt))
		}
	}
	if envs, _ := b.got(); len(envs) != 0 {
		t.Fatal("delivered while the claim was held")
	}
	x.advance(time.Second)
	if n, _ := live.RunOnce(ctx); n != 1 {
		t.Fatalf("the row was not taken after the claim ran out: %d", n)
	}
	// Both nodes look again, many times: it is delivered once.
	for i := 0; i < 3; i++ {
		x.advance(time.Hour)
		live.RunOnce(ctx)
		x.run()
	}
	envs, _ := b.got()
	if len(envs) != 1 {
		t.Fatalf("the message arrived %d times, want once", len(envs))
	}
	got := x.sub(sub.ID)
	if got.Queue != store.QueueDone || states(got) != "anna=delivered" || got.Tries != 2 {
		t.Errorf("submission %+v", got)
	}
	// What the dead node might still try to write is refused.
	dead.Recipients[0].State = store.RcptFailed
	if err := x.st.SaveSubmission(ctx, dead, "dead-node:1", true, x.now()); err == nil {
		t.Error("a node without the claim wrote a result")
	}
}

// Three nodes work the same queue at once: every message is delivered
// exactly once.
func TestSeveralNodesOneQueue(t *testing.T) {
	x := newWorld(t)
	b := startMX(t, "mx.b.test", nil)
	x.domainMX("b.test", b, 10)
	const n = 30
	for i := 0; i < n; i++ {
		x.send("message "+string(rune('A'+i)), "anna@b.test")
	}
	workers := []*Worker{x.w, x.worker("n2"), x.worker("n3")}
	var wg sync.WaitGroup
	counts := make([]int, len(workers))
	for i, w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counts[i], _ = w.RunOnce(ctx)
		}()
	}
	wg.Wait()
	_, msgs := b.got()
	seen := map[string]int{}
	for _, m := range msgs {
		subj, _ := message.Get(message.ParseHeader(m), "Subject")
		seen[strings.TrimSpace(subj)]++
	}
	if len(msgs) != n || len(seen) != n {
		t.Errorf("%d messages arrived, %d different, want %d of each; per node: %v", len(msgs), len(seen), n, counts)
	}
	if counts[0]+counts[1]+counts[2] != n {
		t.Errorf("claims per node %v, want %d in total", counts, n)
	}
}

// PerDestination bounds the connections to one domain.
func TestPerDestinationLimit(t *testing.T) {
	x := newWorld(t)
	x.w.PerDestination = 2
	b := startMX(t, "mx.b.test", nil)
	b.slow = 50 * time.Millisecond
	x.domainMX("b.test", b, 10)
	for i := 0; i < 12; i++ {
		x.send("burst", "anna@b.test")
	}
	x.run()
	if envs, _ := b.got(); len(envs) != 12 {
		t.Fatalf("%d of 12 arrived", len(envs))
	}
	if b.peak > 2 {
		t.Errorf("%d deliveries to one domain at once, with a limit of 2", b.peak)
	}
	if n := b.connections(); n > 2 {
		t.Errorf("%d connections, want at most 2 reused ones", n)
	}
}

// brokenTLS is a server that offers STARTTLS and then fails the
// handshake: a certificate nobody can use.
func brokenTLS(t *testing.T) *tls.Config {
	return &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return nil, net.UnknownNetworkError("no certificate today")
	}}
}

// RFC 3207 in the sending direction, without a policy: TLS is used when
// offered, the certificate is not checked, and a failed handshake means
// the mail goes without TLS rather than not at all.
func TestOpportunisticTLS(t *testing.T) {
	x := newWorld(t)
	selfSigned := newCA(t) // a CA the worker does not trust
	odd := startMX(t, "mx.odd.test", selfSigned.issue(t, true, "some.other.name"))
	x.domainMX("odd.test", odd, 10)
	broken := startMX(t, "mx.broken.test", brokenTLS(t))
	x.domainMX("broken.test", broken, 10)

	rec := &tlsRecorder{}
	x.w.TLSResults = rec
	sub := x.send("no policy", "a@odd.test", "b@broken.test")
	x.run()
	if got := x.sub(sub.ID); states(got) != "a=delivered b=delivered" {
		t.Fatalf("submission %+v", got)
	}
	// For the report (RFC 8460 4.3.2: "no-policy-found"): TLS that was
	// used counts as a session that worked, whatever the certificate; a
	// handshake that failed is a failure, though the mail went through.
	if got := rec.of("odd.test"); len(got) != 1 || got[0].Result != "" || !strings.Contains(got[0].Policy, `"policy-type":"no-policy-found"`) {
		t.Errorf("TLS results without a policy: %+v", got)
	}
	if got := rec.of("broken.test"); len(got) != 1 || got[0].Result != "validation-failure" || got[0].MX != "mx.broken.test" {
		t.Errorf("TLS results of a failed handshake: %+v", got)
	}
	if envs, _ := odd.got(); envs[0].TLS == nil {
		t.Error("an expired certificate for another name: TLS was not used")
	}
	if envs, _ := broken.got(); len(envs) != 1 || envs[0].TLS != nil {
		t.Errorf("after a failed handshake: %+v", envs)
	}
	if n := broken.connections(); n != 2 {
		t.Errorf("%d connections to the server with broken TLS, want one with and one without", n)
	}
}

// RFC 8461 4 and 5 in action. In mode enforce mail only goes to a host the
// policy names, over TLS, with a certificate that is valid for that host.
// Anything else and the message stays in the queue.
func TestMTASTSEnforce(t *testing.T) {
	x := newWorld(t)
	other := newCA(t)
	host, hc := servePolicy(t, x.ca, "mta-sts.good.test", "mta-sts.badcert.test", "mta-sts.expired.test", "mta-sts.notls.test",
		"mta-sts.wrongmx.test", "mta-sts.wrongname.test", "mta-sts.testing.test")
	x.w.HTTP = hc
	// The cache is in rqlite and outlives a test run; an id of this run
	// makes what an earlier run cached stale.
	id := "run" + strings.TrimSuffix(strings.TrimPrefix(x.domain, "q"), ".test")
	policy := func(mode string, mx ...string) string {
		return "version: STSv1\nmode: " + mode + "\nmx: " + strings.Join(mx, "\nmx: ") + "\nmax_age: 86400\n"
	}
	// One policy file for every domain: all the hosts are in it, except
	// the one of wrongmx.test.
	host.body = policy("enforce", "mx.good.test", "mx.badcert.test", "mx.expired.test", "mx.notls.test", "*.allowed.test")
	servers := map[string]*mx{
		"good.test":    startMX(t, "mx.good.test", x.ca.issue(t, false, "mx.good.test")),
		"badcert.test": startMX(t, "mx.badcert.test", other.issue(t, false, "mx.badcert.test")), // unknown CA
		"expired.test": startMX(t, "mx.expired.test", x.ca.issue(t, true, "mx.expired.test")),
		"notls.test":   startMX(t, "mx.notls.test", nil),
		"wrongmx.test": startMX(t, "mx.elsewhere.test", x.ca.issue(t, false, "mx.elsewhere.test")),
	}
	for domain, m := range servers {
		x.domainMX(domain, m, 10)
		x.dns.txt["_mta-sts."+domain] = []string{"v=STSv1; id=" + id}
	}
	// A certificate for another name than the MX host.
	wrongName := startMX(t, "wrongname.allowed.test", x.ca.issue(t, false, "something-else.allowed.test"))
	x.domainMX("wrongname.test", wrongName, 10)
	x.dns.txt["_mta-sts.wrongname.test"] = []string{"v=STSv1; id=" + id}

	// What goes into the TLS report of each domain (RFC 8460 4.3).
	rec := &tlsRecorder{}
	x.w.TLSResults = rec
	sub := x.send("strict", "a@good.test", "b@badcert.test", "c@expired.test", "d@notls.test", "e@wrongmx.test", "f@wrongname.test")
	x.run()
	for domain, want := range map[string]string{
		"good.test":      "", // a session that worked
		"badcert.test":   "certificate-not-trusted",
		"expired.test":   "certificate-expired",
		"notls.test":     "starttls-not-supported",
		"wrongmx.test":   "validation-failure",
		"wrongname.test": "certificate-host-mismatch",
	} {
		got := rec.of(domain)
		if len(got) != 1 || got[0].Result != want {
			t.Errorf("TLS results of %s: %+v, want one %q", domain, got, want)
			continue
		}
		// The policy that applied travels with the count, as the report
		// will show it; a failure names the host and the address.
		if !strings.Contains(got[0].Policy, `"policy-type":"sts"`) || !strings.Contains(got[0].Policy, `"mode: enforce"`) ||
			!strings.Contains(got[0].Policy, `"policy-domain":"`+domain+`"`) || (want != "" && got[0].MX == "") || (want == "" && got[0].MX != "") {
			t.Errorf("TLS result of %s: %+v", domain, got[0])
		}
	}
	got := x.sub(sub.ID)
	if states(got) != "a=delivered b=queued c=queued d=queued e=queued f=queued" {
		t.Fatalf("submission: %s", states(got))
	}
	for i, r := range got.Recipients {
		t.Logf("%-18s %-9s %s %s", r.Address, r.State, r.Status, r.Reply)
		if i > 0 && (r.Status != "4.7.0" || !strings.Contains(r.Reply, "MTA-STS")) {
			t.Errorf("%s: %s %q, want a 4.7.0 that names MTA-STS", r.Address, r.Status, r.Reply)
		}
	}
	for domain, m := range servers {
		envs, _ := m.got()
		if want := map[bool]int{true: 1}[domain == "good.test"]; len(envs) != want {
			t.Errorf("%s got %d messages, want %d", domain, len(envs), want)
		}
	}
	if envs, _ := servers["good.test"].got(); envs[0].TLS == nil || envs[0].TLS.Version < tls.VersionTLS12 {
		t.Error("the message under enforce did not go over TLS 1.2 or later")
	}
	if envs, _ := wrongName.got(); len(envs) != 0 {
		t.Error("delivered to a host whose certificate names another host")
	}
	if !strings.Contains(got.Recipients[1].Reply, "certificate") {
		t.Errorf("the reason for badcert.test does not mention the certificate: %q", got.Recipients[1].Reply)
	}
	// No fallback to plaintext, and no second try on the same host.
	if n := servers["badcert.test"].connections(); n != 1 {
		t.Errorf("%d connections to the server with the bad certificate", n)
	}
	// It is still not delivered an hour later: no policy was forgotten.
	x.advance(time.Hour)
	x.run()
	if got := x.sub(sub.ID); states(got) != "a=delivered b=queued c=queued d=queued e=queued f=queued" {
		t.Errorf("an hour later: %s", states(got))
	}
	// The policy was fetched once per domain and then came from rqlite,
	// also for a second node.
	if host.hits != 6 {
		t.Errorf("%d policy fetches for 6 domains over two tries", host.hits)
	}
	if p, ok, _ := x.st.STSPolicy(ctx, "good.test"); !ok || p.Mode != "enforce" || p.ID != id {
		t.Errorf("cached policy %+v", p)
	}

	// Mode testing (RFC 8461 5): the same failures do not stop delivery.
	host.set(func() { host.body = policy("testing", "mx.testing.test") })
	testing := startMX(t, "mx.testing.test", other.issue(t, false, "mx.testing.test"))
	x.domainMX("testing.test", testing, 10)
	x.dns.txt["_mta-sts.testing.test"] = []string{"v=STSv1; id=" + id}
	sub2 := x.send("lenient", "g@testing.test")
	x.run()
	if got := x.sub(sub2.ID); states(got) != "g=delivered" {
		t.Errorf("mode testing: %+v", got.Recipients)
	}
	// Delivered, and reported as the failure enforce would have made of it.
	if got := rec.of("testing.test"); len(got) != 1 || got[0].Result != "certificate-not-trusted" || !strings.Contains(got[0].Policy, `"mode: testing"`) {
		t.Errorf("TLS results under testing: %+v", got)
	}

	// The owner of badcert.test fixes the certificate: the next try
	// delivers. The old connection attempt left nothing behind.
	fixed := startMX(t, "mx.badcert.test", x.ca.issue(t, false, "mx.badcert.test"))
	ip := x.dns.ip["mx.badcert.test"][0]
	x.net.route(ip+":25", fixed.addr)
	x.advance(4 * time.Hour)
	x.run()
	if got := x.sub(sub.ID); got.Recipients[1].State != store.RcptDelivered {
		t.Errorf("after the certificate was fixed: %+v", got.Recipients[1])
	}
}

// A route sends a domain's mail to a fixed host without asking DNS.
func TestRoutes(t *testing.T) {
	x := newWorld(t)
	relay := startMX(t, "relay.internal", nil)
	x.w.Routes = map[string]string{"b.test": relay.addr, "*": "192.0.2.77:2525"}
	x.net.route(relay.addr, relay.addr)
	sub := x.send("routed", "anna@b.test", "bob@other.test")
	x.run()
	got := x.sub(sub.ID)
	if states(got) != "anna=delivered bob=queued" {
		t.Fatalf("submission %+v", got)
	}
	if x.dns.count("MX ") != 0 || x.dns.count("TXT ") != 0 {
		t.Errorf("DNS was asked for routed domains: %v", x.dns.lookups)
	}
	if dialed := strings.Join(x.net.dialed(), " "); !strings.Contains(dialed, "192.0.2.77:2525") {
		t.Errorf("the catch-all route was not used: %s", dialed)
	}
}

// Worker.Retry (the config's dev.retry_seconds) replaces the schedule: a
// lab sees the second try after seconds, not after five minutes.
func TestRetryOverride(t *testing.T) {
	x := newWorld(t)
	x.w.Retry = func(n int) time.Duration { return time.Duration(n) * 3 * time.Second }
	x.dns.mx["down.test"] = []*net.MX{{Host: "mx.down.test.", Pref: 10}}
	x.dns.ip["mx.down.test"] = []string{"192.0.2.50"} // refuses connections
	sub := x.send("soon again", "anna@down.test")
	for try := 1; try <= 3; try++ {
		if n := x.run(); n != 1 {
			t.Fatalf("try %d: %d attempts", try, n)
		}
		got := x.sub(sub.ID)
		if want := x.now().Add(time.Duration(try) * 3 * time.Second); !got.NextTry.Equal(want) {
			t.Fatalf("after try %d the next is at %v, want %v", try, got.NextTry, want)
		}
		x.advance(time.Duration(try) * 3 * time.Second)
	}
}

// tlsRecorder keeps what a worker reports about TLS.
type tlsRecorder struct {
	mu   sync.Mutex
	list []store.TLSResult
}

func (r *tlsRecorder) CountTLSResult(_ context.Context, res store.TLSResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, res)
	return nil
}

func (r *tlsRecorder) of(domain string) []store.TLSResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []store.TLSResult
	for _, x := range r.list {
		if x.Domain == domain {
			out = append(out, x)
		}
	}
	return out
}
