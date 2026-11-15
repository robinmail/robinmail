package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/oidc"
	"github.com/robinmail/robinmail/internal/oidc/oidctest"
	"github.com/robinmail/robinmail/internal/store"
)

// withProvider gives the site a fake OpenID provider, as the provider
// "test", with the real relying party code in front of it.
func (st *site) withProvider() (*oidctest.Provider, *OIDCProvider) {
	op := oidctest.New()
	st.t.Cleanup(op.Close)
	op.Now = func() time.Time { return st.clock }
	op.RedirectURIs = []string{tlsBase + "/auth/oidc/test/callback"}
	p := &OIDCProvider{Name: "test", Label: "Test ID", Client: oidc.New(oidc.Config{Issuer: op.Issuer, ClientID: op.ClientID,
		ClientSecret: op.ClientSecret, Scopes: []string{"openid", "email"}, Now: func() time.Time { return st.clock }})}
	st.s.OIDC = []*OIDCProvider{p}
	return op, p
}

var crossSite = header("Sec-Fetch-Site", "cross-site")

// begin clicks "Log in with": it returns the flow cookie and the callback
// URL the provider sends the browser to.
func (st *site) begin(query string) (flow *http.Cookie, callback string) {
	st.t.Helper()
	rec := st.do("GET", tlsBase+"/auth/oidc/test/start"+query, "", header("Sec-Fetch-Site", "same-origin"))
	flow = cookieNamed(rec, "__Host-robinmail-oidc")
	if rec.Code != http.StatusSeeOther || flow == nil {
		st.t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(rec.Header().Get("Location"))
	if err != nil {
		st.t.Fatal(err)
	}
	resp.Body.Close()
	callback = resp.Header.Get("Location")
	if resp.StatusCode != 302 || !strings.HasPrefix(callback, tlsBase+"/auth/oidc/test/callback?") {
		st.t.Fatalf("the provider answered %d, Location %q", resp.StatusCode, callback)
	}
	return flow, callback
}

// through runs a whole login through the provider and returns the answer
// of the callback.
func (st *site) through(query string) *httptest.ResponseRecorder {
	st.t.Helper()
	flow, callback := st.begin(query)
	return st.do("GET", callback, "", cookie(flow), crossSite)
}

// lands reports where the page the callback answered with goes on to.
func lands(rec *httptest.ResponseRecorder) string {
	_, rest, ok := strings.Cut(rec.Body.String(), `content="0;url=`)
	if !ok {
		return "(no page that goes on: " + rec.Body.String() + ")"
	}
	to, _, _ := strings.Cut(rest, `"`)
	return strings.ReplaceAll(to, "&amp;", "&")
}

// A login through a provider: the browser goes there with state, nonce
// and a PKCE challenge, comes back with a code, and gets a session for
// the account the verified email claim names.
func TestOIDCLogin(t *testing.T) {
	st := newSite(t)
	op, _ := st.withProvider()

	// The login page learns which providers there are.
	rec := st.do("GET", tlsBase+"/auth/config", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"providers":[{"name":"test","label":"Test ID"}]}` {
		t.Errorf("config: %d %s", rec.Code, rec.Body.String())
	}

	rec = st.do("GET", tlsBase+"/auth/oidc/test/start", "", header("Sec-Fetch-Site", "same-origin"))
	flow := cookieNamed(rec, "__Host-robinmail-oidc")
	if rec.Code != http.StatusSeeOther || flow == nil {
		t.Fatalf("start: %d", rec.Code)
	}
	// The one cookie that is Lax: it has to come back with the redirect
	// from the provider. Everything else as for the others.
	if flow.SameSite != http.SameSiteLaxMode || !flow.Secure || !flow.HttpOnly || flow.Path != "/" || flow.MaxAge != 600 {
		t.Errorf("flow cookie: %+v", flow)
	}
	to, _ := url.Parse(rec.Header().Get("Location"))
	q := to.Query()
	if !strings.HasPrefix(to.String(), op.Issuer+"/authorize?") || q.Get("redirect_uri") != tlsBase+"/auth/oidc/test/callback" ||
		q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 43 || len(q.Get("nonce")) < 43 || q.Get("scope") != "openid email" {
		t.Errorf("authorization request: %s", to)
	}

	rec = st.through("")
	sess := cookieNamed(rec, "__Host-robinmail")
	if rec.Code != 200 || sess == nil || lands(rec) != "/" {
		t.Fatalf("callback: %d, cookie %v, goes on to %s", rec.Code, sess, lands(rec))
	}
	// The session cookie is Strict like any, and the flow cookie is gone.
	if sess.SameSite != http.SameSiteStrictMode || !sess.HttpOnly || !sess.Secure || sess.MaxAge != 0 {
		t.Errorf("session cookie: %+v", sess)
	}
	if gone := cookieNamed(rec, "__Host-robinmail-oidc"); gone == nil || gone.MaxAge >= 0 {
		t.Errorf("the flow cookie was not removed: %+v", gone)
	}
	// Not a redirect: the browser would not send the Strict cookie along a
	// redirect that the provider's site started.
	if rec.Header().Get("Location") != "" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the callback's answer: %v", rec.Header())
	}
	var who struct {
		Username, Method string
		HasPassword      bool
	}
	decode(t, st.do("GET", tlsBase+"/auth/session", "", cookie(sess)), &who)
	if who.Username != "anna@example.test" || who.Method != "oidc:test" || !who.HasPassword {
		t.Errorf("session: %+v", who)
	}
	if got := st.accounts.eventsOf(EventLogin); len(got) != 1 || got[0].Detail != "web: oidc:test" {
		t.Errorf("events: %+v", got)
	}
	if st.accounts.links["acct-anna/test"] != "user-1" {
		t.Errorf("the provider's subject was not recorded: %v", st.accounts.links)
	}

	// The address is compared as addresses are: without regard to case.
	op.User["email"] = "Anna@Example.Test"
	if rec := st.through(""); cookieNamed(rec, "__Host-robinmail") == nil {
		t.Errorf("an address in capitals: goes on to %s", lands(rec))
	}
	// Remember this device carries through the flow.
	if c := cookieNamed(st.through("?remember=1"), "__Host-robinmail"); c == nil || c.MaxAge != int(RememberMax/time.Second) {
		t.Errorf("remember: %+v", c)
	}

	// A mail client is none the wiser: it needs the account password or
	// an app password, as before.
	if got := st.mail(store.ScopeIMAP, "anna@example.test", "correct horse"); got != 0 {
		t.Errorf("IMAP with the account password: %d", got)
	}
}

// What a callback must have to count: the cookie of the flow it answers,
// in this browser, once.
func TestOIDCCallbackIsBoundToTheBrowser(t *testing.T) {
	st := newSite(t)
	st.withProvider()
	refused := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if cookieNamed(rec, "__Host-robinmail") != nil || !strings.HasPrefix(lands(rec), "/login?error=") {
			t.Errorf("%s: cookie %v, goes on to %s", name, cookieNamed(rec, "__Host-robinmail"), lands(rec))
		}
	}
	flow, callback := st.begin("")
	// Without the cookie: a link somebody sent, or another browser.
	refused("no flow cookie", st.do("GET", callback, "", crossSite))
	// With the cookie of another flow: an attacker's callback URL, with
	// the attacker's code, opened in the victim's browser.
	otherFlow, otherCallback := st.begin("")
	refused("another flow's callback", st.do("GET", otherCallback, "", cookie(flow), crossSite))
	refused("another flow's cookie", st.do("GET", callback, "", cookie(otherFlow), crossSite))
	// A state that was changed, or left out.
	u, _ := url.Parse(callback)
	q := u.Query()
	q.Set("state", "changed")
	u.RawQuery = q.Encode()
	refused("a changed state", st.do("GET", u.String(), "", cookie(flow), crossSite))
	q.Del("state")
	u.RawQuery = q.Encode()
	refused("no state", st.do("GET", u.String(), "", cookie(flow), crossSite))
	// A cookie that is not one.
	for _, v := range []string{"", "x", "e30", strings.Repeat("A", 4000)} {
		refused("flow cookie "+v[:min(len(v), 5)], st.do("GET", callback, "", cookie(&http.Cookie{Name: "__Host-robinmail-oidc", Value: v}), crossSite))
	}
	// The real one works, once: the provider gives a code out once.
	if rec := st.do("GET", callback, "", cookie(flow), crossSite); cookieNamed(rec, "__Host-robinmail") == nil {
		t.Fatalf("the callback of the flow: goes on to %s", lands(rec))
	}
	refused("the callback a second time", st.do("GET", callback, "", cookie(flow), crossSite))
	if got := st.accounts.eventsOf(EventOIDCRefused); len(got) < 5 {
		t.Errorf("%d events for refused logins", len(got))
	}

	// Starting a login is for our own pages.
	if rec := st.do("GET", tlsBase+"/auth/oidc/test/start", "", crossSite); rec.Code != 403 {
		t.Errorf("start from another site: %d", rec.Code)
	}
	for _, path := range []string{"/auth/oidc/nobody/start", "/auth/oidc/nobody/callback?code=x&state=y"} {
		if rec := st.do("GET", tlsBase+path, ""); rec.Code != 404 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if rec := st.do("POST", tlsBase+"/auth/oidc/test/callback", "{}"); rec.Code != 405 {
		t.Errorf("POST to the callback: %d", rec.Code)
	}
}

// Which account a token maps to, and when there is none.
func TestOIDCAccountMapping(t *testing.T) {
	const email = "email"
	type setup func(st *site, op *oidctest.Provider, p *OIDCProvider)
	for name, tc := range map[string]struct {
		setup setup
		want  string // where the callback goes on to
		user  string // who is logged in, if anyone
	}{
		"no account for the address": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.User[email] = "carol@example.test" },
			"/login?error=oidc_no_account", ""},
		"the address is not verified": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.User["email_verified"] = false },
			"/login?error=oidc_unverified", ""},
		"no email_verified claim": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { delete(op.User, "email_verified") },
			"/login?error=oidc_unverified", ""},
		`email_verified is the string "true"`: {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.User["email_verified"] = "true" },
			"/login?error=oidc_unverified", ""},
		"not verified, and the provider is trusted with that": {func(st *site, op *oidctest.Provider, p *OIDCProvider) {
			delete(op.User, "email_verified")
			p.AllowUnverified = true
		}, "/", "anna@example.test"},
		"no email claim": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { delete(op.User, email) },
			"/login?error=oidc_no_account", ""},
		"an email claim that is no address": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.User[email] = "anna" },
			"/login?error=oidc_no_account", ""},
		"an email claim that is a list": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.User[email] = []string{"anna@example.test"} },
			"/login?error=oidc_no_account", ""},
		"another claim holds the address": {func(st *site, op *oidctest.Provider, p *OIDCProvider) {
			op.User = map[string]any{"sub": "u", "upn": "bob@example.test", email: "someone@else.example"}
			p.Claim = "upn"
		}, "/", "bob@example.test"},
		"a new account for a listed domain": {func(st *site, op *oidctest.Provider, p *OIDCProvider) {
			op.User[email] = "carol@example.test"
			p.AutoProvisionDomains = []string{"example.test"}
		}, "/", "carol@example.test"},
		"a listed domain the server does not accept mail for": {func(st *site, op *oidctest.Provider, p *OIDCProvider) {
			op.User[email] = "carol@elsewhere.example"
			p.AutoProvisionDomains = []string{"elsewhere.example"}
		}, "/login?error=oidc_no_account", ""},
		"an accepted domain that is not listed": {func(st *site, op *oidctest.Provider, p *OIDCProvider) {
			op.User[email] = "carol@example.test"
			p.AutoProvisionDomains = []string{"other.example"}
		}, "/login?error=oidc_no_account", ""},
		"the user said no at the provider": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.Deny = "access_denied" },
			"/login?error=oidc_denied", ""},
		"the provider answers with another error": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.Deny = "server_error" },
			"/login?error=oidc_failed", ""},
		"a token signed by nobody": {func(st *site, op *oidctest.Provider, p *OIDCProvider) { op.Alg = "none" },
			"/login?error=oidc_failed", ""},
		"a token for another client": {func(st *site, op *oidctest.Provider, p *OIDCProvider) {
			op.Claims = func(c map[string]any) { c["aud"] = "another-client" }
		}, "/login?error=oidc_failed", ""},
	} {
		t.Run(name, func(t *testing.T) {
			st := newSite(t)
			op, p := st.withProvider()
			tc.setup(st, op, p)
			rec := st.through("")
			sess := cookieNamed(rec, "__Host-robinmail")
			if lands(rec) != tc.want || (sess != nil) != (tc.user != "") {
				t.Fatalf("goes on to %s with cookie %v; want %s", lands(rec), sess, tc.want)
			}
			if tc.user != "" {
				var who struct{ Username string }
				decode(t, st.do("GET", tlsBase+"/auth/session", "", cookie(sess)), &who)
				if who.Username != tc.user {
					t.Errorf("logged in as %q, want %q", who.Username, tc.user)
				}
			}
			// Nothing about why is on the page; it is in the events.
			if tc.user == "" && (strings.Contains(rec.Body.String(), "example.test") || len(st.accounts.eventsOf(EventOIDCRefused)) != 1) {
				t.Errorf("page %q, events %+v", rec.Body.String(), st.accounts.eventsOf(EventOIDCRefused))
			}
		})
	}
}

// An account made by a first login has no password: the provider is its
// only way in, and a mail client needs an app password.
func TestOIDCProvisionedAccount(t *testing.T) {
	st := newSite(t)
	op, p := st.withProvider()
	op.User["email"] = "carol@example.test"
	p.AutoProvisionDomains = []string{"example.test"}
	sess := cookieNamed(st.through(""), "__Host-robinmail")
	if sess == nil {
		t.Fatal("no session")
	}
	acct, err := st.accounts.Account(context.Background(), "carol@example.test")
	if err != nil || acct.PasswordHash != store.NoPassword {
		t.Fatalf("account: %+v %v", acct, err)
	}
	if got := st.accounts.eventsOf(EventAccountCreated); len(got) != 1 || got[0].Address != "carol@example.test" {
		t.Errorf("events: %+v", got)
	}
	for _, pw := range []string{"", store.NoPassword, "anything"} {
		if rec := st.login(tlsBase, "carol@example.test", pw); rec.Code != 401 {
			t.Errorf("password login with %q: %d", pw, rec.Code)
		}
		if got := st.mail(store.ScopeIMAP, "carol@example.test", pw); got != 401 {
			t.Errorf("IMAP with %q: %d", pw, got)
		}
	}
	// Right after the login she can make an app password without a
	// password, and that opens IMAP.
	_, secret := st.appPassword(sess, "", "Phone")
	if got := st.mail(store.ScopeIMAP, "carol@example.test", secret); got != 0 {
		t.Errorf("IMAP with the app password: %d", got)
	}
	// The second login finds the account; no second one is made.
	if cookieNamed(st.through(""), "__Host-robinmail") == nil || len(st.accounts.eventsOf(EventAccountCreated)) != 1 {
		t.Error("the second login")
	}
}

// The provider's subject is recorded at the first login. When the same
// address later comes with another subject, it was given to somebody else
// at the provider, and that somebody does not get the mailbox.
func TestOIDCSubjectIsPinned(t *testing.T) {
	st := newSite(t)
	op, _ := st.withProvider()
	if cookieNamed(st.through(""), "__Host-robinmail") == nil {
		t.Fatal("first login")
	}
	op.User["sub"] = "user-2"
	rec := st.through("")
	if cookieNamed(rec, "__Host-robinmail") != nil || lands(rec) != "/login?error=oidc_no_account" {
		t.Errorf("the same address under another subject: goes on to %s", lands(rec))
	}
	op.User["sub"] = "user-1"
	if cookieNamed(st.through(""), "__Host-robinmail") == nil {
		t.Error("the first subject again")
	}
}

// An account with TOTP that logs in through a provider is not asked for
// its code: the second factor is the provider's business. Unless the
// provider is configured otherwise; then the login waits like one with a
// password.
func TestOIDCAndTOTP(t *testing.T) {
	st := newSite(t)
	_, p := st.withProvider()
	secret, _ := st.enrol("anna@example.test", "correct horse")
	rec := st.through("")
	if cookieNamed(rec, "__Host-robinmail") == nil || lands(rec) != "/" {
		t.Fatalf("with TOTP, through the provider: goes on to %s", lands(rec))
	}

	p.RequireTOTP = true
	rec = st.through("?remember=1")
	pending := cookieNamed(rec, "__Host-robinmail-login")
	if cookieNamed(rec, "__Host-robinmail") != nil || pending == nil || lands(rec) != "/login?step=totp" {
		t.Fatalf("require_totp: cookies %v, goes on to %s", rec.Result().Cookies(), lands(rec))
	}
	if rec := st.do("POST", tlsBase+"/jmap/api", "{}", cookie(pending)); rec.Code != 401 {
		t.Errorf("JMAP with the waiting login: %d", rec.Code)
	}
	rec = st.second(pending, fmt.Sprintf(`{"code":%q}`, st.code(secret, 0)))
	sess := cookieNamed(rec, "__Host-robinmail")
	if rec.Code != 200 || sess == nil || sess.MaxAge != int(RememberMax/time.Second) {
		t.Fatalf("the code after the provider: %d %s", rec.Code, rec.Body.String())
	}
	var who struct{ Method string }
	json.Unmarshal(st.do("GET", tlsBase+"/auth/session", "", cookie(sess)).Body.Bytes(), &who)
	if who.Method != "oidc:test+totp" {
		t.Errorf("method %q", who.Method)
	}
	// An account without TOTP is not asked for what it does not have.
	p.RequireTOTP = true
	st.accounts.DisableTOTP(context.Background(), "acct-anna")
	if rec := st.through(""); cookieNamed(rec, "__Host-robinmail") == nil {
		t.Errorf("require_totp for an account without TOTP: goes on to %s", lands(rec))
	}
}

// When the provider is away the login page says so, and failed callbacks
// are counted against the address they come from.
func TestOIDCProviderAwayAndLimits(t *testing.T) {
	st := newSite(t)
	op, _ := st.withProvider()
	flow, callback := st.begin("")
	// Callbacks that fail at the provider (the code was used) count.
	st.do("GET", callback, "", cookie(flow), crossSite)
	for i := 0; i < maxFailsPerIP; i++ {
		st.do("GET", callback, "", cookie(flow), crossSite, from("198.51.100.9:1"))
	}
	requests := op.TokenRequests
	rec := st.do("GET", callback, "", cookie(flow), crossSite, from("198.51.100.9:1"))
	if lands(rec) != "/login?error=too_many" || op.TokenRequests != requests {
		t.Errorf("after %d failed callbacks: goes on to %s, %d more token requests", maxFailsPerIP, lands(rec), op.TokenRequests-requests)
	}

	st = newSite(t)
	op, _ = st.withProvider()
	op.Close()
	rec = st.do("GET", tlsBase+"/auth/oidc/test/start", "", header("Sec-Fetch-Site", "same-origin"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?error=oidc_unavailable" || cookieNamed(rec, "__Host-robinmail-oidc") != nil {
		t.Errorf("start with the provider away: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}
