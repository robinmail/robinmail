package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/oidc"
	"github.com/robinmail/robinmail/internal/oidc/oidctest"
)

var ctx = context.Background()

const redirect = "https://mail.example.test/auth/oidc/test/callback"

// RFC 7636 appendix B, "Example for the S256 code_challenge_method": the
// verifier dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk has the challenge
// E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM.
func TestRFC7636AppendixB(t *testing.T) {
	if got := oidc.Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("challenge %q", got)
	}
}

type harness struct {
	t   *testing.T
	op  *oidctest.Provider
	rp  *oidc.Provider
	now time.Time
}

func start(t *testing.T) *harness {
	h := &harness{t: t, op: oidctest.New(), now: time.Now()}
	t.Cleanup(h.op.Close)
	h.op.Now = func() time.Time { return h.now }
	h.rp = h.client()
	return h
}

// client is a relying party for the provider as it is configured now.
func (h *harness) client() *oidc.Provider {
	return oidc.New(oidc.Config{Issuer: h.op.Issuer, ClientID: h.op.ClientID, ClientSecret: h.op.ClientSecret,
		Scopes: []string{"openid", "email"}, Now: func() time.Time { return h.now }})
}

// code walks the browser's part: to the authorization endpoint and back,
// and returns what the callback would get.
func (h *harness) code(authURL string) url.Values {
	h.t.Helper()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(authURL)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != 302 || !strings.HasPrefix(loc.String(), redirect) {
		h.t.Fatalf("the authorization endpoint answered %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return loc.Query()
}

// login runs a whole flow.
func (h *harness) login() (*oidc.Claims, error) {
	h.t.Helper()
	authURL, flow, err := h.rp.Begin(ctx, redirect)
	if err != nil {
		return nil, err
	}
	back := h.code(authURL)
	if !flow.SameState(back.Get("state")) {
		h.t.Fatalf("state came back as %q", back.Get("state"))
	}
	return h.rp.Exchange(ctx, redirect, back.Get("code"), flow)
}

// The authorization code flow of OpenID Connect Core 3.1 with PKCE, with
// both signature algorithms and both ways of client authentication.
func TestCodeFlow(t *testing.T) {
	h := start(t)
	authURL, flow, err := h.rp.Begin(ctx, redirect)
	if err != nil {
		t.Fatal(err)
	}
	// Core 3.1.2.1 and RFC 7636 4.3: what the authentication request carries.
	u, _ := url.Parse(authURL)
	q := u.Query()
	for k, want := range map[string]string{"response_type": "code", "client_id": "robinmail", "redirect_uri": redirect,
		"scope": "openid email", "state": flow.State, "nonce": flow.Nonce, "code_challenge_method": "S256",
		"code_challenge": oidc.Challenge(flow.Verifier)} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if strings.Contains(authURL, flow.Verifier) || strings.Contains(authURL, h.op.ClientSecret) {
		t.Error("the verifier or the client secret is in the URL the browser gets")
	}
	// RFC 7636 4.1: a verifier of 43 to 128 unreserved characters. State
	// and nonce are as long and each login has its own.
	for name, v := range map[string]string{"state": flow.State, "nonce": flow.Nonce, "verifier": flow.Verifier} {
		if len(v) < 43 || len(v) > 128 || strings.Trim(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~") != "" {
			t.Errorf("%s %q", name, v)
		}
	}
	_, flow2, _ := h.rp.Begin(ctx, redirect)
	if flow2.State == flow.State || flow2.Nonce == flow.Nonce || flow2.Verifier == flow.Verifier || flow.State == flow.Nonce {
		t.Error("values of a flow repeat")
	}
	if flow.SameState("") || flow.SameState(flow2.State) || (oidc.Flow{}).SameState("") {
		t.Error("SameState accepts a state that is not the flow's")
	}

	back := h.code(authURL)
	claims, err := h.rp.Exchange(ctx, redirect, back.Get("code"), flow)
	if err != nil {
		t.Fatal(err)
	}
	email, _ := claims.String("email")
	if claims.Subject != "user-1" || claims.Issuer != h.op.Issuer || email != "anna@example.test" || !claims.True("email_verified") {
		t.Errorf("claims: %+v", claims)
	}
	// A code works once (RFC 6749 4.1.2).
	if _, err := h.rp.Exchange(ctx, redirect, back.Get("code"), flow); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("the code a second time: %v", err)
	}

	for name, setup := range map[string]func(){
		"ES256":              func() { h.op.Alg = "ES256" },
		"client_secret_post": func() { h.op.PostOnly = true },
		"a public client":    func() { h.op.ClientSecret = "" },
		"a secret that needs escaping in Basic (RFC 6749 2.3.1)": func() { h.op.ClientSecret = "p@ss:word/with+signs & spaces" },
	} {
		h := start(t)
		setup()
		h.rp = h.client()
		if c, err := h.login(); err != nil || c.Subject != "user-1" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// RFC 7636: the code is bound to the verifier. Whoever has the code but
// not the verifier gets nothing for it.
func TestPKCEBindsTheCode(t *testing.T) {
	h := start(t)
	authURL, flow, _ := h.rp.Begin(ctx, redirect)
	back := h.code(authURL)
	stolen := flow
	stolen.Verifier = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM-not-the-verifier"
	if _, err := h.rp.Exchange(ctx, redirect, back.Get("code"), stolen); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("a code with another verifier: %v", err)
	}
	// The challenge itself is not the verifier either.
	authURL, flow, _ = h.rp.Begin(ctx, redirect)
	back = h.code(authURL)
	stolen = flow
	stolen.Verifier = oidc.Challenge(flow.Verifier)
	if _, err := h.rp.Exchange(ctx, redirect, back.Get("code"), stolen); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("a code with the challenge for a verifier: %v", err)
	}
	// And the redirect URI of the exchange is the one of the request.
	authURL, flow, _ = h.rp.Begin(ctx, redirect)
	back = h.code(authURL)
	if _, err := h.rp.Exchange(ctx, redirect+"/other", back.Get("code"), flow); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("a code with another redirect URI: %v", err)
	}
}

// OpenID Connect Core 3.1.3.7, "ID Token Validation": every way a token
// can be wrong, each on a token that is otherwise right and that the
// provider's token endpoint really answered with.
func TestIDTokenValidation(t *testing.T) {
	type op = oidctest.Provider
	cases := map[string]func(h *harness){
		// Step 6 and 7: the signature and its algorithm.
		`"alg": "none" (RFC 7515 A.5)`:                        func(h *harness) { h.op.Alg = "none" },
		"HS256 with the public key as the secret":             func(h *harness) { h.op.Alg = "HS256-public-key" },
		"signed with a key that is not the provider's, RS256": func(h *harness) { h.op.SignWithOtherKey = true },
		"signed with a key that is not the provider's, ES256": func(h *harness) { h.op.Alg, h.op.SignWithOtherKey = "ES256", true },
		"a kid the provider does not have":                    func(h *harness) { h.op.Header = func(m map[string]any) { m["kid"] = "unknown" } },
		"RS256 in the header over an ES256 signature":         func(h *harness) { h.op.Alg = "ES256"; h.op.Header = func(m map[string]any) { m["alg"] = "RS256" } },
		"RS256 in the header, the kid of the EC key":          func(h *harness) { h.op.Header = func(m map[string]any) { m["kid"] = "es256-1" } },
		"an algorithm that is not accepted (RS512)":           func(h *harness) { h.op.Header = func(m map[string]any) { m["alg"] = "RS512" } },
		"a crit header": func(h *harness) { h.op.Header = func(m map[string]any) { m["crit"] = []string{"exp"} } },
		// Step 2: the issuer.
		"another issuer":          func(h *harness) { h.op.Claims = func(c map[string]any) { c["iss"] = "https://other.example" } },
		"the issuer with a slash": func(h *harness) { h.op.Claims = func(c map[string]any) { c["iss"] = c["iss"].(string) + "/" } },
		"no issuer":               func(h *harness) { h.op.Claims = func(c map[string]any) { delete(c, "iss") } },
		// Step 3: the audience.
		"another audience":                       func(h *harness) { h.op.Claims = func(c map[string]any) { c["aud"] = "another-client" } },
		"no audience":                            func(h *harness) { h.op.Claims = func(c map[string]any) { delete(c, "aud") } },
		"an empty list of audiences":             func(h *harness) { h.op.Claims = func(c map[string]any) { c["aud"] = []string{} } },
		"an audience that only starts like ours": func(h *harness) { h.op.Claims = func(c map[string]any) { c["aud"] = "robinmail2" } },
		"an audience that is a number":           func(h *harness) { h.op.Claims = func(c map[string]any) { c["aud"] = 7 } },
		// Steps 3 to 5: several audiences and the authorized party.
		"several audiences without azp": func(h *harness) { h.op.Claims = func(c map[string]any) { c["aud"] = []string{"robinmail", "other"} } },
		"several audiences, azp another": func(h *harness) {
			h.op.Claims = func(c map[string]any) { c["aud"], c["azp"] = []string{"robinmail", "other"}, "other" }
		},
		"one audience, azp another": func(h *harness) { h.op.Claims = func(c map[string]any) { c["azp"] = "other" } },
		// Step 9: expiry.
		"expired": func(h *harness) {
			h.op.Claims = func(c map[string]any) { c["exp"] = h.now.Add(-2 * time.Minute).Unix() }
		},
		"no exp":          func(h *harness) { h.op.Claims = func(c map[string]any) { delete(c, "exp") } },
		"exp as a string": func(h *harness) { h.op.Claims = func(c map[string]any) { c["exp"] = "tomorrow" } },
		// Step 10: issued at.
		"issued in the future": func(h *harness) {
			h.op.Claims = func(c map[string]any) { c["iat"] = h.now.Add(5 * time.Minute).Unix() }
		},
		"issued an hour ago": func(h *harness) { h.op.Claims = func(c map[string]any) { c["iat"] = h.now.Add(-time.Hour).Unix() } },
		"no iat":             func(h *harness) { h.op.Claims = func(c map[string]any) { delete(c, "iat") } },
		"not valid before later": func(h *harness) {
			h.op.Claims = func(c map[string]any) { c["nbf"] = h.now.Add(5 * time.Minute).Unix() }
		},
		// Step 11: the nonce.
		"another nonce":  func(h *harness) { h.op.Claims = func(c map[string]any) { c["nonce"] = "the-nonce-of-another-login" } },
		"no nonce":       func(h *harness) { h.op.Claims = func(c map[string]any) { delete(c, "nonce") } },
		"an empty nonce": func(h *harness) { h.op.Claims = func(c map[string]any) { c["nonce"] = "" } },
		// Core 2: the subject.
		"no subject":     func(h *harness) { h.op.Claims = func(c map[string]any) { delete(c, "sub") } },
		"a long subject": func(h *harness) { h.op.Claims = func(c map[string]any) { c["sub"] = strings.Repeat("s", 256) } },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := start(t)
			breakIt(h)
			claims, err := h.login()
			if !errors.Is(err, oidc.ErrToken) || claims != nil {
				t.Errorf("claims %+v, err %v; want the token refused", claims, err)
			}
		})
	}
	_ = op{}

	// What is right, at the edges: a clock off by less than the leeway,
	// the audience as a list, an azp that names us, a fractional date.
	for name, ok := range map[string]func(h *harness){
		"expired half a minute ago by a clock that is off": func(h *harness) {
			h.op.Claims = func(c map[string]any) { c["exp"] = h.now.Add(-30 * time.Second).Unix() }
		},
		"issued half a minute from now": func(h *harness) {
			h.op.Claims = func(c map[string]any) { c["iat"] = h.now.Add(30 * time.Second).Unix() }
		},
		"the audience as a list of one": func(h *harness) { h.op.Claims = func(c map[string]any) { c["aud"] = []string{"robinmail"} } },
		"two audiences and azp ours": func(h *harness) {
			h.op.Claims = func(c map[string]any) { c["aud"], c["azp"] = []string{"other", "robinmail"}, "robinmail" }
		},
		"dates with fractions (RFC 7519 2)": func(h *harness) {
			h.op.Claims = func(c map[string]any) {
				c["iat"], c["exp"] = float64(h.now.Unix())+0.5, float64(h.now.Add(time.Minute).Unix())+0.25
			}
		},
	} {
		h := start(t)
		ok(h)
		if _, err := h.login(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// OpenID Connect Discovery 4.3: the issuer in the document must be the
// one asked for. And the endpoints of an https issuer are https.
func TestDiscovery(t *testing.T) {
	for name, tc := range map[string]func(m map[string]any){
		"another issuer":                           func(m map[string]any) { m["issuer"] = "https://other.example" },
		"the issuer with a slash":                  func(m map[string]any) { m["issuer"] = m["issuer"].(string) + "/" },
		"no token endpoint":                        func(m map[string]any) { delete(m, "token_endpoint") },
		"no key set":                               func(m map[string]any) { delete(m, "jwks_uri") },
		"an authorization endpoint that is no URL": func(m map[string]any) { m["authorization_endpoint"] = "javascript:alert(1)" },
		"only HS256 for ID tokens":                 func(m map[string]any) { m["id_token_signing_alg_values_supported"] = []string{"HS256"} },
		"PKCE with plain only":                     func(m map[string]any) { m["code_challenge_methods_supported"] = []string{"plain"} },
	} {
		h := start(t)
		h.op.Metadata = tc
		if _, _, err := h.rp.Begin(ctx, redirect); !errors.Is(err, oidc.ErrDiscovery) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A provider that says nothing about PKCE or algorithms is taken at
	// its word when the token comes.
	h := start(t)
	h.op.Metadata = func(m map[string]any) {
		delete(m, "code_challenge_methods_supported")
		delete(m, "id_token_signing_alg_values_supported")
		delete(m, "token_endpoint_auth_methods_supported")
	}
	if _, err := h.login(); err != nil {
		t.Errorf("a short discovery document: %v", err)
	}

	// An https issuer whose document points to http: refused, so that the
	// code and the client secret never travel in the clear.
	var issuer string
	mux := http.NewServeMux()
	plain := "https"
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"issuer":"` + issuer + `","authorization_endpoint":"` + issuer + `/authorize","jwks_uri":"` + issuer +
			`/jwks","token_endpoint":"` + strings.Replace(issuer, "https", plain, 1) + `/token"}`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	issuer = srv.URL
	rp := oidc.New(oidc.Config{Issuer: issuer, ClientID: "x", Scopes: []string{"openid"}, HTTP: srv.Client()})
	if _, _, err := rp.Begin(ctx, redirect); err != nil {
		t.Errorf("an https provider: %v", err)
	}
	plain = "http"
	rp = oidc.New(oidc.Config{Issuer: issuer, ClientID: "x", Scopes: []string{"openid"}, HTTP: srv.Client()})
	if _, _, err := rp.Begin(ctx, redirect); !errors.Is(err, oidc.ErrDiscovery) {
		t.Errorf("an https issuer with an http token endpoint: %v", err)
	}

	// A discovery endpoint that redirects is not followed.
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, h.op.Issuer+r.URL.Path, http.StatusFound)
	}))
	defer away.Close()
	rp = oidc.New(oidc.Config{Issuer: away.URL, ClientID: "x", Scopes: []string{"openid"}})
	if _, _, err := rp.Begin(ctx, redirect); !errors.Is(err, oidc.ErrDiscovery) {
		t.Errorf("a redirecting discovery endpoint: %v", err)
	}
}

// The discovery document and the key set are fetched once and kept for an
// hour. A key change at the provider is followed: a token with a key id
// that is not in the set makes the set be fetched again, at most once a
// minute, and keys that left the set are trusted no longer.
func TestKeyCachingAndRotation(t *testing.T) {
	h := start(t)
	for i := 0; i < 5; i++ {
		if _, err := h.login(); err != nil {
			t.Fatal(err)
		}
	}
	if h.op.Discoveries != 1 || h.op.KeyFetches != 1 || h.op.TokenRequests != 5 {
		t.Errorf("five logins: %d discoveries, %d key fetches, %d token requests", h.op.Discoveries, h.op.KeyFetches, h.op.TokenRequests)
	}

	// The provider changes its keys. The next token names a key the
	// relying party does not have; more than a minute after its last
	// fetch, it fetches again and the login works.
	h.op.Rotate()
	h.now = h.now.Add(2 * time.Minute)
	if _, err := h.login(); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if h.op.KeyFetches != 2 {
		t.Errorf("%d key fetches after one rotation", h.op.KeyFetches)
	}

	// A flood of tokens with unknown key ids does not make a flood of
	// fetches: within the minute the set in hand is the answer.
	h.op.Header = func(m map[string]any) { m["kid"] = "made-up" }
	for i := 0; i < 5; i++ {
		if _, err := h.login(); !errors.Is(err, oidc.ErrToken) {
			t.Fatalf("a made-up kid: %v", err)
		}
	}
	if h.op.KeyFetches != 2 {
		t.Errorf("%d key fetches for five tokens with an unknown kid within a minute", h.op.KeyFetches)
	}
	h.now = h.now.Add(2 * time.Minute)
	h.login()
	if h.op.KeyFetches != 3 {
		t.Errorf("%d key fetches two minutes later", h.op.KeyFetches)
	}
	h.op.Header = nil

	// A second rotation right after: the token of the new key waits out
	// the minute, then works.
	h.op.Rotate()
	if _, err := h.login(); !errors.Is(err, oidc.ErrToken) {
		t.Errorf("a new key within the minute after a fetch: %v", err)
	}
	h.now = h.now.Add(2 * time.Minute)
	if _, err := h.login(); err != nil {
		t.Errorf("the new key after the minute: %v", err)
	}

	// After an hour both are fetched again.
	d, k := h.op.Discoveries, h.op.KeyFetches
	h.now = h.now.Add(61 * time.Minute)
	if _, err := h.login(); err != nil {
		t.Fatal(err)
	}
	if h.op.Discoveries != d+1 || h.op.KeyFetches != k+1 {
		t.Errorf("after an hour: %d discoveries (+%d), %d key fetches (+%d)", h.op.Discoveries, h.op.Discoveries-d, h.op.KeyFetches, h.op.KeyFetches-k)
	}
}

// What the user and the provider can do wrong at the endpoints.
func TestExchangeFailures(t *testing.T) {
	h := start(t)
	_, flow, _ := h.rp.Begin(ctx, redirect)
	if _, err := h.rp.Exchange(ctx, redirect, "a-code-nobody-issued", flow); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("an invented code: %v", err)
	}
	// A wrong client secret: the error names the provider's code, and
	// never the secret.
	wrong := oidc.New(oidc.Config{Issuer: h.op.Issuer, ClientID: h.op.ClientID, ClientSecret: "not-the-secret-9f3a", Scopes: []string{"openid"}})
	authURL, flow, _ := wrong.Begin(ctx, redirect)
	_, err := wrong.Exchange(ctx, redirect, h.code(authURL).Get("code"), flow)
	if !errors.Is(err, oidc.ErrExchange) || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "9f3a") {
		t.Errorf("a wrong client secret: %v", err)
	}
	// The provider is gone.
	gone := start(t)
	authURL, flow, _ = gone.rp.Begin(ctx, redirect)
	back := gone.code(authURL)
	gone.op.Close()
	if _, err := gone.rp.Exchange(ctx, redirect, back.Get("code"), flow); !errors.Is(err, oidc.ErrExchange) {
		t.Errorf("a provider that went away: %v", err)
	}
	fresh := oidc.New(oidc.Config{Issuer: gone.op.Issuer, ClientID: "x", Scopes: []string{"openid"}})
	if _, _, err := fresh.Begin(ctx, redirect); !errors.Is(err, oidc.ErrDiscovery) {
		t.Errorf("discovery of a provider that is away: %v", err)
	}
}
