package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/store"
)

var ctx = context.Background()

// table is a node table in memory.
type table struct {
	mu    sync.Mutex
	rows  map[string]store.Node
	down  bool
	reads int
}

func (t *table) Nodes(context.Context) ([]store.Node, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads++
	if t.down {
		return nil, errors.New("the database is down")
	}
	var out []store.Node
	for _, n := range t.rows {
		out = append(out, n)
	}
	return out, nil
}

func (t *table) RegisterNode(_ context.Context, n store.Node, running bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if running {
		n.SeenAt = time.Now()
	}
	t.rows[n.ID] = n
	return nil
}

func (t *table) Heartbeat(_ context.Context, id, cert string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, ok := t.rows[id]
	if !ok || n.Cert != cert {
		return false, nil
	}
	n.SeenAt = time.Now()
	t.rows[id] = n
	return true, nil
}

var testKey = bytes.Repeat([]byte{9}, blob.KeySize)

func verifier(_ context.Context, key string, r io.Reader) error {
	account, sum, err := blob.SplitKey(key)
	if err != nil {
		return err
	}
	plain, err := blob.Decrypt(testKey, account, sum, io.NopCloser(r))
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, plain); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return blob.ErrCorrupt
	}
	return nil
}

func sealed(t *testing.T, content string) (string, []byte) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	name := hex.EncodeToString(sum[:])
	enc, err := blob.Encrypt(testKey, "acct", name, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(enc)
	return blob.Key("acct", name), data
}

// member is a running node: identity, blob directory, listener.
type member struct {
	*Node
	local *blob.Local
	rep   *blob.Replicated
	srv   *http.Server
}

func (m *member) stop() { m.srv.Close() }

// start runs a node on a port of its own and registers it, unless
// register is false.
func start(t *testing.T, dir *table, id string, register bool) *member {
	t.Helper()
	identity, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &member{Node: &Node{ID: id, Address: l.Addr().String(), Identity: identity, Dir: dir, MaxBlobBytes: 1 << 20},
		local: blob.NewLocal(t.TempDir())}
	m.rep = &blob.Replicated{Local: m.local, Peers: m.Node, Verify: verifier, Pause: time.Millisecond}
	m.srv = m.HTTPServer(m.local, verifier)
	go m.srv.ServeTLS(l, "", "")
	t.Cleanup(func() { m.rep.Wait(); m.srv.Close() })
	if register {
		if err := m.Register(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func newTable() *table { return &table{rows: map[string]store.Node{}} }

// forget makes a node read the table again at its next look.
func (m *member) forget() {
	m.mu.Lock()
	m.nodes = nil
	m.mu.Unlock()
}

// call sends one request to a node with the given TLS config.
func call(target *member, conf *tls.Config, method, path string, body []byte) (*http.Response, []byte, error) {
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: conf}, Timeout: 5 * time.Second}
	defer c.CloseIdleConnections()
	req, _ := http.NewRequest(method, "https://"+target.Address+path, bytes.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data, nil
}

// as returns the TLS config a node uses to call target.
func (m *member) as(target *member) *tls.Config { return m.clientTLS(target.Identity.Fingerprint) }

// The peer listener lets in registered nodes and nobody else: not a client
// without a certificate, not one with a certificate that is not in the
// node table, not plain HTTP. What it refuses gets no HTTP answer at all.
func TestUnauthenticatedPeerIsRefused(t *testing.T) {
	dir := newTable()
	a := start(t, dir, "a", true)
	b := start(t, dir, "b", true)
	key, file := sealed(t, "secret mail")
	if err := a.local.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}

	// A registered node is served.
	resp, body, err := call(a, b.as(a), "GET", "/v1/blob/"+key, nil)
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, file) {
		t.Fatalf("registered node: %v %v", resp, err)
	}

	stranger, _ := NewIdentity()
	for name, conf := range map[string]*tls.Config{
		"no client certificate":       {InsecureSkipVerify: true},
		"unknown client certificate":  {InsecureSkipVerify: true, Certificates: []tls.Certificate{stranger.Certificate}},
		"TLS 1.2 without certificate": {InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12},
	} {
		for _, req := range [][2]string{{"GET", "/v1/blob/" + key}, {"PUT", "/v1/blob/" + key}, {"GET", "/v1/ping"}} {
			resp, body, err := call(a, conf, req[0], req[1], file)
			if err == nil {
				t.Errorf("%s: %s %s was answered: %d %q", name, req[0], req[1], resp.StatusCode, body)
			}
		}
	}
	// Plain HTTP on the TLS port.
	resp2, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + a.Address + "/v1/blob/" + key)
	if err == nil {
		body, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		if resp2.StatusCode == 200 || bytes.Contains(body, file[:8]) {
			t.Errorf("plain HTTP was served: %d", resp2.StatusCode)
		}
	}

	// A node that was removed from the table is refused from then on.
	dir.mu.Lock()
	delete(dir.rows, "b")
	dir.mu.Unlock()
	a.forget()
	if resp, _, err := call(a, b.as(a), "GET", "/v1/ping", nil); err == nil {
		t.Errorf("a removed node was answered: %d", resp.StatusCode)
	}

	// The handler refuses by itself too, if it is ever served without the
	// TLS check in front.
	rec := &recorder{header: http.Header{}}
	req, _ := http.NewRequest("GET", "/v1/ping", nil)
	a.Handler(a.local, verifier).ServeHTTP(rec, req)
	if rec.status != http.StatusForbidden {
		t.Errorf("handler without TLS: %d, want 403", rec.status)
	}
}

type recorder struct {
	header http.Header
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(s int)           { r.status = s }

// The calling side checks too: it only talks to the server whose
// certificate the node table has for that node.
func TestImpostorServerIsRefused(t *testing.T) {
	dir := newTable()
	a := start(t, dir, "a", true)
	b := start(t, dir, "b", true)
	impostor := start(t, newTable(), "b", false) // has b's name, not b's key
	// The table says b lives at the impostor's address.
	dir.mu.Lock()
	row := dir.rows["b"]
	row.Address = impostor.Address
	dir.rows["b"] = row
	dir.mu.Unlock()
	a.forget()
	_ = b

	key, file := sealed(t, "not for you")
	a.local.Put(ctx, key, bytes.NewReader(file))
	peers, err := a.Peers(ctx)
	if err != nil || len(peers) != 1 {
		t.Fatalf("Peers = %v, %v", peers, err)
	}
	if err := peers[0].Push(ctx, key, int64(len(file)), bytes.NewReader(file)); err == nil {
		t.Error("pushed a blob to a server with the wrong certificate")
	}
	if impostor.local.Has(key) {
		t.Error("the impostor has the file")
	}
	if _, err := peers[0].Fetch(ctx, key); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("fetch from the impostor: %v", err)
	}
}

// A node that registers after the others have read the table is let in:
// an unknown certificate makes a node look again.
func TestNewNodeIsLetIn(t *testing.T) {
	dir := newTable()
	a := start(t, dir, "a", true)
	if n, _ := a.Members(ctx); n != 1 {
		t.Fatalf("Members = %d", n)
	}
	time.Sleep(listMinAge + 100*time.Millisecond) // a's copy of the table is not brand new
	b := start(t, dir, "b", true)
	if resp, _, err := call(a, b.as(a), "GET", "/v1/ping", nil); err != nil || resp.StatusCode != 204 {
		t.Fatalf("new node: %v %v", resp, err)
	}
	// Strangers do not make a node read the table for every attempt.
	stranger, _ := NewIdentity()
	conf := &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{stranger.Certificate}}
	dir.mu.Lock()
	before := dir.reads
	dir.mu.Unlock()
	for i := 0; i < 20; i++ {
		call(a, conf, "GET", "/v1/ping", nil)
	}
	dir.mu.Lock()
	defer dir.mu.Unlock()
	if dir.reads-before > 3 {
		t.Errorf("20 refused handshakes cost %d reads of the node table", dir.reads-before)
	}
}

// The protocol of the package comment, status by status.
func TestProtocol(t *testing.T) {
	dir := newTable()
	a := start(t, dir, "a", true)
	b := start(t, dir, "b", true)
	conf := b.as(a)
	key, file := sealed(t, "wire format")
	sum := key[len("acct/"):]

	status := func(method, path string, body []byte) (int, []byte, http.Header) {
		t.Helper()
		resp, data, err := call(a, conf, method, path, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp.StatusCode, data, resp.Header
	}

	if code, _, _ := status("GET", "/v1/ping", nil); code != 204 {
		t.Errorf("ping: %d", code)
	}
	if code, _, _ := status("GET", "/v1/blob/"+key, nil); code != 404 {
		t.Errorf("GET of a blob the node lacks: %d", code)
	}
	if code, body, _ := status("PUT", "/v1/blob/"+key, file); code != 204 || len(body) != 0 {
		t.Errorf("PUT: %d %q", code, body)
	}
	// What is on the receiver's disk is the file as it was sent.
	path, _ := a.local.Path(key)
	if got, _ := os.ReadFile(path); !bytes.Equal(got, file) {
		t.Error("the stored file differs from the pushed one")
	}
	code, body, h := status("GET", "/v1/blob/"+key, nil)
	if code != 200 || !bytes.Equal(body, file) || h.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("GET: %d, %d bytes, %q", code, len(body), h.Get("Content-Type"))
	}
	// A second PUT of the same blob replaces the file.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	if code, _, _ := status("PUT", "/v1/blob/"+key, file); code != 204 {
		t.Errorf("second PUT: %d", code)
	}
	if fi, _ := os.Stat(path); time.Since(fi.ModTime()) > time.Minute {
		t.Error("the second PUT did not renew the file")
	}

	// 422: not that blob.
	other, otherFile := sealed(t, "something else")
	damaged := bytes.Clone(file)
	damaged[40] ^= 1
	for name, body := range map[string][]byte{"damaged": damaged, "cut short": file[:len(file)-1], "empty": {},
		"not a blob file": []byte("hello"), "another blob's file": otherFile} {
		target := "/v1/blob/acct/" + strings.Repeat("ab", 32)
		if name == "damaged" || name == "cut short" {
			target = "/v1/blob/" + other // and nothing may land under that name
		}
		if code, _, _ := status("PUT", target, body); code != 422 {
			t.Errorf("PUT %s: %d, want 422", name, code)
		}
	}
	if a.local.Has(other) || a.local.Has(blob.Key("acct", strings.Repeat("ab", 32))) {
		t.Error("a refused file was stored")
	}
	// 400: not a blob name. The path never reaches the disk.
	for _, p := range []string{"/v1/blob/acct/" + sum[:63], "/v1/blob/acct/" + strings.ToUpper(sum), "/v1/blob/a.b/" + sum,
		"/v1/blob/..%2F..%2Fetc/" + sum, "/v1/blob/acct/" + sum[:62] + "%2F."} {
		for _, method := range []string{"GET", "PUT"} {
			if code, _, _ := status(method, p, file); code != 400 {
				t.Errorf("%s %s: %d, want 400", method, p, code)
			}
		}
	}
	// 413: a.MaxBlobBytes is 1 MiB.
	if code, _, _ := status("PUT", "/v1/blob/"+other, make([]byte, 1<<20+1)); code != 413 {
		t.Errorf("PUT too large: %d", code)
	}
	// Everything else.
	for _, r := range [][2]string{{"GET", "/"}, {"GET", "/v1/blob/acct"}, {"GET", "/v1/blob/acct/" + sum + "/x"}, {"GET", "/v2/ping"}} {
		if code, _, _ := status(r[0], r[1], nil); code != 404 {
			t.Errorf("%s %s: %d, want 404", r[0], r[1], code)
		}
	}
	for _, r := range [][2]string{{"DELETE", "/v1/blob/" + key}, {"POST", "/v1/blob/" + key}, {"POST", "/v1/ping"}} {
		if code, _, _ := status(r[0], r[1], nil); code != 405 {
			t.Errorf("%s %s: %d, want 405", r[0], r[1], code)
		}
	}
	if !a.local.Has(key) {
		t.Error("DELETE removed the blob")
	}
}

// Three real nodes: a write on one lands on all, survives one node being
// away, and a node that missed it gets it on a read and through Repair.
func TestThreeNodes(t *testing.T) {
	dir := newTable()
	a, b, c := start(t, dir, "a", true), start(t, dir, "b", true), start(t, dir, "c", true)
	for _, m := range []*member{a, b, c} {
		m.forget()
	}
	key, file := sealed(t, "to everyone")
	if err := a.rep.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	a.rep.Wait()
	for _, m := range []*member{a, b, c} {
		if !m.local.Has(key) {
			t.Errorf("node %s lacks the blob", m.ID)
		}
	}

	c.stop()
	key2, file2 := sealed(t, "while c is away")
	if err := a.rep.Put(ctx, key2, bytes.NewReader(file2)); err != nil {
		t.Fatalf("with one node stopped: %v", err)
	}
	a.rep.Wait()
	if !b.local.Has(key2) || c.local.Has(key2) {
		t.Error("expected the blob on b and not on c")
	}
	b.stop()
	key3, file3 := sealed(t, "alone")
	if err := a.rep.Put(ctx, key3, bytes.NewReader(file3)); !errors.Is(err, blob.ErrNotReplicated) {
		t.Errorf("with two nodes stopped: %v", err)
	}

	// c comes back under the same identity and address.
	l, err := net.Listen("tcp", c.Address)
	if err != nil {
		t.Skip("could not listen on c's port again:", err)
	}
	c.srv = c.HTTPServer(c.local, verifier)
	go c.srv.ServeTLS(l, "", "")
	defer c.srv.Close()
	f, err := c.rep.Get(ctx, key2)
	if err != nil {
		t.Fatalf("read on the node that missed the blob: %v", err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, file2) || !c.local.Has(key2) {
		t.Error("c did not fetch and keep the blob")
	}
	os.RemoveAll(filepath.Dir(filepath.Dir(mustPath(t, c.local, key))))
	res, err := c.rep.Repair(ctx, cat{"acct": {key[5:]: true, key2[5:]: true}})
	if err != nil || res.Fetched != 2 {
		t.Errorf("Repair on a wiped node = %+v, %v", res, err)
	}
}

func mustPath(t *testing.T, l *blob.Local, key string) string {
	p, err := l.Path(key)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type cat map[string]map[string]bool

func (c cat) Accounts(context.Context) ([]string, error) {
	var out []string
	for a := range c {
		out = append(out, a)
	}
	return out, nil
}
func (c cat) AccountBlobs(_ context.Context, a string) (map[string]bool, error) { return c[a], nil }

// The node table going away does not cut the nodes off from each other.
func TestNodeTableUnreachable(t *testing.T) {
	dir := newTable()
	a, b := start(t, dir, "a", true), start(t, dir, "b", true)
	a.forget()
	b.forget()
	key, file := sealed(t, "before")
	if err := a.rep.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	dir.mu.Lock()
	dir.down = true
	dir.mu.Unlock()
	a.mu.Lock()
	a.readAt = time.Time{} // a's copy is old, and the table cannot be read
	a.mu.Unlock()
	key2, file2 := sealed(t, "during")
	if err := a.rep.Put(ctx, key2, bytes.NewReader(file2)); err != nil {
		t.Errorf("with the node table unreachable: %v", err)
	}
	// A node that never read the table has nobody to ask.
	fresh := &Node{ID: "c", Dir: dir}
	if _, err := fresh.Peers(ctx); err == nil {
		t.Error("Peers without ever reading the table succeeded")
	}
}

func TestRegisterAndHeartbeat(t *testing.T) {
	dir := newTable()
	a := start(t, dir, "a", false)
	// A command registers without claiming to run, and only once.
	if err := a.Register(ctx, false); err != nil {
		t.Fatal(err)
	}
	row := dir.rows["a"]
	if row.Address != a.Address || row.Cert != a.Identity.Fingerprint || !row.SeenAt.IsZero() {
		t.Errorf("row = %+v", row)
	}
	row.SeenAt = time.Unix(5, 0)
	dir.rows["a"] = row
	a.Register(ctx, false)
	if !dir.rows["a"].SeenAt.Equal(time.Unix(5, 0)) {
		t.Error("a second Register by a command rewrote the row")
	}

	a.Heartbeat = 10 * time.Millisecond
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.Run(rctx); close(done) }()
	waitFor(t, func() bool {
		dir.mu.Lock()
		defer dir.mu.Unlock()
		return time.Since(dir.rows["a"].SeenAt) < time.Second
	})
	// The row disappears (node remove): the running node puts it back.
	dir.mu.Lock()
	delete(dir.rows, "a")
	dir.mu.Unlock()
	waitFor(t, func() bool {
		dir.mu.Lock()
		defer dir.mu.Unlock()
		return dir.rows["a"].Cert == a.Identity.Fingerprint
	})
	cancel()
	<-done

	bad := &Node{ID: "Not Valid", Identity: a.Identity, Dir: dir}
	if err := bad.Register(ctx, true); err == nil {
		t.Error("registered an invalid id")
	}
	for id, want := range map[string]bool{"n1": true, "mail-eu-1": true, "": false, "-a": false, "A": false, "a_b": false,
		strings.Repeat("a", 33): false, "a/b": false} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v", id, !want)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}

// The identity is made once and read back the same, with a key only the
// owner can read.
func TestIdentityOnDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	a, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateIdentity(dir)
	if err != nil || a.Fingerprint != b.Fingerprint {
		t.Fatalf("second load: %v, %s != %s", err, a.Fingerprint, b.Fingerprint)
	}
	if len(a.Fingerprint) != 64 || a.Fingerprint != Fingerprint(a.Certificate.Certificate[0]) {
		t.Errorf("fingerprint %q", a.Fingerprint)
	}
	fi, err := os.Stat(filepath.Join(dir, "node.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("node.key: %v %v", fi, err)
	}
	other, _ := LoadOrCreateIdentity(t.TempDir())
	if other.Fingerprint == a.Fingerprint {
		t.Error("two nodes got the same identity")
	}
	// A key that does not belong to the certificate is an error, not a
	// reason to quietly become another node.
	otherKey, _ := os.ReadFile(filepath.Join(dir, "node.key"))
	dir2 := t.TempDir()
	id2, _ := LoadOrCreateIdentity(dir2)
	os.WriteFile(filepath.Join(dir2, "node.key"), otherKey, 0o600)
	if _, err := LoadOrCreateIdentity(dir2); err == nil {
		t.Errorf("loaded a certificate with another node's key (%s)", id2.Fingerprint[:8])
	}
}
