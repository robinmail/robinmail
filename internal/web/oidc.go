package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/oidc"
	"github.com/mikepage/robinmail/internal/store"
)

// Login through an OpenID Connect provider, for the webmail only: a mail
// client cannot follow a browser redirect and keeps needing an app
// password.
//
//	GET /auth/oidc/<name>/start      sends the browser to the provider
//	GET /auth/oidc/<name>/callback   where the provider sends it back
//
// Between the two the browser holds a cookie with the flow's state, nonce
// and PKCE verifier (internal/oidc). It is the one cookie here that is
// SameSite=Lax: the way back is a navigation that another site started,
// and a Strict cookie would stay home. It lives ten minutes, is HttpOnly
// and "__Host-" like the others, and holds nothing that is worth anything
// without a code from the provider for exactly this flow. The server
// keeps no state for a flow, so any node can take the callback.
//
// The callback is the only endpoint under /auth that another site may
// start a request to, and it is a GET. What it does is bound to the
// browser by that cookie: the "state" in the URL must be the cookie's, so
// a link made by somebody else (who cannot set a "__Host-" cookie here)
// does nothing, and the code is useless without the verifier, so a code
// that leaks does nothing either.
//
// The callback answers with a small page that goes on to the webmail by
// itself, not with a redirect. A redirect would still count as part of
// the navigation the provider's site started, and a browser would then
// not send the new session cookie, which is SameSite=Strict; a
// navigation from our own page does.
//
// Who the user is comes from one claim of the ID token, by default
// "email", and for that claim only if "email_verified" is true. It names
// an existing account. An account is made on the spot only for the
// domains the provider's configuration lists, and only if robinmail
// accepts mail for the domain. The first login through a provider records
// the provider's subject for the account; a later login that maps to the
// same account under another subject is refused, because then the address
// went to somebody else at the provider.

// OIDCProvider is one configured OpenID Connect provider.
type OIDCProvider struct {
	// Name is the provider's name in URLs and the config: lowercase
	// letters, digits and "-".
	Name string
	// Label is what the login button says after "Log in with".
	Label string
	// Client talks to the provider. *oidc.Provider is one.
	Client OIDCClient
	// Claim holds the address of the account ("email" when empty).
	Claim string
	// AllowUnverified takes the "email" claim without "email_verified".
	AllowUnverified bool
	// RequireTOTP asks an account that has TOTP for its code after a
	// login through this provider too.
	RequireTOTP bool
	// AutoProvisionDomains are the domains for which a login creates the
	// account when there is none.
	AutoProvisionDomains []string
}

// OIDCClient is the relying party side of one provider.
type OIDCClient interface {
	Begin(ctx context.Context, redirectURI string) (string, oidc.Flow, error)
	Exchange(ctx context.Context, redirectURI, code string, f oidc.Flow) (*oidc.Claims, error)
}

// OIDCAccounts is what an OIDC login needs from the store on top of
// Accounts. *store.Store has it.
type OIDCAccounts interface {
	CreateAccount(ctx context.Context, address, passwordHash string) (store.Account, error)
	LinkOIDC(ctx context.Context, account, provider, subject string) error
}

// flowTTL is how long a login may take at the provider.
const flowTTL = 10 * time.Minute

func flowCookie(r *http.Request) string { return sessionCookie(r) + "-oidc" }

// flowState is what the flow cookie holds.
type flowState struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Remember bool   `json:"r,omitempty"`
}

func (s *Server) setFlowCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     flowCookie(r),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   r.TLS != nil,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, // see the top of this file
	})
}

func (s *Server) provider(name string) *OIDCProvider {
	for _, p := range s.OIDC {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func (s *Server) redirectURI(r *http.Request, p *OIDCProvider) string {
	// The host of the request was checked against our name (hostAllowed).
	return ownOrigin(r) + "/auth/oidc/" + p.Name + "/callback"
}

// oidcStart is GET /auth/oidc/<name>/start: the "Log in with" button.
func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := s.provider(r.PathValue("name"))
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if !s.originAllowed(r) {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	authURL, flow, err := p.Client.Begin(r.Context(), s.redirectURI(r, p))
	if err != nil {
		s.Logger.Error("oidc: starting a login failed", "provider", p.Name, "err", err)
		http.Redirect(w, r, "/login?error=oidc_unavailable", http.StatusSeeOther)
		return
	}
	state, _ := json.Marshal(flowState{Provider: p.Name, State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier,
		Remember: r.URL.Query().Get("remember") == "1"})
	s.setFlowCookie(w, r, base64.RawURLEncoding.EncodeToString(state), int(flowTTL/time.Second))
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// goOn answers with a page that takes the browser to a path of the
// webmail (why not a redirect: the top of this file).
func goOn(w http.ResponseWriter, path string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	p := html.EscapeString(path)
	w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=` + p +
		`"><title>robinmail</title></head><body><p><a href="` + p + `">Continue</a></p></body></html>` + "\n"))
}

// oidcCallback is GET /auth/oidc/<name>/callback.
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	p := s.provider(r.PathValue("name"))
	if p == nil {
		http.NotFound(w, r)
		return
	}
	ip := clientIP(r)
	ctx := r.Context()
	// refuse ends the login with a word for the login page. What went
	// wrong in detail is for the log and the auth events, not for whoever
	// stands at the door.
	refuse := func(code, why string, acct store.Account, addr string) {
		s.Logger.Warn("oidc: login refused", "provider", p.Name, "ip", ip, "why", why)
		s.event(ctx, EventOIDCRefused, acct, addr, ip, p.Name+": "+why)
		goOn(w, "/login?error="+url.QueryEscape(code))
	}

	// The flow cookie is used once.
	var flow flowState
	c, err := r.Cookie(flowCookie(r))
	s.setFlowCookie(w, r, "", -1)
	if err == nil && len(c.Value) < 2048 {
		if raw, err := base64.RawURLEncoding.DecodeString(c.Value); err == nil {
			json.Unmarshal(raw, &flow)
		}
	}
	of := oidc.Flow{State: flow.State, Nonce: flow.Nonce, Verifier: flow.Verifier}
	q := r.URL.Query()
	if flow.Provider != p.Name || !of.SameState(q.Get("state")) {
		refuse("oidc_failed", "no login was started in this browser for the state that came back", store.Account{}, "")
		return
	}
	if e := q.Get("error"); e != "" {
		// RFC 6749 4.1.2.1: the user said no, or the provider did.
		code := "oidc_failed"
		if e == "access_denied" {
			code = "oidc_denied"
		}
		refuse(code, "the provider answered "+clipAgent(e), store.Account{}, "")
		return
	}
	byIP := s.limitIP(ip)
	if _, _, blocked := s.blocked(ctx, byIP); blocked {
		goOn(w, "/login?error=too_many")
		return
	}
	claims, err := p.Client.Exchange(ctx, s.redirectURI(r, p), q.Get("code"), of)
	if err != nil {
		// A callback costs a request to the provider: failures count
		// against the address like failed logins.
		s.failed(ctx, byIP)
		refuse("oidc_failed", err.Error(), store.Account{}, "")
		return
	}

	// Who is it?
	claim := p.Claim
	if claim == "" {
		claim = "email"
	}
	value, ok := claims.String(claim)
	addr, perr := address.Parse(value)
	if !ok || perr != nil {
		refuse("oidc_no_account", "the token has no usable "+claim+" claim", store.Account{}, clipAgent(value))
		return
	}
	if claim == "email" && !p.AllowUnverified && !claims.True("email_verified") {
		refuse("oidc_unverified", "the provider has not verified the address", store.Account{}, addr.String())
		return
	}
	acct, found, status := s.lookup(ctx, addr, true)
	if status != 0 {
		goOn(w, "/login?error=oidc_unavailable")
		return
	}
	accounts, _ := s.Accounts.(OIDCAccounts)
	if accounts == nil {
		s.Logger.Error("oidc: the store cannot link accounts")
		goOn(w, "/login?error=oidc_unavailable")
		return
	}
	if !found {
		provision := false
		for _, d := range p.AutoProvisionDomains {
			provision = provision || strings.EqualFold(d, addr.Domain)
		}
		if !provision {
			refuse("oidc_no_account", "no account for the address", store.Account{}, addr.String())
			return
		}
		acct, err = accounts.CreateAccount(ctx, addr.String(), store.NoPassword)
		switch {
		case errors.Is(err, store.ErrNoDomain):
			refuse("oidc_no_account", "the address is in a domain this server does not accept mail for", store.Account{}, addr.String())
			return
		case errors.Is(err, store.ErrAccountExists):
			// Two first logins at once: the other one made it.
			if acct, found, status = s.lookup(ctx, addr, true); !found || status != 0 {
				goOn(w, "/login?error=oidc_unavailable")
				return
			}
		case err != nil:
			s.Logger.Error("oidc: creating the account failed", "err", err)
			goOn(w, "/login?error=oidc_unavailable")
			return
		default:
			s.event(ctx, EventAccountCreated, acct, acct.Address, ip, "by a first login through "+p.Name)
		}
	}
	switch err := accounts.LinkOIDC(ctx, acct.ID, p.Name, claims.Subject); {
	case errors.Is(err, store.ErrOIDCSubject):
		refuse("oidc_no_account", "the account is linked to another subject at the provider; \"robinmail user oidc-unlink\" forgets it", acct, acct.Address)
		return
	case err != nil:
		s.Logger.Error("oidc: linking the account failed", "err", err)
		goOn(w, "/login?error=oidc_unavailable")
		return
	}

	// The provider's login stands for the password, and unless configured
	// otherwise for the second factor too: that is the provider's to ask.
	s.finishLogin(w, r, acct, "oidc:"+p.Name, flow.Remember, acct.TOTP && p.RequireTOTP,
		func() { goOn(w, "/") },
		func() { goOn(w, "/login?step=totp") })
}
