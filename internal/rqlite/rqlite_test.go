package rqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
		c := cluster(t, strings.Replace(follower.URL, "http://", "http://user:secret@", 1))
		method, body = "", ""
		if _, err := c.Execute(context.Background(), Stmt("INSERT")); err != nil {
			t.Fatal(err)
		}
		if method != "POST" || body != `[["INSERT"]]` || auth != "user" {
			t.Errorf("%d: the leader got %s %q, user %q", code, method, body, auth)
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
