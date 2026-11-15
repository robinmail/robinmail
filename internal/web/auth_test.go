package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/store"
	"github.com/robinmail/robinmail/internal/totp"
	"github.com/robinmail/robinmail/internal/users"
)

// node returns another server on the same accounts and clock: a second
// node of the cluster.
func (st *site) node() *site {
	other := &site{t: st.t, accounts: st.accounts}
	s := st.s
	other.s = &Server{Hostname: s.Hostname, Accounts: s.Accounts, Audit: s.Audit, Limits: s.Limits, Now: s.Now,
		Verify: s.Verify, HashPassword: s.HashPassword, WithUser: s.WithUser, JMAP: s.JMAP,
		RequireAppPasswords: s.RequireAppPasswords, AuthChanges: s.AuthChanges}
	other.h = other.s.Handler()
	return other
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("%d %s: %v", rec.Code, rec.Body.String(), err)
	}
}

// in logs anna in and returns her session cookie.
func (st *site) in(user, password string) *http.Cookie {
	st.t.Helper()
	rec := st.login(tlsBase, user, password)
	c := cookieNamed(rec, "__Host-robinmail")
	if rec.Code != 200 || c == nil {
		st.t.Fatalf("login of %s: %d %s", user, rec.Code, rec.Body.String())
	}
	return c
}

// appPassword creates an app password over the API and returns its id and
// secret.
func (st *site) appPassword(c *http.Cookie, password, name string, scopes ...string) (id, secret string) {
	st.t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "scopes": scopes, "password": password})
	rec := st.do("POST", tlsBase+"/auth/app-passwords", string(body), cookie(c))
	var got struct{ ID, Secret string }
	decode(st.t, rec, &got)
	if rec.Code != 201 || got.ID == "" || got.Secret == "" {
		st.t.Fatalf("creating an app password: %d %s", rec.Code, rec.Body.String())
	}
	return got.ID, got.Secret
}

func (st *site) mail(scope, user, secret string) int {
	_, status, _ := st.s.MailLogin(context.Background(), "192.0.2.50", scope, user, secret)
	return status
}

// App passwords: made by the server, shown once, and from the first one on
// the account password no longer opens IMAP, SMTP and Basic auth.
func TestAppPasswords(t *testing.T) {
	st := newSite(t)
	const anna, bob = "anna@example.test", "bob@example.test"

	// Until an account has one, its password works for mail clients.
	for _, scope := range []string{store.ScopeIMAP, store.ScopeSMTP, store.ScopeJMAP} {
		if got := st.mail(scope, anna, "correct horse"); got != 0 {
			t.Fatalf("%s with the account password, no app passwords yet: %d", scope, got)
		}
	}

	c := st.in(anna, "correct horse")
	// Making one asks for the account password again.
	for _, wrong := range []string{"", "wrong"} {
		body, _ := json.Marshal(map[string]any{"name": "Phone", "password": wrong})
		if rec := st.do("POST", tlsBase+"/auth/app-passwords", string(body), cookie(c)); rec.Code != 403 || !strings.Contains(rec.Body.String(), `"reason":"password"`) {
			t.Errorf("creating with password %q: %d %s", wrong, rec.Code, rec.Body.String())
		}
	}
	for _, bad := range []string{`{"name":"","password":"correct horse"}`, `{"name":"a\nb","password":"correct horse"}`,
		`{"name":"x","scopes":["pop3"],"password":"correct horse"}`, `{"name":"x","scopes":["imap","imap"],"password":"correct horse"}`} {
		if rec := st.do("POST", tlsBase+"/auth/app-passwords", bad, cookie(c)); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	id, secret := st.appPassword(c, "correct horse", "Phone")
	if !regexp.MustCompile(`^([a-z2-7]{4}-){7}[a-z2-7]{4}$`).MatchString(secret) {
		t.Fatalf("secret %q", secret)
	}
	// The store has a hash of it, and the list never shows it again.
	normalized, _ := users.NormalizeAppPassword(secret)
	for _, p := range st.accounts.apps["acct-anna"] {
		if p.hash != users.HashAppPassword(normalized) || strings.Contains(p.hash, normalized) {
			t.Errorf("stored: %q", p.hash)
		}
	}
	rec := st.do("GET", tlsBase+"/auth/app-passwords", "", cookie(c))
	var list struct {
		List []struct {
			ID, Name string
			Scopes   []string
			UsedAt   *time.Time
			Secret   string
		}
		PasswordOpensMail bool
	}
	decode(t, rec, &list)
	if rec.Code != 200 || len(list.List) != 1 || list.List[0].ID != id || list.List[0].Name != "Phone" || list.List[0].Secret != "" ||
		list.List[0].UsedAt != nil || list.PasswordOpensMail || strings.Contains(rec.Body.String(), normalized[:8]) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	// It opens all three doors, however it is typed, without a single
	// Argon2id computation.
	checks := st.verified.Load()
	for _, typed := range []string{secret, normalized, strings.ToUpper(secret), strings.ReplaceAll(secret, "-", " ")} {
		for _, scope := range []string{store.ScopeIMAP, store.ScopeSMTP, store.ScopeJMAP} {
			if got := st.mail(scope, anna, typed); got != 0 {
				t.Errorf("%s with %q: %d", scope, typed, got)
			}
		}
	}
	// Nor does a refusal of something that looks like one, for an account
	// that exists or not: the time says nothing about the account.
	other, _ := users.NewAppPassword()
	for _, user := range []string{anna, bob, "nobody@example.test", "not an address"} {
		if got := st.mail(store.ScopeIMAP, user, other); got != 401 {
			t.Errorf("an unknown app password for %s: %d", user, got)
		}
	}
	if got := st.mail(store.ScopeIMAP, bob, secret); got != 401 {
		t.Errorf("anna's app password for bob: %d", got)
	}
	if n := st.verified.Load() - checks; n != 0 {
		t.Errorf("%d password checks for app passwords", n)
	}

	// The account password is now for the web login only. The refusal
	// costs a password check, like any wrong password.
	checks = st.verified.Load()
	for _, scope := range []string{store.ScopeIMAP, store.ScopeSMTP, store.ScopeJMAP} {
		if got := st.mail(scope, anna, "correct horse"); got != 401 {
			t.Errorf("%s with the account password once an app password exists: %d", scope, got)
		}
	}
	if n := st.verified.Load() - checks; n != 3 {
		t.Errorf("%d password checks for three refused account passwords", n)
	}
	if rec := st.login(tlsBase, anna, "correct horse"); rec.Code != 200 {
		t.Errorf("web login with the account password: %d", rec.Code)
	}
	// And an app password is not a way into the webmail.
	if rec := st.login(tlsBase, anna, secret); rec.Code != 401 {
		t.Errorf("web login with an app password: %d", rec.Code)
	}
	// Bob has none: his password still works for his mail client.
	if got := st.mail(store.ScopeIMAP, bob, "battery staple"); got != 0 {
		t.Errorf("bob, who has no app password: %d", got)
	}

	// Scope.
	_, imapOnly := st.appPassword(c, "correct horse", "Reader", "imap")
	for scope, want := range map[string]int{store.ScopeIMAP: 0, store.ScopeSMTP: 401, store.ScopeJMAP: 401} {
		if got := st.mail(scope, anna, imapOnly); got != want {
			t.Errorf("an imap-only app password for %s: %d, want %d", scope, got, want)
		}
	}

	// Basic auth over JMAP takes it, on any node, and stops at once when
	// it is revoked: nothing about an app password is remembered.
	b := st.node()
	for _, n := range []*site{st, b} {
		if rec := n.do("POST", tlsBase+"/jmap/api", "{}", basic(anna, secret)); rec.Code != 200 {
			t.Fatalf("Basic with an app password: %d", rec.Code)
		}
	}
	cred, status, _ := st.s.MailLogin(context.Background(), "192.0.2.50", store.ScopeIMAP, anna, secret)
	if ok, err := b.s.CredentialValid(context.Background(), cred, store.ScopeIMAP); status != 0 || !ok || err != nil {
		t.Fatalf("credential: %d %v %v", status, ok, err)
	}
	if rec := st.do("DELETE", tlsBase+"/auth/app-passwords/"+id, "", cookie(c)); rec.Code != 204 {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do("DELETE", tlsBase+"/auth/app-passwords/"+id, "", cookie(c)); rec.Code != 404 {
		t.Errorf("revoke again: %d", rec.Code)
	}
	for _, n := range []*site{st, b} {
		if rec := n.do("POST", tlsBase+"/jmap/api", "{}", basic(anna, secret)); rec.Code != 401 {
			t.Errorf("Basic with a revoked app password: %d", rec.Code)
		}
	}
	if ok, err := b.s.CredentialValid(context.Background(), cred, store.ScopeIMAP); ok || err != nil {
		t.Errorf("a revoked credential on the other node: %v %v", ok, err)
	}
	// One account cannot revoke another's.
	bc := st.in(bob, "battery staple")
	if rec := st.do("DELETE", tlsBase+"/auth/app-passwords/"+st.accounts.apps["acct-anna"][0].ID, "", cookie(bc)); rec.Code != 404 {
		t.Errorf("bob revoking anna's app password: %d", rec.Code)
	}

	created, revoked := st.accounts.eventsOf(EventAppPasswordCreated), st.accounts.eventsOf(EventAppPasswordRevoked)
	if len(created) != 2 || len(revoked) != 1 || created[0].AccountID != "acct-anna" || !strings.Contains(created[0].Detail, "Phone") {
		t.Errorf("events: created %+v revoked %+v", created, revoked)
	}
}

// The switch that takes the account password away from mail clients for
// everyone, and TOTP, which does the same for its account: a second
// factor that IMAP could walk around would be none.
func TestAccountPasswordForMail(t *testing.T) {
	st := newSite(t)
	st.s.RequireAppPasswords = true
	if got := st.mail(store.ScopeIMAP, "anna@example.test", "correct horse"); got != 401 {
		t.Errorf("require_app_passwords: the account password over IMAP: %d", got)
	}
	if rec := st.do("POST", tlsBase+"/jmap/api", "{}", basic("anna@example.test", "correct horse")); rec.Code != 401 {
		t.Errorf("require_app_passwords: Basic: %d", rec.Code)
	}
	if rec := st.login(tlsBase, "anna@example.test", "correct horse"); rec.Code != 200 {
		t.Errorf("require_app_passwords: web login: %d", rec.Code)
	}

	st = newSite(t)
	st.enrol("anna@example.test", "correct horse")
	if got := st.mail(store.ScopeSMTP, "anna@example.test", "correct horse"); got != 401 {
		t.Errorf("an account with TOTP, its password over SMTP: %d", got)
	}
}

// An account password over Basic is remembered for a few minutes, but what
// is remembered is checked against the account as it is now: a changed
// password or a first app password ends it at once, on every node.
func TestBasicMemoryFollowsTheAccount(t *testing.T) {
	st := newSite(t)
	b := st.node()
	for _, n := range []*site{st, b} {
		if rec := n.do("POST", tlsBase+"/jmap/api", "{}", basic("anna@example.test", "correct horse")); rec.Code != 200 {
			t.Fatal(rec.Code)
		}
	}
	c := st.in("anna@example.test", "correct horse")
	if rec := st.do("POST", tlsBase+"/auth/password", `{"current":"correct horse","new":"a new password"}`, cookie(c)); rec.Code != 204 {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	for name, n := range map[string]*site{"this node": st, "the other node": b} {
		if rec := n.do("POST", tlsBase+"/jmap/api", "{}", basic("anna@example.test", "correct horse")); rec.Code != 401 {
			t.Errorf("%s: Basic with the old password: %d", name, rec.Code)
		}
		if rec := n.do("POST", tlsBase+"/jmap/api", "{}", basic("anna@example.test", "a new password")); rec.Code != 200 {
			t.Errorf("%s: Basic with the new password: %d", name, rec.Code)
		}
	}
	st.appPassword(c, "a new password", "Phone")
	if rec := b.do("POST", tlsBase+"/jmap/api", "{}", basic("anna@example.test", "a new password")); rec.Code != 401 {
		t.Errorf("Basic with the account password after the first app password: %d", rec.Code)
	}
}

// The use of an app password is written rarely: once, then again after
// ten minutes or from another address.
func TestAppPasswordLastUse(t *testing.T) {
	st := newSite(t)
	c := st.in("anna@example.test", "correct horse")
	id, secret := st.appPassword(c, "correct horse", "Phone")
	login := func(n *site, ip string) {
		t.Helper()
		if _, status, _ := n.s.MailLogin(context.Background(), ip, store.ScopeIMAP, "anna@example.test", secret); status != 0 {
			t.Fatalf("login: %d", status)
		}
	}
	for i := 0; i < 20; i++ {
		login(st, "192.0.2.50")
	}
	if st.accounts.writes != 1 {
		t.Errorf("20 logins wrote %d times", st.accounts.writes)
	}
	p, _ := st.accounts.AppPassword(context.Background(), "acct-anna", id)
	if p.UsedIP != "192.0.2.50" || !p.UsedAt.Equal(st.clock) {
		t.Errorf("last use: %+v", p)
	}
	// Another node reads the row and sees it is fresh.
	b := st.node()
	login(b, "192.0.2.50")
	if st.accounts.writes != 1 {
		t.Errorf("a login on another node wrote again: %d", st.accounts.writes)
	}
	login(st, "198.51.100.1")
	if st.accounts.writes != 2 {
		t.Errorf("a login from another address: %d writes", st.accounts.writes)
	}
	st.clock = st.clock.Add(lastUseEvery + time.Second)
	login(st, "198.51.100.1")
	if st.accounts.writes != 3 {
		t.Errorf("a login ten minutes later: %d writes", st.accounts.writes)
	}
	if got := st.accounts.eventsOf(EventLogin); len(got) != 4 || !strings.Contains(got[1].Detail, `imap: app password "Phone"`) {
		t.Errorf("login events (one for the web login, three for the app password): %+v", got)
	}
}

// Wrong secrets from a mail client are counted apart from wrong passwords
// at the web login: a phone with a stale password does not close the
// webmail. The address is still limited at each door, and the IP overall.
func TestMailFailuresDoNotCloseTheWebLogin(t *testing.T) {
	st := newSite(t)
	limits := &fakeLimits{now: func() time.Time { return st.clock }, m: map[string]store.LoginFailure{}}
	st.s.Limits = limits
	for i := 0; i < maxFailsPerAddr; i++ {
		if got := st.mail(store.ScopeIMAP, "anna@example.test", "stale password"); got != 401 {
			t.Fatalf("attempt %d: %d", i, got)
		}
	}
	if got := st.mail(store.ScopeIMAP, "anna@example.test", "correct horse"); got != 429 {
		t.Errorf("IMAP after %d failures: %d", maxFailsPerAddr, got)
	}
	// On another node too, and for the other mail protocols.
	b := st.node()
	if got := b.mail(store.ScopeSMTP, "anna@example.test", "correct horse"); got != 429 {
		t.Errorf("SMTP on another node: %d", got)
	}
	if rec := st.login(tlsBase, "anna@example.test", "correct horse", from("203.0.113.1:1")); rec.Code != 200 {
		t.Errorf("web login after the mail client's failures: %d", rec.Code)
	}
	if limits.m["m:anna@example.test"].Count != maxFailsPerAddr || limits.m["a:anna@example.test"].Count != 0 ||
		limits.m["i:192.0.2.50"].Count != maxFailsPerAddr {
		t.Errorf("counters: %+v", limits.m)
	}
}

// enrol turns TOTP on for a user and returns the secret and the recovery
// codes.
func (st *site) enrol(user, password string) (secret []byte, codes []string) {
	st.t.Helper()
	c := st.in(user, password)
	body, _ := json.Marshal(map[string]string{"password": password})
	rec := st.do("POST", tlsBase+"/auth/totp/enrol", string(body), cookie(c))
	var e struct{ Secret, URI string }
	decode(st.t, rec, &e)
	if rec.Code != 200 {
		st.t.Fatalf("enrol: %d %s", rec.Code, rec.Body.String())
	}
	state, _ := st.accounts.TOTPState(context.Background(), st.accountID(user))
	if totp.EncodeSecret(state.Secret) != e.Secret {
		st.t.Fatalf("the secret shown is not the one stored")
	}
	rec = st.do("POST", tlsBase+"/auth/totp/confirm", fmt.Sprintf(`{"code":%q}`, totp.Code(state.Secret, totp.Step(st.clock))), cookie(c))
	var got struct{ RecoveryCodes []string }
	decode(st.t, rec, &got)
	if rec.Code != 200 || len(got.RecoveryCodes) != recoveryCodes {
		st.t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	// The time step that confirmed is spent; tests go on in the next.
	st.clock = st.clock.Add(totp.Period * time.Second)
	return state.Secret, got.RecoveryCodes
}

func (st *site) accountID(user string) string {
	a, err := st.accounts.Account(context.Background(), user)
	if err != nil {
		st.t.Fatal(err)
	}
	return a.ID
}

func (st *site) code(secret []byte, steps int) string {
	return totp.Code(secret, totp.Step(st.clock)+int64(steps))
}

func (st *site) second(c *http.Cookie, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	return st.do("POST", tlsBase+"/auth/login/totp", body, append(opts, cookie(c))...)
}

// Setting TOTP up: a secret as text, as otpauth URI and as a QR code of
// our own drawing, confirmed with a code before it counts.
func TestTOTPEnrol(t *testing.T) {
	st := newSite(t)
	const anna = "anna@example.test"
	c := st.in(anna, "correct horse")
	if rec := st.do("POST", tlsBase+"/auth/totp/enrol", `{"password":"wrong"}`, cookie(c)); rec.Code != 403 {
		t.Errorf("enrol with a wrong password: %d", rec.Code)
	}
	rec := st.do("POST", tlsBase+"/auth/totp/enrol", `{"password":"correct horse"}`, cookie(c))
	var e struct {
		Secret, URI string
		QR          []string
	}
	decode(t, rec, &e)
	if rec.Code != 200 || len(e.Secret) != 32 ||
		!strings.HasPrefix(e.URI, "otpauth://totp/mail.example.test:anna@example.test?") || !strings.Contains(e.URI, "secret="+e.Secret) {
		t.Fatalf("enrol: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.QR) < 21 || len(e.QR) != len(e.QR[0]) || strings.Trim(e.QR[0], "01") != "" {
		t.Errorf("QR code: %d rows of %q", len(e.QR), e.QR[0])
	}
	// Nothing changed for the login yet, and the mail client still gets in.
	if rec := st.login(tlsBase, anna, "correct horse"); !strings.Contains(rec.Body.String(), "accountId") {
		t.Errorf("login while TOTP is pending: %s", rec.Body.String())
	}
	var state struct {
		Enabled       bool
		RecoveryCodes int
	}
	decode(t, st.do("GET", tlsBase+"/auth/totp", "", cookie(c)), &state)
	if state.Enabled {
		t.Error("enabled before the code was confirmed")
	}
	secret, _ := st.accounts.TOTPState(context.Background(), "acct-anna")
	for _, wrong := range []string{"000000", "", "12345", st.code(secret.Secret, 5)} {
		if rec := st.do("POST", tlsBase+"/auth/totp/confirm", fmt.Sprintf(`{"code":%q}`, wrong), cookie(c)); rec.Code != 401 {
			t.Errorf("confirming with %q: %d", wrong, rec.Code)
		}
	}
	rec = st.do("POST", tlsBase+"/auth/totp/confirm", fmt.Sprintf(`{"code":%q}`, st.code(secret.Secret, 0)), cookie(c))
	var got struct{ RecoveryCodes []string }
	decode(t, rec, &got)
	seen := map[string]bool{}
	for _, code := range got.RecoveryCodes {
		if !regexp.MustCompile(`^([a-z2-7]{4}-){3}[a-z2-7]{4}$`).MatchString(code) || seen[code] {
			t.Errorf("recovery code %q", code)
		}
		seen[code] = true
		n, _ := users.NormalizeRecoveryCode(code)
		if !st.accounts.recovery["acct-anna"][users.HashRecoveryCode(n)] || st.accounts.recovery["acct-anna"][n] {
			t.Errorf("recovery code %q is not stored as its hash", code)
		}
	}
	if rec.Code != 200 || len(seen) != recoveryCodes {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	decode(t, st.do("GET", tlsBase+"/auth/totp", "", cookie(c)), &state)
	if !state.Enabled || state.RecoveryCodes != recoveryCodes {
		t.Errorf("state: %+v", state)
	}
	// Once on, a new secret needs it turned off first.
	if rec := st.do("POST", tlsBase+"/auth/totp/enrol", `{"password":"correct horse"}`, cookie(c)); rec.Code != 409 {
		t.Errorf("enrol while on: %d", rec.Code)
	}
	if rec := st.do("POST", tlsBase+"/auth/totp/confirm", `{"code":"123456"}`, cookie(c)); rec.Code != 409 {
		t.Errorf("confirm while on: %d", rec.Code)
	}
	if len(st.accounts.eventsOf(EventTOTPEnabled)) != 1 {
		t.Error("no event for turning TOTP on")
	}
}

// The login in two steps. Nothing is said about the password or the
// account's TOTP until the password was right; a code works once, on any
// node; guesses at codes are limited.
func TestTwoStepLogin(t *testing.T) {
	st := newSite(t)
	const anna, bob = "anna@example.test", "bob@example.test"
	secret, recovery := st.enrol(anna, "correct horse")

	// A wrong password for the account with TOTP and for one without: the
	// same status, the same body, no cookie. An unknown address too.
	var answers []string
	for _, user := range []string{anna, bob, "nobody@example.test"} {
		rec := st.login(tlsBase, user, "wrong", from("203.0.113.1:1"))
		if rec.Code != 401 || setCookie(rec) != nil {
			t.Errorf("%s with a wrong password: %d, cookie %v", user, rec.Code, setCookie(rec))
		}
		answers = append(answers, rec.Body.String())
	}
	if answers[0] != answers[1] || answers[0] != answers[2] || strings.Contains(strings.ToLower(answers[0]), "totp") {
		t.Errorf("answers to a wrong password differ or mention the second step: %q", answers)
	}

	// The right password: no session yet, but a login that waits.
	rec := st.login(tlsBase, anna, "correct horse")
	pending := cookieNamed(rec, "__Host-robinmail-login")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"step":"totp"}` || pending == nil || cookieNamed(rec, "__Host-robinmail") != nil {
		t.Fatalf("step one: %d %s, cookies %v", rec.Code, rec.Body.String(), rec.Result().Cookies())
	}
	if !pending.Secure || !pending.HttpOnly || pending.SameSite != http.SameSiteStrictMode || pending.MaxAge != int(PendingTTL/time.Second) {
		t.Errorf("the cookie of the waiting login: %+v", pending)
	}
	// That cookie opens nothing, under either name.
	asSession := &http.Cookie{Name: "__Host-robinmail", Value: pending.Value}
	for _, c := range []*http.Cookie{pending, asSession} {
		if rec := st.do("POST", tlsBase+"/jmap/api", "{}", cookie(c)); rec.Code != 401 {
			t.Errorf("JMAP with the cookie of a waiting login as %s: %d", c.Name, rec.Code)
		}
		if rec := st.do("GET", tlsBase+"/auth/app-passwords", "", cookie(c)); rec.Code != 401 {
			t.Errorf("settings with the cookie of a waiting login as %s: %d", c.Name, rec.Code)
		}
	}
	// The login page can ask where it is.
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(pending)); rec.Code != 401 || !strings.Contains(rec.Body.String(), `"step":"totp"`) {
		t.Errorf("session while waiting: %d %s", rec.Code, rec.Body.String())
	}

	// Step two without step one.
	if rec := st.second(nil, fmt.Sprintf(`{"code":%q}`, st.code(secret, 0))); rec.Code != 401 || !strings.Contains(rec.Body.String(), "password first") {
		t.Errorf("a code without a waiting login: %d %s", rec.Code, rec.Body.String())
	}
	// A wrong code.
	if rec := st.second(pending, `{"code":"000000"}`); rec.Code != 401 || setCookie(rec) != nil {
		t.Errorf("a wrong code: %d", rec.Code)
	}
	// The right one, on another node: a session, a new token, and the
	// waiting login is gone.
	b := st.node()
	rec = b.second(pending, fmt.Sprintf(`{"code":%q}`, st.code(secret, 0)))
	sess := cookieNamed(rec, "__Host-robinmail")
	if rec.Code != 200 || sess == nil || sess.Value == pending.Value || !strings.Contains(rec.Body.String(), `"username":"anna@example.test"`) {
		t.Fatalf("step two: %d %s", rec.Code, rec.Body.String())
	}
	if gone := cookieNamed(rec, "__Host-robinmail-login"); gone == nil || gone.MaxAge >= 0 {
		t.Errorf("the cookie of the waiting login was not removed: %+v", gone)
	}
	if rec := st.do("POST", tlsBase+"/jmap/api", "{}", cookie(sess)); rec.Code != 200 {
		t.Errorf("JMAP with the session: %d", rec.Code)
	}
	if rec := st.second(pending, fmt.Sprintf(`{"code":%q}`, st.code(secret, 1))); rec.Code != 401 {
		t.Errorf("the waiting login a second time: %d", rec.Code)
	}
	var who struct {
		Method string
		TOTP   bool
	}
	decode(t, st.do("GET", tlsBase+"/auth/session", "", cookie(sess)), &who)
	if who.Method != "password+totp" || !who.TOTP {
		t.Errorf("session: %+v", who)
	}

	// RFC 6238 5.2: "The verifier MUST NOT accept the second attempt of
	// the OTP after the successful validation has been issued for the
	// first OTP". The same code, a new login, the first node.
	used := st.code(secret, 0)
	pending = cookieNamed(st.login(tlsBase, anna, "correct horse"), "__Host-robinmail-login")
	if rec := st.second(pending, fmt.Sprintf(`{"code":%q}`, used)); rec.Code != 401 || rec.Body.String() != "{\"error\":\"wrong code\"}\n" {
		t.Errorf("a code used before: %d %s", rec.Code, rec.Body.String())
	}
	// Nor the code of the step before the one that was used.
	if rec := st.second(pending, fmt.Sprintf(`{"code":%q}`, st.code(secret, -1))); rec.Code != 401 {
		t.Errorf("an older code after a newer one was used: %d", rec.Code)
	}
	// The next step's code is accepted (a clock one step ahead), with
	// spaces around it as people paste them.
	if rec := st.second(pending, fmt.Sprintf(`{"code":" %s "}`, st.code(secret, 1))); rec.Code != 200 {
		t.Errorf("the code of the next step: %d %s", rec.Code, rec.Body.String())
	}
	// Two steps off is too far.
	st.clock = st.clock.Add(10 * totp.Period * time.Second)
	pending = cookieNamed(st.login(tlsBase, anna, "correct horse"), "__Host-robinmail-login")
	for _, off := range []int{-2, 2} {
		if rec := st.second(pending, fmt.Sprintf(`{"code":%q}`, st.code(secret, off))); rec.Code != 401 {
			t.Errorf("a code %d steps off: %d", off, rec.Code)
		}
	}

	// A recovery code works instead, once.
	rec = st.second(pending, fmt.Sprintf(`{"recoveryCode":%q}`, strings.ToUpper(recovery[0])))
	if rec.Code != 200 || cookieNamed(rec, "__Host-robinmail") == nil {
		t.Fatalf("a recovery code: %d %s", rec.Code, rec.Body.String())
	}
	decode(t, st.do("GET", tlsBase+"/auth/session", "", cookie(cookieNamed(rec, "__Host-robinmail"))), &who)
	if who.Method != "password+recovery" {
		t.Errorf("method %q", who.Method)
	}
	pending = cookieNamed(st.login(tlsBase, anna, "correct horse"), "__Host-robinmail-login")
	if rec := b.second(pending, fmt.Sprintf(`{"recoveryCode":%q}`, recovery[0])); rec.Code != 401 {
		t.Errorf("the recovery code a second time, on another node: %d", rec.Code)
	}
	// Bob's waiting login does not take Anna's codes: there is none, he
	// has no TOTP, and a made-up cookie is no waiting login.
	if rec := st.second(&http.Cookie{Name: "__Host-robinmail-login", Value: "made-up"}, fmt.Sprintf(`{"recoveryCode":%q}`, recovery[1])); rec.Code != 401 {
		t.Errorf("a forged waiting login: %d", rec.Code)
	}
	var state struct{ RecoveryCodes int }
	decode(t, st.do("GET", tlsBase+"/auth/totp", "", cookie(sess)), &state)
	if state.RecoveryCodes != recoveryCodes-1 {
		t.Errorf("%d recovery codes left", state.RecoveryCodes)
	}

	// A login waits five minutes for its code.
	st.clock = st.clock.Add(PendingTTL + time.Second)
	if rec := st.second(pending, fmt.Sprintf(`{"code":%q}`, st.code(secret, 0))); rec.Code != 401 || !strings.Contains(rec.Body.String(), "password first") {
		t.Errorf("a code for a login that waited too long: %d %s", rec.Code, rec.Body.String())
	}

	if got := st.accounts.eventsOf(EventCodeFailed); len(got) < 5 {
		t.Errorf("%d events for wrong codes", len(got))
	}
	if got := st.accounts.eventsOf(EventRecoveryUsed); len(got) != 1 {
		t.Errorf("%d events for the recovery code", len(got))
	}
}

// Guessing codes: five wrong ones in fifteen minutes close the second step
// for the account, on every node, also for the right code.
func TestCodeRateLimit(t *testing.T) {
	st := newSite(t)
	limits := &fakeLimits{now: func() time.Time { return st.clock }, m: map[string]store.LoginFailure{}}
	st.s.Limits = limits
	secret, recovery := st.enrol("anna@example.test", "correct horse")
	b := st.node()
	pending := cookieNamed(st.login(tlsBase, "anna@example.test", "correct horse"), "__Host-robinmail-login")
	for i := 0; i < maxFailsPerCode; i++ {
		n := []*site{st, b}[i%2]
		body := fmt.Sprintf(`{"code":"%06d"}`, i)
		if i == 3 {
			body = `{"recoveryCode":"aaaa-aaaa-aaaa-aaaa"}` // guesses at recovery codes count too
		}
		if rec := n.second(pending, body, from(fmt.Sprintf("198.51.100.%d:1", i))); rec.Code != 401 {
			t.Fatalf("guess %d: %d", i, rec.Code)
		}
	}
	for name, body := range map[string]string{"the right code": fmt.Sprintf(`{"code":%q}`, st.code(secret, 0)),
		"a recovery code": fmt.Sprintf(`{"recoveryCode":%q}`, recovery[0])} {
		rec := st.node().second(pending, body, from("203.0.113.5:1"))
		if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s after %d wrong ones: %d", name, maxFailsPerCode, rec.Code)
		}
	}
	if limits.m["t:anna@example.test"].Count != maxFailsPerCode || limits.m["a:anna@example.test"].Count != 0 {
		t.Errorf("counters: %+v", limits.m)
	}
	// The password step is not closed by it, and after the window the
	// code works. The recovery code was not spent by the refused try.
	st.clock = st.clock.Add(loginWindow + time.Second)
	pending = cookieNamed(st.login(tlsBase, "anna@example.test", "correct horse"), "__Host-robinmail-login")
	if rec := st.second(pending, fmt.Sprintf(`{"recoveryCode":%q}`, recovery[0])); rec.Code != 200 {
		t.Errorf("after the window: %d %s", rec.Code, rec.Body.String())
	}
}

// Turning TOTP off, and making new recovery codes, take the password and
// a code.
func TestTOTPDisable(t *testing.T) {
	st := newSite(t)
	const anna = "anna@example.test"
	secret, recovery := st.enrol(anna, "correct horse")
	rec := st.login(tlsBase, anna, "correct horse")
	rec = st.second(cookieNamed(rec, "__Host-robinmail-login"), fmt.Sprintf(`{"code":%q}`, st.code(secret, 0)))
	c := cookieNamed(rec, "__Host-robinmail")
	st.clock = st.clock.Add(totp.Period * time.Second)

	// Both proofs are needed, for both. (Each wrong code counts against
	// the limit of five, so the three wrong ones are spread over the two.)
	for i, tc := range []struct {
		name, body string
		want       int
	}{
		{"no password", fmt.Sprintf(`{"code":%q}`, st.code(secret, 0)), 403},
		{"wrong password", fmt.Sprintf(`{"password":"wrong","code":%q}`, st.code(secret, 0)), 403},
		{"no code", `{"password":"correct horse"}`, 401},
		{"wrong code", `{"password":"correct horse","code":"000000"}`, 401},
		{"an invented recovery code", `{"password":"correct horse","recoveryCode":"aaaa-aaaa-aaaa-aaaa"}`, 401},
	} {
		paths := []string{"/auth/totp/disable", "/auth/totp/recovery-codes"}
		if tc.want == 401 {
			paths = paths[i%2 : i%2+1]
		}
		for _, path := range paths {
			if rec := st.do("POST", tlsBase+path, tc.body, cookie(c)); rec.Code != tc.want {
				t.Errorf("%s, %s: %d, want %d", path, tc.name, rec.Code, tc.want)
			}
		}
	}
	if a, _ := st.accounts.Account(context.Background(), anna); !a.TOTP {
		t.Fatal("TOTP went off")
	}

	// New recovery codes: the old ones are dead.
	rec = st.do("POST", tlsBase+"/auth/totp/recovery-codes", fmt.Sprintf(`{"password":"correct horse","code":%q}`, st.code(secret, 0)), cookie(c))
	var fresh struct{ RecoveryCodes []string }
	decode(t, rec, &fresh)
	if rec.Code != 200 || len(fresh.RecoveryCodes) != recoveryCodes || fresh.RecoveryCodes[0] == recovery[0] {
		t.Fatalf("new recovery codes: %d %s", rec.Code, rec.Body.String())
	}
	pending := cookieNamed(st.login(tlsBase, anna, "correct horse"), "__Host-robinmail-login")
	if rec := st.second(pending, fmt.Sprintf(`{"recoveryCode":%q}`, recovery[0])); rec.Code != 401 {
		t.Errorf("an old recovery code after new ones were made: %d", rec.Code)
	}

	// Off, with a recovery code as the second proof.
	rec = st.do("POST", tlsBase+"/auth/totp/disable", fmt.Sprintf(`{"password":"correct horse","recoveryCode":%q}`, fresh.RecoveryCodes[0]), cookie(c))
	if rec.Code != 204 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	// The login is one step again, the login that waited is gone, and so
	// are the recovery codes.
	if rec := st.second(pending, fmt.Sprintf(`{"recoveryCode":%q}`, fresh.RecoveryCodes[1])); rec.Code != 401 {
		t.Errorf("a waiting login after TOTP went off: %d", rec.Code)
	}
	if rec := st.login(tlsBase, anna, "correct horse"); !strings.Contains(rec.Body.String(), "accountId") {
		t.Errorf("login after disabling: %s", rec.Body.String())
	}
	if rec := st.do("POST", tlsBase+"/auth/totp/disable", `{"password":"correct horse","code":"123456"}`, cookie(c)); rec.Code != 409 {
		t.Errorf("disable when off: %d", rec.Code)
	}
	if len(st.accounts.eventsOf(EventTOTPDisabled)) != 1 || len(st.accounts.eventsOf(EventRecoveryReplaced)) != 1 {
		t.Error("events for turning TOTP off and for new recovery codes")
	}
}

// Sessions: a list with device and address, revoking one or all others,
// and rotation: a login never keeps the session the browser came with.
func TestSessionList(t *testing.T) {
	st := newSite(t)
	const anna = "anna@example.test"
	firefox := header("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:140.0) Gecko/20100101 Firefox/140.0")
	phone := header("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/19.0 Mobile/15E148 Safari/604.1")
	one := cookieNamed(st.login(tlsBase, anna, "correct horse", firefox, from("192.0.2.10:1")), "__Host-robinmail")
	st.clock = st.clock.Add(time.Hour)
	two := cookieNamed(st.login(tlsBase, anna, "correct horse", phone, from("198.51.100.20:1")), "__Host-robinmail")
	other := st.in("bob@example.test", "battery staple")

	type row struct {
		ID, Device, Method, IP, SeenIP string
		Current, Remember              bool
		CreatedAt, SeenAt, ExpiresAt   time.Time
	}
	list := func(c *http.Cookie, opts ...reqOpt) []row {
		t.Helper()
		var got struct{ List []row }
		rec := st.do("GET", tlsBase+"/auth/sessions", "", append(opts, cookie(c))...)
		decode(t, rec, &got)
		if rec.Code != 200 {
			t.Fatalf("sessions: %d %s", rec.Code, rec.Body.String())
		}
		return got.List
	}
	got := list(two, from("198.51.100.20:1"))
	if len(got) != 2 || !got[0].Current || got[0].Device != "Safari on iPhone" || got[0].IP != "198.51.100.20" ||
		got[1].Current || got[1].Device != "Firefox on macOS" || got[1].IP != "192.0.2.10" || got[1].Method != "password" ||
		!got[1].CreatedAt.Equal(st.clock.Add(-time.Hour)) || !got[1].ExpiresAt.Equal(got[1].SeenAt.Add(SessionIdle)) {
		t.Fatalf("list: %+v", got)
	}
	if strings.Contains(fmt.Sprint(got), one.Value) || strings.Contains(fmt.Sprint(got), tokenHash(one.Value)) {
		t.Error("the list shows a token or its hash")
	}
	// Bob sees his own only and cannot revoke Anna's.
	if l := list(other); len(l) != 1 {
		t.Errorf("bob's list: %+v", l)
	}
	if rec := st.do("DELETE", tlsBase+"/auth/sessions/"+got[1].ID, "", cookie(other)); rec.Code != 404 {
		t.Errorf("bob revoking anna's session: %d", rec.Code)
	}

	// Revoke the other one.
	if rec := st.do("DELETE", tlsBase+"/auth/sessions/"+got[1].ID, "", cookie(two)); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := st.do("POST", tlsBase+"/jmap/api", "{}", cookie(one)); rec.Code != 401 {
		t.Errorf("the revoked session: %d", rec.Code)
	}
	if rec := st.do("POST", tlsBase+"/jmap/api", "{}", cookie(two)); rec.Code != 200 {
		t.Errorf("the session that revoked: %d", rec.Code)
	}

	// Revoke all others.
	three := st.in(anna, "correct horse")
	four := st.in(anna, "correct horse")
	rec := st.do("POST", tlsBase+"/auth/sessions/revoke-others", "{}", cookie(two))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"revoked":2}` {
		t.Errorf("revoke others: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []*http.Cookie{three, four} {
		if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(c)); rec.Code != 401 {
			t.Errorf("a session after revoke-others: %d", rec.Code)
		}
	}
	if l := list(two); len(l) != 1 || !l[0].Current {
		t.Errorf("after revoke-others: %+v", l)
	}
	// Revoking your own is a logout: the cookie goes.
	rec = st.do("DELETE", tlsBase+"/auth/sessions/"+got[0].ID, "", cookie(two))
	if gone := cookieNamed(rec, "__Host-robinmail"); rec.Code != 204 || gone == nil || gone.MaxAge >= 0 {
		t.Errorf("revoking the current session: %d %+v", rec.Code, gone)
	}

	// Rotation: logging in with a session cookie in the browser replaces
	// that session, whoever it belonged to.
	old := st.in(anna, "correct horse")
	fresh := cookieNamed(st.login(tlsBase, anna, "correct horse", cookie(old)), "__Host-robinmail")
	if fresh.Value == old.Value {
		t.Fatal("a login kept the token it came with")
	}
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(old)); rec.Code != 401 {
		t.Errorf("the session a login came with still works: %d", rec.Code)
	}
	// A cookie somebody planted is not adopted: its value is never what
	// the new session gets.
	planted := &http.Cookie{Name: "__Host-robinmail", Value: "chosen-by-an-attacker"}
	got2 := cookieNamed(st.login(tlsBase, anna, "correct horse", cookie(planted)), "__Host-robinmail")
	if got2.Value == planted.Value || len(got2.Value) < 40 {
		t.Errorf("session fixation: %q", got2.Value)
	}
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(planted)); rec.Code != 401 {
		t.Errorf("the planted cookie: %d", rec.Code)
	}
}

// A session has two lifetimes: it ends when it was not used for the idle
// time, and at the latest after the absolute one. "Remember this device"
// makes both longer and the cookie outlive the browser.
func TestSessionLifetimes(t *testing.T) {
	const anna = "anna@example.test"
	alive := func(st *site, c *http.Cookie) bool {
		return st.do("GET", tlsBase+"/auth/session", "", cookie(c)).Code == 200
	}

	st := newSite(t)
	c := st.in(anna, "correct horse")
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Errorf("without remember the cookie should go with the browser: %+v", c)
	}
	// In use every few hours, it lives until the absolute end.
	start := st.clock
	for st.clock.Sub(start) < SessionMax-6*time.Hour {
		st.clock = st.clock.Add(6 * time.Hour)
		if !alive(st, c) {
			t.Fatalf("a session in use ended after %v", st.clock.Sub(start))
		}
	}
	st.clock = start.Add(SessionMax + time.Second)
	if alive(st, c) {
		t.Errorf("a session outlived its absolute lifetime of %v", SessionMax)
	}
	// Left alone, it ends after the idle time.
	c = st.in(anna, "correct horse")
	st.clock = st.clock.Add(SessionIdle - time.Minute)
	if !alive(st, c) {
		t.Error("ended before the idle time")
	}
	st.clock = st.clock.Add(SessionIdle + time.Second)
	if alive(st, c) {
		t.Errorf("a session that was not used for %v is still alive", SessionIdle)
	}

	// "Last seen" is written every few minutes, not with every request.
	st = newSite(t)
	c = st.in(anna, "correct horse")
	for i := 0; i < 50; i++ {
		st.do("POST", tlsBase+"/jmap/api", "{}", cookie(c))
	}
	if st.accounts.writes != 0 {
		t.Errorf("50 requests in the same minute wrote %d times", st.accounts.writes)
	}
	st.clock = st.clock.Add(touchEvery + time.Second)
	for i := 0; i < 50; i++ {
		st.do("POST", tlsBase+"/jmap/api", "{}", cookie(c))
	}
	if st.accounts.writes != 1 {
		t.Errorf("after five minutes: %d writes", st.accounts.writes)
	}
	// From another address it is written at once: the list should show it.
	st.do("POST", tlsBase+"/jmap/api", "{}", cookie(c), from("203.0.113.77:1"))
	if st.accounts.writes != 2 {
		t.Errorf("a request from another address: %d writes", st.accounts.writes)
	}

	// Remember this device.
	st = newSite(t)
	rec := st.do("POST", tlsBase+"/auth/login", `{"username":"anna@example.test","password":"correct horse","remember":true}`)
	c = cookieNamed(rec, "__Host-robinmail")
	if c == nil || c.MaxAge != int(RememberMax/time.Second) {
		t.Fatalf("remember: cookie %+v", c)
	}
	st.clock = st.clock.Add(RememberIdle - time.Hour)
	if !alive(st, c) {
		t.Error("a remembered session ended before its idle time")
	}
	st.clock = st.clock.Add(RememberIdle + time.Second)
	if alive(st, c) {
		t.Error("a remembered session outlived its idle time")
	}
}

// Changing the password takes the current one, and ends every other
// session of the account.
func TestChangePassword(t *testing.T) {
	st := newSite(t)
	const anna = "anna@example.test"
	mine := st.in(anna, "correct horse")
	other := st.in(anna, "correct horse")
	bob := st.in("bob@example.test", "battery staple")
	app, _ := users.NewAppPassword()
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"wrong current":       {`{"current":"wrong","new":"a new password"}`, 403},
		"no current":          {`{"new":"a new password"}`, 403},
		"too short":           {`{"current":"correct horse","new":"short"}`, 400},
		"an app password":     {fmt.Sprintf(`{"current":"correct horse","new":%q}`, app), 400},
		"longer than allowed": {fmt.Sprintf(`{"current":"correct horse","new":%q}`, strings.Repeat("x", maxPasswordLen+1)), 400},
	} {
		if rec := st.do("POST", tlsBase+"/auth/password", tc.body, cookie(mine)); rec.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if rec := st.do("POST", tlsBase+"/auth/password", `{"current":"correct horse","new":"a new password"}`); rec.Code != 401 {
		t.Errorf("without a session: %d", rec.Code)
	}
	if rec := st.do("POST", tlsBase+"/auth/password", `{"current":"correct horse","new":"a new password"}`, cookie(mine)); rec.Code != 204 {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(mine)); rec.Code != 200 {
		t.Errorf("the session that changed the password: %d", rec.Code)
	}
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(other)); rec.Code != 401 {
		t.Errorf("another session of the account: %d", rec.Code)
	}
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(bob)); rec.Code != 200 {
		t.Errorf("another account's session: %d", rec.Code)
	}
	if rec := st.login(tlsBase, anna, "correct horse"); rec.Code != 401 {
		t.Errorf("login with the old password: %d", rec.Code)
	}
	if rec := st.login(tlsBase, anna, "a new password"); rec.Code != 200 {
		t.Errorf("login with the new password: %d", rec.Code)
	}
	if got := st.accounts.eventsOf(EventPasswordChanged); len(got) != 1 || got[0].AccountID != "acct-anna" {
		t.Errorf("events: %+v", got)
	}

	// Wrong current passwords are guesses at the password: they count.
	st = newSite(t)
	mine = st.in(anna, "correct horse")
	for i := 0; i < maxFailsPerAddr; i++ {
		st.do("POST", tlsBase+"/auth/password", `{"current":"guess","new":"a new password"}`, cookie(mine))
	}
	if rec := st.do("POST", tlsBase+"/auth/password", `{"current":"correct horse","new":"a new password"}`, cookie(mine)); rec.Code != 429 {
		t.Errorf("after %d wrong current passwords: %d", maxFailsPerAddr, rec.Code)
	}
}

// An account that an OIDC login made has no password. It cannot log in
// with one, and its sensitive changes ask for a fresh login instead.
func TestAccountWithoutPassword(t *testing.T) {
	st := newSite(t)
	st.accounts.accounts["acct-oidc"] = &store.Account{ID: "acct-oidc", Address: "carol@example.test", PasswordHash: store.NoPassword}
	for _, pw := range []string{"", "!", store.NoPassword, "hash:!"} {
		if rec := st.login(tlsBase, "carol@example.test", pw); rec.Code != 401 {
			t.Errorf("login with %q: %d", pw, rec.Code)
		}
		if got := st.mail(store.ScopeIMAP, "carol@example.test", pw); got != 401 {
			t.Errorf("IMAP with %q: %d", pw, got)
		}
	}
	// A session as an OIDC login would make it.
	token, _ := newToken()
	st.accounts.CreateSession(context.Background(), tokenHash(token), store.Session{ID: "s", AccountID: "acct-oidc", Stage: store.StageFull,
		Method: "oidc:test", ExpiresAt: st.clock.Add(SessionMax), Idle: SessionIdle}, "")
	c := &http.Cookie{Name: "__Host-robinmail", Value: token}
	if rec := st.do("POST", tlsBase+"/auth/app-passwords", `{"name":"Phone"}`, cookie(c)); rec.Code != 201 {
		t.Errorf("an app password right after logging in: %d %s", rec.Code, rec.Body.String())
	}
	if rec := st.do("POST", tlsBase+"/auth/password", `{"current":"","new":"a new password"}`, cookie(c)); rec.Code != 403 {
		t.Errorf("setting a password on an account without one: %d", rec.Code)
	}
	st.clock = st.clock.Add(freshLogin + time.Minute)
	rec := st.do("POST", tlsBase+"/auth/app-passwords", `{"name":"Laptop"}`, cookie(c))
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), `"reason":"stale"`) {
		t.Errorf("an app password with a login that is not fresh: %d %s", rec.Code, rec.Body.String())
	}
}

// Every endpoint under /auth that changes something refuses a request
// from another site, one that is not JSON, and (but for the login) one
// without a session. None of them changes anything on GET.
func TestAuthEndpointsAreGuarded(t *testing.T) {
	st := newSite(t)
	c := st.in("anna@example.test", "correct horse")
	changing := []struct{ method, path string }{
		{"POST", "/auth/login"}, {"POST", "/auth/login/totp"}, {"POST", "/auth/logout"},
		{"POST", "/auth/password"},
		{"POST", "/auth/app-passwords"}, {"DELETE", "/auth/app-passwords/some-id"},
		{"POST", "/auth/totp/enrol"}, {"POST", "/auth/totp/confirm"}, {"POST", "/auth/totp/disable"}, {"POST", "/auth/totp/recovery-codes"},
		{"DELETE", "/auth/sessions/some-id"}, {"POST", "/auth/sessions/revoke-others"},
	}
	needSession := func(path string) bool { return !strings.HasPrefix(path, "/auth/login") && path != "/auth/logout" }
	for _, e := range changing {
		name := e.method + " " + e.path
		// From another site, with the user's cookie, as a browser would
		// send it if the cookie were not SameSite=Strict.
		for _, o := range []reqOpt{header("Origin", "https://evil.example"), header("Sec-Fetch-Site", "cross-site"), header("Sec-Fetch-Site", "same-site")} {
			if rec := st.do(e.method, tlsBase+e.path, "{}", cookie(c), o); rec.Code != 403 {
				t.Errorf("%s from another site: %d", name, rec.Code)
			}
		}
		// As a form on another site could send it without a preflight.
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
			if rec := st.do(e.method, tlsBase+e.path, "{}", cookie(c), func(r *http.Request) {
				r.Header.Del("Content-Type")
				if ct != "" {
					r.Header.Set("Content-Type", ct)
				}
			}); rec.Code != 415 {
				t.Errorf("%s with content type %q: %d", name, ct, rec.Code)
			}
		}
		if needSession(e.path) {
			if rec := st.do(e.method, tlsBase+e.path, "{}"); rec.Code != 401 {
				t.Errorf("%s without a session: %d", name, rec.Code)
			}
		}
		// The same path does nothing on GET.
		if rec := st.do("GET", tlsBase+e.path, "", cookie(c)); e.path != "/auth/app-passwords" && rec.Code != 405 {
			t.Errorf("GET %s: %d", e.path, rec.Code)
		}
	}
	// Reading needs the session too, and the same origin.
	for _, path := range []string{"/auth/app-passwords", "/auth/totp", "/auth/sessions"} {
		if rec := st.do("GET", tlsBase+path, ""); rec.Code != 401 {
			t.Errorf("GET %s without a session: %d", path, rec.Code)
		}
		if rec := st.do("GET", tlsBase+path, "", cookie(c), header("Origin", "https://evil.example")); rec.Code != 403 {
			t.Errorf("GET %s from another site: %d", path, rec.Code)
		}
		if rec := st.do("GET", tlsBase+path, "", cookie(c)); rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s: %d, Cache-Control %q", path, rec.Code, rec.Header().Get("Cache-Control"))
		}
		// Basic credentials are for JMAP, not for the settings.
		if rec := st.do("GET", tlsBase+path, "", basic("anna@example.test", "correct horse")); rec.Code != 401 {
			t.Errorf("GET %s with Basic auth: %d", path, rec.Code)
		}
	}
	// After all that the session is still there and nothing was made.
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(c)); rec.Code != 200 {
		t.Errorf("the session after the refused requests: %d", rec.Code)
	}
	if len(st.accounts.apps) != 0 || len(st.accounts.totp) != 0 {
		t.Errorf("a refused request changed something: %v %v", st.accounts.apps, st.accounts.totp)
	}
	if rec := st.do("GET", tlsBase+"/auth/nothing", ""); rec.Code != 404 {
		t.Errorf("an unknown path: %d", rec.Code)
	}
}

// fakeChanges is a notify.Hub for one test: Changed wakes the listeners.
type fakeChanges struct {
	mu   sync.Mutex
	subs map[string][]chan struct{}
}

func (f *fakeChanges) Subscribe(account string) (<-chan struct{}, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = map[string][]chan struct{}{}
	}
	c := make(chan struct{}, 1)
	f.subs[account] = append(f.subs[account], c)
	return c, func() {}
}

func (f *fakeChanges) changed(account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.subs[account] {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (f *fakeChanges) listeners(account string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs[account])
}

// An event stream is open for hours. It ends when the session, or the app
// password, it was opened with is revoked, and not when something else of
// the account changes.
func TestEventStreamEndsWithItsLogin(t *testing.T) {
	st := newSite(t)
	changes := &fakeChanges{}
	st.s.AuthChanges = changes
	ended := make(chan string, 4)
	st.s.JMAP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/jmap/eventsource") {
			return
		}
		<-r.Context().Done()
		ended <- r.URL.RawQuery
	})
	st.h = st.s.Handler()
	const anna = "anna@example.test"
	c := st.in(anna, "correct horse")
	keep := st.in(anna, "correct horse")
	_, secret := st.appPassword(c, "correct horse", "Client")
	appID := st.accounts.apps["acct-anna"][0].ID

	open := func(name string, opt reqOpt) context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		before := changes.listeners("acct-anna")
		go func() {
			req := httptest.NewRequest("GET", tlsBase+"/jmap/eventsource/?"+name, nil).WithContext(ctx)
			opt(req)
			st.h.ServeHTTP(httptest.NewRecorder(), req)
		}()
		for changes.listeners("acct-anna") == before {
			time.Sleep(time.Millisecond)
		}
		return cancel
	}
	defer open("session", cookie(c))()
	defer open("kept", cookie(keep))()
	defer open("app", basic(anna, secret))()

	wait := func(want string) {
		t.Helper()
		select {
		case got := <-ended:
			if got != want {
				t.Fatalf("stream %q ended, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("stream %q did not end", want)
		}
	}
	none := func(why string) {
		t.Helper()
		select {
		case got := <-ended:
			t.Fatalf("%s: stream %q ended", why, got)
		case <-time.After(50 * time.Millisecond):
		}
	}
	// auth_seq moved for something else: every stream stays.
	changes.changed("acct-anna")
	none("a change that revoked nothing")

	var list struct{ List []struct{ ID string } }
	decode(t, st.do("GET", tlsBase+"/auth/sessions", "", cookie(c)), &list)
	var mine string
	for _, s := range list.List {
		if sess, _, _ := st.accounts.SessionByToken(context.Background(), tokenHash(c.Value)); sess.ID == s.ID {
			mine = s.ID
		}
	}
	if rec := st.do("DELETE", tlsBase+"/auth/sessions/"+mine, "", cookie(keep)); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	changes.changed("acct-anna")
	wait("session")
	none("revoking one session")

	if rec := st.do("DELETE", tlsBase+"/auth/app-passwords/"+appID, "", cookie(keep)); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	changes.changed("acct-anna")
	wait("app")
	none("revoking the app password")
}
