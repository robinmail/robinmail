package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/store"
	"github.com/robinmail/robinmail/internal/totp"
	"github.com/robinmail/robinmail/internal/users"
)

// The web login.
//
// Step one is address and password. For an account without TOTP that is
// all: the answer sets the session cookie. For one with TOTP the answer
// says a code is needed and sets a second cookie, for a login that waits
// (a session row of stage "totp", five minutes); step two sends the code,
// or a recovery code, and trades that cookie for the session cookie. A
// wrong password gets the same answer whether or not the account has
// TOTP, so nothing is learned about the password or the account before
// the password was right.
//
// Cookies and cross-site requests. Both cookies are HttpOnly (no script
// reads them), Secure and "__Host-" on HTTPS (this host only, never over
// HTTP, not settable from a sibling domain) and SameSite=Strict (not sent
// with any request another site starts, not even a link). On top of that
// every endpoint under /auth and JMAP checks the Origin of a request, and
// everything that changes state wants a JSON body with its content type,
// which a page on another site cannot send without this server's consent.
// No endpoint changes anything on GET. So there is no CSRF token: there
// is no request for it to protect that the three checks let through.
// The one cookie that is not Strict is the short-lived one of an OIDC
// login (oidc.go), which has to come back with a redirect from the
// provider; it opens nothing by itself.
//
// A login always gets a new token, and the session a browser came with is
// deleted in the same write (rotation): a token planted in a browser
// before the login is worth nothing after it.

// Lifetimes of a web session. It ends when it was not used for the idle
// time, and at the latest after the absolute time. Without "remember this
// device" the cookie also goes when the browser closes.
const (
	SessionIdle  = 12 * time.Hour
	SessionMax   = 7 * 24 * time.Hour
	RememberIdle = 14 * 24 * time.Hour
	RememberMax  = 30 * 24 * time.Hour
	// PendingTTL is how long a login may wait for its second step.
	PendingTTL = 5 * time.Minute
	// touchEvery is how often "last seen" is written for a session in use.
	touchEvery = 5 * time.Minute
	// freshLogin is how old a session may be for a change that would
	// otherwise ask for the password, on an account that has none.
	freshLogin = 15 * time.Minute
)

// sessionCookie is the cookie of a complete login. On HTTPS it has the
// "__Host-" prefix, with which a browser only accepts it as Secure, for
// this host alone and for all paths, so another site on a sibling domain
// cannot plant one.
func sessionCookie(r *http.Request) string {
	if r.TLS != nil {
		return "__Host-robinmail"
	}
	return "robinmail"
}

// pendingCookie is the cookie of a login that waits for its second step.
func pendingCookie(r *http.Request) string { return sessionCookie(r) + "-login" }

// setCookie sets one of the cookies. maxAge 0 makes a cookie that goes
// when the browser closes, a negative one removes it.
func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, name, token string, maxAge time.Duration) {
	c := &http.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		Secure:   r.TLS != nil, // the plain listener is for development on localhost
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
	switch {
	case maxAge < 0:
		c.MaxAge = -1
	case maxAge > 0:
		c.MaxAge = int(maxAge / time.Second)
	}
	http.SetCookie(w, c)
}

// cookieSession returns the session of the named cookie if it is alive and
// in the given stage, its account and the hash of its token.
func (s *Server) cookieSession(r *http.Request, name, stage string) (store.Session, store.Account, string, bool, error) {
	c, err := r.Cookie(name)
	if err != nil || c.Value == "" || len(c.Value) > 200 {
		return store.Session{}, store.Account{}, "", false, nil
	}
	hash := tokenHash(c.Value)
	sess, acct, err := s.Accounts.SessionByToken(r.Context(), hash)
	if errors.Is(err, store.ErrNoSession) || (err == nil && sess.Stage != stage) {
		return store.Session{}, store.Account{}, "", false, nil
	}
	if err != nil {
		return store.Session{}, store.Account{}, "", false, err
	}
	return sess, acct, hash, true, nil
}

// touch notes that a session is in use, every few minutes and when it
// shows up from another address.
func (s *Server) touch(r *http.Request, sess store.Session, hash string) {
	ip := clientIP(r)
	if s.Now().Sub(sess.SeenAt) < touchEvery && ip == sess.SeenIP {
		return
	}
	if err := s.Accounts.TouchSession(r.Context(), hash, ip); err != nil {
		s.Logger.Warn("noting the use of a session failed", "err", err)
	}
}

// startSession creates a session and sets its cookie. replaces is the
// token hash of the session this login takes the place of.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, acct store.Account, stage, method string, remember bool, replaces string) (store.Session, bool) {
	token, err := newToken()
	id, err2 := newID()
	if err != nil || err2 != nil {
		jsonError(w, http.StatusInternalServerError, "the server cannot log you in right now")
		return store.Session{}, false
	}
	now := s.Now()
	sess := store.Session{ID: id, AccountID: acct.ID, Stage: stage, Method: method, IP: clientIP(r),
		Agent: clipAgent(r.UserAgent()), Remember: remember}
	cookie, maxAge := sessionCookie(r), time.Duration(0)
	switch {
	case stage == store.StageTOTP:
		sess.Idle, sess.ExpiresAt = PendingTTL, now.Add(PendingTTL)
		cookie, maxAge = pendingCookie(r), PendingTTL
	case remember:
		sess.Idle, sess.ExpiresAt = RememberIdle, now.Add(RememberMax)
		maxAge = RememberMax
	default:
		sess.Idle, sess.ExpiresAt = SessionIdle, now.Add(SessionMax)
	}
	if err := s.Accounts.CreateSession(r.Context(), tokenHash(token), sess, replaces); err != nil {
		s.Logger.Error("login: storing the session failed", "err", err)
		jsonError(w, http.StatusServiceUnavailable, "the server cannot log you in right now")
		return store.Session{}, false
	}
	s.setCookie(w, r, cookie, token, maxAge)
	return sess, true
}

func clipAgent(ua string) string {
	if len(ua) > 200 {
		ua = ua[:200]
	}
	return strings.ToValidUTF8(ua, "")
}

// oldSession is the token hash of the session cookie a request came with,
// whatever state it is in, for rotation.
func oldSession(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie(r)); err == nil && c.Value != "" && len(c.Value) <= 200 {
		return tokenHash(c.Value)
	}
	return ""
}

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
}

type whoami struct {
	AccountID string `json:"accountId"`
	Username  string `json:"username"`
}

// login is POST /auth/login with {"username": "...", "password": "...",
// "remember": false}. It answers with the account and sets the session
// cookie, or with {"step": "totp"} and the cookie of a login that waits
// for POST /auth/login/totp.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username *string `json:"username"`
		Password *string `json:"password"`
		Remember bool    `json:"remember"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Username == nil || req.Password == nil {
		jsonError(w, http.StatusBadRequest, `the body must be {"username": "...", "password": "..."}`)
		return
	}
	acct, status, wait := s.passwordLogin(r.Context(), clientIP(r), *req.Username, *req.Password)
	switch status {
	case 0:
	case http.StatusTooManyRequests:
		retryAfter(w, wait)
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
	s.finishLogin(w, r, acct, "password", req.Remember, acct.TOTP, func() {
		writeJSON(w, http.StatusOK, whoami{acct.ID, acct.Address})
	}, func() {
		writeJSON(w, http.StatusOK, map[string]string{"step": "totp"})
	})
}

// finishLogin is the end of a first step that succeeded: a session, or a
// login that waits for a TOTP code. method is how the user got here.
func (s *Server) finishLogin(w http.ResponseWriter, r *http.Request, acct store.Account, method string, remember, needTOTP bool, done, pending func()) {
	if needTOTP {
		if _, ok := s.startSession(w, r, acct, store.StageTOTP, method, remember, ""); ok {
			pending()
		}
		return
	}
	if _, ok := s.startSession(w, r, acct, store.StageFull, method, remember, oldSession(r)); !ok {
		return
	}
	s.event(r.Context(), EventLogin, acct, acct.Address, clientIP(r), "web: "+method)
	s.Logger.Info("login", "address", acct.Address, "ip", clientIP(r), "method", method)
	done()
}

// secondFactor checks a TOTP code or a recovery code of an account that
// has TOTP on, with the limit on wrong ones, and spends what it accepts:
// a code's time step can be claimed once for all nodes together, a
// recovery code is deleted. It returns what was used ("totp" or
// "recovery"), or the status to refuse with.
func (s *Server) secondFactor(ctx context.Context, acct store.Account, ip, code, recovery string) (used string, status int, wait time.Duration) {
	byCode, byIP := s.limitCode(acct.Address), s.limitIP(ip)
	done, ok := s.turn(ctx, byIP, byCode)
	if !ok {
		return "", http.StatusServiceUnavailable, 0
	}
	defer done()
	shared, wait, blocked := s.blocked(ctx, byIP, byCode)
	if blocked {
		return "", http.StatusTooManyRequests, wait
	}
	fail := func(why string) (string, int, time.Duration) {
		s.failed(ctx, byIP, byCode)
		s.event(ctx, EventCodeFailed, acct, acct.Address, ip, why)
		return "", http.StatusUnauthorized, 0
	}
	unavailable := func(err error) (string, int, time.Duration) {
		s.Logger.Error("checking the second step failed", "err", err)
		return "", http.StatusServiceUnavailable, 0
	}
	if recovery != "" {
		normalized, ok := users.NormalizeRecoveryCode(recovery)
		if !ok {
			return fail("recovery code: not one")
		}
		spent, err := s.Accounts.UseRecoveryCode(ctx, acct.ID, users.HashRecoveryCode(normalized))
		if err != nil {
			return unavailable(err)
		}
		if !spent {
			return fail("recovery code: wrong or used before")
		}
		s.cleared(ctx, shared, byCode)
		s.event(ctx, EventRecoveryUsed, acct, acct.Address, ip, "")
		return "recovery", 0, 0
	}
	state, err := s.Accounts.TOTPState(ctx, acct.ID)
	if errors.Is(err, store.ErrNoTOTP) || (err == nil && !state.Confirmed) {
		return fail("code: the account has no TOTP")
	}
	if err != nil {
		return unavailable(err)
	}
	step, ok := totp.Match(state.Secret, strings.TrimSpace(code), s.Now())
	if !ok {
		return fail("code: wrong")
	}
	claimed, err := s.Accounts.UseTOTPStep(ctx, acct.ID, step)
	if err != nil {
		return unavailable(err)
	}
	if !claimed {
		// RFC 6238 5.2: a code is good for one login. Counted like a wrong
		// one: whoever sends it again has seen it somewhere.
		return fail("code: used before")
	}
	s.cleared(ctx, shared, byCode)
	return "totp", 0, 0
}

// refuseCode writes the answer for a second step that was not accepted.
func refuseCode(w http.ResponseWriter, status int, wait time.Duration) {
	switch status {
	case http.StatusTooManyRequests:
		retryAfter(w, wait)
		jsonError(w, status, "too many wrong codes; try again later")
	case http.StatusUnauthorized:
		jsonError(w, status, "wrong code")
	default:
		jsonError(w, status, "the server cannot check the code right now")
	}
}

// loginTOTP is POST /auth/login/totp with {"code": "123456"} or
// {"recoveryCode": "abcd-..."}: the second step of a login that waits.
func (s *Server) loginTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code         string `json:"code"`
		RecoveryCode string `json:"recoveryCode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	pending, acct, hash, ok, err := s.cookieSession(r, pendingCookie(r), store.StageTOTP)
	if err != nil {
		jsonError(w, http.StatusServiceUnavailable, "the server cannot log you in right now")
		return
	}
	if !ok {
		jsonError(w, http.StatusUnauthorized, "log in with your password first")
		return
	}
	used, status, wait := s.secondFactor(r.Context(), acct, clientIP(r), req.Code, req.RecoveryCode)
	if status != 0 {
		refuseCode(w, status, wait)
		return
	}
	method := pending.Method + "+" + used
	// The login that waited is replaced by the session; the cookie of a
	// session the browser still had goes with it.
	if old := oldSession(r); old != "" {
		if err := s.Accounts.DeleteSession(r.Context(), old); err != nil {
			s.Logger.Warn("login: removing the previous session failed", "err", err)
		}
	}
	if _, ok := s.startSession(w, r, acct, store.StageFull, method, pending.Remember, hash); !ok {
		return
	}
	s.setCookie(w, r, pendingCookie(r), "", -1)
	s.event(r.Context(), EventLogin, acct, acct.Address, clientIP(r), "web: "+method)
	s.Logger.Info("login", "address", acct.Address, "ip", clientIP(r), "method", method)
	writeJSON(w, http.StatusOK, whoami{acct.ID, acct.Address})
}

// logout is POST /auth/logout: it ends the session of the cookie, and a
// login that waits.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{sessionCookie(r), pendingCookie(r)} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" && len(c.Value) <= 200 {
			if err := s.Accounts.DeleteSession(r.Context(), tokenHash(c.Value)); err != nil {
				s.Logger.Error("logout: removing the session failed", "err", err)
				jsonError(w, http.StatusServiceUnavailable, "the server cannot log you out right now")
				return
			}
		}
		s.setCookie(w, r, name, "", -1)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// session is GET /auth/session: who the cookie belongs to, or 401. A
// login that waits for its code answers 401 with "step": "totp", so the
// login page can pick up where it was after a reload.
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	sess, acct, hash, ok, err := s.cookieSession(r, sessionCookie(r), store.StageFull)
	switch {
	case err != nil:
		jsonError(w, http.StatusServiceUnavailable, "the server cannot check the session right now")
	case !ok:
		if _, _, _, waiting, _ := s.cookieSession(r, pendingCookie(r), store.StageTOTP); waiting {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "the login waits for its code", "step": "totp"})
			return
		}
		jsonError(w, http.StatusUnauthorized, "not logged in")
	default:
		s.touch(r, sess, hash)
		writeJSON(w, http.StatusOK, map[string]any{
			"accountId": acct.ID, "username": acct.Address, "sessionId": sess.ID, "method": sess.Method,
			"totp": acct.TOTP, "hasPassword": acct.PasswordHash != store.NoPassword,
			"appPasswords": acct.AppPasswords, "passwordOpensMail": s.passwordOpensMail(acct),
		})
	}
}

// config is GET /auth/config: what the login page needs to know before
// anyone is logged in.
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	type provider struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	}
	list := []provider{}
	for _, p := range s.OIDC {
		list = append(list, provider{p.Name, p.Label})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": list})
}
