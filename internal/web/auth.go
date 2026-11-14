package web

import (
	"context"
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
	"github.com/mikepage/robinmail/internal/users"
)

// This file checks credentials: the account password for the web login,
// and for mail clients (IMAP, SMTP submission, JMAP with Basic auth) an app
// password, or the account password while the account may still use that
// there. login.go has the web login's steps and the sessions, settings.go
// what a logged-in user changes, oidc.go the login through a provider.
//
// Which secret opens which door:
//
//	                   web login   IMAP, SMTP, Basic
//	account password   yes         only while the account has no app
//	                               password and no TOTP, and the config
//	                               does not require app passwords
//	app password       no          yes, within its scope
//	TOTP code          second step no
//
// The account password stops working for mail clients with the first app
// password, so that it is typed into one place only, and with TOTP,
// because a second factor that a mail client can walk around is none.

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// readJSON reads the body of a request that changes something. It must be
// JSON and say so: a form on another site cannot send that content type
// without the browser asking this server first, which is half of the
// protection against cross-site requests (the Origin check is the other).
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		jsonError(w, http.StatusUnsupportedMediaType, "the body must be application/json")
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLoginBody+1))
	if err != nil || len(body) > maxLoginBody || json.Unmarshal(body, v) != nil {
		jsonError(w, http.StatusBadRequest, "the body is not the JSON object this endpoint takes")
		return false
	}
	return true
}

// tokenHash is what the session table holds instead of the token.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newToken makes a session token: 256 random bits.
func newToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// newID makes a random id for something a user sees in a list.
func newID() (string, error) {
	var raw [9]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// check verifies a password. The number of Argon2id computations running
// at once is bounded: each takes 64 MiB. ok is false if the request went
// away while waiting for a slot.
func (s *Server) check(ctx context.Context, hash, password string) (match, ok bool) {
	select {
	case s.hashing <- struct{}{}:
	case <-ctx.Done():
		return false, false
	}
	defer func() { <-s.hashing }()
	if hash == store.NoPassword {
		// An account without a password costs what an unknown one does.
		hash = ""
	}
	return s.Verify(hash, password), true
}

// A limit counts failures under one key: in this node's memory, and in
// the counters all nodes share.
type limit struct {
	mem    *limiter
	key    string // in memory
	shared string // in the shared counters
	max    int
}

// Keys of the shared counters. A wrong account password at the web login,
// a wrong secret from a mail client and a wrong second step are counted
// apart: a phone with a stale password must not close the webmail, and
// somebody guessing codes must not be able to hide among mail clients.
func addressKey(address string) string { return "a:" + address }
func mailKey(address string) string    { return "m:" + address }
func codeKey(address string) string    { return "t:" + address }
func ipKey(ip string) string           { return "i:" + ip }

func (s *Server) limitWeb(key string) limit {
	return limit{s.byAddress, key, addressKey(key), maxFailsPerAddr}
}
func (s *Server) limitMail(key string) limit {
	return limit{s.byMail, key, mailKey(key), maxFailsPerAddr}
}
func (s *Server) limitCode(key string) limit {
	return limit{s.byCode, key, codeKey(key), maxFailsPerCode}
}
func (s *Server) limitIP(ip string) limit { return limit{s.byIP, ip, ipKey(ip), maxFailsPerIP} }

// blocked asks whether any of the limits is reached, and for how long. It
// also returns what the shared counters hold, for cleared.
//
// The shared counters are the ones that count: this node's own, in memory,
// are what is left when rqlite does not answer, and then the limits hold
// per node. The cost for a login is one read that the nearest rqlite node
// answers from its own copy (store.LoginFailures). Only a failure writes.
func (s *Server) blocked(ctx context.Context, limits ...limit) (map[string]store.LoginFailure, time.Duration, bool) {
	for _, l := range limits {
		if wait, blocked := l.mem.blocked(l.key); blocked {
			return nil, wait, true
		}
	}
	if s.Limits == nil {
		return nil, 0, false
	}
	keys := make([]string, len(limits))
	for i, l := range limits {
		keys[i] = l.shared
	}
	fails, err := s.Limits.LoginFailures(ctx, loginWindow, keys...)
	if err != nil {
		s.Logger.Warn("login: reading the shared failure counters failed, using this node's own", "err", err)
		return nil, 0, false
	}
	for _, l := range limits {
		if f, ok := fails[l.shared]; ok && f.Count >= l.max {
			if left := f.Started.Add(loginWindow).Sub(s.Now()); left > 0 {
				return fails, left, true
			}
		}
	}
	return fails, 0, false
}

// failed counts one failure under each limit.
func (s *Server) failed(ctx context.Context, limits ...limit) {
	keys := make([]string, len(limits))
	for i, l := range limits {
		l.mem.fail(l.key)
		keys[i] = l.shared
	}
	if s.Limits != nil {
		if err := s.Limits.AddLoginFailure(ctx, loginWindow, keys...); err != nil {
			s.Logger.Warn("login: counting the failure for the other nodes failed", "err", err)
		}
	}
}

// cleared forgets the failures under a limit after a success. shared is
// what blocked returned: only when there is something to forget does this
// write, and most logins have no failures behind them.
func (s *Server) cleared(ctx context.Context, shared map[string]store.LoginFailure, l limit) {
	l.mem.reset(l.key)
	if _, had := shared[l.shared]; had && s.Limits != nil {
		if err := s.Limits.ClearLoginFailures(ctx, l.shared); err != nil {
			s.Logger.Warn("login: clearing the failures failed", "err", err)
		}
	}
}

// limiterKey is what failures for a username are counted under: the
// address in its one spelling, or, for text that is no address, the text
// itself, cut: guessing at nonsense names is limited like guessing at real
// ones.
func limiterKey(username string) (key string, addr address.Address, ok bool) {
	key = username
	if len(key) > 320 {
		key = key[:320]
	}
	addr, err := address.Parse(username)
	if err == nil {
		key = addr.String()
	}
	return key, addr, err == nil
}

// lookup finds the account of a username. found is false for an address
// nobody has; status is not 0 when the store could not say.
func (s *Server) lookup(ctx context.Context, addr address.Address, isAddress bool) (acct store.Account, found bool, status int) {
	if !isAddress {
		return store.Account{}, false, 0
	}
	acct, err := s.Accounts.Account(ctx, addr.String())
	switch {
	case err == nil:
		return acct, true, 0
	case errors.Is(err, store.ErrNoAccount):
		return store.Account{}, false, 0
	}
	s.Logger.Error("login: looking up the account failed", "err", err)
	return store.Account{}, false, http.StatusServiceUnavailable
}

// passwordLogin checks an address and the account password for the web
// login, with the rate limits. It returns the account, or an HTTP status
// that says why not (401 wrong, 429 too many failures, 503 cannot tell
// right now) and how long the client should wait.
func (s *Server) passwordLogin(ctx context.Context, ip, username, password string) (store.Account, int, time.Duration) {
	s.init()
	key, addr, isAddress := limiterKey(username)
	byAddr, byIP := s.limitWeb(key), s.limitIP(ip)
	shared, wait, blocked := s.blocked(ctx, byIP, byAddr)
	if blocked {
		return store.Account{}, http.StatusTooManyRequests, wait
	}
	acct, found, status := s.lookup(ctx, addr, isAddress)
	if status != 0 {
		return store.Account{}, status, 0
	}
	// An unknown account costs as much time as a wrong password.
	match, ok := s.check(ctx, acct.PasswordHash, password)
	if !ok {
		return store.Account{}, http.StatusServiceUnavailable, 0
	}
	if !found || !match {
		s.failed(ctx, byIP, byAddr)
		s.event(ctx, EventLoginFailed, acct, key, ip, "web: wrong address or password")
		return store.Account{}, http.StatusUnauthorized, 0
	}
	s.cleared(ctx, shared, byAddr)
	return acct, 0, 0
}

// Credential is what a mail client logged in with.
type Credential struct {
	Account store.Account
	// AppPassword is the id of the app password; empty for the account
	// password.
	AppPassword string
}

// passwordOpensMail reports whether the account password is still good for
// mail clients (the table at the top of this file).
func (s *Server) passwordOpensMail(acct store.Account) bool {
	return !s.RequireAppPasswords && acct.AppPasswords == 0 && !acct.TOTP
}

// MailLogin checks the address and secret of a mail client, with the rate
// limits. scope is the protocol: store.ScopeIMAP, ScopeSMTP or ScopeJMAP.
// The answer is as for passwordLogin. Every way into a node for a mail
// client goes through here, so they share the failure counters and the
// bound on password hashing.
//
// A secret that has the form of an app password is only ever looked up as
// one: a SHA-256 and a read, no Argon2id. Any other is only ever checked
// as the account password, with Argon2id, also when the account does not
// exist or may not use its password here. So the time a refusal takes
// depends on what was sent, not on whether the account exists, has app
// passwords or has TOTP.
func (s *Server) MailLogin(ctx context.Context, ip, scope, username, secret string) (Credential, int, time.Duration) {
	s.init()
	key, addr, isAddress := limiterKey(username)
	byAddr, byIP := s.limitMail(key), s.limitIP(ip)
	shared, wait, blocked := s.blocked(ctx, byIP, byAddr)
	if blocked {
		return Credential{}, http.StatusTooManyRequests, wait
	}
	acct, found, status := s.lookup(ctx, addr, isAddress)
	if status != 0 {
		return Credential{}, status, 0
	}
	refuse := func(why string) (Credential, int, time.Duration) {
		s.failed(ctx, byIP, byAddr)
		s.event(ctx, EventLoginFailed, acct, key, ip, scope+": "+why)
		return Credential{}, http.StatusUnauthorized, 0
	}

	if normalized, isApp := users.NormalizeAppPassword(secret); isApp {
		// Also for an unknown account, whose id is empty and has none.
		p, err := s.Accounts.AppPasswordByHash(ctx, acct.ID, users.HashAppPassword(normalized))
		switch {
		case errors.Is(err, store.ErrNoAppPassword) || (err == nil && !found):
			return refuse("no such app password")
		case err != nil:
			s.Logger.Error("login: looking up the app password failed", "err", err)
			return Credential{}, http.StatusServiceUnavailable, 0
		case !p.Allows(scope):
			return refuse("app password " + strconv.Quote(p.Name) + " is not for this protocol")
		}
		s.cleared(ctx, shared, byAddr)
		s.appPasswordUsed(ctx, acct, p, ip, scope)
		return Credential{Account: acct, AppPassword: p.ID}, 0, 0
	}

	match, ok := s.check(ctx, acct.PasswordHash, secret)
	switch {
	case !ok:
		return Credential{}, http.StatusServiceUnavailable, 0
	case !found || !match:
		return refuse("wrong address or password")
	case !s.passwordOpensMail(acct):
		// The same answer as for a wrong password: whoever sent it is not
		// told that it is the account's.
		return refuse("the account password is not accepted here: an app password is needed")
	}
	s.cleared(ctx, shared, byAddr)
	return Credential{Account: acct}, 0, 0
}

// appPasswordUsed notes the login in the app password's row and in the
// log of auth events, at most once per lastUseEvery for one app password
// from one address: a mail client logs in every few minutes on several
// connections, and each note is a write.
func (s *Server) appPasswordUsed(ctx context.Context, acct store.Account, p store.AppPassword, ip, scope string) {
	now := s.Now()
	if ip == p.UsedIP && now.Sub(p.UsedAt) < lastUseEvery {
		return
	}
	id := acct.ID + "/" + p.ID + "/" + ip
	s.usedMu.Lock()
	if at, ok := s.used[id]; ok && now.Sub(at) < lastUseEvery {
		s.usedMu.Unlock()
		return
	}
	if len(s.used) >= basicCacheEntries {
		s.used = map[string]time.Time{}
	}
	s.used[id] = now
	s.usedMu.Unlock()
	if err := s.Accounts.UseAppPassword(ctx, acct.ID, p.ID, ip); err != nil {
		s.Logger.Warn("login: noting the use of an app password failed", "err", err)
	}
	s.event(ctx, EventLogin, acct, acct.Address, ip, scope+": app password "+strconv.Quote(p.Name))
}

// CredentialValid reports whether what a mail client logged in with still
// opens the protocol: the app password exists and has the scope, or the
// account password is unchanged and still allowed there. Connections that
// stay open ask this when the account's auth_seq moved. An error means the
// store could not say; the caller then leaves the connection be.
func (s *Server) CredentialValid(ctx context.Context, c Credential, scope string) (bool, error) {
	if c.AppPassword != "" {
		p, err := s.Accounts.AppPassword(ctx, c.Account.ID, c.AppPassword)
		if errors.Is(err, store.ErrNoAppPassword) {
			return false, nil
		}
		return err == nil && p.Allows(scope), err
	}
	acct, err := s.Accounts.AccountByID(ctx, c.Account.ID)
	if errors.Is(err, store.ErrNoAccount) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return acct.PasswordHash == c.Account.PasswordHash && s.passwordOpensMail(acct), nil
}

// authenticate finds who a JMAP request is from: by session cookie, or by
// Basic credentials, which standard JMAP clients send with each request.
// It returns 0 or the status to refuse with. valid, when not nil, says
// whether the login still stands; a long-lived response asks it when the
// account's auth_seq moves.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (acct store.Account, status int, valid func(context.Context) bool) {
	sess, acct, hash, ok, err := s.cookieSession(r, sessionCookie(r), store.StageFull)
	if err != nil {
		s.Logger.Error("looking up the session failed", "err", err)
		return store.Account{}, http.StatusServiceUnavailable, nil
	}
	if ok {
		s.touch(r, sess, hash)
		return acct, 0, func(ctx context.Context) bool {
			_, _, err := s.Accounts.SessionByToken(ctx, hash)
			return !errors.Is(err, store.ErrNoSession)
		}
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return store.Account{}, http.StatusUnauthorized, nil
	}
	check := func(c Credential) func(context.Context) bool {
		return func(ctx context.Context) bool {
			ok, err := s.CredentialValid(ctx, c, store.ScopeJMAP)
			return ok || err != nil
		}
	}
	// An account password that was just verified is not hashed again for
	// every request of a client: Argon2id is slow on purpose. What is
	// remembered is checked against the account as it is now, which is
	// one read: a changed password or a first app password ends it at
	// once, on every node. App passwords need no memory: they are cheap.
	if c, ok := s.basic.get(username, password); ok {
		if ok, err := s.CredentialValid(r.Context(), c, store.ScopeJMAP); err != nil {
			return store.Account{}, http.StatusServiceUnavailable, nil
		} else if ok {
			return c.Account, 0, check(c)
		}
		s.basic.drop(username, password)
	}
	c, status, wait := s.MailLogin(r.Context(), clientIP(r), store.ScopeJMAP, username, password)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
	}
	if status != 0 {
		return store.Account{}, status, nil
	}
	if c.AppPassword == "" {
		s.basic.put(username, password, c)
	}
	return c.Account, 0, check(c)
}

// limiter counts failures per key in a fixed window and blocks a key that
// reached the maximum until its window ends. It lives in memory: per node,
// and gone at a restart. The counters all nodes share are in rqlite
// (blocked); this one is the fallback for when rqlite is away.
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

// basicCache remembers, for a few minutes, that a username and account
// password were verified. It holds no password: the key is an HMAC of both
// under a key that exists only in this process.
type basicCache struct {
	mu  sync.Mutex
	key [32]byte
	now func() time.Time
	m   map[string]basicEntry
}

type basicEntry struct {
	cred    Credential
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

func (c *basicCache) get(username, password string) (Credential, bool) {
	id := c.id(username, password)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || !c.now().Before(e.expires) {
		delete(c.m, id)
		return Credential{}, false
	}
	return e.cred, true
}

func (c *basicCache) put(username, password string, cred Credential) {
	id := c.id(username, password)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= basicCacheEntries {
		c.m = map[string]basicEntry{}
	}
	// The hash stays: it is what CredentialValid compares with the
	// account's current one.
	c.m[id] = basicEntry{cred: cred, expires: c.now().Add(basicCacheTTL)}
}

func (c *basicCache) drop(username, password string) {
	id := c.id(username, password)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
}
