package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/config"
	"github.com/mikepage/robinmail/internal/queue"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
)

// TestMain keeps the outbound worker of every "serve" in these tests away
// from the rest of the world. The tests share one database with each other
// and with the tests of other packages, which run at the same time: a
// worker that took any row would deliver other tests' mail. So by default
// a worker here takes rows of a domain nobody has, and cannot look up or
// connect to anything. A test that sends sets queueSetup itself.
func TestMain(m *testing.M) {
	queueSetup = idleQueue
	os.Exit(m.Run())
}

func idleQueue(w *queue.Worker) {
	net := &sendNet{}
	w.Domains = []string{"no-senders-here.invalid"}
	w.Resolver = net
	w.Dial = net.dial
}

// sendNet is the DNS and the network between the servers of a test:
// fixed answers, and connections only to listeners the test named.
// Nothing here reaches a real name server or a real host.
type sendNet struct {
	mu    sync.Mutex
	mx    map[string][]*net.MX
	ip    map[string]string // host -> address (a documentation address)
	txt   map[string][]string
	to    map[string]string // "192.0.2.1:25" -> "127.0.0.1:port"
	dials map[string]int
}

func newSendNet() *sendNet {
	return &sendNet{mx: map[string][]*net.MX{}, ip: map[string]string{}, txt: map[string][]string{}, to: map[string]string{}, dials: map[string]int{}}
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (n *sendNet) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if recs, ok := n.mx[name]; ok {
		return recs, nil
	}
	return nil, notFound(name)
}

func (n *sendNet) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ip, ok := n.ip[host]; ok {
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}
	return nil, notFound(host)
}

func (n *sendNet) LookupTXT(_ context.Context, name string) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if recs, ok := n.txt[name]; ok {
		return recs, nil
	}
	return nil, notFound(name)
}

func (n *sendNet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	if n.dials != nil {
		n.dials[address]++
	}
	real, ok := n.to[address]
	n.mu.Unlock()
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, real)
}

func (n *sendNet) dialed(address string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials[address]
}

// host gives a domain a mail server: MX record, address, and the listener
// behind the address ("" for an address nobody listens on).
func (n *sendNet) host(domain, mxHost, ip, listener string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.mx[domain] = []*net.MX{{Host: mxHost + ".", Pref: 10}}
	n.ip[mxHost] = ip
	if listener != "" {
		n.to[ip+":25"] = listener
	}
}

// instance is one robinmail server of a test: its own directory and blobs,
// its own domain, and a "serve" running in this process.
type instance struct {
	*cli
	t          *testing.T
	domain     string
	host       string
	pool       *x509.CertPool // trusts the server's certificate
	smtp       string         // port 25
	submission string         // 587, STARTTLS
	implicit   string         // 465
	web        string
}

// startInstance makes a server for a new domain on the rqlite at base and
// starts it. setup configures its outbound worker.
func startInstance(t *testing.T, base, name string, setup func(*queue.Worker, *instance)) *instance {
	t.Helper()
	return startInstanceWith(t, base, name, "", setup)
}

// startInstanceWith is startInstance with more members for the config,
// each followed by a comma.
func startInstanceWith(t *testing.T, base, name, extra string, setup func(*queue.Worker, *instance)) *instance {
	t.Helper()
	in := &instance{t: t, domain: name + "-" + randomHex() + ".test"}
	in.host = "mail." + in.domain
	dir := t.TempDir()
	cert, key, pool := selfSigned(t, dir, in.host)
	in.pool = pool
	cfg := filepath.Join(dir, "robinmail.json")
	body := fmt.Sprintf(`{"hostname":%q,"data_dir":"data","master_key_file":"master.key","rqlite":{"url":%q},`+extra+`
		"tls":{"cert_file":%q,"key_file":%q},
		"smtp":{"listen":"127.0.0.1:0"},
		"submission":{"listen":"127.0.0.1:0","listen_tls":"127.0.0.1:0","undo_seconds":0},
		"web":{"http_listen":"127.0.0.1:0"}}`, in.host, base, cert, key)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	in.cli = &cli{t: t, dir: dir, cfg: cfg}
	in.ok("", "key", "generate")
	in.ok("", "domain", "add", in.domain)

	queueSetup = func(w *queue.Worker) {
		short := 5 * time.Second
		w.Domains = []string{in.domain}
		w.Interval = 50 * time.Millisecond
		w.Timeouts = smtp.Timeouts{Greeting: short, Mail: short, Rcpt: short, DataInit: short, DataBlock: short, DataDone: short, Other: short}
		setup(w, in)
	}
	onSubmissionListen = func(plain, implicit net.Addr) { in.submission, in.implicit = plain.String(), implicit.String() }
	t.Cleanup(func() { queueSetup, onSubmissionListen = idleQueue, nil })
	srv := startServe(t, in.cli)
	in.smtp, in.web = srv.addr, srv.web
	queueSetup = idleQueue
	return in
}

func (in *instance) user(local string) string {
	in.t.Helper()
	addr := local + "@" + in.domain
	in.ok("correct horse\n", "user", "add", addr)
	// The database outlives the test: what the user still has queued at
	// the end is removed, so that no server started on it later finds it.
	in.t.Cleanup(func() { in.purge(addr) })
	return addr
}

func (in *instance) purge(addr string) {
	cfg, err := config.Load(in.cfg)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := open(ctx, cfg, false)
	if err != nil {
		return
	}
	if acct, err := a.store.Account(ctx, addr); err == nil {
		a.store.PurgeSubmissions(ctx, acct.ID)
	}
}

// purgeSystem removes what the system account of the instance's domain
// (forwards, bounces on their way back) still has queued.
func (in *instance) purgeSystem() {
	cfg, err := config.Load(in.cfg)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if a, err := open(ctx, cfg, false); err == nil {
		a.store.PurgeSubmissions(ctx, store.SystemAccountID(in.domain))
	}
}

// inbox returns the listing of a user's Inbox.
func (in *instance) inbox(user string) string {
	in.t.Helper()
	return in.ok("", "list", user)
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *smtpClient) startTLS() {
	c.t.Helper()
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		c.t.Fatal(err)
	}
	c.conn, c.r = tc, bufio.NewReader(tc)
}

func plainAuth(user, password string) string {
	return "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00"+user+"\x00"+password))
}

// jmapSession logs a user in over the web listener and returns a client
// and the account id.
func jmapSession(t *testing.T, in *instance, user string) (*jmapClient, string) {
	t.Helper()
	c := newJMAPClient(t, "http://"+in.web)
	resp, data := c.do("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"correct horse"}`, user), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %s", resp.StatusCode, data)
	}
	var who struct {
		AccountID string `json:"accountId"`
	}
	json.Unmarshal(data, &who)
	return c, who.AccountID
}

// call posts method calls with the submission capability.
func (c *jmapClient) call(calls string) []any {
	c.t.Helper()
	body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],"methodCalls":[` + calls + `]}`
	c.t.Logf("C: %s", compact(body))
	resp, data := c.do("POST", "/jmap/api", body, nil)
	c.t.Logf("S: %d %s", resp.StatusCode, clipLog(string(data)))
	if resp.StatusCode != 200 {
		c.t.Fatalf("API status %d: %s", resp.StatusCode, data)
	}
	var out struct {
		MethodResponses []any `json:"methodResponses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		c.t.Fatal(err)
	}
	return out.MethodResponses
}

// TestSendBetweenTwoServers is the exit of phase 5: two robinmail servers
// with a domain each, each on a database of its own, and between them
// only DNS (a test resolver) and SMTP on loopback.
//
//   - Anna on server A submits a message over SMTP on port 587, with
//     STARTTLS and AUTH. A's queue looks up B's MX, connects, and B stores
//     it for Bob.
//   - Bob replies over JMAP: an Email in Drafts, an EmailSubmission, and on
//     success a move to Sent. B's queue delivers to A.
//   - Mail to an address B does not have comes back to Anna as a bounce at
//     once; mail for a colleague at A's own domain never leaves A.
//   - With an MTA-STS policy in mode enforce, mail goes to a server whose
//     certificate is valid for its name and not to one whose is not.
//
// It needs two rqlite databases:
//
//	docker compose -f contrib/compose/compose.yaml up -d
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RQLITE_B=http://127.0.0.1:4003 \
//	  go test -v -run TestSendBetweenTwoServers ./cmd/robinmail
func TestSendBetweenTwoServers(t *testing.T) {
	baseA, baseB := os.Getenv("ROBINMAIL_TEST_RQLITE"), os.Getenv("ROBINMAIL_TEST_RQLITE_B")
	if baseA == "" || baseB == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE and ROBINMAIL_TEST_RQLITE_B are not both set")
	}
	world := newSendNet()
	var a, b *instance
	// The policy host of MTA-STS: one HTTPS server for every domain, with
	// a certificate the workers trust for those names.
	policies := map[string]string{}
	var policyMu sync.Mutex
	policySrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policyMu.Lock()
		body, ok := policies[r.Host]
		policyMu.Unlock()
		if !ok || r.URL.Path != "/.well-known/mta-sts.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(body))
	}))
	setup := func(w *queue.Worker, in *instance) {
		w.Resolver = world
		w.Dial = world.dial
		// The roots a worker trusts: the certificate of B's mail server,
		// and the policy host's.
		roots := x509.NewCertPool()
		if b != nil {
			roots = b.pool.Clone()
		}
		w.RootCAs = roots
		w.HTTP = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the policy host is this test's own
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, policySrv.Listener.Addr().String())
			}}}
	}
	// B first, so that A's worker can be given B's certificate to trust.
	b = startInstance(t, baseB, "b", setup)
	a = startInstance(t, baseA, "a", setup)
	policySrv.StartTLS()
	defer policySrv.Close()
	world.host(a.domain, a.host, "192.0.2.1", a.smtp)
	world.host(b.domain, b.host, "192.0.2.2", b.smtp)
	anna, carol, bob := a.user("anna"), a.user("carol"), b.user("bob")

	// --- 1. Anna submits over SMTP on 587. ---
	s := dialSMTP(t, a.submission)
	ehlo := s.cmd("EHLO annas-laptop.home.arpa", 250)
	if strings.Contains(ehlo, "AUTH") || !strings.Contains(ehlo, "STARTTLS") {
		t.Errorf("EHLO before TLS: %q", ehlo)
	}
	s.cmd("MAIL FROM:<"+anna+">", 530) // RFC 3207: STARTTLS first
	s.cmd("STARTTLS", 220)
	s.startTLS()
	if ehlo := s.cmd("EHLO annas-laptop.home.arpa", 250); !strings.Contains(ehlo, "AUTH PLAIN LOGIN") {
		t.Errorf("EHLO inside TLS: %q", ehlo)
	}
	s.cmd("MAIL FROM:<"+anna+">", 530) // RFC 4954: AUTH first
	s.cmd(plainAuth(anna, "wrong password"), 535)
	s.cmd(plainAuth(anna, "correct horse"), 235)
	s.cmd("MAIL FROM:<"+bob+">", 550) // not her address
	// Her message has no Date and no Message-ID: the server adds them.
	s.cmd("MAIL FROM:<"+anna+">", 250)
	s.cmd("RCPT TO:<"+bob+">", 250)
	s.cmd("DATA", 354)
	s.conn.Write([]byte("From: Anna <" + anna + ">\r\nTo: Bob <" + bob + ">\r\nBcc: " + carol + "\r\nSubject: Lunch on Tuesday?\r\n\r\nHi Bob, are you free on Tuesday?\r\n.\r\n"))
	s.read(250)
	// From someone else in the header: refused after DATA.
	s.cmd("MAIL FROM:<"+anna+">", 250)
	s.cmd("RCPT TO:<"+bob+">", 250)
	s.cmd("DATA", 354)
	s.conn.Write([]byte("From: The Boss <boss@" + a.domain + ">\r\nSubject: Do as I say\r\n\r\nx\r\n.\r\n"))
	if reply := s.read(550); !strings.HasPrefix(reply, "550 5.7.1 ") {
		t.Errorf("forged From: %q", reply)
	}
	s.cmd("QUIT", 221)

	waitFor(t, "Anna's message in Bob's Inbox", func() bool { return strings.Contains(b.inbox(bob), "Lunch on Tuesday?") })
	got := b.ok("", "cat", bob, "e1")
	t.Logf("in Bob's Inbox on server B:\n%s", strings.ReplaceAll(got, "\r\n", "\n"))
	b.expect(got,
		// B's own lines: who it came from on the envelope, and from which host.
		`^Return-Path: <`+regexp.QuoteMeta(anna)+`>\r\nReceived: from `+regexp.QuoteMeta(a.host)+` \((localhost )?\[127\.0\.0\.1\]\)\r\n\tby `+regexp.QuoteMeta(b.host)+` \(robinmail\) with ESMTPS id `,
		// A's line from the submission: no name and no address of Anna's computer.
		`\r\nReceived: by `+regexp.QuoteMeta(a.host)+` \(robinmail\) with ESMTPSA id [0-9A-F]{16}\r\n\t\(TLS 1\.3 [A-Z0-9_]+\);\r\n`,
		`\r\nDate: [A-Z][a-z]{2}, \d+ [A-Z][a-z]{2} \d{4} \d\d:\d\d:\d\d [+-]\d{4}\r\n`,
		`\r\nMessage-ID: <[0-9a-f]{32}@`+regexp.QuoteMeta(a.host)+`>\r\n`,
		`\r\n\r\nHi Bob, are you free on Tuesday\?\r\n$`)
	for _, leak := range []string{"annas-laptop", "Bcc", carol} {
		if strings.Contains(got, leak) {
			t.Errorf("what Bob got has %q", leak)
		}
	}
	messageID := regexp.MustCompile(`Message-ID: <([^>]+)>`).FindStringSubmatch(got)[1]

	// --- 2. Bob replies over JMAP. ---
	jb, bobAcct := jmapSession(t, b, bob)
	session := jb.call(`["Identity/get", {"accountId": "` + bobAcct + `", "ids": null}, "0"]`)
	if email := dig(t, session, 0, 1, "list", 0, "email"); email != bob {
		t.Fatalf("Bob's identity is %v", email)
	}
	// One request: the draft, the submission, and the move to Sent.
	res := jb.call(`["Email/set", {"accountId": "` + bobAcct + `", "create": {"draft": {
			"mailboxIds": {"m2": true}, "keywords": {"$draft": true, "$seen": true},
			"from": [{"name": "Bob", "email": "` + bob + `"}], "to": [{"name": "Anna", "email": "` + anna + `"}],
			"subject": "Re: Lunch on Tuesday?", "inReplyTo": ["` + messageID + `"], "references": ["` + messageID + `"],
			"textBody": [{"partId": "t", "type": "text/plain"}],
			"bodyValues": {"t": {"value": "Tuesday is fine. Café Zoë at noon?\n"}}}}}, "0"],
		["EmailSubmission/set", {"accountId": "` + bobAcct + `", "create": {"send": {"identityId": "i1", "emailId": "#draft"}},
			"onSuccessUpdateEmail": {"#send": {"mailboxIds/m2": null, "mailboxIds/m3": true, "keywords/$draft": null}}}, "1"]`)
	if dig(t, res, 1, 1, "created") == nil || dig(t, res, 2, 0) != "Email/set" {
		t.Fatalf("the reply was not submitted: %v", res)
	}
	subID := dig(t, res, 1, 1, "created", "send", "id").(string)
	replyID := dig(t, res, 0, 1, "created", "draft", "id").(string)
	// The reply is in the thread of the message it answers.
	if dig(t, res, 0, 1, "created", "draft", "threadId") != "t1" {
		t.Errorf("the reply is in thread %v", dig(t, res, 0, 1, "created", "draft", "threadId"))
	}
	waitFor(t, "Bob's reply in Anna's Inbox", func() bool { return strings.Contains(a.inbox(anna), "Re: Lunch on Tuesday?") })
	reply := a.ok("", "cat", anna, "e1")
	t.Logf("in Anna's Inbox on server A:\n%s", strings.ReplaceAll(reply, "\r\n", "\n"))
	a.expect(reply,
		`^Return-Path: <`+regexp.QuoteMeta(bob)+`>\r\nReceived: from `+regexp.QuoteMeta(b.host)+` `,
		`\r\nReceived: by `+regexp.QuoteMeta(b.host)+` \(robinmail\) \(JMAP\) id `,
		`\r\nIn-Reply-To: <`+regexp.QuoteMeta(messageID)+`>\r\n`,
		`\r\nContent-Transfer-Encoding: quoted-printable\r\n`,
		`Caf=C3=A9 Zo=C3=AB at noon\?`)
	// What JMAP says about the submission once it is out: no undo any
	// more, and B knows that A took the message, nothing beyond that.
	var status any
	waitFor(t, "the delivery status of the reply", func() bool {
		st := jb.call(`["EmailSubmission/get", {"accountId": "` + bobAcct + `", "ids": ["` + subID + `"]}, "0"]`)
		status = dig(t, st, 0, 1, "list", 0)
		return dig(t, status, "deliveryStatus", anna, "delivered") == "unknown"
	})
	if dig(t, status, "undoStatus") != "final" || !strings.HasPrefix(dig(t, status, "deliveryStatus", anna, "smtpReply").(string), "250 2.0.0 ") {
		t.Errorf("submission after delivery: %v", status)
	}
	sent := jb.call(`["Email/get", {"accountId": "` + bobAcct + `", "ids": ["` + replyID + `"], "properties": ["mailboxIds", "keywords"]}, "0"]`)
	if m := dig(t, sent, 0, 1, "list", 0, "mailboxIds").(map[string]any); len(m) != 1 || m["m3"] != true {
		t.Errorf("the reply is in %v, want Sent", m)
	}

	// --- 3. A recipient B does not have: a bounce at once. ---
	s = dialSMTP(t, a.implicitTLS(t))
	s.cmd("EHLO annas-laptop.home.arpa", 250)
	s.cmd(plainAuth(anna, "correct horse"), 235)
	s.send(anna, "nobody@"+b.domain, "From: "+anna+"\r\nTo: nobody@"+b.domain+"\r\nSubject: Anyone there?\r\nMessage-ID: <lost-1@"+a.domain+">\r\n\r\nHello?\r\n", 250)
	waitFor(t, "the bounce in Anna's Inbox", func() bool { return strings.Contains(a.inbox(anna), "Undelivered Mail Returned to Sender") })
	bounce := a.ok("", "cat", anna, "e2")
	t.Logf("the bounce in Anna's Inbox:\n%s", strings.ReplaceAll(bounce, "\r\n", "\n"))
	a.expect(bounce,
		`^Return-Path: <>\r\n`,
		`\r\nContent-Type: multipart/report; report-type=delivery-status;`,
		`\r\nFinal-Recipient: rfc822; nobody@`+regexp.QuoteMeta(b.domain)+`\r\nAction: failed\r\nStatus: 5\.1\.1\r\nRemote-MTA: dns; `+regexp.QuoteMeta(b.host)+`\r\nDiagnostic-Code: smtp; 550 5\.1\.1 No such user\r\n`,
		`\r\nMessage-ID: <lost-1@`+regexp.QuoteMeta(a.domain)+`>\r\n`)
	if n := strings.Count(b.inbox(bob), "\ne"); n != 1 {
		t.Errorf("Bob's Inbox has %d emails after the bounce", n)
	}

	// --- 4. A colleague at A's own domain: delivered on A, no SMTP. ---
	before := world.dialed("192.0.2.1:25")
	ja, annaAcct := jmapSession(t, a, anna)
	res = ja.call(`["Email/set", {"accountId": "` + annaAcct + `", "create": {"d": {"mailboxIds": {"m2": true},
			"from": [{"name": null, "email": "` + anna + `"}], "to": [{"name": null, "email": "` + carol + `"}], "subject": "Coffee?",
			"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "Now?"}}}}}, "0"],
		["EmailSubmission/set", {"accountId": "` + annaAcct + `", "create": {"s": {"identityId": "i1", "emailId": "#d"}}, "onSuccessDestroyEmail": ["#s"]}, "1"]`)
	localSub := dig(t, res, 1, 1, "created", "s", "id").(string)
	waitFor(t, "the message in Carol's Inbox", func() bool { return strings.Contains(a.inbox(carol), "Coffee?") })
	waitFor(t, "its delivery status", func() bool {
		st := ja.call(`["EmailSubmission/get", {"accountId": "` + annaAcct + `", "ids": ["` + localSub + `"]}, "0"]`)
		return dig(t, st, 0, 1, "list", 0, "deliveryStatus", carol, "delivered") == "yes"
	})
	if world.dialed("192.0.2.1:25") != before {
		t.Error("mail for A's own domain went through SMTP")
	}
	a.expect(a.ok("", "cat", carol, "e1"), `^Return-Path: <`+regexp.QuoteMeta(anna)+`>\r\nReceived: by `+regexp.QuoteMeta(a.host)+` \(robinmail\) \(JMAP\)`)

	// --- 5. MTA-STS (RFC 8461), mode enforce. ---
	// B publishes a policy that names its mail server. A trusts B's
	// certificate for that name: delivered, as before.
	policyMu.Lock()
	policies["mta-sts."+b.domain] = "version: STSv1\nmode: enforce\nmx: " + b.host + "\nmax_age: 86400\n"
	// Another domain of B's owner, whose MX is the same machine under a
	// name its certificate is not valid for.
	other := "other-" + b.domain
	policies["mta-sts."+other] = "version: STSv1\nmode: enforce\nmx: mx." + other + "\nmax_age: 86400\n"
	policyMu.Unlock()
	world.mu.Lock()
	world.txt["_mta-sts."+b.domain] = []string{"v=STSv1; id=" + randomHex()}
	world.txt["_mta-sts."+other] = []string{"v=STSv1; id=" + randomHex()}
	world.mu.Unlock()
	world.host(other, "mx."+other, "192.0.2.3", b.smtp)
	b.ok("", "domain", "add", other)
	dave := "dave@" + other
	b.ok("correct horse\n", "user", "add", dave)

	send := func(to, subject string) string {
		t.Helper()
		res := ja.call(`["Email/set", {"accountId": "` + annaAcct + `", "create": {"d": {"mailboxIds": {"m3": true},
				"from": [{"name": null, "email": "` + anna + `"}], "to": [{"name": null, "email": "` + to + `"}], "subject": "` + subject + `",
				"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "x"}}}}}, "0"],
			["EmailSubmission/set", {"accountId": "` + annaAcct + `", "create": {"s": {"identityId": "i1", "emailId": "#d"}}}, "1"]`)
		return dig(t, res, 1, 1, "created", "s", "id").(string)
	}
	delivery := func(sub, to string) (delivered, reply string) {
		st := ja.call(`["EmailSubmission/get", {"accountId": "` + annaAcct + `", "ids": ["` + sub + `"]}, "0"]`)
		d := dig(t, st, 0, 1, "list", 0, "deliveryStatus", to)
		return dig(t, d, "delivered").(string), dig(t, d, "smtpReply").(string)
	}
	good := send(bob, "Under a policy")
	waitFor(t, "delivery under MTA-STS enforce with a valid certificate", func() bool { return strings.Contains(b.inbox(bob), "Under a policy") })
	b.expect(b.ok("", "cat", bob, "e3"), `with ESMTPS id [0-9A-F]+\r\n\t\(TLS 1\.3 `)
	if d, _ := delivery(good, bob); d != "unknown" {
		waitFor(t, "its status", func() bool { d, _ := delivery(good, bob); return d == "unknown" })
	}

	bad := send(dave, "To a server with a bad certificate")
	var why string
	waitFor(t, "an attempt at the server with the bad certificate", func() bool {
		_, why = delivery(bad, dave)
		return why != ""
	})
	t.Logf("MTA-STS enforce, certificate not valid for the MX host: %s", why)
	if d, _ := delivery(bad, dave); d != "queued" || !strings.Contains(why, "MTA-STS") || !strings.Contains(why, "certificate") {
		t.Errorf("delivered=%s, reason %q", d, why)
	}
	time.Sleep(300 * time.Millisecond) // several rounds of the worker
	if strings.Contains(b.ok("", "list", dave), "bad certificate") {
		t.Error("MTA-STS enforce with a bad certificate: the message was delivered")
	}
	// The same server without the policy takes the mail: TLS without
	// verification, as SMTP has always done.
	world.mu.Lock()
	delete(world.txt, "_mta-sts."+other)
	world.mu.Unlock()
	third := "third-" + b.domain
	world.host(third, "mx."+third, "192.0.2.4", b.smtp)
	b.ok("", "domain", "add", third)
	b.ok("correct horse\n", "user", "add", "erin@"+third)
	send("erin@"+third, "No policy here")
	waitFor(t, "delivery to a domain without a policy", func() bool { return strings.Contains(b.ok("", "list", "erin@"+third), "No policy here") })
}

// implicitTLS connects to the 465 listener and returns the address of a
// local relay that speaks plain SMTP to the test's client and TLS to the
// server, so the hand-written client can be used as it is.
func (in *instance) implicitTLS(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			plain, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer plain.Close()
				secure, err := tls.Dial("tcp", in.implicit, &tls.Config{RootCAs: in.pool, ServerName: in.host})
				if err != nil {
					return
				}
				defer secure.Close()
				go func() { copyAll(secure, plain); secure.Close() }()
				copyAll(plain, secure)
			}()
		}
	}()
	return l.Addr().String()
}

func copyAll(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// TestSendRetriesThenBounces runs a server whose queue has a clock the
// test moves. The destination refuses every connection: the message is
// tried again and again, later each time, and after five days it comes
// back to the sender.
func TestSendRetriesThenBounces(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	world := newSendNet()
	var mu sync.Mutex
	offset := time.Duration(0)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return time.Now().Add(offset)
	}
	a := startInstance(t, base, "retry", func(w *queue.Worker, _ *instance) {
		w.Resolver, w.Dial, w.Now = world, world.dial, now
	})
	world.host("down.test", "mx.down.test", "192.0.2.9", "") // nobody listens
	anna := a.user("anna")
	ja, acct := jmapSession(t, a, anna)
	res := ja.call(`["Email/set", {"accountId": "` + acct + `", "create": {"d": {"mailboxIds": {"m3": true},
			"from": [{"name": null, "email": "` + anna + `"}], "to": [{"name": null, "email": "zoe@down.test"}], "subject": "Are you there?",
			"textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "Hello?"}}}}}, "0"],
		["EmailSubmission/set", {"accountId": "` + acct + `", "create": {"s": {"identityId": "i1", "emailId": "#d"}}}, "1"]`)
	sub := dig(t, res, 1, 1, "created", "s", "id").(string)
	status := func() (string, string) {
		st := ja.call(`["EmailSubmission/get", {"accountId": "` + acct + `", "ids": ["` + sub + `"], "properties": ["deliveryStatus"]}, "0"]`)
		d := dig(t, st, 0, 1, "list", 0, "deliveryStatus", "zoe@down.test")
		return dig(t, d, "delivered").(string), dig(t, d, "smtpReply").(string)
	}
	const addr = "192.0.2.9:25"
	waitFor(t, "the first attempt", func() bool { return world.dialed(addr) == 1 })
	waitFor(t, "its result", func() bool { _, reply := status(); return reply != "" })
	if d, reply := status(); d != "queued" || !strings.Contains(reply, "connection refused") {
		t.Fatalf("after the first attempt: %s %q", d, reply)
	}
	// Nothing happens while the clock stands still.
	time.Sleep(300 * time.Millisecond)
	if n := world.dialed(addr); n != 1 {
		t.Fatalf("%d attempts without time passing", n)
	}
	// The schedule: 5 minutes, doubling, 4 hours at most.
	var waits []string
	elapsed := time.Duration(0)
	for try := 1; ; try++ {
		wait := queue.Backoff(try)
		// A minute before the next try is due: still nothing.
		mu.Lock()
		offset += wait - time.Minute
		mu.Unlock()
		time.Sleep(120 * time.Millisecond)
		if n := world.dialed(addr); n != try {
			t.Fatalf("attempt %d came before it was due", n)
		}
		mu.Lock()
		offset += time.Minute + time.Second
		mu.Unlock()
		elapsed += wait + time.Second
		waits = append(waits, wait.String())
		waitFor(t, fmt.Sprintf("attempt %d", try+1), func() bool { return world.dialed(addr) == try+1 })
		if elapsed >= 5*24*time.Hour {
			break
		}
		if try > 60 {
			t.Fatal("no end to the retries")
		}
	}
	t.Logf("%d attempts in %v; waits between them: %s", world.dialed(addr), elapsed.Round(time.Hour), strings.Join(waits, " "))
	waitFor(t, "the bounce", func() bool { return strings.Contains(a.inbox(anna), "Undelivered Mail Returned to Sender") })
	bounce := a.ok("", "cat", anna, "e2")
	a.expect(bounce,
		`^Return-Path: <>\r\n`,
		`\r\nFinal-Recipient: rfc822; zoe@down\.test\r\nAction: failed\r\nStatus: 4\.4\.7\r\n`,
		`Diagnostic-Code: smtp; 451 4\.4\.7 delivery was tried for 120h0m0s without success; the last attempt: 451\r?\n? ?4\.4\.1 connecting to\r?\n? ?mx\.down\.test`,
		`\r\nSubject: Are you there\?\r\n`)
	waitFor(t, "the final status", func() bool { d, _ := status(); return d == "no" })
	// And that was the last of it.
	n := world.dialed(addr)
	mu.Lock()
	offset += 24 * time.Hour
	mu.Unlock()
	time.Sleep(300 * time.Millisecond)
	if world.dialed(addr) != n {
		t.Error("another attempt after the bounce")
	}
}
