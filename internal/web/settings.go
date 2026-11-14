package web

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mikepage/robinmail/internal/qr"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/totp"
	"github.com/mikepage/robinmail/internal/users"
)

// What a logged-in user changes about the ways into the account: the
// password, app passwords, TOTP and sessions. All of it needs the session
// cookie; a mail client's credentials do not get here.
//
// The changes that would let somebody who has a session keep the account
// (a new app password, TOTP on or off, new recovery codes, a new
// password) ask for the account password again. A session cookie that
// leaked, or a browser left open, is then not enough to turn a visit into
// a way back in.

// user is who a settings request is from.
type user struct {
	acct store.Account
	sess store.Session
	hash string // of the session token
}

// authed wraps a handler that needs a complete web login.
func (s *Server) authed(h func(http.ResponseWriter, *http.Request, user)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, acct, hash, ok, err := s.cookieSession(r, sessionCookie(r), store.StageFull)
		if err != nil {
			s.Logger.Error("looking up the session failed", "err", err)
			jsonError(w, http.StatusServiceUnavailable, "the server cannot check the session right now")
			return
		}
		if !ok {
			jsonError(w, http.StatusUnauthorized, "not logged in")
			return
		}
		s.touch(r, sess, hash)
		h(w, r, user{acct, sess, hash})
	}
}

// confirm checks that a sensitive change comes from the account's owner:
// by the account password, with the limit on wrong ones, or, for an
// account that has no password (it logs in through an OIDC provider), by
// a login that is no older than freshLogin.
func (s *Server) confirm(w http.ResponseWriter, r *http.Request, u user, password string) bool {
	if u.acct.PasswordHash == store.NoPassword {
		if s.Now().Sub(u.sess.CreatedAt) > freshLogin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "log in again to change this", "reason": "stale"})
			return false
		}
		return true
	}
	ip := clientIP(r)
	byAddr, byIP := s.limitWeb(u.acct.Address), s.limitIP(ip)
	shared, wait, blocked := s.blocked(r.Context(), byIP, byAddr)
	if blocked {
		retryAfter(w, wait)
		jsonError(w, http.StatusTooManyRequests, "too many wrong passwords; try again later")
		return false
	}
	match, ok := s.check(r.Context(), u.acct.PasswordHash, password)
	if !ok {
		jsonError(w, http.StatusServiceUnavailable, "the server cannot check the password right now")
		return false
	}
	if !match {
		s.failed(r.Context(), byIP, byAddr)
		s.event(r.Context(), EventLoginFailed, u.acct, u.acct.Address, ip, "web: wrong password for a change of settings")
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong password", "reason": "password"})
		return false
	}
	s.cleared(r.Context(), shared, byAddr)
	return true
}

func (s *Server) unavailable(w http.ResponseWriter, what string, err error) {
	s.Logger.Error(what+" failed", "err", err)
	jsonError(w, http.StatusServiceUnavailable, "the server cannot do that right now")
}

// changePassword is POST /auth/password with {"current": "...", "new":
// "..."}. Every other session of the account ends.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if u.acct.PasswordHash == store.NoPassword {
		jsonError(w, http.StatusForbidden, "this account has no password: it logs in through a provider")
		return
	}
	if !s.confirm(w, r, u, req.Current) {
		return
	}
	if len(req.New) > maxPasswordLen || !utf8.ValidString(req.New) {
		jsonError(w, http.StatusBadRequest, "that password cannot be used")
		return
	}
	if _, isApp := users.NormalizeAppPassword(req.New); isApp {
		// MailLogin would never try it as the account password.
		jsonError(w, http.StatusBadRequest, "that has the form of an app password; choose another")
		return
	}
	hash, err := s.HashPassword(req.New)
	if errors.Is(err, users.ErrWeakPassword) {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.unavailable(w, "hashing the new password", err)
		return
	}
	switch err := s.Accounts.SetPassword(r.Context(), u.acct.ID, u.acct.PasswordHash, hash, u.hash); {
	case errors.Is(err, store.ErrPasswordChanged):
		jsonError(w, http.StatusConflict, "the password was changed somewhere else meanwhile")
		return
	case err != nil:
		s.unavailable(w, "storing the new password", err)
		return
	}
	s.event(r.Context(), EventPasswordChanged, u.acct, u.acct.Address, clientIP(r), "the other sessions were ended")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

type appPasswordJSON struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"createdAt"`
	UsedAt    *time.Time `json:"usedAt"`
	UsedIP    string     `json:"usedIp,omitempty"`
	// Secret is only in the answer that creates the app password.
	Secret string `json:"secret,omitempty"`
}

func appPasswordView(p store.AppPassword) appPasswordJSON {
	v := appPasswordJSON{ID: p.ID, Name: p.Name, Scopes: p.Scopes, CreatedAt: p.CreatedAt.UTC(), UsedIP: p.UsedIP}
	if v.Scopes == nil {
		v.Scopes = []string{}
	}
	if !p.UsedAt.IsZero() {
		at := p.UsedAt.UTC()
		v.UsedAt = &at
	}
	return v
}

// appPasswords is GET /auth/app-passwords.
func (s *Server) appPasswords(w http.ResponseWriter, r *http.Request, u user) {
	list, err := s.Accounts.AppPasswords(r.Context(), u.acct.ID)
	if err != nil {
		s.unavailable(w, "listing app passwords", err)
		return
	}
	out := make([]appPasswordJSON, len(list))
	for i, p := range list {
		out[i] = appPasswordView(p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": out, "passwordOpensMail": s.passwordOpensMail(u.acct)})
}

// cleanName checks a name a user gives to something: some text on one
// line, not too long.
func cleanName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || !utf8.ValidString(name) {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}

// createAppPassword is POST /auth/app-passwords with {"name": "Phone",
// "scopes": ["imap", "smtp"], "password": "..."}. The answer holds the
// secret, this once.
func (s *Server) createAppPassword(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Name     string   `json:"name"`
		Scopes   []string `json:"scopes"`
		Password string   `json:"password"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name, ok := cleanName(req.Name)
	if !ok {
		jsonError(w, http.StatusBadRequest, "give the app password a name of at most 100 characters")
		return
	}
	var scopes []string
	for _, known := range []string{store.ScopeIMAP, store.ScopeSMTP, store.ScopeJMAP} {
		for _, sc := range req.Scopes {
			if sc == known {
				scopes = append(scopes, known)
				break
			}
		}
	}
	if len(scopes) != len(req.Scopes) {
		jsonError(w, http.StatusBadRequest, `scopes are "imap", "smtp" and "jmap", each once; none is all`)
		return
	}
	if len(scopes) == 3 {
		scopes = nil
	}
	if !s.confirm(w, r, u, req.Password) {
		return
	}
	secret, err := users.NewAppPassword()
	id, err2 := newID()
	if err != nil || err2 != nil {
		s.unavailable(w, "making an app password", errors.Join(err, err2))
		return
	}
	normalized, _ := users.NormalizeAppPassword(secret)
	switch err := s.Accounts.CreateAppPassword(r.Context(), u.acct.ID, id, name, users.HashAppPassword(normalized), scopes); {
	case errors.Is(err, store.ErrAppPasswords):
		jsonError(w, http.StatusConflict, "the account has as many app passwords as it can have; revoke one first")
		return
	case err != nil:
		s.unavailable(w, "storing the app password", err)
		return
	}
	for_ := "all protocols"
	if len(scopes) > 0 {
		for_ = strings.Join(scopes, ", ")
	}
	s.event(r.Context(), EventAppPasswordCreated, u.acct, u.acct.Address, clientIP(r), name+" (for "+for_+")")
	v := appPasswordView(store.AppPassword{ID: id, Name: name, Scopes: scopes, CreatedAt: s.Now()})
	v.Secret = secret
	writeJSON(w, http.StatusCreated, v)
}

// revokeAppPassword is DELETE /auth/app-passwords/{id}. Open IMAP sessions
// of that app password end, on every node.
func (s *Server) revokeAppPassword(w http.ResponseWriter, r *http.Request, u user) {
	id := r.PathValue("id")
	p, err := s.Accounts.AppPassword(r.Context(), u.acct.ID, id)
	if errors.Is(err, store.ErrNoAppPassword) {
		jsonError(w, http.StatusNotFound, "no such app password")
		return
	}
	if err == nil {
		_, err = s.Accounts.RevokeAppPassword(r.Context(), u.acct.ID, id)
	}
	if err != nil {
		s.unavailable(w, "revoking the app password", err)
		return
	}
	s.event(r.Context(), EventAppPasswordRevoked, u.acct, u.acct.Address, clientIP(r), p.Name)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// totpState is GET /auth/totp.
func (s *Server) totpState(w http.ResponseWriter, r *http.Request, u user) {
	state, err := s.Accounts.TOTPState(r.Context(), u.acct.ID)
	if err != nil && !errors.Is(err, store.ErrNoTOTP) {
		s.unavailable(w, "reading the TOTP state", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": state.Confirmed, "recoveryCodes": state.RecoveryCodes})
}

// totpEnrol is POST /auth/totp/enrol with {"password": "..."}. It makes a
// secret and answers with it three ways: as text for typing, as the
// otpauth URI, and as the QR code of that URI, drawn here. TOTP is not on
// until POST /auth/totp/confirm.
func (s *Server) totpEnrol(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Password string `json:"password"`
	}
	if !readJSON(w, r, &req) || !s.confirm(w, r, u, req.Password) {
		return
	}
	secret, err := totp.NewSecret()
	if err != nil {
		s.unavailable(w, "making a TOTP secret", err)
		return
	}
	switch err := s.Accounts.SetPendingTOTP(r.Context(), u.acct.ID, secret); {
	case errors.Is(err, store.ErrTOTPEnabled):
		jsonError(w, http.StatusConflict, "TOTP is already on; turn it off first to set it up again")
		return
	case err != nil:
		s.unavailable(w, "storing the TOTP secret", err)
		return
	}
	uri := totp.URI(s.Hostname, u.acct.Address, secret)
	code, err := qr.Encode([]byte(uri))
	var rows []string
	if err == nil {
		rows = code.Rows()
	}
	// An address so long that the URI does not fit a code is typed by hand.
	writeJSON(w, http.StatusOK, map[string]any{"secret": totp.EncodeSecret(secret), "uri": uri, "qr": rows})
}

// newRecoveryCodes makes a set of codes and their hashes.
func newRecoveryCodes() (codes, hashes []string, err error) {
	for i := 0; i < recoveryCodes; i++ {
		c, err := users.NewRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		n, _ := users.NormalizeRecoveryCode(c)
		codes, hashes = append(codes, c), append(hashes, users.HashRecoveryCode(n))
	}
	return codes, hashes, nil
}

// totpConfirm is POST /auth/totp/confirm with {"code": "123456"}: the
// user shows that the authenticator has the secret. TOTP is on from here,
// and the answer holds the recovery codes, this once.
func (s *Server) totpConfirm(w http.ResponseWriter, r *http.Request, u user) {
	var req struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	byCode, byIP := s.limitCode(u.acct.Address), s.limitIP(ip)
	shared, wait, blocked := s.blocked(r.Context(), byIP, byCode)
	if blocked {
		refuseCode(w, http.StatusTooManyRequests, wait)
		return
	}
	state, err := s.Accounts.TOTPState(r.Context(), u.acct.ID)
	if errors.Is(err, store.ErrNoTOTP) || (err == nil && state.Confirmed) {
		jsonError(w, http.StatusConflict, "no TOTP set-up is waiting for a code")
		return
	}
	if err != nil {
		s.unavailable(w, "reading the TOTP secret", err)
		return
	}
	step, ok := totp.Match(state.Secret, strings.TrimSpace(req.Code), s.Now())
	if !ok {
		s.failed(r.Context(), byIP, byCode)
		refuseCode(w, http.StatusUnauthorized, 0)
		return
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		s.unavailable(w, "making recovery codes", err)
		return
	}
	switch err := s.Accounts.ConfirmTOTP(r.Context(), u.acct.ID, step, hashes); {
	case errors.Is(err, store.ErrTOTPNotPending):
		jsonError(w, http.StatusConflict, "no TOTP set-up is waiting for a code")
		return
	case err != nil:
		s.unavailable(w, "turning TOTP on", err)
		return
	}
	s.cleared(r.Context(), shared, byCode)
	s.event(r.Context(), EventTOTPEnabled, u.acct, u.acct.Address, ip, "")
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// provenTwice reads {"password", "code" | "recoveryCode"} and checks
// both: what turning TOTP off and making new recovery codes ask for.
func (s *Server) provenTwice(w http.ResponseWriter, r *http.Request, u user) bool {
	var req struct {
		Password     string `json:"password"`
		Code         string `json:"code"`
		RecoveryCode string `json:"recoveryCode"`
	}
	if !readJSON(w, r, &req) {
		return false
	}
	if !u.acct.TOTP {
		jsonError(w, http.StatusConflict, "TOTP is not on")
		return false
	}
	if !s.confirm(w, r, u, req.Password) {
		return false
	}
	if _, status, wait := s.secondFactor(r.Context(), u.acct, clientIP(r), req.Code, req.RecoveryCode); status != 0 {
		refuseCode(w, status, wait)
		return false
	}
	return true
}

// totpDisable is POST /auth/totp/disable with the password and a code or
// a recovery code.
func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request, u user) {
	if !s.provenTwice(w, r, u) {
		return
	}
	if _, err := s.Accounts.DisableTOTP(r.Context(), u.acct.ID); err != nil {
		s.unavailable(w, "turning TOTP off", err)
		return
	}
	s.event(r.Context(), EventTOTPDisabled, u.acct, u.acct.Address, clientIP(r), "by the user")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// totpRecoveryCodes is POST /auth/totp/recovery-codes with the password
// and a code: a new set of recovery codes; the old ones stop working.
func (s *Server) totpRecoveryCodes(w http.ResponseWriter, r *http.Request, u user) {
	if !s.provenTwice(w, r, u) {
		return
	}
	codes, hashes, err := newRecoveryCodes()
	if err == nil {
		err = s.Accounts.ReplaceRecoveryCodes(r.Context(), u.acct.ID, hashes)
	}
	if err != nil {
		s.unavailable(w, "making new recovery codes", err)
		return
	}
	s.event(r.Context(), EventRecoveryReplaced, u.acct, u.acct.Address, clientIP(r), "")
	writeJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// device says in a few words what a User-Agent header is: enough for a
// user to tell their sessions apart, no more.
func device(agent string) string {
	browser := ""
	for _, b := range []struct{ mark, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"},
		{"CriOS/", "Chrome"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(agent, b.mark) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, o := range []struct{ mark, name string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Windows", "Windows"},
		{"Mac OS X", "macOS"}, {"Macintosh", "macOS"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(agent, o.mark) {
			system = o.name
			break
		}
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return "A browser on " + system
	case agent == "":
		return "Unknown device"
	}
	return "Another program"
}

// sessions is GET /auth/sessions.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request, u user) {
	list, err := s.Accounts.Sessions(r.Context(), u.acct.ID)
	if err != nil {
		s.unavailable(w, "listing sessions", err)
		return
	}
	type sessionJSON struct {
		ID        string    `json:"id"`
		Current   bool      `json:"current"`
		Device    string    `json:"device"`
		Agent     string    `json:"agent"`
		Method    string    `json:"method"`
		IP        string    `json:"ip"`
		SeenIP    string    `json:"seenIp"`
		CreatedAt time.Time `json:"createdAt"`
		SeenAt    time.Time `json:"seenAt"`
		ExpiresAt time.Time `json:"expiresAt"`
		Remember  bool      `json:"remember"`
	}
	out := make([]sessionJSON, len(list))
	for i, x := range list {
		ends := x.ExpiresAt
		if idle := x.SeenAt.Add(x.Idle); idle.Before(ends) {
			ends = idle
		}
		out[i] = sessionJSON{ID: x.ID, Current: x.ID == u.sess.ID, Device: device(x.Agent), Agent: x.Agent, Method: x.Method,
			IP: x.IP, SeenIP: x.SeenIP, CreatedAt: x.CreatedAt.UTC(), SeenAt: x.SeenAt.UTC(), ExpiresAt: ends.UTC(), Remember: x.Remember}
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": out})
}

// revokeSession is DELETE /auth/sessions/{id}. Revoking the session the
// request came with is a logout.
func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, u user) {
	id := r.PathValue("id")
	found, err := s.Accounts.RevokeSession(r.Context(), u.acct.ID, id)
	if err != nil {
		s.unavailable(w, "revoking the session", err)
		return
	}
	if !found {
		jsonError(w, http.StatusNotFound, "no such session")
		return
	}
	if id == u.sess.ID {
		s.setCookie(w, r, sessionCookie(r), "", -1)
	}
	s.event(r.Context(), EventSessionRevoked, u.acct, u.acct.Address, clientIP(r), "session "+id)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// revokeOtherSessions is POST /auth/sessions/revoke-others.
func (s *Server) revokeOtherSessions(w http.ResponseWriter, r *http.Request, u user) {
	var req struct{}
	if !readJSON(w, r, &req) {
		return
	}
	n, err := s.Accounts.RevokeSessions(r.Context(), u.acct.ID, u.hash)
	if err != nil {
		s.unavailable(w, "revoking the other sessions", err)
		return
	}
	s.event(r.Context(), EventSessionRevoked, u.acct, u.acct.Address, clientIP(r), "all other sessions")
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}
