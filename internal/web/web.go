// Package web is the HTTP side of robinmail: one origin that serves the
// login endpoints under /auth, JMAP under /jmap and /.well-known/jmap, and
// the static webmail everywhere else.
//
// It decides who a request is from and hands JMAP requests on with the
// user in their context. A browser logs in once, with a password and, if
// the account has one, a TOTP code, or through an OpenID Connect provider,
// and holds a session cookie (Secure, HttpOnly, SameSite=Strict); the
// session is a row in the store, so any node accepts it. Other JMAP
// clients send an app password with every request (Basic). The same
// checks serve IMAP and SMTP submission (MailLogin). Every request to
// /auth and JMAP has its origin checked, logins are rate-limited per
// address and per IP, and every response carries the security headers
// below.
package web

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/users"
)

// Accounts is what the server needs from the store. *store.Store has it.
type Accounts interface {
	Account(ctx context.Context, address string) (store.Account, error)
	AccountByID(ctx context.Context, id string) (store.Account, error)
	SetPassword(ctx context.Context, account, oldHash, newHash, keepTokenHash string) error

	CreateSession(ctx context.Context, tokenHash string, sess store.Session, replaces string) error
	SessionByToken(ctx context.Context, tokenHash string) (store.Session, store.Account, error)
	TouchSession(ctx context.Context, tokenHash, ip string) error
	DeleteSession(ctx context.Context, tokenHash string) error
	Sessions(ctx context.Context, account string) ([]store.Session, error)
	RevokeSession(ctx context.Context, account, id string) (bool, error)
	RevokeSessions(ctx context.Context, account, exceptTokenHash string) (int64, error)

	CreateAppPassword(ctx context.Context, account, id, name, hash string, scopes []string) error
	AppPasswords(ctx context.Context, account string) ([]store.AppPassword, error)
	AppPasswordByHash(ctx context.Context, account, hash string) (store.AppPassword, error)
	AppPassword(ctx context.Context, account, id string) (store.AppPassword, error)
	UseAppPassword(ctx context.Context, account, id, ip string) error
	RevokeAppPassword(ctx context.Context, account, id string) (bool, error)

	SetPendingTOTP(ctx context.Context, account string, secret []byte) error
	TOTPState(ctx context.Context, account string) (store.TOTP, error)
	ConfirmTOTP(ctx context.Context, account string, step int64, recoveryHashes []string) error
	ReplaceRecoveryCodes(ctx context.Context, account string, hashes []string) error
	UseTOTPStep(ctx context.Context, account string, step int64) (bool, error)
	UseRecoveryCode(ctx context.Context, account, hash string) (bool, error)
	DisableTOTP(ctx context.Context, account string) (bool, error)
}

// Limits counts failed logins for all nodes together. *store.Store has it.
type Limits interface {
	LoginFailures(ctx context.Context, window time.Duration, keys ...string) (map[string]store.LoginFailure, error)
	AddLoginFailure(ctx context.Context, window time.Duration, keys ...string) error
	ClearLoginFailures(ctx context.Context, keys ...string) error
}

// Audit is the log of auth events. *store.Store has it.
type Audit interface {
	AddAuthEvent(ctx context.Context, e store.AuthEvent) error
}

// AuthChanges tells when a way into an account was withdrawn: a
// *notify.Hub that follows the accounts' auth_seq.
type AuthChanges interface {
	Subscribe(account string) (<-chan struct{}, func())
}

// Server is the HTTP handler of one node. Fill the exported fields, then
// use Handler or HTTPServer.
type Server struct {
	// Hostname is the name the server is reached under. Requests for
	// another host are refused, except localhost on a plain listener.
	Hostname string
	Accounts Accounts
	// Limits shares the login failure counters between the nodes. nil
	// leaves only this node's own, in memory.
	Limits Limits
	// Audit takes the auth events. nil writes none.
	Audit Audit
	// AuthChanges, when set, ends an event stream whose session or app
	// password was revoked, on whatever node that happened.
	AuthChanges AuthChanges
	// RequireAppPasswords refuses the account password for IMAP, SMTP
	// submission and Basic auth for every account, also those without an
	// app password.
	RequireAppPasswords bool
	// OIDC lists the providers the login page offers.
	OIDC []*OIDCProvider
	// JMAP serves /jmap/ and /.well-known/jmap. It gets only requests of
	// an authenticated user, with the user in the context.
	JMAP http.Handler
	// WithUser puts the authenticated account into a request context, in
	// the form JMAP wants.
	WithUser func(ctx context.Context, accountID, username string) context.Context
	// Static is the webmail: index.html and its assets. nil serves a
	// placeholder page.
	Static fs.FS
	// DevOrigins are origins besides our own that may send requests, such
	// as "http://localhost:5173" for the Vite dev server. For development
	// only.
	DevOrigins []string
	Logger     *slog.Logger

	// Verify checks a password against a stored hash (users.Verify when
	// nil). An empty hash is an unknown account.
	Verify func(hash, password string) bool
	// HashPassword makes the hash of a new password (users.Hash when nil).
	HashPassword func(password string) (string, error)
	// Now is the clock (time.Now when nil).
	Now func() time.Time

	once       sync.Once
	static     fs.FS
	devHosts   map[string]bool
	byAddress  *limiter // wrong account passwords at the web login
	byMail     *limiter // wrong secrets from mail clients
	byCode     *limiter // wrong TOTP and recovery codes
	byIP       *limiter
	usedMu     sync.Mutex
	used       map[string]time.Time // when the use of an app password was last written
	hashing    chan struct{}        // bounds password checks running at once
	basic      *basicCache
	fileServer http.Handler
}

//go:embed placeholder
var placeholder embed.FS

// Placeholder is the page served until the webmail is built in.
func Placeholder() fs.FS {
	sub, err := fs.Sub(placeholder, "placeholder")
	if err != nil {
		panic(err)
	}
	return sub
}

// Login rate limits: failed attempts per window.
const (
	LoginWindow     = 15 * time.Minute
	loginWindow     = LoginWindow
	maxFailsPerAddr = 10
	maxFailsPerIP   = 50
	// maxFailsPerCode is for the second step. Five wrong codes in the
	// window leave a guesser, with three codes valid at any moment, a
	// chance of 15 in a million per window.
	maxFailsPerCode = 5
	// recoveryCodes is how many recovery codes a set has.
	recoveryCodes  = 10
	maxPasswordLen = 1024
	// lastUseEvery is how often the use of an app password is written.
	lastUseEvery      = 10 * time.Minute
	maxHashing        = 4 // Argon2id takes 64 MiB per check
	maxLoginBody      = 4 << 10
	basicCacheTTL     = 5 * time.Minute
	basicCacheEntries = 10_000
)

func (s *Server) init() {
	s.once.Do(func() {
		if s.Verify == nil {
			s.Verify = users.Verify
		}
		if s.HashPassword == nil {
			s.HashPassword = users.Hash
		}
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.Logger == nil {
			s.Logger = slog.New(slog.DiscardHandler)
		}
		s.static = s.Static
		if s.static == nil {
			s.static = Placeholder()
		}
		s.fileServer = http.FileServerFS(s.static)
		s.devHosts = map[string]bool{}
		for _, o := range s.DevOrigins {
			if u, err := url.Parse(o); err == nil {
				s.devHosts[u.Hostname()] = true
			}
		}
		s.byAddress = newLimiter(maxFailsPerAddr, loginWindow, s.Now)
		s.byMail = newLimiter(maxFailsPerAddr, loginWindow, s.Now)
		s.byCode = newLimiter(maxFailsPerCode, loginWindow, s.Now)
		s.byIP = newLimiter(maxFailsPerIP, loginWindow, s.Now)
		s.used = map[string]time.Time{}
		s.hashing = make(chan struct{}, maxHashing)
		s.basic = newBasicCache(s.Now)
	})
}

// HTTPServer returns an http.Server for the handler, with timeouts and
// header limits set. A download of a large message has ten minutes.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.Logger.Handler(), slog.LevelWarn),
	}
}

// Handler returns the handler for all paths.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	// Everything under /auth goes through guard: the origin check, and
	// for a request that changes something the JSON content type.
	allowed := map[string][]string{} // path pattern -> methods
	auth := func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		allowed[path] = append(allowed[path], method)
		mux.HandleFunc(pattern, s.guard(h))
	}
	auth("POST /auth/login", s.login)
	auth("POST /auth/login/totp", s.loginTOTP)
	auth("POST /auth/logout", s.logout)
	auth("GET /auth/session", s.session)
	auth("GET /auth/config", s.config)
	auth("POST /auth/password", s.authed(s.changePassword))
	auth("GET /auth/app-passwords", s.authed(s.appPasswords))
	auth("POST /auth/app-passwords", s.authed(s.createAppPassword))
	auth("DELETE /auth/app-passwords/{id}", s.authed(s.revokeAppPassword))
	auth("GET /auth/totp", s.authed(s.totpState))
	auth("POST /auth/totp/enrol", s.authed(s.totpEnrol))
	auth("POST /auth/totp/confirm", s.authed(s.totpConfirm))
	auth("POST /auth/totp/disable", s.authed(s.totpDisable))
	auth("POST /auth/totp/recovery-codes", s.authed(s.totpRecoveryCodes))
	auth("GET /auth/sessions", s.authed(s.sessions))
	auth("DELETE /auth/sessions/{id}", s.authed(s.revokeSession))
	auth("POST /auth/sessions/revoke-others", s.authed(s.revokeOtherSessions))
	s.oidcRoutes(mux)
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		// What arrives here matched no endpoint with its method.
		for pattern, methods := range allowed {
			if pathMatches(pattern, r.URL.Path) {
				w.Header().Set("Allow", strings.Join(methods, ", "))
				jsonError(w, http.StatusMethodNotAllowed, "use "+strings.Join(methods, " or "))
				return
			}
		}
		jsonError(w, http.StatusNotFound, "no such endpoint")
	})
	mux.HandleFunc("/jmap/", s.jmap)
	mux.HandleFunc("/.well-known/jmap", s.jmap)
	mux.HandleFunc("/", s.serveStatic)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.securityHeaders(w, r)
		if !s.hostAllowed(r) {
			// Not a name of ours: a stale DNS record, or a page that
			// rebinds its own name to this address to get at the API.
			http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// appCSP is the policy of the webmail's own pages. Scripts and styles only
// from this origin (the UI library sets inline styles), no plugins, no
// framing by others. The sanitised HTML view of a message replaces it with
// a stricter one of its own.
const appCSP = "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; font-src 'self' data:; " +
	"connect-src 'self'; frame-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets what every response carries. A handler may replace
// one, as the HTML view does for its policy and framing.
func (s *Server) securityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", appCSP)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	if r.TLS != nil {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

// hostname is the host of a Host header or URL host without its port.
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func isLoopbackName(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// hostAllowed checks the Host header: our hostname, or on a plain
// listener, which only exists for development, a loopback name or the
// host of a configured dev origin.
func (s *Server) hostAllowed(r *http.Request) bool {
	h := hostname(r.Host)
	if h == strings.ToLower(s.Hostname) {
		return true
	}
	return r.TLS == nil && (isLoopbackName(h) || s.devHosts[h])
}

// ownOrigin is the origin of the request's own URL.
func ownOrigin(r *http.Request) string {
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// originAllowed is the cross-site request check. A browser names the page
// a request comes from in Origin (always on a POST) and says how the two
// relate in Sec-Fetch-Site; the session cookie is SameSite=Strict on top.
//
//   - With an Origin: it must be this origin or a configured dev origin.
//   - Without one, with Sec-Fetch-Site: it must be "same-origin", or "none"
//     for a GET the user typed or bookmarked.
//   - With neither, the client is not a browser, and no page can make a
//     browser send such a request with the user's cookie.
func (s *Server) originAllowed(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		if strings.EqualFold(origin, ownOrigin(r)) {
			return true
		}
		for _, o := range s.DevOrigins {
			if strings.EqualFold(origin, o) {
				return true
			}
		}
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin":
		return true
	case "none":
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	}
	return false
}

// pathMatches reports whether a path fits a mux pattern's path, in which
// "{name}" stands for one segment.
func pathMatches(pattern, path string) bool {
	ps, xs := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(ps) != len(xs) {
		return false
	}
	for i, p := range ps {
		if !(strings.HasPrefix(p, "{") && xs[i] != "") && p != xs[i] {
			return false
		}
	}
	return true
}

// guard is in front of every endpoint under /auth. It refuses requests
// from other sites, and requests that change something unless their
// content type is application/json, body or not: a page on another site
// can make a browser send a form or a plain DELETE, but not that header,
// without this server allowing it first (which it never does).
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.originAllowed(r) {
			jsonError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				jsonError(w, http.StatusUnsupportedMediaType, "the body must be application/json")
				return
			}
		}
		h(w, r)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// jmap authenticates a request and passes it to the JMAP handler.
func (s *Server) jmap(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.originAllowed(r) {
		jsonError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	acct, status, valid := s.authenticate(w, r)
	if status != 0 {
		// Only a client that is not a browser is told to send Basic: in a
		// browser that header opens a password dialog.
		if status == http.StatusUnauthorized && r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Origin") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="robinmail", charset="UTF-8"`)
		}
		jsonError(w, status, http.StatusText(status))
		return
	}
	ctx := r.Context()
	if s.AuthChanges != nil && valid != nil && strings.HasPrefix(r.URL.Path, "/jmap/eventsource") {
		// An event stream stays open for hours. It ends when the session
		// or app password it was opened with is revoked: the account's
		// auth_seq moves, on any node, and the login is looked at again.
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		changes, stop := s.AuthChanges.Subscribe(acct.ID)
		defer stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-changes:
					if !valid(ctx) {
						cancel()
						return
					}
				}
			}
		}()
	}
	s.JMAP.ServeHTTP(w, r.WithContext(s.WithUser(ctx, acct.ID, acct.Address)))
}

// serveStatic serves the webmail. A path that is not a file is a route of
// the single-page app and gets index.html.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	info, err := fs.Stat(s.static, name)
	switch {
	case err == nil && !info.IsDir():
	case errors.Is(err, fs.ErrNotExist) || (err == nil && info.IsDir()):
		if strings.Contains(path.Base(name), ".") {
			http.NotFound(w, r) // a missing asset is missing
			return
		}
		name = "index.html"
	default:
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if name == "index.html" {
		// The entry point names the current assets: always revalidate.
		w.Header().Set("Cache-Control", "no-cache")
		data, err := fs.ReadFile(s.static, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		w.Write(data)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		// Vite names assets by their content.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + name
	s.fileServer.ServeHTTP(w, r2)
}
