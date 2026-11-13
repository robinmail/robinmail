// Package jmap speaks JMAP: the core protocol of RFC 8620 and the mail data
// model of RFC 8621. It is protocol only. Mail lives behind the Store
// interface, and who is asking is decided by whoever calls the handler and
// passed in through the request context (WithUser).
//
// What exists: the session resource, the API endpoint with result
// references and creation ids, Core/echo, Mailbox/get, /changes, /query,
// /queryChanges and /set, Thread/get and /changes, Email/get, /changes,
// /query, /queryChanges and /set (keywords, mailboxIds and destroy), blob
// download, and a sanitised HTML view of an email that is not part of JMAP.
// Email/set create, Email/import, /copy and /parse, SearchSnippet, upload,
// submission and push are not here yet.
package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/mikepage/robinmail/internal/store"
)

// Capability URIs.
const (
	CapCore = "urn:ietf:params:jmap:core"
	CapMail = "urn:ietf:params:jmap:mail"
)

// Store is what the handler needs from storage. *store.Store has it.
type Store interface {
	State(ctx context.Context, account string) (int64, error)
	ChangesOf(ctx context.Context, account, typ string, since int64, max int) (store.ChangeSet, int64, bool, error)

	Mailboxes(ctx context.Context, account string) ([]store.Mailbox, error)
	NewMailbox(ctx context.Context, account string, n store.NewMailbox) (store.Mailbox, error)
	UpdateMailbox(ctx context.Context, account string, id int64, u store.MailboxUpdate) (int64, error)
	DestroyMailbox(ctx context.Context, account string, id int64, removeEmails bool, ifState int64) (int64, error)

	EmailsByID(ctx context.Context, account string, ids []int64) ([]store.Email, error)
	Threads(ctx context.Context, account string, ids []int64) (map[int64][]int64, error)
	QueryEmails(ctx context.Context, account string, q store.EmailQuery) (store.QueryResult, error)
	UpdateEmail(ctx context.Context, account string, id int64, u store.Update) (int64, error)
	DestroyEmailIf(ctx context.Context, account string, id, ifState int64) (int64, error)
	Raw(ctx context.Context, account string, id int64) ([]byte, store.Email, error)
	OpenBlob(ctx context.Context, account, blobID string) (io.ReadCloser, int64, error)
}

// User is who a request is from. A user has one account, their own.
type User struct {
	AccountID string
	Username  string // the address
}

type userKey struct{}

// WithUser marks a request context as authenticated.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom returns the user WithUser put in the context.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

// Limits are the limits announced in the session object (RFC 8620 2) and
// enforced by the handler.
type Limits struct {
	MaxSizeUpload         int64
	MaxConcurrentUpload   int
	MaxSizeRequest        int64
	MaxConcurrentRequests int
	MaxCallsInRequest     int
	MaxObjectsInGet       int
	MaxObjectsInSet       int
}

// DefaultLimits are used for every zero field of Handler.Limits.
var DefaultLimits = Limits{
	MaxSizeUpload:         50_000_000,
	MaxConcurrentUpload:   4,
	MaxSizeRequest:        10_000_000,
	MaxConcurrentRequests: 4,
	MaxCallsInRequest:     32,
	MaxObjectsInGet:       500,
	MaxObjectsInSet:       500,
}

// Limits that are not part of the core capability.
const (
	// maxQueryLimit is the most ids one /query returns.
	maxQueryLimit = 500
	// maxChangesLimit caps maxChanges of a /queryChanges.
	maxChangesLimit = 5000
	// MaxMailboxDepth is how deep mailboxes may nest (RFC 8621 1.3.1).
	MaxMailboxDepth = 10
	// maxMailboxName is the announced maxSizeMailboxName, in octets.
	maxMailboxName = 255
	// maxFilterNodes bounds the size of a filter.
	maxFilterNodes = 200
	maxFilterDepth = 10
	// maxPathItems bounds the output of a result reference with "*".
	maxPathItems = 100_000
)

// Handler serves the JMAP endpoints. The request context must carry a User.
type Handler struct {
	Store  Store
	Limits Limits

	once     sync.Once
	mux      *http.ServeMux
	limits   Limits
	mu       sync.Mutex
	inFlight map[string]int // account id -> API requests being processed
}

func (h *Handler) init() {
	h.once.Do(func() {
		h.limits = h.Limits
		d := DefaultLimits
		for _, f := range []struct{ v, def *int64 }{{&h.limits.MaxSizeUpload, &d.MaxSizeUpload}, {&h.limits.MaxSizeRequest, &d.MaxSizeRequest}} {
			if *f.v <= 0 {
				*f.v = *f.def
			}
		}
		for _, f := range []struct{ v, def *int }{{&h.limits.MaxConcurrentUpload, &d.MaxConcurrentUpload},
			{&h.limits.MaxConcurrentRequests, &d.MaxConcurrentRequests}, {&h.limits.MaxCallsInRequest, &d.MaxCallsInRequest},
			{&h.limits.MaxObjectsInGet, &d.MaxObjectsInGet}, {&h.limits.MaxObjectsInSet, &d.MaxObjectsInSet}} {
			if *f.v <= 0 {
				*f.v = *f.def
			}
		}
		h.inFlight = map[string]int{}
		h.mux = http.NewServeMux()
		h.mux.HandleFunc("GET /.well-known/jmap", h.serveSession)
		h.mux.HandleFunc("GET /jmap/session", h.serveSession)
		h.mux.HandleFunc("POST /jmap/api", h.serveAPI)
		h.mux.HandleFunc("GET /jmap/download/{accountId}/{blobId}/{name}", h.serveDownload)
		h.mux.HandleFunc("GET /jmap/view/{accountId}/{emailId}", h.serveView)
		h.mux.HandleFunc("/jmap/upload/", h.serveNotYet)
		h.mux.HandleFunc("/jmap/eventsource/", h.serveNotYet)
	})
}

// Paths are the URL path prefixes this handler serves.
var Paths = []string{"/.well-known/jmap", "/jmap/"}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.init()
	if _, ok := UserFrom(r.Context()); !ok {
		problem(w, http.StatusUnauthorized, "about:blank", "authentication required", nil)
		return
	}
	// Nothing here may be cached by anyone but the user's own browser.
	w.Header().Set("Cache-Control", "no-store")
	h.mux.ServeHTTP(w, r)
}

// problem writes a problem details object (RFC 7807), which is how RFC 8620
// 3.6.1 reports request-level errors.
func problem(w http.ResponseWriter, status int, typ, detail string, extra map[string]any) {
	body := map[string]any{"type": typ, "status": status, "detail": detail}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// baseURL is the origin the client used, for the URLs in the session.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// sessionState changes when the session object would (RFC 8620 2). Ours
// depends on the user and on the limits and capabilities of this build.
func (h *Handler) sessionState(u User) string {
	b, _ := json.Marshal([]any{u, h.limits, store.SortProperties, MaxMailboxDepth, "1"})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// serveSession is the JMAP Session resource (RFC 8620 2).
func (h *Handler) serveSession(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	base := baseURL(r)
	l := h.limits
	mail := map[string]any{
		"maxMailboxesPerEmail":       nil,
		"maxMailboxDepth":            MaxMailboxDepth,
		"maxSizeMailboxName":         maxMailboxName,
		"maxSizeAttachmentsPerEmail": l.MaxSizeUpload,
		"emailQuerySortOptions":      store.SortProperties,
		"mayCreateTopLevelMailbox":   true,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capabilities": map[string]any{
			CapCore: map[string]any{
				"maxSizeUpload":         l.MaxSizeUpload,
				"maxConcurrentUpload":   l.MaxConcurrentUpload,
				"maxSizeRequest":        l.MaxSizeRequest,
				"maxConcurrentRequests": l.MaxConcurrentRequests,
				"maxCallsInRequest":     l.MaxCallsInRequest,
				"maxObjectsInGet":       l.MaxObjectsInGet,
				"maxObjectsInSet":       l.MaxObjectsInSet,
				"collationAlgorithms":   []string{"i;ascii-casemap"},
			},
			CapMail: map[string]any{},
		},
		"accounts": map[string]any{
			u.AccountID: map[string]any{
				"name":       u.Username,
				"isPersonal": true,
				"isReadOnly": false,
				"accountCapabilities": map[string]any{
					CapCore: map[string]any{},
					CapMail: mail,
				},
			},
		},
		"primaryAccounts": map[string]any{CapCore: u.AccountID, CapMail: u.AccountID},
		"username":        u.Username,
		"apiUrl":          base + "/jmap/api",
		"downloadUrl":     base + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"uploadUrl":       base + "/jmap/upload/{accountId}/",
		"eventSourceUrl":  base + "/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
		"state":           h.sessionState(u),
	})
}

// serveNotYet answers the endpoints the session has to name (RFC 8620 2)
// but that later phases build: upload and push.
func (h *Handler) serveNotYet(w http.ResponseWriter, r *http.Request) {
	problem(w, http.StatusNotImplemented, "about:blank", "this server does not have uploads and push yet", nil)
}

// State strings are the account's modseq in decimal.
func stateString(n int64) string { return strconv.FormatInt(n, 10) }

func parseState(s string) (int64, bool) {
	if s == "" || len(s) > 19 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
