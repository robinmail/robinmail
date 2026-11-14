package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mikepage/robinmail/internal/store"
)

// fakeAccounts keeps accounts and sessions in memory. Passwords are
// "hashed" as "hash:" plus the password, which fakeVerify undoes.
type fakeAccounts struct {
	mu       sync.Mutex
	accounts map[string]store.Account
	sessions map[string]session
	now      func() time.Time
}

type session struct {
	account string
	expires time.Time
}

func (f *fakeAccounts) Account(_ context.Context, address string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[address]
	if !ok {
		return store.Account{}, store.ErrNoAccount
	}
	return a, nil
}

func (f *fakeAccounts) CreateSession(_ context.Context, hash, account string, expires time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[hash] = session{account, expires}
	return nil
}

func (f *fakeAccounts) SessionAccount(_ context.Context, hash string) (store.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[hash]
	if !ok || !f.now().Before(s.expires) {
		return store.Account{}, store.ErrNoSession
	}
	for _, a := range f.accounts {
		if a.ID == s.account {
			return a, nil
		}
	}
	return store.Account{}, store.ErrNoSession
}

func (f *fakeAccounts) DeleteSession(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, hash)
	return nil
}

type userCtx struct{}

type site struct {
	t        *testing.T
	s        *Server
	h        http.Handler
	accounts *fakeAccounts
	clock    time.Time
	verified atomic.Int64 // password checks done
}

const (
	tlsBase   = "https://mail.example.test"
	plainBase = "http://127.0.0.1:8080"
)

func newSite(t *testing.T) *site {
	st := &site{t: t, clock: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
	now := func() time.Time { return st.clock }
	st.accounts = &fakeAccounts{now: now, sessions: map[string]session{}, accounts: map[string]store.Account{
		"anna@example.test": {ID: "acct-anna", Address: "anna@example.test", PasswordHash: "hash:correct horse"},
		"bob@example.test":  {ID: "acct-bob", Address: "bob@example.test", PasswordHash: "hash:battery staple"},
	}}
	st.s = &Server{
		Hostname: "mail.example.test",
		Accounts: st.accounts,
		Now:      now,
		Verify: func(hash, password string) bool {
			st.verified.Add(1)
			return hash != "" && hash == "hash:"+password
		},
		WithUser: func(ctx context.Context, id, username string) context.Context {
			return context.WithValue(ctx, userCtx{}, id+" "+username)
		},
		// The JMAP handler of these tests says who it was called for.
		JMAP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "jmap for %v", r.Context().Value(userCtx{}))
		}),
	}
	st.h = st.s.Handler()
	return st
}

type reqOpt func(*http.Request)

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func cookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
	}
}
func basic(u, p string) reqOpt { return func(r *http.Request) { r.SetBasicAuth(u, p) } }
func from(addr string) reqOpt  { return func(r *http.Request) { r.RemoteAddr = addr } }

func (st *site) do(method, url, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	st.h.ServeHTTP(rec, req)
	return rec
}

func (st *site) login(base, user, password string, opts ...reqOpt) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"username": user, "password": password})
	return st.do("POST", base+"/auth/login", string(b), opts...)
}

// sessionCookie returns the cookie a response set.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		return c
	}
	return nil
}

// Login sets a session cookie that script cannot read, that is only sent
// over TLS and only on same-site requests; the store gets a hash of the
// token, not the token.
func TestLoginAndSession(t *testing.T) {
	st := newSite(t)
	rec := st.login(tlsBase, "Anna@Example.Test", "correct horse")
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"accountId":"acct-anna","username":"anna@example.test"}` {
		t.Errorf("body %s", got)
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no cookie")
	}
	if c.Name != "__Host-robinmail" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" ||
		c.Domain != "" || c.MaxAge != int(SessionTTL/time.Second) {
		t.Errorf("cookie %+v", c)
	}
	if len(c.Value) < 40 {
		t.Errorf("token of %d characters", len(c.Value))
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a login response may be cached")
	}
	if _, raw := st.accounts.sessions[c.Value]; raw {
		t.Error("the token itself is in the session table")
	}
	if _, hashed := st.accounts.sessions[tokenHash(c.Value)]; !hashed || len(st.accounts.sessions) != 1 {
		t.Errorf("sessions: %v", st.accounts.sessions)
	}
	// Two logins, two tokens.
	if c2 := sessionCookie(st.login(tlsBase, "anna@example.test", "correct horse")); c2.Value == c.Value {
		t.Error("the same token twice")
	}

	// The cookie is who you are.
	rec = st.do("GET", tlsBase+"/auth/session", "", cookie(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"username":"anna@example.test"`) {
		t.Errorf("session: %d %s", rec.Code, rec.Body.String())
	}
	rec = st.do("POST", tlsBase+"/jmap/api", "{}", cookie(c))
	if rec.Code != 200 || rec.Body.String() != "jmap for acct-anna anna@example.test" {
		t.Errorf("jmap: %d %s", rec.Code, rec.Body.String())
	}
	rec = st.do("GET", tlsBase+"/.well-known/jmap", "", cookie(c))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("well-known: %d", rec.Code)
	}
	for _, bad := range []*http.Cookie{nil, {Name: c.Name, Value: "forged"}, {Name: "robinmail", Value: c.Value},
		{Name: c.Name, Value: tokenHash(c.Value)}} {
		if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(bad)); rec.Code != 401 {
			t.Errorf("cookie %v: %d", bad, rec.Code)
		}
		if rec := st.do("POST", tlsBase+"/jmap/api", "{}", cookie(bad)); rec.Code != 401 {
			t.Errorf("jmap with cookie %v: %d", bad, rec.Code)
		}
	}

	// Sessions end: after 30 days, and at logout.
	st.clock = st.clock.Add(SessionTTL + time.Second)
	if rec := st.do("GET", tlsBase+"/auth/session", "", cookie(c)); rec.Code != 401 {
		t.Errorf("expired session: %d", rec.Code)
	}
	c = sessionCookie(st.login(tlsBase, "anna@example.test", "correct horse"))
	rec = st.do("POST", tlsBase+"/auth/logout", "", cookie(c))
	gone := sessionCookie(rec)
	if rec.Code != 204 || gone == nil || gone.MaxAge >= 0 || gone.Value != "" {
		t.Errorf("logout: %d %+v", rec.Code, gone)
	}
	if rec := st.do("POST", tlsBase+"/jmap/api", "{}", cookie(c)); rec.Code != 401 {
		t.Errorf("after logout: %d", rec.Code)
	}
	if rec := st.do("POST", tlsBase+"/auth/logout", ""); rec.Code != 204 {
		t.Errorf("logout without a session: %d", rec.Code)
	}

	// On the plain listener, for development: no Secure, no __Host- prefix.
	c = sessionCookie(st.login(plainBase, "anna@example.test", "correct horse"))
	if c == nil || c.Name != "robinmail" || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("plain cookie %+v", c)
	}
	if rec := st.do("POST", plainBase+"/jmap/api", "{}", cookie(c)); rec.Code != 200 {
		t.Errorf("plain jmap: %d", rec.Code)
	}
}

// A wrong password and an unknown address get the same answer, and both
// cost a password check, so neither the text nor the time tells them apart.
func TestLoginRefusals(t *testing.T) {
	st := newSite(t)
	var bodies []string
	for _, tc := range []struct{ user, password string }{
		{"anna@example.test", "wrong"},
		{"nobody@example.test", "correct horse"},
		{"not an address", "x"},
		{"", ""},
	} {
		before := st.verified.Load()
		rec := st.login(tlsBase, tc.user, tc.password)
		if rec.Code != 401 || sessionCookie(rec) != nil {
			t.Errorf("%q: %d", tc.user, rec.Code)
		}
		if st.verified.Load() != before+1 {
			t.Errorf("%q: no password check", tc.user)
		}
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Errorf("answers differ: %q and %q", bodies[0], b)
		}
	}
	if len(st.accounts.sessions) != 0 {
		t.Error("a refused login made a session")
	}

	for name, tc := range map[string]struct {
		method, body, contentType string
		want                      int
	}{
		"not JSON":         {"POST", `username=anna`, "application/json", 400},
		"missing password": {"POST", `{"username":"anna@example.test"}`, "application/json", 400},
		"wrong types":      {"POST", `{"username":1,"password":2}`, "application/json", 400},
		"a form":           {"POST", `username=anna@example.test&password=correct+horse`, "application/x-www-form-urlencoded", 415},
		"text/plain":       {"POST", `{"username":"anna@example.test","password":"correct horse"}`, "text/plain", 415},
		"too large":        {"POST", `{"username":"` + strings.Repeat("a", 5000) + `","password":"x"}`, "application/json", 400},
		"GET":              {"GET", ``, "", 405},
	} {
		rec := st.do(tc.method, tlsBase+"/auth/login", tc.body, header("Content-Type", tc.contentType))
		if rec.Code != tc.want || sessionCookie(rec) != nil {
			t.Errorf("%s: %d, want %d", name, rec.Code, tc.want)
		}
	}
	if rec := st.do("GET", tlsBase+"/auth/nothing", ""); rec.Code != 404 {
		t.Errorf("unknown auth path: %d", rec.Code)
	}
}

// Failed logins are counted per address and per IP. Ten for an address
// lock it for the rest of the window, also for the right password.
func TestLoginRateLimit(t *testing.T) {
	st := newSite(t)
	for i := 0; i < maxFailsPerAddr; i++ {
		if rec := st.login(tlsBase, "anna@example.test", "guess", from(fmt.Sprintf("198.51.100.%d:1000", i))); rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	checks := st.verified.Load()
	rec := st.login(tlsBase, "anna@example.test", "correct horse", from("203.0.113.9:1000"))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" || sessionCookie(rec) != nil {
		t.Errorf("after %d failures: %d, Retry-After %q", maxFailsPerAddr, rec.Code, rec.Header().Get("Retry-After"))
	}
	if st.verified.Load() != checks {
		t.Error("a locked-out login still cost a password check")
	}
	// The spelling of the address does not get around it.
	if rec := st.login(tlsBase, " ANNA@Example.Test ", "correct horse"); rec.Code != 429 {
		t.Errorf("other spelling: %d", rec.Code)
	}
	// Someone else is not affected.
	if rec := st.login(tlsBase, "bob@example.test", "battery staple", from("203.0.113.9:1000")); rec.Code != 200 {
		t.Errorf("another account: %d", rec.Code)
	}
	// When the window has passed, the account is open again.
	st.clock = st.clock.Add(loginWindow + time.Second)
	if rec := st.login(tlsBase, "anna@example.test", "correct horse"); rec.Code != 200 {
		t.Errorf("after the window: %d", rec.Code)
	}

	// One IP guessing at many addresses.
	st = newSite(t)
	for i := 0; i < maxFailsPerIP; i++ {
		st.login(tlsBase, fmt.Sprintf("user%d@example.test", i), "guess", from("198.51.100.7:4000"))
	}
	if rec := st.login(tlsBase, "bob@example.test", "battery staple", from("198.51.100.7:5000")); rec.Code != 429 {
		t.Errorf("IP after %d failures: %d", maxFailsPerIP, rec.Code)
	}
	if rec := st.login(tlsBase, "bob@example.test", "battery staple", from("198.51.100.8:5000")); rec.Code != 200 {
		t.Errorf("another IP: %d", rec.Code)
	}
	// A success does not clear the failures of the IP it came from, or an
	// attacker with one account of their own would never be limited.
	st = newSite(t)
	for i := 0; i < maxFailsPerIP-1; i++ {
		st.login(tlsBase, fmt.Sprintf("user%d@example.test", i), "guess", from("198.51.100.7:4000"))
	}
	st.login(tlsBase, "bob@example.test", "battery staple", from("198.51.100.7:4000"))
	st.login(tlsBase, "anna@example.test", "guess", from("198.51.100.7:4000"))
	if rec := st.login(tlsBase, "anna@example.test", "correct horse", from("198.51.100.7:4000")); rec.Code != 429 {
		t.Errorf("IP limit after a success in between: %d", rec.Code)
	}

	// The limiter's memory is bounded.
	l := newLimiter(3, time.Minute, time.Now)
	for i := 0; i < limiterMaxKeys+10; i++ {
		l.fail(fmt.Sprint(i))
	}
	if len(l.m) > limiterMaxKeys {
		t.Errorf("limiter holds %d keys", len(l.m))
	}
}

// Every state-changing request and every JMAP request has its origin
// checked: a page on another site cannot use the user's session.
func TestOriginCheck(t *testing.T) {
	st := newSite(t)
	c := sessionCookie(st.login(tlsBase, "anna@example.test", "correct horse"))
	plain := sessionCookie(st.login(plainBase, "anna@example.test", "correct horse"))
	for _, tc := range []struct {
		name string
		base string
		opts []reqOpt
		ok   bool
	}{
		{"no Origin: not a browser", tlsBase, nil, true},
		{"own origin", tlsBase, []reqOpt{header("Origin", "https://mail.example.test")}, true},
		{"own origin, other case", tlsBase, []reqOpt{header("Origin", "HTTPS://Mail.Example.Test")}, true},
		{"another site", tlsBase, []reqOpt{header("Origin", "https://evil.example")}, false},
		{"our name over http", tlsBase, []reqOpt{header("Origin", "http://mail.example.test")}, false},
		{"our name, another port", tlsBase, []reqOpt{header("Origin", "https://mail.example.test:8443")}, false},
		{"a subdomain", tlsBase, []reqOpt{header("Origin", "https://x.mail.example.test")}, false},
		{"a sandboxed frame", tlsBase, []reqOpt{header("Origin", "null")}, false},
		{"the dev origin, not configured", tlsBase, []reqOpt{header("Origin", "http://localhost:5173")}, false},
		{"Sec-Fetch-Site same-origin", tlsBase, []reqOpt{header("Sec-Fetch-Site", "same-origin")}, true},
		{"Sec-Fetch-Site cross-site", tlsBase, []reqOpt{header("Sec-Fetch-Site", "cross-site")}, false},
		{"Sec-Fetch-Site same-site", tlsBase, []reqOpt{header("Sec-Fetch-Site", "same-site")}, false},
		{"Sec-Fetch-Site none on a POST", tlsBase, []reqOpt{header("Sec-Fetch-Site", "none")}, false},
		{"plain, own origin", plainBase, []reqOpt{header("Origin", "http://127.0.0.1:8080")}, true},
		{"plain, another site", plainBase, []reqOpt{header("Origin", "http://evil.example")}, false},
	} {
		ck := c
		if tc.base == plainBase {
			ck = plain
		}
		opts := append([]reqOpt{cookie(ck)}, tc.opts...)
		for _, path := range []string{"/jmap/api", "/auth/logout"} {
			rec := st.do("POST", tc.base+path, "{}", opts...)
			if ok := rec.Code != 403; ok != tc.ok {
				t.Errorf("%s: POST %s: %d", tc.name, path, rec.Code)
			}
			if path == "/auth/logout" && rec.Code == 204 { // log in again for the next case
				c = sessionCookie(st.login(tlsBase, "anna@example.test", "correct horse"))
				plain = sessionCookie(st.login(plainBase, "anna@example.test", "correct horse"))
			}
		}
		rec := st.login(tc.base, "anna@example.test", "correct horse", tc.opts...)
		if ok := rec.Code == 200; ok != tc.ok {
			t.Errorf("%s: login: %d", tc.name, rec.Code)
		}
	}
	// A GET the user typed (Sec-Fetch-Site: none) is fine; one from another
	// site is not, also for a download.
	if rec := st.do("GET", tlsBase+"/jmap/session", "", cookie(c), header("Sec-Fetch-Site", "none")); rec.Code != 200 {
		t.Errorf("typed GET: %d", rec.Code)
	}
	if rec := st.do("GET", tlsBase+"/jmap/download/a/b/c", "", cookie(c), header("Sec-Fetch-Site", "cross-site")); rec.Code != 403 {
		t.Errorf("cross-site GET: %d", rec.Code)
	}

	// The Vite dev server's origin is allowed only when the config names it.
	st = newSite(t)
	st.s.DevOrigins = []string{"http://localhost:5173"}
	st.h = st.s.Handler()
	rec := st.login(plainBase, "anna@example.test", "correct horse", header("Origin", "http://localhost:5173"))
	if rec.Code != 200 {
		t.Errorf("dev origin: %d", rec.Code)
	}
	if rec := st.login(plainBase, "anna@example.test", "correct horse", header("Origin", "http://localhost:5174")); rec.Code != 403 {
		t.Errorf("another local origin: %d", rec.Code)
	}
}

// Standard JMAP clients send Basic credentials with every request. The
// password is checked once and then remembered for a few minutes.
func TestBasicAuth(t *testing.T) {
	st := newSite(t)
	rec := st.do("POST", tlsBase+"/jmap/api", "{}", basic("anna@example.test", "correct horse"))
	if rec.Code != 200 || rec.Body.String() != "jmap for acct-anna anna@example.test" {
		t.Fatalf("basic: %d %s", rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) != nil {
		t.Error("Basic auth set a cookie")
	}
	checks := st.verified.Load()
	for i := 0; i < 5; i++ {
		if rec := st.do("GET", tlsBase+"/jmap/session", "", basic("anna@example.test", "correct horse")); rec.Code != 200 {
			t.Fatalf("repeat: %d", rec.Code)
		}
	}
	if st.verified.Load() != checks {
		t.Errorf("%d more password checks for the same credentials", st.verified.Load()-checks)
	}
	// The memory of it is not a way in for another password or user.
	for _, tc := range [][2]string{{"anna@example.test", "wrong"}, {"bob@example.test", "correct horse"},
		{"anna@example.testcorrect", " horse"}, {"nobody@example.test", "x"}} {
		rec := st.do("POST", tlsBase+"/jmap/api", "{}", basic(tc[0], tc[1]))
		if rec.Code != 401 {
			t.Errorf("%v: %d", tc, rec.Code)
		}
		// A client that is not a browser is told how to authenticate.
		if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("%v: WWW-Authenticate %q", tc, rec.Header().Get("WWW-Authenticate"))
		}
	}
	// It expires.
	st.clock = st.clock.Add(basicCacheTTL + time.Second)
	checks = st.verified.Load()
	st.do("GET", tlsBase+"/jmap/session", "", basic("anna@example.test", "correct horse"))
	if st.verified.Load() != checks+1 {
		t.Error("the cache did not expire")
	}

	// A browser gets no WWW-Authenticate: it would open a password dialog
	// over the webmail.
	rec = st.do("POST", tlsBase+"/jmap/api", "{}", header("Sec-Fetch-Site", "same-origin"), header("Origin", "https://mail.example.test"))
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Errorf("browser without session: %d, WWW-Authenticate %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	// Guessing over Basic is limited like guessing at the login.
	st = newSite(t)
	for i := 0; i < maxFailsPerAddr; i++ {
		st.do("POST", tlsBase+"/jmap/api", "{}", basic("bob@example.test", fmt.Sprint("guess", i)))
	}
	rec = st.do("POST", tlsBase+"/jmap/api", "{}", basic("bob@example.test", "battery staple"))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Errorf("Basic after %d failures: %d", maxFailsPerAddr, rec.Code)
	}
}

// Argon2id takes 64 MiB a check, so only a few run at once; a request that
// gives up while waiting is not answered with a result.
func TestPasswordCheckBound(t *testing.T) {
	st := newSite(t)
	release := make(chan struct{})
	var running, peak atomic.Int64
	st.s.Verify = func(hash, password string) bool {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return hash == "hash:"+password
	}
	var wg sync.WaitGroup
	codes := make([]int, 12)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = st.login(tlsBase, "anna@example.test", "correct horse", from(fmt.Sprintf("198.51.100.%d:1", i))).Code
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for running.Load() < maxHashing && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // would a fifth start, it has had the time
	if got := running.Load(); got != maxHashing {
		t.Errorf("%d checks running, want %d", got, maxHashing)
	}
	// One more, whose client goes away while it waits.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", tlsBase+"/auth/login", strings.NewReader(`{"username":"bob@example.test","password":"battery staple"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { st.h.ServeHTTP(rec, req.WithContext(ctx)); close(done) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done
	if rec.Code != 503 {
		t.Errorf("cancelled while waiting: %d", rec.Code)
	}
	close(release)
	wg.Wait()
	for i, code := range codes {
		if code != 200 {
			t.Errorf("login %d: %d", i, code)
		}
	}
	if peak.Load() > maxHashing {
		t.Errorf("peak of %d checks at once", peak.Load())
	}
}

// Requests for a host that is not ours are refused: that is how a page
// that points its own name at this address (DNS rebinding) would arrive.
func TestHostCheck(t *testing.T) {
	st := newSite(t)
	for url, want := range map[string]int{
		"https://mail.example.test/":     200,
		"https://MAIL.example.test:443/": 200,
		"https://evil.example/":          421,
		"https://localhost/":             421, // only on the plain listener
		"https://127.0.0.1/":             421,
		"http://mail.example.test/":      200,
		"http://localhost:8080/":         200,
		"http://127.0.0.1:8080/":         200,
		"http://[::1]:8080/":             200,
		"http://evil.example:8080/":      421,
		"http://192.0.2.5:8080/":         421,
		"http://localhost.evil.example/": 421,
	} {
		if rec := st.do("GET", url, ""); rec.Code != want {
			t.Errorf("%s: %d, want %d", url, rec.Code, want)
		}
	}
	rec := st.do("POST", "https://evil.example/auth/login", `{"username":"anna@example.test","password":"correct horse"}`)
	if rec.Code != 421 || sessionCookie(rec) != nil {
		t.Errorf("login under another host: %d", rec.Code)
	}
	// The host of a dev origin is accepted on the plain listener.
	st.s = &Server{Hostname: "mail.example.test", Accounts: st.accounts, JMAP: st.s.JMAP, WithUser: st.s.WithUser,
		DevOrigins: []string{"http://dev.box.test:5173"}}
	st.h = st.s.Handler()
	if rec := st.do("GET", "http://dev.box.test:5173/", ""); rec.Code != 200 {
		t.Errorf("dev host: %d", rec.Code)
	}
	if rec := st.do("GET", "https://dev.box.test/", ""); rec.Code != 421 {
		t.Errorf("dev host over TLS: %d", rec.Code)
	}
}

// Every response carries the security headers; HSTS only over TLS.
func TestSecurityHeaders(t *testing.T) {
	st := newSite(t)
	c := sessionCookie(st.login(tlsBase, "anna@example.test", "correct horse"))
	for _, tc := range []struct{ method, url string }{
		{"GET", tlsBase + "/"},
		{"GET", tlsBase + "/assets/missing.js"},
		{"POST", tlsBase + "/auth/login"},
		{"GET", tlsBase + "/auth/session"},
		{"POST", tlsBase + "/jmap/api"},
		{"GET", "https://evil.example/"},
	} {
		h := st.do(tc.method, tc.url, "", cookie(c)).Header()
		for name, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Referrer-Policy":              "no-referrer",
			"X-Frame-Options":              "DENY",
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
			"Strict-Transport-Security":    "max-age=31536000",
		} {
			if h.Get(name) != want {
				t.Errorf("%s %s: %s = %q", tc.method, tc.url, name, h.Get(name))
			}
		}
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q: %s", tc.url, want, csp)
			}
		}
		// No inline or foreign script: there is no script-src, so
		// default-src 'self' decides.
		if strings.Contains(csp, "script-src") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "*") {
			t.Errorf("CSP: %s", csp)
		}
		if h.Get("Permissions-Policy") == "" {
			t.Errorf("%s: no Permissions-Policy", tc.url)
		}
	}
	if h := st.do("GET", plainBase+"/", "").Header(); h.Get("Strict-Transport-Security") != "" || h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("plain: %v", h)
	}
	// A handler may replace a header, as the HTML view does.
	st.s.JMAP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	})
	h := st.do("GET", tlsBase+"/jmap/view/a/e1", "", cookie(c)).Header()
	if h.Get("Content-Security-Policy") != "default-src 'none'; sandbox" || h.Get("X-Frame-Options") != "SAMEORIGIN" ||
		len(h.Values("Content-Security-Policy")) != 1 {
		t.Errorf("replaced headers: %v", h)
	}
}

// Outside the API paths the server serves the webmail from an fs.FS: the
// placeholder for now. A path that is no file is a route of the app.
func TestStatic(t *testing.T) {
	st := newSite(t)
	for _, path := range []string{"/", "/index.html", "/mail/inbox", "/thread/t12", "/login"} {
		rec := st.do("GET", tlsBase+path, "")
		body, _ := io.ReadAll(rec.Body)
		if rec.Code != 200 || !strings.Contains(string(body), "<title>robinmail</title>") ||
			rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := st.do("GET", tlsBase+"/assets/app.js", ""); rec.Code != 404 {
		t.Errorf("missing asset: %d", rec.Code)
	}
	if rec := st.do("POST", tlsBase+"/", ""); rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST /: %d", rec.Code)
	}
	if rec := st.do("HEAD", tlsBase+"/", ""); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD /: %d, %d bytes", rec.Code, rec.Body.Len())
	}

	// The embedded webmail will replace the placeholder like this.
	st.s = &Server{Hostname: "mail.example.test", Accounts: st.accounts, JMAP: st.s.JMAP, WithUser: st.s.WithUser,
		Static: fstest.MapFS{
			"index.html":          {Data: []byte("<!doctype html><title>webmail</title>")},
			"assets/app-1a2b.js":  {Data: []byte("console.log('app')")},
			"assets/app-1a2b.css": {Data: []byte("body{}")},
			"favicon.ico":         {Data: []byte{0}},
		}}
	st.h = st.s.Handler()
	rec := st.do("GET", tlsBase+"/mail/inbox", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<title>webmail</title>") {
		t.Errorf("app route: %d %s", rec.Code, rec.Body.String())
	}
	rec = st.do("GET", tlsBase+"/assets/app-1a2b.js", "")
	if rec.Code != 200 || rec.Body.String() != "console.log('app')" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"))
	}
	if rec := st.do("GET", tlsBase+"/favicon.ico", ""); rec.Code != 200 || rec.Header().Get("Cache-Control") != "" {
		t.Errorf("favicon: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	for _, path := range []string{"/assets/", "/assets", "/assets/../index.html", "/../../etc/passwd", "/assets/nope.js", "/%2e%2e/secret.txt"} {
		rec := st.do("GET", tlsBase+path, "")
		if rec.Code == 200 && !strings.Contains(rec.Body.String(), "<title>webmail</title>") {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	// The API paths never fall through to the app.
	if rec := st.do("GET", tlsBase+"/jmap/anything", ""); rec.Code != 401 {
		t.Errorf("/jmap/anything: %d", rec.Code)
	}
	if rec := st.do("GET", tlsBase+"/auth/anything", ""); rec.Code != 404 || strings.Contains(rec.Body.String(), "webmail") {
		t.Errorf("/auth/anything: %d", rec.Code)
	}
}

// fakeLimits is the failure counters in rqlite: one map shared by every
// node of a test.
type fakeLimits struct {
	mu     sync.Mutex
	now    func() time.Time
	m      map[string]store.LoginFailure
	down   bool
	reads  int
	writes int
}

func (f *fakeLimits) LoginFailures(_ context.Context, window time.Duration, keys ...string) (map[string]store.LoginFailure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.down {
		return nil, errors.New("rqlite is down")
	}
	out := map[string]store.LoginFailure{}
	for _, k := range keys {
		if v, ok := f.m[k]; ok && v.Started.Add(window).After(f.now()) {
			out[k] = v
		}
	}
	return out, nil
}

func (f *fakeLimits) AddLoginFailure(_ context.Context, window time.Duration, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.down {
		return errors.New("rqlite is down")
	}
	for _, k := range keys {
		v, ok := f.m[k]
		if !ok || !v.Started.Add(window).After(f.now()) {
			v = store.LoginFailure{Started: f.now()}
		}
		v.Count++
		f.m[k] = v
	}
	return nil
}

func (f *fakeLimits) ClearLoginFailures(_ context.Context, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	for _, k := range keys {
		delete(f.m, k)
	}
	return nil
}

// Two nodes share the failure counters: guesses spread over both are
// counted together, and the lock-out holds on a node that saw none of
// them.
func TestLoginRateLimitAcrossNodes(t *testing.T) {
	a, b := newSite(t), newSite(t)
	limits := &fakeLimits{now: func() time.Time { return a.clock }, m: map[string]store.LoginFailure{}}
	a.s.Limits, b.s.Limits = limits, limits

	for i := 0; i < maxFailsPerAddr; i++ {
		node := []*site{a, b}[i%2]
		if rec := node.login(tlsBase, "anna@example.test", "guess", from(fmt.Sprintf("198.51.100.%d:1000", i))); rec.Code != 401 {
			t.Fatalf("attempt %d: %d", i, rec.Code)
		}
	}
	// Each node saw half: neither would lock out by itself.
	c := newSite(t) // a third node that saw nothing
	c.s.Limits = limits
	for name, node := range map[string]*site{"a": a, "b": b, "c": c} {
		checks := node.verified.Load()
		rec := node.login(tlsBase, "anna@example.test", "correct horse", from("203.0.113.9:1000"))
		if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
			t.Errorf("node %s after %d failures in the cluster: %d", name, maxFailsPerAddr, rec.Code)
		}
		if node.verified.Load() != checks {
			t.Errorf("node %s: a locked-out login still cost a password check", name)
		}
	}
	if rec := c.login(tlsBase, "bob@example.test", "battery staple", from("203.0.113.9:1000")); rec.Code != 200 {
		t.Errorf("another account: %d", rec.Code)
	}
	// The window ends for all nodes together.
	a.clock = a.clock.Add(loginWindow + time.Second)
	c.clock = a.clock
	if rec := c.login(tlsBase, "anna@example.test", "correct horse"); rec.Code != 200 {
		t.Errorf("after the window: %d", rec.Code)
	}

	// The same for one IP guessing at many addresses through two nodes.
	limits.m = map[string]store.LoginFailure{}
	a, b = newSite(t), newSite(t)
	a.s.Limits, b.s.Limits = limits, limits
	limits.now = func() time.Time { return a.clock }
	for i := 0; i < maxFailsPerIP; i++ {
		[]*site{a, b}[i%2].login(tlsBase, fmt.Sprintf("user%d@example.test", i), "guess", from("198.51.100.7:4000"))
	}
	if rec := b.login(tlsBase, "bob@example.test", "battery staple", from("198.51.100.7:5000")); rec.Code != 429 {
		t.Errorf("IP after %d failures over two nodes: %d", maxFailsPerIP, rec.Code)
	}
}

// What the shared counters cost: a login without failures behind it reads
// once and writes nothing; a success after failures clears the address and
// leaves the IP; and when rqlite is away the node's own counters still
// limit.
func TestLoginRateLimitCostAndFallback(t *testing.T) {
	st := newSite(t)
	limits := &fakeLimits{now: func() time.Time { return st.clock }, m: map[string]store.LoginFailure{}}
	st.s.Limits = limits

	if rec := st.login(tlsBase, "anna@example.test", "correct horse"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if limits.reads != 1 || limits.writes != 0 {
		t.Errorf("a clean login cost %d reads and %d writes, want 1 and 0", limits.reads, limits.writes)
	}
	st.login(tlsBase, "anna@example.test", "guess", from("198.51.100.7:1"))
	st.login(tlsBase, "anna@example.test", "guess", from("198.51.100.7:1"))
	if limits.m["a:anna@example.test"].Count != 2 || limits.m["i:198.51.100.7"].Count != 2 {
		t.Errorf("after two failures: %+v", limits.m)
	}
	st.login(tlsBase, "anna@example.test", "correct horse", from("198.51.100.7:1"))
	if _, ok := limits.m["a:anna@example.test"]; ok || limits.m["i:198.51.100.7"].Count != 2 {
		t.Errorf("after a success: %+v", limits.m)
	}
	// A name that is not an address is counted under its own text.
	st.login(tlsBase, "not an address", "guess")
	if limits.m["a:not an address"].Count != 1 {
		t.Errorf("unparsable name: %+v", limits.m)
	}

	limits.down = true
	for i := 0; i < maxFailsPerAddr; i++ {
		if rec := st.login(tlsBase, "bob@example.test", "guess", from(fmt.Sprintf("198.51.100.%d:1000", i))); rec.Code != 401 {
			t.Fatalf("attempt %d with rqlite down: %d", i, rec.Code)
		}
	}
	if rec := st.login(tlsBase, "bob@example.test", "battery staple"); rec.Code != 429 {
		t.Errorf("with rqlite down the node's own limit did not hold: %d", rec.Code)
	}
}
