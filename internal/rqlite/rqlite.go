// Package rqlite is a small client for rqlite's HTTP API.
//
// rqlite has no interactive transactions. What it offers is a batch of
// statements run atomically (POST /db/request?transaction): a statement that
// fails rolls back the whole batch. Execute sends such a batch, Query sends a
// read-only one with a chosen read consistency level.
//
// The client knows several nodes of the cluster and survives a node or a
// leader going away: see Client for what it tries again and what it does not.
package rqlite

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mikepage/robinmail/internal/metrics"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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

// ErrUnknownOutcome wraps the error of a write that may or may not have been
// applied: the request left this process and no answer came back, or the
// answer was an error that does not say. Execute returns it; ExecuteIdempotent
// tries again instead.
var ErrUnknownOutcome = errors.New("the outcome of the write is unknown")

// ErrStale is returned by a Query at level None when the node that was asked
// has been without a leader for too long to answer from its own copy.
var ErrStale = errors.New("rqlite: this node's copy is stale")

// Client talks to an rqlite cluster through any of its nodes.
//
// A node that is not the leader hands writes, and reads that need the
// leader, to the leader itself; if it answers with a redirect instead, the
// client follows it. The client stays with the node that answered last and
// moves to the next one when that node stops answering, so a dead node, a
// leader change or a restart shows up as a short wait, not as an error.
//
// What is tried again, with a backoff, until Wait has passed:
//
//   - Reads (Query): always. A read changes nothing.
//   - Any request that provably never reached rqlite's log: the connection
//     could not be made, the node redirected, or it answered that the
//     cluster has no leader right now.
//   - Writes whose outcome is unknown (the connection broke after the
//     request was sent, a timeout, a leader that lost its place while
//     committing): only with ExecuteIdempotent. Execute returns the error,
//     wrapped in ErrUnknownOutcome, because a second run of a batch that
//     is not idempotent could apply it twice.
//
// A statement that SQLite refused (*Error) is an answer, not a failure: it
// is never tried again.
//
// Credentials (basic auth) go with every request to a node of the
// configured list and nowhere else: a redirect to a host that is not on
// the list is not followed when there are credentials to lose. They are
// never part of an error.
type Client struct {
	nodes  []*url.URL
	client *http.Client
	// user and pass are the credentials; set is whether there are any.
	user, pass string
	set        bool
	// Wait is how long one call keeps trying before it gives up. [15s]
	Wait time.Duration

	mu   sync.Mutex
	pref int // the node that answered last
}

// New returns a client for the cluster the node at baseURL belongs to, for
// example "http://127.0.0.1:4001". See NewCluster.
func New(baseURL string, httpClient *http.Client) (*Client, error) {
	return NewCluster([]string{baseURL}, httpClient)
}

// NewCluster returns a client that knows several nodes of one cluster. The
// first is tried first. Credentials in a URL are sent as basic auth. A nil
// httpClient gets a default: 2 seconds to connect, 10 seconds per attempt.
func NewCluster(urls []string, httpClient *http.Client) (*Client, error) {
	if len(urls) == 0 {
		return nil, errors.New("rqlite: no node URL")
	}
	c := &Client{client: httpClient, Wait: 15 * time.Second}
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			// The error of url.Parse quotes the URL, password and all.
			return nil, errors.New("rqlite: a node URL does not parse")
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			u.User = nil
			return nil, fmt.Errorf("rqlite: %q is not an http(s) URL", u.String())
		}
		// Credentials in a URL become the client's and leave the URL,
		// so that no error or log line that names a node has them.
		if u.User != nil && !c.set {
			c.user = u.User.Username()
			c.pass, _ = u.User.Password()
			c.set = true
		}
		u.User = nil
		c.nodes = append(c.nodes, u)
	}
	if c.client == nil {
		c.client = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     60 * time.Second,
			},
		}
	}
	// Redirects are followed here, with the method and the body kept:
	// net/http would turn a redirected POST into a GET.
	cp := *c.client
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.client = &cp
	return c, nil
}

// SetCredentials makes the client send this user and password, as basic
// auth, with every request to a configured node. Call it before the client
// is used.
func (c *Client) SetCredentials(user, password string) {
	c.user, c.pass, c.set = user, password, true
}

// TrustCA makes the client verify the nodes' HTTPS certificates against
// the certificates in pool instead of the system's. Call it before the
// client is used.
func (c *Client) TrustCA(pool *x509.CertPool) error {
	t, ok := c.client.Transport.(*http.Transport)
	if !ok {
		return errors.New("rqlite: this HTTP client has no transport to give a CA to")
	}
	t = t.Clone()
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.RootCAs = pool
	t.TLSClientConfig.MinVersion = tls.VersionTLS12
	cp := *c.client
	cp.Transport = t
	c.client = &cp
	return nil
}

// Security says how the client talks to the cluster: whether every
// configured node is reached over HTTPS, and whether it sends credentials.
func (c *Client) Security() (encrypted, credentials bool) {
	encrypted = true
	for _, n := range c.nodes {
		if n.Scheme != "https" {
			encrypted = false
		}
	}
	return encrypted, c.set
}

// Hosts returns the host names of the configured nodes, without ports.
func (c *Client) Hosts() []string {
	out := make([]string, len(c.nodes))
	for i, n := range c.nodes {
		out[i] = n.Hostname()
	}
	return out
}

// RequiresAuth asks a node for one row without credentials and reports
// whether it refused: whether the cluster is closed to whoever reaches
// its port. It is a read and changes nothing.
func (c *Client) RequiresAuth(ctx context.Context) (bool, error) {
	var lastErr error
	start := c.preferred()
	for i := range c.nodes {
		target := *c.nodes[(start+i)%len(c.nodes)]
		target.Path = strings.TrimSuffix(target.Path, "/") + "/db/query"
		target.RawQuery = "q=" + url.QueryEscape("SELECT 1")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return false, fmt.Errorf("rqlite: %w", err)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = redact(err)
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true, nil
		case http.StatusOK:
			return false, nil
		}
		lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return false, fmt.Errorf("rqlite: %w", lastErr)
}

// ErrUnauthorized is in the error chain when a node refused the request
// for want of credentials it accepts (HTTP 401 or 403).
var ErrUnauthorized = errors.New("the node refused the credentials")

// refused words a 401 or 403. It says what to look at and never what was
// sent.
func (c *Client) refused(status int) error {
	if c.set {
		return fmt.Errorf("rqlite: HTTP %d: %w: the user or password of rqlite.auth_file is not in rqlite's auth file, or lacks a permission (execute, query; backup and load for those commands)", status, ErrUnauthorized)
	}
	return fmt.Errorf("rqlite: HTTP %d: %w: rqlite wants a user and password, and none is configured (rqlite.auth_file)", status, ErrUnauthorized)
}

// configured reports whether a URL is one of the nodes of the config:
// same scheme, same host and port.
func (c *Client) configured(u *url.URL) bool {
	for _, n := range c.nodes {
		if n.Scheme == u.Scheme && strings.EqualFold(n.Host, u.Host) {
			return true
		}
	}
	return false
}

// authorize puts the credentials on a request, if it goes to a configured
// node.
func (c *Client) authorize(req *http.Request) {
	if c.set && c.configured(req.URL) {
		req.SetBasicAuth(c.user, c.pass)
	}
}

// follow decides about a redirect: the URL to go to next, or an error.
// With credentials only a configured node is followed to, since the
// credentials would otherwise go to wherever a node, or somebody
// answering in its place, points.
func (c *Client) follow(from *url.URL, location string, status int) (*url.URL, error) {
	next, err := from.Parse(location)
	if err != nil || location == "" || (next.Scheme != "http" && next.Scheme != "https") {
		return nil, fmt.Errorf("HTTP %d without a usable Location", status)
	}
	next.User = nil
	if c.set && !c.configured(next) {
		return nil, fmt.Errorf("HTTP %d to %s://%s, which is not a node of the config: not followed, the credentials stay here (list every node in rqlite.urls)", status, next.Scheme, next.Host)
	}
	return next, nil
}

// Execute runs the statements as one transaction and returns one result per
// statement. If a statement fails, nothing is applied and the error is an
// *Error naming it. If the cluster did not say whether the batch was
// applied, the error wraps ErrUnknownOutcome and nothing is tried again.
func (c *Client) Execute(ctx context.Context, stmts ...Statement) ([]Result, error) {
	return c.do(ctx, "transaction", stmts, write)
}

// ExecuteIdempotent is Execute for a batch that is safe to run twice: one
// that leaves the same state the second time, or that carries a guard which
// makes the second run fail as a whole. After an unknown outcome it sends
// the batch again. The caller must be ready for the answer of the second
// run: RowsAffected of a repeated DELETE is 0, a repeated guarded insert
// fails its guard.
func (c *Client) ExecuteIdempotent(ctx context.Context, stmts ...Statement) ([]Result, error) {
	return c.do(ctx, "transaction", stmts, idempotent)
}

// Query runs read-only statements at the given consistency level, all against
// the same snapshot. Do not pass statements that write: use Execute.
func (c *Client) Query(ctx context.Context, level Level, stmts ...Statement) ([]Result, error) {
	q := "transaction&level=" + url.QueryEscape(string(level))
	if level == None {
		// A node that lost its leader longer ago than this refuses instead
		// of answering from a copy that stopped moving.
		q += "&freshness=5s"
	}
	return c.do(ctx, q, stmts, read)
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

type mode int

const (
	read mode = iota
	write
	idempotent
)

// outcome is what one attempt on one node told us.
type outcome int

const (
	done    outcome = iota // an answer: results, or a statement error
	notSent                // rqlite never took the request: safe to send again
	unknown                // it may or may not have been applied
)

const (
	backoffStart = 50 * time.Millisecond
	backoffMax   = time.Second
	maxRedirects = 3
)

func (c *Client) do(ctx context.Context, query string, stmts []Statement, m mode) ([]Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(stmts)
	if err != nil {
		return nil, fmt.Errorf("rqlite: %w", err)
	}
	wait := c.Wait
	if wait <= 0 {
		wait = 15 * time.Second
	}
	deadline := time.Now().Add(wait)
	c.mu.Lock()
	node := c.pref
	c.mu.Unlock()

	backoff := backoffStart
	var lastErr error
	for failed := 0; ; {
		target := *c.nodes[node]
		target.Path = strings.TrimSuffix(target.Path, "/") + "/db/request"
		target.RawQuery = query
		results, out, err := c.attempt(ctx, &target, body, len(stmts))
		switch {
		case out != done:
			metrics.DBRequests.Inc("failed")
		case err != nil:
			metrics.DBRequests.Inc("error")
		default:
			metrics.DBRequests.Inc("ok")
		}
		if out == done {
			if failed > 0 || node != c.preferred() {
				c.mu.Lock()
				c.pref = node
				c.mu.Unlock()
			}
			return results, err
		}
		lastErr = err
		if out == unknown && m == write {
			return nil, fmt.Errorf("rqlite: %w: %w", ErrUnknownOutcome, err)
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("rqlite: %w", lastErr)
		}
		// Next node. After a round over all of them, wait before the next.
		failed++
		node = (node + 1) % len(c.nodes)
		if failed%len(c.nodes) == 0 {
			if time.Now().Add(backoff).After(deadline) {
				return nil, fmt.Errorf("rqlite: giving up after %s: %w", wait, lastErr)
			}
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, fmt.Errorf("rqlite: %w", lastErr)
			case <-t.C:
			}
			if backoff *= 2; backoff > backoffMax {
				backoff = backoffMax
			}
		}
	}
}

func (c *Client) preferred() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pref
}

// attempt sends the batch to one node, following its redirects.
func (c *Client) attempt(ctx context.Context, target *url.URL, body []byte, want int) ([]Result, outcome, error) {
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
		if err != nil {
			return nil, done, fmt.Errorf("rqlite: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		c.authorize(req)
		resp, err := c.client.Do(req)
		if err != nil {
			err = redact(err)
			// A connection that was never made carried nothing.
			var op *net.OpError
			var dns *net.DNSError
			if (errors.As(err, &op) && op.Op == "dial") || errors.As(err, &dns) {
				return nil, notSent, err
			}
			return nil, unknown, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		resp.Body.Close()
		if err != nil {
			return nil, unknown, fmt.Errorf("reading response: %w", err)
		}
		if len(data) > maxResponse {
			return nil, done, errors.New("rqlite: response too large")
		}
		switch resp.StatusCode {
		case http.StatusOK:
			return decode(data, want)
		case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			// "Not me, ask the leader": the node did nothing with it.
			if hop == maxRedirects {
				return nil, notSent, fmt.Errorf("HTTP %d: too many redirects", resp.StatusCode)
			}
			next, err := c.follow(target, resp.Header.Get("Location"), resp.StatusCode)
			if err != nil {
				return nil, notSent, err
			}
			target = next
			continue
		case http.StatusUnauthorized, http.StatusForbidden:
			// No other node will think otherwise: they share the file.
			return nil, done, c.refused(resp.StatusCode)
		case http.StatusBadRequest:
			// No other node will think otherwise.
			return nil, done, fmt.Errorf("rqlite: HTTP %d: %s", resp.StatusCode, clip(data))
		}
		err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(data))
		if resp.StatusCode == http.StatusServiceUnavailable && noLeader(data) {
			return nil, notSent, err
		}
		return nil, unknown, err
	}
}

// noLeader recognises rqlite's answer for "this node knows no leader to
// give the request to": an election is running, or the node is cut off.
// Nothing was applied.
func noLeader(body []byte) bool {
	return bytes.Contains(body, []byte("leader not found"))
}

func decode(data []byte, want int) ([]Result, outcome, error) {
	var out struct {
		Error   string       `json:"error"`
		Results []wireResult `json:"results"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, unknown, fmt.Errorf("bad response: %w", err)
	}
	if out.Error != "" {
		// Not about a statement: the node could not run the batch as a
		// whole. "leadership lost while committing log" is the case that
		// matters: the batch may still be committed by the next leader.
		if noLeader([]byte(out.Error)) || strings.Contains(out.Error, "not leader") {
			return nil, notSent, errors.New(out.Error)
		}
		if out.Error == "stale read" {
			// A read at level none on a node that lost its leader longer
			// ago than the freshness bound. That is the answer the caller
			// asked for with this level: it is not worth waiting for.
			return nil, done, ErrStale
		}
		return nil, unknown, errors.New(out.Error)
	}
	results := make([]Result, len(out.Results))
	for i, r := range out.Results {
		if r.Error != "" {
			e := &Error{Index: i, Message: r.Error}
			if r.ErrorV2 != nil && r.ErrorV2.SQLite != nil {
				e.Code = r.ErrorV2.SQLite.ExtendedCode
			}
			return nil, done, e
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
	if len(results) != want {
		return nil, done, fmt.Errorf("rqlite: %d results for %d statements", len(results), want)
	}
	return results, done, nil
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

// stream is the client for Backup and Load: the same transport without the
// per-request timeout, which a large database would outlive.
func (c *Client) stream() *http.Client {
	return &http.Client{Transport: c.client.Transport, CheckRedirect: c.client.CheckRedirect}
}

// transfer sends one request to /db/<path> on the nodes in turn, following
// a redirect to the leader, and hands the first 200 answer to use.
func (c *Client) transfer(ctx context.Context, method, path, contentType string, body func() (io.Reader, int64, error), use func(io.Reader) error) error {
	var lastErr error
	start := c.preferred()
	for i := range c.nodes {
		target := *c.nodes[(start+i)%len(c.nodes)]
		target.Path = strings.TrimSuffix(target.Path, "/") + path
		for hop := 0; hop <= maxRedirects; hop++ {
			var rd io.Reader
			var size int64
			if body != nil {
				var err error
				if rd, size, err = body(); err != nil {
					return fmt.Errorf("rqlite: %w", err)
				}
			}
			req, err := http.NewRequestWithContext(ctx, method, target.String(), rd)
			if err != nil {
				return fmt.Errorf("rqlite: %w", err)
			}
			if body != nil {
				req.ContentLength = size
				req.Header.Set("Content-Type", contentType)
			}
			c.authorize(req)
			resp, err := c.stream().Do(req)
			if err != nil {
				lastErr = redact(err)
				break
			}
			switch resp.StatusCode {
			case http.StatusOK:
				err := use(resp.Body)
				resp.Body.Close()
				if err != nil {
					return fmt.Errorf("rqlite: %w", err)
				}
				return nil
			case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
				loc := resp.Header.Get("Location")
				resp.Body.Close()
				next, err := c.follow(&target, loc, resp.StatusCode)
				if err != nil {
					lastErr = err
					hop = maxRedirects
					continue
				}
				target = *next
				continue
			case http.StatusUnauthorized, http.StatusForbidden:
				resp.Body.Close()
				return c.refused(resp.StatusCode)
			}
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(data))
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("rqlite: %s %s: %w", method, path, lastErr)
}

// Backup writes a copy of the database to w, as one SQLite file. rqlite
// makes it on the leader at one point of its log. When Backup fails, w
// may hold part of a file.
func (c *Client) Backup(ctx context.Context, w io.Writer) error {
	return c.transfer(ctx, http.MethodGet, "/db/backup", "", nil, func(r io.Reader) error {
		_, err := io.Copy(w, r)
		return err
	})
}

// Load replaces the database of the cluster by the SQLite file in r. It
// goes through the log like any write, so every node ends up with it.
func (c *Client) Load(ctx context.Context, r io.ReadSeeker) error {
	body := func() (io.Reader, int64, error) {
		size, err := r.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, 0, err
		}
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return nil, 0, err
		}
		return io.NopCloser(r), size, nil
	}
	return c.transfer(ctx, http.MethodPost, "/db/load", "application/octet-stream", body, func(resp io.Reader) error {
		data, err := io.ReadAll(io.LimitReader(resp, 64<<10))
		if err != nil {
			return err
		}
		// {"results":[]} when it worked; an "error" member otherwise.
		var answer struct {
			Error   string `json:"error"`
			Results []struct {
				Error string `json:"error"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &answer); err != nil {
			return fmt.Errorf("load: unexpected answer %s", clip(data))
		}
		if answer.Error != "" {
			return errors.New("load: " + answer.Error)
		}
		for _, res := range answer.Results {
			if res.Error != "" {
				return errors.New("load: " + res.Error)
			}
		}
		return nil
	})
}

// LoadCredentials reads the user and password for rqlite from a file with
// one line, "user:password". Like a key file it must be readable by its
// owner only. The password may hold colons; the user may not.
func LoadCredentials(path string) (user, password string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("rqlite credentials: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", "", fmt.Errorf("rqlite credentials: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", "", fmt.Errorf("rqlite credentials: %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", "", fmt.Errorf("rqlite credentials: %s has mode %04o, want 0600", path, perm)
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", "", fmt.Errorf("rqlite credentials: %w", err)
	}
	line := strings.TrimRight(string(data), "\r\n")
	user, password, ok := strings.Cut(line, ":")
	if !ok || user == "" || password == "" || strings.ContainsAny(line, "\r\n") {
		// Nothing of the file's content goes into the error.
		return "", "", fmt.Errorf("rqlite credentials: %s does not hold one line \"user:password\"", path)
	}
	return user, password, nil
}

// LoadCA reads the certificates of a PEM file into a pool for TrustCA.
func LoadCA(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rqlite CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("rqlite CA: %s holds no certificate", path)
	}
	return pool, nil
}
