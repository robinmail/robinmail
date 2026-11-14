package rqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatementJSON(t *testing.T) {
	for want, s := range map[string]Statement{
		`["SELECT 1"]`:                  Stmt("SELECT 1"),
		`["SELECT ?, ?",1,"a"]`:         Stmt("SELECT ?, ?", 1, "a"),
		`["SELECT :a",{"a":"x"}]`:       {SQL: "SELECT :a", Named: map[string]any{"a": "x"}},
		`["SELECT ?",null]`:             Stmt("SELECT ?", nil),
		`["SELECT ?",9007199254740993]`: Stmt("SELECT ?", int64(9007199254740993)),
	} {
		got, err := json.Marshal(s)
		if err != nil || string(got) != want {
			t.Errorf("Marshal = %s, %v; want %s", got, err, want)
		}
	}
	if _, err := json.Marshal(Statement{SQL: "x", Args: []any{1}, Named: map[string]any{"a": 1}}); err == nil {
		t.Error("accepted positional and named parameters together")
	}
}

// fake serves one canned reply and records the request.
func fake(t *testing.T, status int, reply string) (*Client, *http.Request, *string) {
	t.Helper()
	var got http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	c, err := New(strings.Replace(srv.URL, "http://", "http://user:secret@", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Wait = 200 * time.Millisecond
	return c, &got, &body
}

func TestRequestAndResults(t *testing.T) {
	c, req, body := fake(t, 200, `{"results":[{"last_insert_id":7,"rows_affected":1},
		{"columns":["a","n","f"],"types":["text","integer","real"],"values":[["x",9007199254740993,1.5],[null,-2,2]]}]}`)
	res, err := c.Query(context.Background(), Weak, Stmt("INSERT"), Stmt("SELECT ?", 1))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.URL.Path != "/db/request" || req.URL.RawQuery != "transaction&level=weak" {
		t.Errorf("request %s %s?%s", req.Method, req.URL.Path, req.URL.RawQuery)
	}
	if u, p, ok := req.BasicAuth(); !ok || u != "user" || p != "secret" {
		t.Errorf("basic auth %q %q %v", u, p, ok)
	}
	if *body != `[["INSERT"],["SELECT ?",1]]` {
		t.Errorf("body %s", *body)
	}
	if res[0].LastInsertID != 7 || res[0].RowsAffected != 1 {
		t.Errorf("result 0 = %+v", res[0])
	}
	v := res[1].Values
	if v[0][0] != "x" || v[0][1] != int64(9007199254740993) || v[0][2] != 1.5 || v[1][0] != nil || v[1][1] != int64(-2) || v[1][2] != int64(2) {
		t.Errorf("values = %#v", v)
	}

	c, req, _ = fake(t, 200, `{"results":[{}]}`)
	if _, err := c.Execute(context.Background(), Stmt("x")); err != nil || req.URL.RawQuery != "transaction" {
		t.Errorf("Execute: %v, query %q", err, req.URL.RawQuery)
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()

	c, _, _ := fake(t, 200, `{"results":[{"rows_affected":1},{"error":"UNIQUE constraint failed: t.a",
		"error_v2":{"message":"UNIQUE constraint failed: t.a","sqlite":{"code":19,"extended_code":1555}}}]}`)
	_, err := c.Execute(ctx, Stmt("a"), Stmt("b"), Stmt("c"))
	var e *Error
	if !errors.As(err, &e) || e.Index != 1 || !e.Unique() || e.Check() || !IsUnique(err) {
		t.Errorf("unique violation: %#v", err)
	}

	c, _, _ = fake(t, 200, `{"results":[{"error":"CHECK constraint failed: ok = 0"}]}`)
	_, err = c.Execute(ctx, Stmt("a"))
	if !errors.As(err, &e) || e.Index != 0 || !e.Check() || IsUnique(err) {
		t.Errorf("check violation without a code: %#v", err)
	}

	for name, f := range map[string]struct {
		status int
		reply  string
	}{
		"top-level error": {200, `{"error":"leader not found"}`},
		"http status":     {503, `not the leader`},
		"not json":        {200, `<html>`},
		"too few results": {200, `{"results":[]}`},
	} {
		c, _, _ := fake(t, f.status, f.reply)
		_, err := c.Execute(ctx, Stmt("a"))
		if err == nil || errors.As(err, &e) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	c, _ = New("http://user:secret@127.0.0.1:1", nil)
	c.Wait = 200 * time.Millisecond
	if _, err := c.Execute(ctx, Stmt("a")); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("unreachable: %v", err)
	}
	if _, err := New("127.0.0.1:4001", nil); err == nil {
		t.Error("accepted a URL without scheme")
	}
}

// TestReal checks the client's assumptions against a real rqlite:
//
//	docker compose -f contrib/compose/compose.yaml up -d
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 go test ./internal/rqlite
func TestReal(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	c, err := New(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var r [6]byte
	rand.Read(r[:])
	table := "rqlite_test_" + hex.EncodeToString(r[:])
	if _, err := c.Execute(ctx, Stmt(`CREATE TABLE `+table+` (a TEXT PRIMARY KEY, n INTEGER CHECK (n >= 0))`)); err != nil {
		t.Fatal(err)
	}
	defer c.Execute(ctx, Stmt(`DROP TABLE `+table))

	ins := `INSERT INTO ` + table + ` VALUES (:a, :n)`
	res, err := c.Execute(ctx,
		Statement{SQL: ins, Named: map[string]any{"a": "x", "n": int64(9007199254740993)}},
		Stmt(`INSERT INTO `+table+` VALUES (?, ?)`, "y", 2),
		Stmt(`SELECT a, n FROM `+table+` ORDER BY a`))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].RowsAffected != 1 || len(res[2].Values) != 2 || res[2].Values[0][1] != int64(9007199254740993) || res[2].Values[1][0] != "y" {
		t.Errorf("results = %+v", res)
	}

	// A failing statement names its position and rolls back what came before.
	var e *Error
	_, err = c.Execute(ctx,
		Stmt(`INSERT INTO `+table+` VALUES ('z', 1)`),
		Stmt(`INSERT INTO `+table+` VALUES ('x', 1)`),
		Stmt(`INSERT INTO `+table+` VALUES ('w', 1)`))
	if !errors.As(err, &e) || e.Index != 1 || !e.Unique() || e.Code == 0 {
		t.Errorf("unique violation: %#v", err)
	}
	_, err = c.Execute(ctx, Stmt(`INSERT INTO `+table+` VALUES ('z', -1)`))
	if !errors.As(err, &e) || e.Index != 0 || !e.Check() || e.Unique() {
		t.Errorf("check violation: %#v", err)
	}
	for _, level := range []Level{None, Weak, Linearizable, Strong} {
		res, err = c.Query(ctx, level, Stmt(`SELECT count(*) FROM `+table), Stmt(`SELECT a FROM `+table+` WHERE a = 'nobody'`))
		if err != nil {
			t.Fatalf("level %s: %v", level, err)
		}
		if res[0].Values[0][0] != int64(2) || len(res[1].Values) != 0 {
			t.Errorf("level %s: %+v", level, res)
		}
	}
	if _, err := c.Query(ctx, Weak, Stmt(`SELECT * FROM `+table+`_nope`)); !errors.As(err, &e) || !strings.Contains(e.Message, "no such table") {
		t.Errorf("missing table: %#v", err)
	}
}

// node is a fake rqlite node whose behaviour a test sets per request.
type node struct {
	*httptest.Server
	hits    atomic.Int64
	handler atomic.Value // func(w, r)
}

func newNode(t *testing.T, h http.HandlerFunc) *node {
	n := &node{}
	n.handler.Store(h)
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.hits.Add(1)
		n.handler.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(n.Close)
	return n
}

func ok(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	io.WriteString(w, `{"results":[{"rows_affected":1}]}`)
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(code)
		io.WriteString(w, body)
	}
}

// hangUp reads the request and then drops the connection without an answer:
// what a client sees when a node dies after it took the request.
func hangUp(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func cluster(t *testing.T, urls ...string) *Client {
	t.Helper()
	c, err := NewCluster(urls, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Wait = 2 * time.Second
	return c
}

// A node that cannot be reached is skipped, and the client stays with the
// node that answered.
func TestFailoverToNextNode(t *testing.T) {
	ctx := context.Background()
	live := newNode(t, ok)
	c := cluster(t, "http://127.0.0.1:1", live.URL)
	for _, call := range []func() error{
		func() error { _, err := c.Execute(ctx, Stmt("a")); return err },
		func() error { _, err := c.ExecuteIdempotent(ctx, Stmt("a")); return err },
		func() error { _, err := c.Query(ctx, Weak, Stmt("a")); return err },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	if c.preferred() != 1 || live.hits.Load() != 3 {
		t.Errorf("preferred node %d, %d requests on the live node", c.preferred(), live.hits.Load())
	}
}

// A redirect to the leader is followed with the method and the body kept.
func TestFollowsRedirect(t *testing.T) {
	var method, body, auth string
	leader := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, body = r.Method, string(b)
		auth, _, _ = r.BasicAuth()
		io.WriteString(w, `{"results":[{"rows_affected":1}]}`)
	})
	for _, code := range []int{301, 307} {
		follower := newNode(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, leader.URL+r.URL.RequestURI(), code)
		})
		// The leader is a node of the config: the credentials go along.
		c := cluster(t, strings.Replace(follower.URL, "http://", "http://user:secret@", 1), leader.URL)
		method, body = "", ""
		if _, err := c.Execute(context.Background(), Stmt("INSERT")); err != nil {
			t.Fatal(err)
		}
		if method != "POST" || body != `[["INSERT"]]` || auth != "user" {
			t.Errorf("%d: the leader got %s %q, user %q", code, method, body, auth)
		}
		// Without credentials a redirect is followed wherever it leads,
		// as before: there is nothing to lose.
		c = cluster(t, follower.URL)
		method, auth = "", "x"
		if _, err := c.Execute(context.Background(), Stmt("INSERT")); err != nil || method != "POST" || auth != "" {
			t.Errorf("%d without credentials: %v, the leader got %s, user %q", code, err, method, auth)
		}
	}
	// A node that redirects to itself for ever is given up on.
	var loop *node
	loop = newNode(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, loop.URL+"/db/request", 301) })
	c := cluster(t, loop.URL)
	c.Wait = 100 * time.Millisecond
	if _, err := c.Execute(context.Background(), Stmt("a")); err == nil {
		t.Error("a redirect loop ended without an error")
	}
}

// The rule that keeps a write from being applied twice: after an answer
// that does not say whether the batch was applied, Execute stops with
// ErrUnknownOutcome and sends nothing more; ExecuteIdempotent and Query go
// on to the next node.
func TestUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	for name, h := range map[string]http.HandlerFunc{
		"connection dropped after the request": hangUp,
		"leadership lost":                      status(200, `{"error":"leadership lost while committing log"}`),
		"http 500":                             status(500, `boom`),
		"http 503 without a reason":            status(503, `forwarding failed`),
		"half an answer":                       status(200, `{"results":[{"rows_`),
	} {
		bad, good := newNode(t, h), newNode(t, ok)
		c := cluster(t, bad.URL, good.URL)
		_, err := c.Execute(ctx, Stmt("a"))
		if !errors.Is(err, ErrUnknownOutcome) {
			t.Errorf("%s: Execute err = %v, want ErrUnknownOutcome", name, err)
		}
		if good.hits.Load() != 0 || bad.hits.Load() != 1 {
			t.Errorf("%s: Execute sent the batch %d more times", name, good.hits.Load()+bad.hits.Load()-1)
		}
		if _, err := c.ExecuteIdempotent(ctx, Stmt("a")); err != nil {
			t.Errorf("%s: ExecuteIdempotent: %v", name, err)
		}
		c = cluster(t, bad.URL, good.URL)
		if _, err := c.Query(ctx, Weak, Stmt("a")); err != nil {
			t.Errorf("%s: Query: %v", name, err)
		}
	}
}

// "No leader right now" means nothing was applied, so every kind of call
// waits for the election and goes through.
func TestWaitsForLeader(t *testing.T) {
	ctx := context.Background()
	for name, h := range map[string]http.HandlerFunc{
		"http 503":        status(503, "leader not found\n"),
		"top-level error": status(200, `{"error":"not leader"}`),
	} {
		n := newNode(t, h)
		c := cluster(t, n.URL)
		go func() {
			time.Sleep(150 * time.Millisecond)
			n.handler.Store(http.HandlerFunc(ok))
		}()
		start := time.Now()
		if _, err := c.Execute(ctx, Stmt("a")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if n.hits.Load() < 2 || time.Since(start) < 100*time.Millisecond {
			t.Errorf("%s: %d requests in %s", name, n.hits.Load(), time.Since(start))
		}
	}
}

// Retries back off and end: at Wait, or when the context does.
func TestGivesUp(t *testing.T) {
	n := newNode(t, status(503, "leader not found"))
	c := cluster(t, n.URL, "http://127.0.0.1:1")
	c.Wait = 400 * time.Millisecond
	start := time.Now()
	_, err := c.Query(context.Background(), Weak, Stmt("a"))
	if err == nil || errors.Is(err, ErrUnknownOutcome) || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %s", err, time.Since(start))
	}
	// 50, 100, 200 ms of backoff fit in 400 ms: at most 4 rounds.
	if h := n.hits.Load(); h < 2 || h > 4 {
		t.Errorf("%d requests in %s: no backoff?", h, time.Since(start))
	}

	c.Wait = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := c.Query(ctx, Weak, Stmt("a")); err == nil || time.Since(start) > time.Second {
		t.Errorf("err = %v after %s", err, time.Since(start))
	}
}

// A node without a leader for too long refuses a read at level none; the
// client reports that at once instead of waiting.
func TestStaleRead(t *testing.T) {
	n := newNode(t, status(200, `{"results":[],"error":"stale read"}`))
	c := cluster(t, n.URL)
	start := time.Now()
	_, err := c.Query(context.Background(), None, Stmt("a"))
	if !errors.Is(err, ErrStale) || n.hits.Load() != 1 || time.Since(start) > time.Second {
		t.Errorf("err = %v after %d requests and %s", err, n.hits.Load(), time.Since(start))
	}
}

// A statement SQLite refused is an answer. It is not sent to another node.
func TestStatementErrorIsFinal(t *testing.T) {
	a := newNode(t, status(200, `{"results":[{"error":"UNIQUE constraint failed: t.a"}]}`))
	b := newNode(t, ok)
	c := cluster(t, a.URL, b.URL)
	if _, err := c.ExecuteIdempotent(context.Background(), Stmt("a")); !IsUnique(err) {
		t.Errorf("err = %v", err)
	}
	if b.hits.Load() != 0 {
		t.Error("the batch went to a second node")
	}
	for _, code := range []int{400, 401} {
		a.handler.Store(status(code, "no"))
		if _, err := c.Query(context.Background(), Weak, Stmt("a")); err == nil || a.hits.Load() < 2 || b.hits.Load() != 0 {
			t.Errorf("HTTP %d: err = %v, second node asked %d times", code, err, b.hits.Load())
		}
	}
}

// Backup and Load against a stand-in for rqlite's /db/backup and /db/load:
// the body is passed through whole, a follower's redirect to the leader is
// followed with the method and the body kept, and a node that is away is
// passed over.
func TestBackupAndLoad(t *testing.T) {
	database := bytes.Repeat([]byte("SQLite format 3\x00 and the rest of a database. "), 5000)
	var loaded []byte
	var loadType string
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/db/backup":
			w.Write(database)
		case r.Method == http.MethodPost && r.URL.Path == "/db/load":
			loadType = r.Header.Get("Content-Type")
			loaded, _ = io.ReadAll(r.Body)
			if int64(len(loaded)) != r.ContentLength {
				t.Errorf("load: %d bytes with Content-Length %d", len(loaded), r.ContentLength)
			}
			io.WriteString(w, `{"results":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer leader.Close()
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leader.URL+r.URL.Path, http.StatusMovedPermanently)
	}))
	defer follower.Close()
	dead := httptest.NewServer(nil)
	dead.Close()

	c, err := NewCluster([]string{dead.URL, follower.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := c.Backup(context.Background(), &got); err != nil || !bytes.Equal(got.Bytes(), database) {
		t.Fatalf("Backup: %d bytes, %v", got.Len(), err)
	}
	if err := c.Load(context.Background(), bytes.NewReader(database)); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(loaded, database) || loadType != "application/octet-stream" {
		t.Errorf("Load sent %d bytes as %q", len(loaded), loadType)
	}

	// What rqlite answers when it could not load, and a node that fails.
	for name, answer := range map[string]func(w http.ResponseWriter){
		"an error member": func(w http.ResponseWriter) { io.WriteString(w, `{"error":"file is not a database"}`) },
		"a failed result": func(w http.ResponseWriter) { io.WriteString(w, `{"results":[{"error":"disk I/O error"}]}`) },
		"not JSON":        func(w http.ResponseWriter) { io.WriteString(w, `<html>`) },
		"a server error":  func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
		"a redirect loop": func(w http.ResponseWriter) { w.Header().Set("Location", "/db/load"); w.WriteHeader(http.StatusFound) },
		"a bare redirect": func(w http.ResponseWriter) { w.WriteHeader(http.StatusFound) },
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { answer(w) }))
		c, _ := New(srv.URL, nil)
		if err := c.Load(context.Background(), bytes.NewReader(database)); err == nil {
			t.Errorf("Load with %s: no error", name)
		}
		if name == "a server error" {
			if err := c.Backup(context.Background(), io.Discard); err == nil {
				t.Errorf("Backup with %s: no error", name)
			}
		}
		srv.Close()
	}
}

// Backup and Load on a real rqlite: what Load puts in is what Backup gives.
// It runs on a database of its own table name and leaves the rest alone:
// only Backup is used against the shared database.
func TestBackupReal(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	c, err := New(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := c.Backup(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	// "SQLite format 3\000" starts every database file (SQLite's file
	// format, section 1.3).
	if !bytes.HasPrefix(got.Bytes(), []byte("SQLite format 3\x00")) || got.Len() < 4096 {
		t.Errorf("the backup is %d bytes and starts %q", got.Len(), got.Bytes()[:min(got.Len(), 16)])
	}
}

// secretTransport notes every request that leaves the client: where it
// went and whether it carried credentials.
type sentRequest struct {
	host string
	auth bool
}

type recorder struct {
	mu   sync.Mutex
	sent []sentRequest
}

func (rec *recorder) wrap(c *Client) {
	inner := c.client.Transport
	cp := *c.client
	cp.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		_, _, has := r.BasicAuth()
		rec.mu.Lock()
		rec.sent = append(rec.sent, sentRequest{r.URL.Host, has})
		rec.mu.Unlock()
		return inner.RoundTrip(r)
	})
	c.client = &cp
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// authNode is an rqlite with an auth file: 401 without the right user and
// password, as rqlite 10.7 answers (checked against the real one: an empty
// body, 401 also for a user that lacks the permission).
func authNode(t *testing.T, user, password string, got *atomic.Int64) *node {
	return newNode(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != password {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got != nil {
			got.Add(1)
		}
		io.WriteString(w, `{"results":[{"columns":["1"],"values":[[1]]}]}`)
	})
}

const testSecret = "s3cr3t-of-the-test-0nly"

// A cluster that wants credentials: without them, and with wrong ones, a
// request is refused at once and for good (no other node is asked and
// nothing is tried again), and the error says what to look at without
// saying what was sent.
func TestCredentials(t *testing.T) {
	var served atomic.Int64
	n := authNode(t, "robinmail", testSecret, &served)
	ctx := context.Background()

	c := cluster(t, n.URL)
	c.SetCredentials("robinmail", testSecret)
	if _, err := c.Query(ctx, Strong, Stmt("SELECT 1")); err != nil {
		t.Fatalf("with the right credentials: %v", err)
	}
	if _, err := c.Execute(ctx, Stmt("INSERT")); err != nil {
		t.Fatalf("a write with the right credentials: %v", err)
	}
	// From the URL, as the config also allows.
	c = cluster(t, strings.Replace(n.URL, "http://", "http://robinmail:"+testSecret+"@", 1))
	if _, err := c.Query(ctx, Strong, Stmt("SELECT 1")); err != nil {
		t.Fatalf("with credentials in the URL: %v", err)
	}
	if enc, creds := c.Security(); enc || !creds {
		t.Errorf("Security: encrypted %v, credentials %v", enc, creds)
	}

	for name, set := range map[string]func(*Client){
		"no credentials":   func(*Client) {},
		"a wrong password": func(c *Client) { c.SetCredentials("robinmail", "not-"+testSecret) },
		"a wrong user":     func(c *Client) { c.SetCredentials("someone", testSecret) },
	} {
		c := cluster(t, n.URL)
		set(c)
		before, hits := served.Load(), n.hits.Load()
		start := time.Now()
		for what, call := range map[string]func() error{
			"Query":             func() error { _, err := c.Query(ctx, Strong, Stmt("SELECT 1")); return err },
			"Execute":           func() error { _, err := c.Execute(ctx, Stmt("INSERT")); return err },
			"ExecuteIdempotent": func() error { _, err := c.ExecuteIdempotent(ctx, Stmt("INSERT")); return err },
			"Backup":            func() error { return c.Backup(ctx, io.Discard) },
			"Load":              func() error { return c.Load(ctx, bytes.NewReader([]byte("x"))) },
		} {
			err := call()
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("%s with %s: %v, want ErrUnauthorized", what, name, err)
				continue
			}
			if strings.Contains(err.Error(), testSecret) || !strings.Contains(err.Error(), "rqlite.auth_file") {
				t.Errorf("%s with %s: the error is %q", what, name, err)
			}
		}
		if served.Load() != before {
			t.Errorf("%s: a request was served", name)
		}
		// Five calls, five requests: a refusal is an answer, not a
		// failure to try again.
		if got := n.hits.Load() - hits; got != 5 || time.Since(start) > time.Second {
			t.Errorf("%s: %d requests in %s, want 5 at once", name, got, time.Since(start))
		}
	}

	// RequiresAuth: whether the cluster is closed to whoever reaches it.
	c = cluster(t, n.URL)
	c.SetCredentials("robinmail", testSecret)
	if closed, err := c.RequiresAuth(ctx); err != nil || !closed {
		t.Errorf("RequiresAuth on a node with an auth file: %v %v", closed, err)
	}
	open := newNode(t, ok)
	c = cluster(t, open.URL)
	c.SetCredentials("robinmail", testSecret) // sent, and nobody asks for them
	if closed, err := c.RequiresAuth(ctx); err != nil || closed {
		t.Errorf("RequiresAuth on an open node: %v %v", closed, err)
	}
}

// Credentials go to the nodes of the config and nowhere else. A node, or
// whoever answers in its place, that redirects to another host does not
// get them sent there: with credentials such a redirect is not followed.
func TestCredentialsStayHome(t *testing.T) {
	ctx := context.Background()
	var foreignAuth atomic.Value
	foreignAuth.Store("")
	var foreignHits atomic.Int64
	foreign := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		if h := r.Header.Get("Authorization"); h != "" {
			foreignAuth.Store(h)
		}
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"results":[{"rows_affected":1}]}`)
	})
	redirecting := newNode(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	for name, make := range map[string]func() *Client{
		"SetCredentials": func() *Client { c := cluster(t, redirecting.URL); c.SetCredentials("robinmail", testSecret); return c },
		"the URL": func() *Client {
			return cluster(t, strings.Replace(redirecting.URL, "http://", "http://robinmail:"+testSecret+"@", 1))
		},
	} {
		c := make()
		c.Wait = 200 * time.Millisecond
		rec := &recorder{}
		rec.wrap(c)
		for what, call := range map[string]func() error{
			"Execute": func() error { _, err := c.Execute(ctx, Stmt("INSERT")); return err },
			"Query":   func() error { _, err := c.Query(ctx, Strong, Stmt("SELECT 1")); return err },
			"Backup":  func() error { return c.Backup(ctx, io.Discard) },
			"Load":    func() error { return c.Load(ctx, bytes.NewReader([]byte("x"))) },
		} {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "not a node of the config") {
				t.Errorf("credentials from %s, %s: %v", name, what, err)
			}
			if err != nil && strings.Contains(err.Error(), testSecret) {
				t.Errorf("credentials from %s, %s: the error has the password: %v", name, what, err)
			}
		}
		if foreignHits.Load() != 0 || foreignAuth.Load() != "" {
			t.Errorf("credentials from %s: the foreign host got %d requests, Authorization %q", name, foreignHits.Load(), foreignAuth.Load())
		}
		rec.mu.Lock()
		for _, r := range rec.sent {
			if r.host != redirecting.Listener.Addr().String() {
				t.Errorf("credentials from %s: a request left for %s", name, r.host)
			}
		}
		rec.mu.Unlock()
	}

	// The same host with another scheme or port is another host.
	c := cluster(t, redirecting.URL)
	c.SetCredentials("robinmail", testSecret)
	for _, loc := range []string{"https://" + redirecting.Listener.Addr().String() + "/db/request", "http://" + redirecting.Listener.Addr().String() + "0/db/request",
		"//evil.example/db/request", "http://robinmail:x@evil.example/"} {
		if next, err := c.follow(c.nodes[0], loc, 301); err == nil {
			t.Errorf("a redirect to %s is followed, to %s", loc, next)
		}
	}
	if next, err := c.follow(c.nodes[0], "/db/request?x=1", 307); err != nil || next.Host != c.nodes[0].Host {
		t.Errorf("a redirect on the same node: %v %v", next, err)
	}
}

// Nothing the client says holds the password: not the errors of a node
// that is away, of a URL that does not parse, of a refused login, and not
// what it logs (it logs nothing).
func TestNoSecretInErrors(t *testing.T) {
	ctx := context.Background()
	dead := httptest.NewServer(nil)
	dead.Close()
	withSecret := func(u string) string { return strings.Replace(u, "http://", "http://robinmail:"+testSecret+"@", 1) }
	var errs []error
	c := cluster(t, withSecret(dead.URL))
	c.Wait = 100 * time.Millisecond
	_, err := c.Execute(ctx, Stmt("INSERT"))
	errs = append(errs, err)
	_, err = c.Query(ctx, Weak, Stmt("SELECT 1"))
	errs = append(errs, err, c.Backup(ctx, io.Discard))
	_, err = c.RequiresAuth(ctx)
	errs = append(errs, err)
	slow := newNode(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
	c = cluster(t, withSecret(slow.URL))
	c.client.Timeout = 50 * time.Millisecond
	c.Wait = 100 * time.Millisecond
	_, err = c.Query(ctx, Weak, Stmt("SELECT 1"))
	errs = append(errs, err)
	for _, bad := range []string{"http://robinmail:" + testSecret + "@host:port:4001", "ftp://robinmail:" + testSecret + "@host/", "http://robinmail:" + testSecret + "@%zz/"} {
		_, err := NewCluster([]string{bad}, nil)
		errs = append(errs, err)
	}
	for i, err := range errs {
		if err == nil {
			t.Errorf("call %d: no error", i)
		} else if strings.Contains(err.Error(), testSecret) || strings.Contains(fmt.Sprintf("%#v %+v", err, err), testSecret) {
			t.Errorf("call %d: the error has the password: %v", i, err)
		}
	}
	// And the node list the client keeps has no user in it.
	c = cluster(t, withSecret(slow.URL))
	for _, n := range c.nodes {
		if n.User != nil || strings.Contains(n.String(), testSecret) {
			t.Errorf("a node URL kept its credentials: %s", n.Redacted())
		}
	}
}

// HTTPS with a CA of one's own: the node's certificate is checked against
// it, and a node with another certificate is not talked to.
func TestTrustCA(t *testing.T) {
	ctx := context.Background()
	var served atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if u, p, _ := r.BasicAuth(); u != "robinmail" || p != testSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		served.Add(1)
		io.WriteString(w, `{"results":[{"columns":["1"],"values":[[1]]}]}`)
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	c := cluster(t, srv.URL)
	c.Wait = 200 * time.Millisecond
	c.SetCredentials("robinmail", testSecret)
	// Without the CA the certificate is unknown and nothing is sent.
	if _, err := c.Query(ctx, Weak, Stmt("SELECT 1")); err == nil || served.Load() != 0 {
		t.Errorf("an unknown certificate: %v, %d served", err, served.Load())
	}
	if err := c.TrustCA(pool); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Query(ctx, Weak, Stmt("SELECT 1")); err != nil || served.Load() != 1 {
		t.Errorf("with the CA: %v, %d served", err, served.Load())
	}
	if err := c.Backup(ctx, io.Discard); err != nil {
		t.Errorf("Backup with the CA: %v", err)
	}
	if enc, creds := c.Security(); !enc || !creds {
		t.Errorf("Security: encrypted %v, credentials %v", enc, creds)
	}
	// Another server, with a certificate of another CA: refused.
	other := httptest.NewUnstartedServer(http.HandlerFunc(ok))
	other.StartTLS()
	defer other.Close()
	// httptest gives every server the same certificate; a pool without
	// it stands for "another CA".
	c2 := cluster(t, other.URL)
	c2.Wait = 200 * time.Millisecond
	if err := c2.TrustCA(x509.NewCertPool()); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Query(ctx, Weak, Stmt("SELECT 1")); err == nil {
		t.Error("a certificate that the CA did not sign is accepted")
	}
	// One node over http makes the cluster "not encrypted".
	mixed := cluster(t, srv.URL, "http://127.0.0.1:1")
	if enc, _ := mixed.Security(); enc {
		t.Error("a cluster with a plain http node counts as encrypted")
	}
}

// The credentials file: one line "user:password", for its owner only. What
// is wrong with it is said without quoting it.
func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	for content, want := range map[string][2]string{
		"robinmail:" + testSecret + "\n":   {"robinmail", testSecret},
		"robinmail:" + testSecret:          {"robinmail", testSecret},
		"robinmail:" + testSecret + "\r\n": {"robinmail", testSecret},
		"u:pass:with:colons\n":             {"u", "pass:with:colons"},
		"u: a password with spaces \n":     {"u", " a password with spaces "},
	} {
		user, pass, err := LoadCredentials(write("ok", content, 0o600))
		if err != nil || user != want[0] || pass != want[1] {
			t.Errorf("%q: %q %q %v", content, user, pass, err)
		}
	}
	for name, content := range map[string]string{
		"empty": "", "no colon": testSecret + "\n", "no user": ":" + testSecret + "\n", "no password": "robinmail:\n",
		"two lines": "robinmail:" + testSecret + "\nother:" + testSecret + "\n",
	} {
		_, _, err := LoadCredentials(write("bad", content, 0o600))
		if err == nil || strings.Contains(err.Error(), testSecret) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Readable by others: refused, like a key file.
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		_, _, err := LoadCredentials(write("open", "robinmail:"+testSecret+"\n", mode))
		if err == nil || !strings.Contains(err.Error(), "0600") || strings.Contains(err.Error(), testSecret) {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
	if _, _, err := LoadCredentials(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing file")
	}
	if _, _, err := LoadCredentials(dir); err == nil {
		t.Error("a directory")
	}
	if _, err := LoadCA(write("ca", "not a certificate\n", 0o644)); err == nil {
		t.Error("a CA file without a certificate")
	}
}
