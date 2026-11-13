// Package web is the HTTP side of robinmail: one origin that serves the
// login endpoints under /auth, JMAP under /jmap and /.well-known/jmap, and
// the static webmail everywhere else.
//
// It decides who a request is from and hands JMAP requests on with the
// user in their context. A browser logs in once and holds a session cookie
// (Secure, HttpOnly, SameSite=Strict); the session is a row in the store,
// so any node accepts it. Other JMAP clients send their password with
// every request (Basic). Every request to /auth and JMAP has its origin
// checked, logins are rate-limited per address and per IP, and every
// response carries the security headers below.
package web

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
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
	CreateSession(ctx context.Context, tokenHash, account string, expires time.Time) error
	SessionAccount(ctx context.Context, tokenHash string) (store.Account, error)
	DeleteSession(ctx context.Context, tokenHash string) error
}

// SessionTTL is how long a login lasts.
const SessionTTL = 30 * 24 * time.Hour

// Server is the HTTP handler of one node. Fill the exported fields, then
// use Handler or HTTPServer.
type Server struct {
	// Hostname is the name the server is reached under. Requests for
	// another host are refused, except localhost on a plain listener.
	Hostname string
	Accounts Accounts
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
	// Now is the clock (time.Now when nil).
	Now func() time.Time

	once       sync.Once
	static     fs.FS
	devHosts   map[string]bool
	byAddress  *limiter
	byIP       *limiter
	hashing    chan struct{} // bounds password checks running at once
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
	loginWindow       = 15 * time.Minute
	maxFailsPerAddr   = 10
	maxFailsPerIP     = 50
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
		s.byIP = newLimiter(maxFailsPerIP, loginWindow, s.Now)
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
	mux.HandleFunc("POST /auth/login", s.login)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /auth/session", s.session)
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/login", "/auth/logout":
			w.Header().Set("Allow", "POST")
			jsonError(w, http.StatusMethodNotAllowed, "use POST")
		case "/auth/session":
			w.Header().Set("Allow", "GET")
			jsonError(w, http.StatusMethodNotAllowed, "use GET")
		default:
			jsonError(w, http.StatusNotFound, "no such endpoint")
		}
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
	acct, status := s.authenticate(w, r)
	if status != 0 {
		// Only a client that is not a browser is told to send Basic: in a
		// browser that header opens a password dialog.
		if status == http.StatusUnauthorized && r.Header.Get("Sec-Fetch-Site") == "" && r.Header.Get("Origin") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="robinmail", charset="UTF-8"`)
		}
		jsonError(w, status, http.StatusText(status))
		return
	}
	s.JMAP.ServeHTTP(w, r.WithContext(s.WithUser(r.Context(), acct.ID, acct.Address)))
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
