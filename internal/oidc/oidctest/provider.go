// Package oidctest is an OpenID Connect provider for tests: discovery, an
// authorization endpoint that logs a fixed user in without asking, a token
// endpoint that checks the client, the redirect URI and PKCE, and a key
// set. It can be told to misbehave in the ways a relying party has to
// withstand: tokens with a wrong algorithm, issuer, audience, nonce or
// time, signed by another key or by nobody.
//
// It shares no code with internal/oidc or internal/jose: tokens are signed
// here with the standard library, and the PKCE check is written out again.
package oidctest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider is a running fake provider. Change the exported fields under
// Lock, or before the first request.
type Provider struct {
	sync.Mutex
	Server *httptest.Server
	// Issuer is the issuer identifier: the server's URL.
	Issuer string

	ClientID     string
	ClientSecret string // empty: a public client
	// RedirectURIs the client registered; empty accepts any.
	RedirectURIs []string
	// User holds the claims of whoever "logs in": sub, email and so on.
	User map[string]any
	// Deny makes the authorization endpoint answer with this error code
	// instead of a code, as when the user cancels: "access_denied".
	Deny string
	// Alg signs ID tokens: "RS256" (the default), "ES256", "none" (no
	// signature), or "HS256-public-key" (an HMAC with the RSA public key,
	// in DER, as the secret).
	Alg string
	// Header and Claims change an ID token before it is signed.
	Header func(h map[string]any)
	Claims func(c map[string]any)
	// SignWithOtherKey signs with a key that is not in the key set, under
	// the id of the one that is.
	SignWithOtherKey bool
	// Metadata changes the discovery document.
	Metadata func(m map[string]any)
	// PostOnly makes the token endpoint refuse HTTP Basic and say so in
	// the discovery document.
	PostOnly bool
	// Now is the provider's clock.
	Now func() time.Time

	// Counters of requests, for tests of caching.
	Discoveries, KeyFetches, TokenRequests int
	// LastAuth is the query of the last authorization request.
	LastAuth url.Values

	kid    int
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
	other  *rsa.PrivateKey
	codes  map[string]grant
}

type grant struct {
	challenge, nonce, redirect string
	user                       map[string]any
}

// New starts a provider with one client and one user, on a free port.
func New() *Provider { return NewAt("") }

// NewAt is New on a given address, such as "127.0.0.1:8090", for a
// provider whose issuer has to be known beforehand. It panics when the
// address cannot be listened on.
func NewAt(addr string) *Provider {
	p := &Provider{ClientID: "robinmail", ClientSecret: "test-secret-of-the-fake-provider", Now: time.Now, codes: map[string]grant{},
		User: map[string]any{"sub": "user-1", "email": "anna@example.test", "email_verified": true}}
	p.newKeys()
	p.other, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /jwks", p.jwks)
	p.Server = httptest.NewUnstartedServer(mux)
	if addr != "" {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			panic(err)
		}
		p.Server.Listener.Close()
		p.Server.Listener = l
	}
	p.Server.Start()
	p.Issuer = p.Server.URL
	return p
}

// Close stops the server.
func (p *Provider) Close() { p.Server.Close() }

func (p *Provider) newKeys() {
	p.kid++
	p.rsaKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	p.ecKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// Rotate replaces the provider's keys: the old ones leave the key set.
func (p *Provider) Rotate() {
	p.Lock()
	defer p.Unlock()
	p.newKeys()
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	p.Discoveries++
	methods := []string{"client_secret_basic", "client_secret_post"}
	if p.PostOnly {
		methods = []string{"client_secret_post"}
	}
	m := map[string]any{
		"issuer":                                p.Issuer,
		"authorization_endpoint":                p.Issuer + "/authorize",
		"token_endpoint":                        p.Issuer + "/token",
		"jwks_uri":                              p.Issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": methods,
	}
	if p.Metadata != nil {
		p.Metadata(m)
	}
	writeJSON(w, 200, m)
}

func (p *Provider) keyID(alg string) string { return fmt.Sprintf("%s-%d", strings.ToLower(alg), p.kid) }

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	p.KeyFetches++
	pub := p.rsaKey.PublicKey
	e := []byte{byte(pub.E >> 16), byte(pub.E >> 8), byte(pub.E)}
	ec := p.ecKey.PublicKey
	writeJSON(w, 200, map[string]any{"keys": []map[string]any{
		{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": p.keyID("RS256"), "n": b64(pub.N.Bytes()), "e": b64(e)},
		{"kty": "EC", "use": "sig", "crv": "P-256", "kid": p.keyID("ES256"),
			"x": b64(ec.X.FillBytes(make([]byte, 32))), "y": b64(ec.Y.FillBytes(make([]byte, 32)))},
		// What a real provider also publishes: a key for encryption.
		{"kty": "RSA", "use": "enc", "kid": "enc-1", "n": b64(p.other.N.Bytes()), "e": "AQAB"},
	}})
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	q := r.URL.Query()
	p.LastAuth = q
	redirect := q.Get("redirect_uri")
	known := len(p.RedirectURIs) == 0
	for _, u := range p.RedirectURIs {
		known = known || u == redirect
	}
	// RFC 6749 4.1.2.1: with a bad client or redirect URI the user is
	// told, not redirected.
	if q.Get("client_id") != p.ClientID || !known || redirect == "" {
		http.Error(w, "unknown client or redirect URI", http.StatusBadRequest)
		return
	}
	back, _ := url.Parse(redirect)
	out := back.Query()
	out.Set("state", q.Get("state"))
	fail := func(code string) {
		out.Set("error", code)
		back.RawQuery = out.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	}
	switch {
	case p.Deny != "":
		fail(p.Deny)
		return
	case q.Get("response_type") != "code":
		fail("unsupported_response_type")
		return
	case !strings.Contains(" "+q.Get("scope")+" ", " openid "):
		fail("invalid_scope")
		return
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		fail("invalid_request")
		return
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	code := hex.EncodeToString(raw)
	user := map[string]any{}
	for k, v := range p.User {
		user[k] = v
	}
	p.codes[code] = grant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: redirect, user: user}
	out.Set("code", code)
	back.RawQuery = out.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	p.TokenRequests++
	r.ParseForm()
	fail := func(status int, code string) { writeJSON(w, status, map[string]string{"error": code}) }
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if u, pw, ok := r.BasicAuth(); ok {
		if p.PostOnly {
			fail(401, "invalid_client")
			return
		}
		id, _ = url.QueryUnescape(u)
		secret, _ = url.QueryUnescape(pw)
	}
	if id != p.ClientID || secret != p.ClientSecret {
		fail(401, "invalid_client")
		return
	}
	g, ok := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code")) // a code works once
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirect ||
		b64(sum[:]) != g.challenge {
		fail(400, "invalid_grant")
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": "not-used", "token_type": "Bearer", "expires_in": 300,
		"id_token": p.idToken(g)})
}

// idToken makes and signs the ID token of a grant. Callers hold the lock.
func (p *Provider) idToken(g grant) string {
	alg := p.Alg
	if alg == "" {
		alg = "RS256"
	}
	now := p.Now()
	header := map[string]any{"alg": alg, "typ": "JWT", "kid": p.keyID(alg)}
	claims := map[string]any{"iss": p.Issuer, "aud": p.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": g.nonce}
	for k, v := range g.user {
		claims[k] = v
	}
	switch alg {
	case "none":
		delete(header, "kid")
	case "HS256-public-key":
		header["alg"], header["kid"] = "HS256", p.keyID("RS256")
	}
	if p.Header != nil {
		p.Header(header)
	}
	if p.Claims != nil {
		p.Claims(claims)
	}
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signed := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signed))
	var sig []byte
	switch alg {
	case "RS256":
		key := p.rsaKey
		if p.SignWithOtherKey {
			key = p.other
		}
		sig, _ = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	case "ES256":
		key := p.ecKey
		if p.SignWithOtherKey {
			key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		}
		r, s, _ := ecdsa.Sign(rand.Reader, key, sum[:])
		sig = append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	case "HS256-public-key":
		der, _ := x509.MarshalPKIXPublicKey(&p.rsaKey.PublicKey)
		mac := hmac.New(sha256.New, der)
		mac.Write([]byte(signed))
		sig = mac.Sum(nil)
	}
	return signed + "." + b64(sig)
}
