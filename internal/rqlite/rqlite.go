// Package rqlite is a small client for rqlite's HTTP API.
//
// rqlite has no interactive transactions. What it offers is a batch of
// statements run atomically (POST /db/request?transaction): a statement that
// fails rolls back the whole batch. Execute sends such a batch, Query sends a
// read-only one with a chosen read consistency level.
package rqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Level is rqlite's read consistency level.
type Level string

const (
	// None reads the local node without any check. May be stale.
	None Level = "none"
	// Weak reads the leader after it checked, locally, that it is the leader.
	Weak Level = "weak"
	// Linearizable has the leader confirm its leadership with a quorum first.
	Linearizable Level = "linearizable"
	// Strong sends the read through the Raft log.
	Strong Level = "strong"
)

// maxResponse bounds what Do reads from the server.
const maxResponse = 256 << 20

// Statement is one SQL statement with its parameters: positional (Args, for
// "?") or named (Named, for ":name"), not both.
type Statement struct {
	SQL   string
	Args  []any
	Named map[string]any
}

// Stmt builds a statement with positional parameters.
func Stmt(sql string, args ...any) Statement { return Statement{SQL: sql, Args: args} }

// MarshalJSON encodes the statement the way rqlite expects it:
// ["SQL", arg, ...] or ["SQL", {"name": arg, ...}].
func (s Statement) MarshalJSON() ([]byte, error) {
	if len(s.Named) > 0 {
		if len(s.Args) > 0 {
			return nil, errors.New("rqlite: statement has both positional and named parameters")
		}
		return json.Marshal([]any{s.SQL, s.Named})
	}
	return json.Marshal(append([]any{s.SQL}, s.Args...))
}

// Result is the outcome of one statement. A value is nil, int64, float64,
// string or bool.
type Result struct {
	Columns      []string
	Types        []string
	Values       [][]any
	LastInsertID int64
	RowsAffected int64
}

// SQLite extended result codes for constraint violations.
const (
	codeConstraintCheck      = 275
	codeConstraintPrimaryKey = 1555
	codeConstraintUnique     = 2067
)

// Error is a statement that SQLite refused. The batch it was part of has
// been rolled back.
type Error struct {
	Index   int    // position of the statement in the batch
	Message string // SQLite's message
	Code    int    // SQLite's extended result code, 0 if rqlite did not send one
}

func (e *Error) Error() string { return fmt.Sprintf("rqlite: statement %d: %s", e.Index, e.Message) }

// Unique reports whether the statement broke a UNIQUE or PRIMARY KEY
// constraint.
func (e *Error) Unique() bool {
	if e.Code != 0 {
		return e.Code == codeConstraintUnique || e.Code == codeConstraintPrimaryKey
	}
	return strings.HasPrefix(e.Message, "UNIQUE constraint failed")
}

// Check reports whether the statement broke a CHECK constraint.
func (e *Error) Check() bool {
	if e.Code != 0 {
		return e.Code == codeConstraintCheck
	}
	return strings.HasPrefix(e.Message, "CHECK constraint failed")
}

// IsUnique reports whether err is a UNIQUE or PRIMARY KEY violation.
func IsUnique(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Unique()
}

// Client talks to one rqlite node. The node forwards writes, and reads that
// need the leader, to the leader itself.
type Client struct {
	base   *url.URL
	client *http.Client
}

// New returns a client for the node at baseURL, for example
// "http://127.0.0.1:4001". Credentials in the URL are sent as basic auth.
// A nil httpClient gets a default with a 30 second timeout.
func New(baseURL string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("rqlite: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("rqlite: %q is not an http(s) URL", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: u, client: httpClient}, nil
}

// Execute runs the statements as one transaction and returns one result per
// statement. If a statement fails, nothing is applied and the error is an
// *Error naming it.
func (c *Client) Execute(ctx context.Context, stmts ...Statement) ([]Result, error) {
	return c.do(ctx, "transaction", stmts)
}

// Query runs read-only statements at the given consistency level, all against
// the same snapshot. Do not pass statements that write: use Execute.
func (c *Client) Query(ctx context.Context, level Level, stmts ...Statement) ([]Result, error) {
	return c.do(ctx, "transaction&level="+url.QueryEscape(string(level)), stmts)
}

type wireResult struct {
	Columns      []string `json:"columns"`
	Types        []string `json:"types"`
	Values       [][]any  `json:"values"`
	LastInsertID int64    `json:"last_insert_id"`
	RowsAffected int64    `json:"rows_affected"`
	Error        string   `json:"error"`
	ErrorV2      *struct {
		SQLite *struct {
			ExtendedCode int `json:"extended_code"`
		} `json:"sqlite"`
	} `json:"error_v2"`
}

func (c *Client) do(ctx context.Context, query string, stmts []Statement) ([]Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(stmts)
	if err != nil {
		return nil, fmt.Errorf("rqlite: %w", err)
	}
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/db/request"
	u.RawQuery = query
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rqlite: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rqlite: %w", redact(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("rqlite: reading response: %w", err)
	}
	if len(data) > maxResponse {
		return nil, errors.New("rqlite: response too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rqlite: HTTP %d: %s", resp.StatusCode, clip(data))
	}

	var out struct {
		Error   string       `json:"error"`
		Results []wireResult `json:"results"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("rqlite: bad response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("rqlite: %s", out.Error)
	}
	results := make([]Result, len(out.Results))
	for i, r := range out.Results {
		if r.Error != "" {
			e := &Error{Index: i, Message: r.Error}
			if r.ErrorV2 != nil && r.ErrorV2.SQLite != nil {
				e.Code = r.ErrorV2.SQLite.ExtendedCode
			}
			return nil, e
		}
		for _, row := range r.Values {
			for j, v := range row {
				if n, ok := v.(json.Number); ok {
					row[j] = number(n)
				}
			}
		}
		results[i] = Result{Columns: r.Columns, Types: r.Types, Values: r.Values,
			LastInsertID: r.LastInsertID, RowsAffected: r.RowsAffected}
	}
	if len(results) != len(stmts) {
		return nil, fmt.Errorf("rqlite: %d results for %d statements", len(results), len(stmts))
	}
	return results, nil
}

// number keeps integers exact: encoding/json would turn them into float64.
func number(n json.Number) any {
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, _ := n.Float64()
	return f
}

// redact drops the URL, which may hold a password, from a transport error.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300]
	}
	return strings.ToValidUTF8(s, "")
}
