package queue

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/rqlite"
	"github.com/robinmail/robinmail/internal/smtp"
	"github.com/robinmail/robinmail/internal/store"
)

// Nothing in these tests asks a real name server or connects anywhere but
// 127.0.0.1: DNS is testDNS, and connections go through testNet, which
// only knows the addresses a test gave it.

// testDNS is a Resolver with fixed answers. A name it does not know does
// not exist.
type testDNS struct {
	mu      sync.Mutex
	mx      map[string][]*net.MX
	ip      map[string][]string
	txt     map[string][]string
	broken  map[string]bool // names whose lookup fails for now
	lookups []string
}

func newDNS() *testDNS {
	return &testDNS{mx: map[string][]*net.MX{}, ip: map[string][]string{}, txt: map[string][]string{}, broken: map[string]bool{}}
}

func noSuch(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (d *testDNS) note(kind, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lookups = append(d.lookups, kind+" "+name)
	if d.broken[name] {
		return &net.DNSError{Err: "server misbehaving", Name: name, IsTemporary: true}
	}
	return nil
}

func (d *testDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if err := d.note("MX", name); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if recs, ok := d.mx[name]; ok {
		return append([]*net.MX(nil), recs...), nil
	}
	return nil, noSuch(name)
}

func (d *testDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if err := d.note("A", host); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []net.IPAddr
	for _, s := range d.ip[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	if len(out) == 0 {
		return nil, noSuch(host)
	}
	return out, nil
}

func (d *testDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if err := d.note("TXT", name); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if recs, ok := d.txt[name]; ok {
		return recs, nil
	}
	return nil, noSuch(name)
}

func (d *testDNS) count(prefix string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, l := range d.lookups {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// testNet maps the addresses DNS hands out (documentation addresses, RFC
// 5737 and RFC 3849) to listeners on 127.0.0.1. An address without a
// listener refuses the connection.
type testNet struct {
	mu    sync.Mutex
	to    map[string]string // "192.0.2.1:25" -> "127.0.0.1:port"
	dials []string
}

func newNet() *testNet { return &testNet{to: map[string]string{}} }

func (n *testNet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	n.dials = append(n.dials, address)
	real, ok := n.to[address]
	n.mu.Unlock()
	if !ok {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, real)
}

func (n *testNet) dialed() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.dials...)
}

func (n *testNet) route(address, real string) {
	n.mu.Lock()
	n.to[address] = real
	n.mu.Unlock()
}

func (n *testNet) unroute(address string) {
	n.mu.Lock()
	delete(n.to, address)
	n.mu.Unlock()
}

// testCA signs certificates for the test's mail and web servers.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) *testCA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "robinmail test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert, key, pool}
}

// issue makes a certificate for the names, valid around now unless
// expired is set.
func (ca *testCA) issue(t *testing.T, expired bool, names ...string) *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if expired {
		tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// mx is a mail server of another domain: our own smtp.Server with a
// handler that records what it gets.
type mx struct {
	name string
	addr string
	srv  *smtp.Server
	// rcpt decides about a recipient; nil accepts everyone.
	rcpt func(to string) error
	// slow makes every delivery take that long.
	slow time.Duration

	mu     sync.Mutex
	conns  int
	active int
	peak   int
	envs   []smtp.Envelope
	msgs   [][]byte
}

func (m *mx) Rcpt(_ context.Context, _ *smtp.Envelope, to string) error {
	if m.rcpt != nil {
		return m.rcpt(to)
	}
	return nil
}

func (m *mx) Deliver(_ context.Context, env *smtp.Envelope, msg []byte) error {
	m.mu.Lock()
	m.active++
	m.peak = max(m.peak, m.active)
	m.mu.Unlock()
	time.Sleep(m.slow)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active--
	m.envs = append(m.envs, *env)
	m.msgs = append(m.msgs, bytes.Clone(msg))
	return nil
}

func (m *mx) got() ([]smtp.Envelope, [][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]smtp.Envelope(nil), m.envs...), append([][]byte(nil), m.msgs...)
}

func (m *mx) connections() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns
}

type countingListener struct {
	net.Listener
	m *mx
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.m.mu.Lock()
		l.m.conns++
		l.m.mu.Unlock()
	}
	return c, err
}

// startMX runs a mail server on 127.0.0.1. tc is its certificate; nil
// offers no STARTTLS.
func startMX(t *testing.T, name string, tc *tls.Config) *mx {
	t.Helper()
	m := &mx{name: name}
	m.srv = &smtp.Server{Hostname: name, Handler: m, TLSConfig: tc, MaxConnsPerIP: 1000}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m.addr = l.Addr().String()
	go m.srv.Serve(countingListener{l, m})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		m.srv.Shutdown(ctx)
	})
	return m
}

// localFake is the Local of a Worker: mailboxes on this server.
type localFake struct {
	mu        sync.Mutex
	domains   map[string]bool
	unknown   map[string]bool
	delivered []string // "from -> to"
}

func (l *localFake) LocalDomain(_ context.Context, domain string) (bool, error) {
	return l.domains[domain], nil
}

func (l *localFake) Local(_ context.Context, from, to string, msg []byte) error {
	if l.unknown[to] {
		return &smtp.Error{Code: 550, Enhanced: "5.1.1", Msg: "No such user"}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.delivered = append(l.delivered, from+" -> "+to)
	return nil
}

// world is everything a queue test needs: a store on the real rqlite, an
// account that sends, DNS, a network, a clock and a Worker.
type world struct {
	t      *testing.T
	st     *store.Store
	acct   store.Account
	domain string // of the sender; the worker only takes this domain's rows
	dns    *testDNS
	net    *testNet
	ca     *testCA
	local  *localFake
	w      *Worker

	mu    sync.Mutex
	clock time.Time
	next  int
}

var ctx = context.Background()

func (x *world) now() time.Time {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.clock
}

func (x *world) advance(d time.Duration) {
	x.mu.Lock()
	x.clock = x.clock.Add(d)
	x.mu.Unlock()
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	db, err := rqlite.New(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	master, err := blob.NewMasterKey(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(db, blob.NewLocal(t.TempDir()), master)
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func newWorld(t *testing.T) *world {
	t.Helper()
	x := &world{t: t, st: openStore(t), dns: newDNS(), net: newNet(), ca: newCA(t), clock: time.Now().Truncate(time.Second),
		local: &localFake{domains: map[string]bool{}, unknown: map[string]bool{}}}
	x.st.Now = x.now
	var r [6]byte
	rand.Read(r[:])
	x.domain = "q" + hex.EncodeToString(r[:]) + ".test"
	if err := x.st.AddDomain(ctx, x.domain); err != nil {
		t.Fatal(err)
	}
	acct, err := x.st.CreateAccount(ctx, "mike@"+x.domain, "$argon2id$test")
	if err != nil {
		t.Fatal(err)
	}
	x.acct = acct
	// The database outlives the test: leave no row behind that a server
	// started on it later would find in its queue.
	t.Cleanup(func() { x.st.PurgeSubmissions(ctx, acct.ID) })
	x.local.domains[x.domain] = true
	x.w = x.worker("n1")
	return x
}

// worker makes another node working the same queue.
func (x *world) worker(node string) *Worker {
	short := 5 * time.Second
	w := &Worker{Hostname: "mail." + x.domain, NodeID: node, Store: x.st, Local: x.local, Resolver: x.dns, Dial: x.net.dial,
		RootCAs: x.ca.pool, Domains: []string{x.domain}, Now: x.now,
		Timeouts: smtp.Timeouts{Greeting: short, Mail: short, Rcpt: short, DataInit: short, DataBlock: short, DataDone: short, Other: short}}
	x.t.Cleanup(w.Close)
	return w
}

// domainMX gives a recipient domain one mail server: an MX record, an
// address for it, and a route from that address to the server.
func (x *world) domainMX(domain string, m *mx, pref uint16) string {
	x.mu.Lock()
	x.next++
	ip := fmt.Sprintf("192.0.2.%d", x.next)
	x.mu.Unlock()
	x.dns.mu.Lock()
	x.dns.mx[domain] = append(x.dns.mx[domain], &net.MX{Host: m.name + ".", Pref: pref})
	x.dns.ip[m.name] = append(x.dns.ip[m.name], ip)
	x.dns.mu.Unlock()
	x.net.route(ip+":25", m.addr)
	return ip
}

// send queues a message from the account, as a submission over SMTP.
func (x *world) send(subject string, rcpts ...string) store.Submission {
	x.t.Helper()
	return x.sendFrom(x.acct.Address, subject, rcpts...)
}

func (x *world) sendFrom(from, subject string, rcpts ...string) store.Submission {
	x.t.Helper()
	x.mu.Lock()
	x.next++
	id := fmt.Sprintf("m%d.%d@%s", x.next, time.Now().UnixNano(), x.domain)
	x.mu.Unlock()
	raw := "From: Mike <" + x.acct.Address + ">\r\nTo: " + strings.Join(rcpts, ", ") + "\r\nSubject: " + subject +
		"\r\nMessage-ID: <" + id + ">\r\n\r\nThe body of " + subject + ".\r\n"
	sub, err := x.st.Submit(ctx, x.acct.ID, store.NewSubmission{Raw: []byte(raw), MailFrom: from, Rcpts: rcpts})
	if err != nil {
		x.t.Fatal(err)
	}
	return sub
}

// run has the worker do everything that is due now.
func (x *world) run() int {
	x.t.Helper()
	n, err := x.w.RunOnce(ctx)
	if err != nil {
		x.t.Fatal(err)
	}
	return n
}

func (x *world) sub(id int64) store.Submission {
	x.t.Helper()
	s, err := x.st.Submission(ctx, x.acct.ID, id)
	if err != nil {
		x.t.Fatal(err)
	}
	return s
}

// inbox returns the raw messages in the sender's Inbox: where bounces go.
func (x *world) inbox() [][]byte {
	x.t.Helper()
	emails, err := x.st.Emails(ctx, x.acct.ID, 1)
	if err != nil {
		x.t.Fatal(err)
	}
	var out [][]byte
	for _, e := range emails {
		raw, _, err := x.st.Raw(ctx, x.acct.ID, e.ID)
		if err != nil {
			x.t.Fatal(err)
		}
		out = append(out, raw)
	}
	return out
}
