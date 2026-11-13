package rqlite

import (
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
	"testing"
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
