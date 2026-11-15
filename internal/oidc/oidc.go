// Package oidc is an OpenID Connect relying party: it sends a browser to
// a provider and, when it comes back, learns who logged in there.
//
// The flow is the authorization code flow (OpenID Connect Core 1.0, 3.1)
// with PKCE (RFC 7636), and nothing else:
//
//  1. Begin makes three random values and the URL of the provider's
//     authorization endpoint: "state" ties the answer to this browser,
//     "nonce" ties the ID token to this request, and the PKCE verifier,
//     of which the provider only gets a hash, ties the code to whoever
//     started the flow.
//  2. The provider sends the browser back with a code.
//  3. Exchange trades the code, with the verifier, for an ID token at the
//     provider's token endpoint, and validates that token: signature by
//     the provider's keys (internal/jose), issuer, audience, authorized
//     party, times and nonce (Core 3.1.3.7).
//
// The provider is found by discovery (OpenID Connect Discovery 1.0): a
// document under the issuer's URL names the endpoints and the key set.
// Both are fetched when first needed and kept for an hour; the key set is
// fetched again at once when a token names a key that is not in it, which
// is how a provider's key rotation is followed, but not more often than
// once a minute.
//
// The package knows nothing of accounts or cookies: internal/web keeps the
// three values of a flow in a cookie and maps the claims to an account.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/robinmail/robinmail/internal/jose"
)

// Config describes one provider.
type Config struct {
	// Issuer is the provider's issuer identifier, exactly as its discovery
	// document states it.
	Issuer   string
	ClientID string
	// ClientSecret is empty for a public client.
	ClientSecret string
	// Scopes to ask for; "openid" must be among them.
	Scopes []string
	// HTTP is the client for the provider's endpoints (a default with
	// timeouts when nil). It must not follow redirects on its own.
	HTTP *http.Client
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// Provider is a configured provider with what was learned about it.
type Provider struct {
	cfg Config

	mu         sync.Mutex
	meta       *metadata
	metaAt     time.Time
	keys       jose.Set
	keysAt     time.Time
	keysTried  time.Time // the last fetch, also a failed one
	haveKeys   bool
	metaFailed time.Time
}

const (
	// cacheFor is how long the discovery document and the key set are
	// used before they are fetched again.
	cacheFor = time.Hour
	// refetchAfter is the least time between two fetches of the key set
	// that a token with an unknown key id causes.
	refetchAfter = time.Minute
	// retryDiscovery is how long a failed discovery is not tried again.
	retryDiscovery = 10 * time.Second
	// Leeway is how far the provider's clock may be off from ours.
	Leeway = time.Minute
	// maxAge is how old an ID token may be when it arrives: it comes
	// straight from the token endpoint, seconds after it was made.
	maxAge = 10 * time.Minute
	// maxBody bounds a discovery document, key set or token response.
	maxBody = 1 << 20
)

// New returns a provider. Nothing is fetched until it is used.
func New(cfg Config) *Provider {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	// A redirect from an endpoint is not followed: the document that names
	// the endpoints is the only authority on where they are.
	hc := *cfg.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &hc
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Provider{cfg: cfg}
}

// Errors of Exchange. ErrToken wraps every reason an ID token is refused.
var (
	ErrDiscovery = errors.New("oidc: discovery failed")
	ErrExchange  = errors.New("oidc: the code was not exchanged")
	ErrToken     = errors.New("oidc: ID token refused")
)

// metadata is the part of the discovery document that is used (Discovery
// 3: "OpenID Provider Metadata").
type metadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	SigningAlgs           []string `json:"id_token_signing_alg_values_supported"`
	ChallengeMethods      []string `json:"code_challenge_methods_supported"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

func (p *Provider) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("the answer of %s is too large", rawURL)
	}
	return data, nil
}

// endpointOK checks a URL from the discovery document: https, or http when
// the issuer itself is http, which the configuration only allows on
// loopback, for development.
func (p *Provider) endpointOK(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && strings.HasPrefix(p.cfg.Issuer, "http://"))
}

// discover returns the provider's metadata, fetching it when there is none
// or it is old. An old one keeps being used while a new one cannot be had.
func (p *Provider) discover(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	if p.meta != nil && now.Sub(p.metaAt) < cacheFor {
		return p.meta, nil
	}
	if !p.metaFailed.IsZero() && now.Sub(p.metaFailed) < retryDiscovery {
		if p.meta != nil {
			return p.meta, nil
		}
		return nil, fmt.Errorf("%w: it failed a moment ago", ErrDiscovery)
	}
	m, err := p.fetchMetadata(ctx)
	if err != nil {
		p.metaFailed = now
		if p.meta != nil {
			return p.meta, nil
		}
		return nil, fmt.Errorf("%w: %v", ErrDiscovery, err)
	}
	p.meta, p.metaAt, p.metaFailed = m, now, time.Time{}
	return m, nil
}

func (p *Provider) fetchMetadata(ctx context.Context) (*metadata, error) {
	// Discovery 4.1: the document is at the issuer plus this path, "after
	// removing any terminating /" from the issuer.
	data, err := p.get(ctx, strings.TrimSuffix(p.cfg.Issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		return nil, err
	}
	var m metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("the discovery document is not JSON: %v", err)
	}
	// Discovery 4.3: "The issuer value returned MUST be identical to the
	// Issuer URL that was used as the prefix": otherwise any server could
	// present another's document and be taken for it.
	if m.Issuer != p.cfg.Issuer {
		return nil, fmt.Errorf("the document is for issuer %q, not %q", m.Issuer, p.cfg.Issuer)
	}
	for name, u := range map[string]string{"authorization_endpoint": m.AuthorizationEndpoint, "token_endpoint": m.TokenEndpoint, "jwks_uri": m.JWKSURI} {
		if !p.endpointOK(u) {
			return nil, fmt.Errorf("%s %q is missing or not https", name, u)
		}
	}
	if len(m.SigningAlgs) > 0 && !contains(m.SigningAlgs, jose.RS256) && !contains(m.SigningAlgs, jose.ES256) {
		return nil, fmt.Errorf("the provider signs ID tokens with %v; RS256 or ES256 is needed", m.SigningAlgs)
	}
	// RFC 8414 2: when the member is there, it lists what is supported.
	if len(m.ChallengeMethods) > 0 && !contains(m.ChallengeMethods, "S256") {
		return nil, fmt.Errorf("the provider's PKCE methods are %v; S256 is needed", m.ChallengeMethods)
	}
	return &m, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// keySet returns the provider's keys. With a token given, the set is
// fetched again when it has no key for the token and the last fetch is
// more than a minute ago: the provider may have changed keys.
func (p *Provider) keySet(ctx context.Context, m *metadata, tok *jose.Token) (jose.Set, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	fresh := p.haveKeys && now.Sub(p.keysAt) < cacheFor
	if fresh && (tok == nil || p.keys.Has(tok) || now.Sub(p.keysTried) < refetchAfter) {
		return p.keys, nil
	}
	if !fresh && p.haveKeys && now.Sub(p.keysTried) < retryDiscovery {
		return p.keys, nil
	}
	p.keysTried = now
	data, err := p.get(ctx, m.JWKSURI)
	if err == nil {
		var set jose.Set
		if set, err = jose.ParseSet(data); err == nil {
			p.keys, p.keysAt, p.haveKeys = set, now, true
			return set, nil
		}
	}
	if p.haveKeys && now.Sub(p.keysAt) < 24*time.Hour {
		// The provider's key endpoint is away: the keys of the last day
		// are still its keys.
		return p.keys, nil
	}
	return jose.Set{}, fmt.Errorf("oidc: fetching the provider's keys: %w", err)
}

// Flow is what one login keeps between the redirect to the provider and
// the way back. All three are secrets of the browser that started it.
type Flow struct {
	State    string
	Nonce    string
	Verifier string
}

func random() (string, error) {
	// 32 octets, which base64url turns into 43 characters: the length
	// RFC 7636 4.1 recommends for a verifier, and 256 bits of entropy.
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Challenge is the S256 code challenge of a verifier (RFC 7636 4.2):
// BASE64URL-ENCODE(SHA256(ASCII(code_verifier))).
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Begin starts a login: it returns the URL to send the browser to (Core
// 3.1.2.1) and the values to keep for Exchange.
func (p *Provider) Begin(ctx context.Context, redirectURI string) (string, Flow, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", Flow{}, err
	}
	var f Flow
	for _, v := range []*string{&f.State, &f.Nonce, &f.Verifier} {
		if *v, err = random(); err != nil {
			return "", Flow{}, err
		}
	}
	u, err := url.Parse(m.AuthorizationEndpoint)
	if err != nil {
		return "", Flow{}, fmt.Errorf("%w: %v", ErrDiscovery, err)
	}
	// The endpoint may carry a query of its own, which stays (RFC 6749 3.1).
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("state", f.State)
	q.Set("nonce", f.Nonce)
	q.Set("code_challenge", Challenge(f.Verifier))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), f, nil
}

// SameState compares the state a callback brought with the flow's, in
// constant time.
func (f Flow) SameState(got string) bool {
	return f.State != "" && subtle.ConstantTimeCompare([]byte(f.State), []byte(got)) == 1
}

// Claims are the claims of a validated ID token.
type Claims struct {
	Issuer   string
	Subject  string
	Audience []string
	// All holds every claim, for the one the configuration maps to an
	// account.
	All map[string]json.RawMessage
}

// String returns a claim that is a string.
func (c *Claims) String(name string) (string, bool) {
	var s string
	if raw, ok := c.All[name]; !ok || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// True reports whether a claim is the JSON value true. "email_verified"
// is a boolean (Core 5.1); a provider that sends the string "true" has
// not said true.
func (c *Claims) True(name string) bool {
	var b bool
	return json.Unmarshal(c.All[name], &b) == nil && b && string(c.All[name]) == "true"
}

// Exchange trades the code a callback brought for an ID token (Core
// 3.1.3.1, RFC 7636 4.5) and returns its claims once the token is
// validated.
func (p *Provider) Exchange(ctx context.Context, redirectURI, code string, f Flow) (*Claims, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", f.Verifier)
	// How the client says who it is (Core 9): a public client by its id
	// in the body; one with a secret by HTTP Basic, which is the default
	// when the provider lists no methods, or in the body when the
	// provider only takes that.
	basic := false
	switch {
	case p.cfg.ClientSecret == "":
		form.Set("client_id", p.cfg.ClientID)
	case len(m.TokenAuthMethods) > 0 && !contains(m.TokenAuthMethods, "client_secret_basic") && contains(m.TokenAuthMethods, "client_secret_post"):
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	default:
		basic = true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExchange, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 2.3.1: id and secret are form-encoded first.
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		// The error of the HTTP client names the URL, not the body: the
		// secret and the code are not in it.
		return nil, fmt.Errorf("%w: %v", ErrExchange, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		return nil, fmt.Errorf("%w: reading the answer of the token endpoint failed", ErrExchange)
	}
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if resp.StatusCode != http.StatusOK {
		json.Unmarshal(data, &out)
		// RFC 6749 5.2: "error" is a code from a short list; only that is
		// passed on, cut, not what else the body may hold.
		return nil, fmt.Errorf("%w: HTTP %d from the token endpoint, error %q", ErrExchange, resp.StatusCode, clip(out.Error, 60))
	}
	if err := json.Unmarshal(data, &out); err != nil || out.IDToken == "" {
		return nil, fmt.Errorf("%w: the token endpoint's answer has no id_token", ErrExchange)
	}
	return p.validate(ctx, m, out.IDToken, f.Nonce)
}

func clip(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}

// audience is the "aud" claim: a string or an array of strings (RFC 7519
// 4.1.3).
type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var one string
	if json.Unmarshal(data, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// numericDate is seconds since the epoch, possibly with a fraction (RFC
// 7519 2).
type numericDate float64

func (n numericDate) time() time.Time { return time.Unix(int64(n), 0) }

// validate is OpenID Connect Core 3.1.3.7, "ID Token Validation", for a
// token from the token endpoint. The numbers are the steps of that list.
func (p *Provider) validate(ctx context.Context, m *metadata, idToken, nonce string) (*Claims, error) {
	refuse := func(format string, a ...any) (*Claims, error) {
		return nil, fmt.Errorf("%w: "+format, append([]any{ErrToken}, a...)...)
	}
	// 6 and 7. The signature, by a key of the provider, with RS256 or
	// ES256. Step 6 allows skipping this for a token that came over TLS
	// from the token endpoint; it is checked all the same, so that the
	// token stands on the provider's key and not on how it travelled.
	tok, err := jose.Parse(idToken)
	if err != nil {
		return refuse("%v", err)
	}
	keys, err := p.keySet(ctx, m, tok)
	if err != nil {
		return nil, err
	}
	if err := keys.Verify(tok); err != nil {
		return refuse("%v", err)
	}

	var c struct {
		Iss   string       `json:"iss"`
		Sub   string       `json:"sub"`
		Aud   audience     `json:"aud"`
		Azp   *string      `json:"azp"`
		Exp   *numericDate `json:"exp"`
		Iat   *numericDate `json:"iat"`
		Nbf   *numericDate `json:"nbf"`
		Nonce string       `json:"nonce"`
	}
	all := map[string]json.RawMessage{}
	if json.Unmarshal(tok.Payload, &c) != nil || json.Unmarshal(tok.Payload, &all) != nil {
		return refuse("the claims are not what RFC 7519 says they are")
	}
	// 2. The issuer is the one the keys were fetched for.
	if c.Iss != p.cfg.Issuer || c.Iss != m.Issuer {
		return refuse("issuer %q, want %q", clip(c.Iss, 200), p.cfg.Issuer)
	}
	// 3. The token is for this client.
	if !contains(c.Aud, p.cfg.ClientID) {
		return refuse("audience %q does not include this client", c.Aud)
	}
	// 3, 4 and 5. Other audiences are not trusted by themselves: with
	// more than one, "azp" must be there, and whenever it is there it
	// must name this client.
	if len(c.Aud) > 1 && c.Azp == nil {
		return refuse("several audiences and no authorized party")
	}
	if c.Azp != nil && *c.Azp != p.cfg.ClientID {
		return refuse("authorized party %q is not this client", clip(*c.Azp, 200))
	}
	// 9. Not expired.
	now := p.cfg.Now()
	if c.Exp == nil || !now.Before(c.Exp.time().Add(Leeway)) {
		return refuse("expired")
	}
	// 10. Issued a moment ago: not in the future, not long past.
	if c.Iat == nil || c.Iat.time().After(now.Add(Leeway)) || now.Sub(c.Iat.time()) > maxAge {
		return refuse("issued at a time that is not now")
	}
	if c.Nbf != nil && c.Nbf.time().After(now.Add(Leeway)) {
		return refuse("not valid yet")
	}
	// 11. The nonce of the request that this token answers.
	if nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1 {
		return refuse("nonce is not the one that was sent")
	}
	// Core 2: "sub" is required, at most 255 ASCII characters.
	if c.Sub == "" || len(c.Sub) > 255 {
		return refuse("no usable subject")
	}
	return &Claims{Issuer: c.Iss, Subject: c.Sub, Audience: c.Aud, All: all}, nil
}
