package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/store"
)

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// cookieName is the session cookie. On HTTPS it has the "__Host-" prefix,
// with which a browser only accepts it as Secure, for this host alone and
// for all paths, so another site on a sibling domain cannot plant one.
func cookieName(r *http.Request) string {
	if r.TLS != nil {
		return "__Host-robinmail"
	}
	return "robinmail"
}

// tokenHash is what the session table holds instead of the token.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName(r),
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   r.TLS != nil, // the plain listener is for development on localhost
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// check verifies a password. The number of Argon2id computations running
// at once is bounded: each takes 64 MiB. ok is false if the request went
// away while waiting for a slot.
func (s *Server) check(r *http.Request, hash, password string) (match, ok bool) {
	select {
	case s.hashing <- struct{}{}:
	case <-r.Context().Done():
		return false, false
	}
	defer func() { <-s.hashing }()
	return s.Verify(hash, password), true
}

// credentials checks an address and password, with the rate limits. It
// returns the account, or the HTTP status to refuse with and how long the
// client should wait.
func (s *Server) credentials(r *http.Request, username, password string) (store.Account, int, time.Duration) {
	ip := clientIP(r)
	// The limiter key of an address that does not parse is the text itself,
	// cut: guessing at nonsense names is limited like guessing at real ones.
	key := username
	if len(key) > 320 {
		key = key[:320]
	}
	addr, perr := address.Parse(username)
	if perr == nil {
		key = addr.String()
	}
	if wait, blocked := s.byIP.blocked(ip); blocked {
		return store.Account{}, http.StatusTooManyRequests, wait
	}
	if wait, blocked := s.byAddress.blocked(key); blocked {
		return store.Account{}, http.StatusTooManyRequests, wait
	}
	shared, wait, blocked := s.sharedFailures(r, key, ip)
	if blocked {
		return store.Account{}, http.StatusTooManyRequests, wait
	}

	var acct store.Account
	hash := "" // an unknown account costs as much time as a wrong password
	if perr == nil {
		var err error
		acct, err = s.Accounts.Account(r.Context(), addr.String())
		switch {
		case err == nil:
			hash = acct.PasswordHash
		case !errors.Is(err, store.ErrNoAccount):
			s.Logger.Error("login: looking up the account failed", "err", err)
			return store.Account{}, http.StatusServiceUnavailable, 0
		}
	}
	match, ok := s.check(r, hash, password)
	if !ok {
		return store.Account{}, http.StatusServiceUnavailable, 0
	}
	if !match {
		s.byIP.fail(ip)
		s.byAddress.fail(key)
		if s.Limits != nil {
			if err := s.Limits.AddLoginFailure(r.Context(), loginWindow, addressKey(key), ipKey(ip)); err != nil {
				s.Logger.Warn("login: counting the failure for the other nodes failed", "err", err)
			}
		}
		s.Logger.Warn("login failed", "address", key, "ip", ip)
		return store.Account{}, http.StatusUnauthorized, 0
	}
	s.byAddress.reset(key)
	// Only when there is something to forget: most logins have no failures
	// behind them and then cost no write.
	if _, had := shared[addressKey(key)]; had {
		if err := s.Limits.ClearLoginFailures(r.Context(), addressKey(key)); err != nil {
			s.Logger.Warn("login: clearing the failures failed", "err", err)
		}
	}
	return acct, 0, 0
}

// Keys of the shared counters.
func addressKey(address string) string { return "a:" + address }
func ipKey(ip string) string           { return "i:" + ip }

// sharedFailures asks the counters all nodes share whether the address or
// the IP is locked out. The limits are the same as for this node's own
// counters in memory, which stay as they are: they are what is left when
// rqlite does not answer, and then the limits hold per node again.
//
// The cost for a login is one read that the nearest rqlite node answers
// from its own copy (store.LoginFailures), next to an Argon2id check that
// takes a hundred times longer. Only a failed login writes.
func (s *Server) sharedFailures(r *http.Request, key, ip string) (map[string]store.LoginFailure, time.Duration, bool) {
	if s.Limits == nil {
		return nil, 0, false
	}
	fails, err := s.Limits.LoginFailures(r.Context(), loginWindow, addressKey(key), ipKey(ip))
	if err != nil {
		s.Logger.Warn("login: reading the shared failure counters failed, using this node's own", "err", err)
		return nil, 0, false
	}
	for k, max := range map[string]int{addressKey(key): maxFailsPerAddr, ipKey(ip): maxFailsPerIP} {
		if f, ok := fails[k]; ok && f.Count >= max {
			if left := f.Started.Add(loginWindow).Sub(s.Now()); left > 0 {
				return fails, left, true
			}
		}
	}
	return fails, 0, false
}

// login is POST /auth/login with {"username": "...", "password": "..."}.
// On success it sets the session cookie and answers with the account.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		jsonError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	// JSON only: a form on another site cannot send that without asking.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		jsonError(w, http.StatusUnsupportedMediaType, "the body must be application/json")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBody+1))
	var req struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
	}
	if err != nil || len(body) > maxLoginBody || json.Unmarshal(body, &req) != nil || req.Username == nil || req.Password == nil {
		jsonError(w, http.StatusBadRequest, `the body must be {"username": "...", "password": "..."}`)
		return
	}
	acct, status, wait := s.credentials(r, *req.Username, *req.Password)
	switch status {
	case 0:
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		jsonError(w, status, "too many failed logins; try again later")
		return
	case http.StatusUnauthorized:
		// The same answer for an unknown address and a wrong password.
		jsonError(w, status, "wrong username or password")
		return
	default:
		jsonError(w, status, "the server cannot log you in right now")
		return
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		jsonError(w, http.StatusInternalServerError, "the server cannot log you in right now")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := s.Accounts.CreateSession(r.Context(), tokenHash(token), acct.ID, s.Now().Add(SessionTTL)); err != nil {
		s.Logger.Error("login: storing the session failed", "err", err)
		jsonError(w, http.StatusServiceUnavailable, "the server cannot log you in right now")
		return
	}
	s.setCookie(w, r, token, int(SessionTTL/time.Second))
	s.Logger.Info("login", "address", acct.Address, "ip", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"accountId": acct.ID, "username": acct.Address})
}

// logout is POST /auth/logout: it ends the session of the cookie.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		jsonError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if c, err := r.Cookie(cookieName(r)); err == nil && c.Value != "" {
		if err := s.Accounts.DeleteSession(r.Context(), tokenHash(c.Value)); err != nil {
			s.Logger.Error("logout: removing the session failed", "err", err)
			jsonError(w, http.StatusServiceUnavailable, "the server cannot log you out right now")
			return
		}
	}
	s.setCookie(w, r, "", -1)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// session is GET /auth/session: who the cookie belongs to, or 401.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	if !s.originAllowed(r) {
		jsonError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	acct, ok, err := s.cookieAccount(r)
	switch {
	case err != nil:
		jsonError(w, http.StatusServiceUnavailable, "the server cannot check the session right now")
	case !ok:
		jsonError(w, http.StatusUnauthorized, "not logged in")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"accountId": acct.ID, "username": acct.Address})
	}
}

// cookieAccount returns the account of the request's session cookie.
func (s *Server) cookieAccount(r *http.Request) (store.Account, bool, error) {
	c, err := r.Cookie(cookieName(r))
	if err != nil || c.Value == "" || len(c.Value) > 200 {
		return store.Account{}, false, nil
	}
	acct, err := s.Accounts.SessionAccount(r.Context(), tokenHash(c.Value))
	if errors.Is(err, store.ErrNoSession) {
		return store.Account{}, false, nil
	}
	if err != nil {
		return store.Account{}, false, err
	}
	return acct, true, nil
}

// authenticate finds the account of a JMAP request: by session cookie, or
// by Basic credentials, which standard JMAP clients send with each
// request. It returns 0 or the status to refuse with.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (store.Account, int) {
	acct, ok, err := s.cookieAccount(r)
	if err != nil {
		s.Logger.Error("looking up the session failed", "err", err)
		return store.Account{}, http.StatusServiceUnavailable
	}
	if ok {
		return acct, 0
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return store.Account{}, http.StatusUnauthorized
	}
	// A password that was just verified is not hashed again for every
	// request of a client: Argon2id is slow on purpose.
	if acct, ok := s.basic.get(username, password); ok {
		return acct, 0
	}
	acct, status, wait := s.credentials(r, username, password)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
	}
	if status != 0 {
		return store.Account{}, status
	}
	s.basic.put(username, password, acct)
	return acct, 0
}

// limiter counts failures per key in a fixed window and blocks a key that
// reached the maximum until its window ends. It lives in memory: per node,
// and gone at a restart. The counters all nodes share are in rqlite
// (sharedFailures); this one is the fallback for when rqlite is away.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	m      map[string]*bucket
}

type bucket struct {
	count int
	start time.Time
}

// limiterMaxKeys bounds the memory of a limiter under a flood of keys.
const limiterMaxKeys = 100_000

func newLimiter(max int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{max: max, window: window, now: now, m: map[string]*bucket{}}
}

// blocked reports whether the key is locked out and for how long.
func (l *limiter) blocked(key string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[key]
	if !ok {
		return 0, false
	}
	left := b.start.Add(l.window).Sub(l.now())
	if left <= 0 {
		delete(l.m, key)
		return 0, false
	}
	return left, b.count >= l.max
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.m[key]
	if !ok || !now.Before(b.start.Add(l.window)) {
		if len(l.m) >= limiterMaxKeys {
			for k, old := range l.m {
				if !now.Before(old.start.Add(l.window)) {
					delete(l.m, k)
				}
			}
			if len(l.m) >= limiterMaxKeys {
				// Still full of live keys: start over rather than grow.
				l.m = map[string]*bucket{}
			}
		}
		b = &bucket{start: now}
		l.m[key] = b
	}
	b.count++
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

// basicCache remembers, for a few minutes, that a username and password
// were verified. It holds no password: the key is an HMAC of both under a
// key that exists only in this process.
type basicCache struct {
	mu  sync.Mutex
	key [32]byte
	now func() time.Time
	m   map[string]basicEntry
}

type basicEntry struct {
	acct    store.Account
	expires time.Time
}

func newBasicCache(now func() time.Time) *basicCache {
	c := &basicCache{now: now, m: map[string]basicEntry{}}
	rand.Read(c.key[:])
	return c
}

func (c *basicCache) id(username, password string) string {
	mac := hmac.New(sha256.New, c.key[:])
	mac.Write([]byte(strconv.Itoa(len(username)) + ":" + username + password))
	return string(mac.Sum(nil))
}

func (c *basicCache) get(username, password string) (store.Account, bool) {
	id := c.id(username, password)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || !c.now().Before(e.expires) {
		delete(c.m, id)
		return store.Account{}, false
	}
	return e.acct, true
}

func (c *basicCache) put(username, password string, acct store.Account) {
	id := c.id(username, password)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= basicCacheEntries {
		c.m = map[string]basicEntry{}
	}
	acct.PasswordHash = ""
	c.m[id] = basicEntry{acct: acct, expires: c.now().Add(basicCacheTTL)}
}
