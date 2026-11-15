package clustertest

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/totp"
)

// reply is one HTTP answer of a robinmail node.
type reply struct {
	status  int
	body    []byte
	cookies map[string]string // name -> "name=value", as set by this answer
}

// auth sends a request to a node. cookie is "name=value" pairs; basic is
// "user:secret" or empty.
func auth(t testing.TB, node int, method, path, cookie, basic string, body any) reply {
	t.Helper()
	j := newJMAP(node)
	var rd io.Reader
	if method != "GET" {
		data, _ := json.Marshal(body)
		if body == nil {
			data = []byte("{}")
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "https://"+j.host+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if user, secret, ok := strings.Cut(basic, ":"); ok {
		req.SetBasicAuth(user, secret)
	}
	resp, err := j.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s on %s: %v", method, path, j.service, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, body: data, cookies: map[string]string{}}
	for _, c := range resp.Cookies() {
		r.cookies[c.Name] = c.Name + "=" + c.Value
	}
	return r
}

func (r reply) into(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("answer %d %s: %v", r.status, r.body, err)
	}
}

const (
	sessionCookie = "__Host-robinmail"
	pendingCookie = "__Host-robinmail-login"
)

// streamEnds opens an event stream on a node and returns a channel that is
// closed when the server ends it.
func streamEnds(t testing.TB, node int, cookie, basic string) <-chan struct{} {
	t.Helper()
	j := newJMAP(node)
	j.hc.Timeout = 0
	req, _ := http.NewRequest("GET", "https://"+j.host+"/jmap/eventsource/?types=*&closeafter=no&ping=0", nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if user, secret, ok := strings.Cut(basic, ":"); ok {
		req.SetBasicAuth(user, secret)
	}
	resp, err := j.hc.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("event stream on %s: %v %v", j.service, resp, err)
	}
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		close(ended)
	}()
	return ended
}

func ends(t *testing.T, what string, ended <-chan struct{}, since time.Time) {
	t.Helper()
	select {
	case <-ended:
		t.Logf("%s after %v", what, time.Since(since).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Errorf("%s: not within 15 seconds", what)
	}
}

// authAcrossNodes is phase 7 on the real cluster: three robinmail nodes,
// each asking its own rqlite node first, of which two are followers. What
// one node is told has to hold on the others: an app password and its
// revocation, a revoked session, a spent TOTP code, and the counters of
// failed logins and wrong codes, which a node reads from its own rqlite
// node's copy without asking the leader.
func authAcrossNodes(t *testing.T) {
	// The cluster's containers all reach the nodes from one address: the
	// counter of that address is this test's to reset.
	rqliteAdmin(0, "POST", "/db/execute", []byte(`["DELETE FROM login_failure WHERE key LIKE 'i:%'"]`))
	user := fmt.Sprintf("auth%d@%s", time.Now().UnixNano()%1_000_000_000, domain)
	pw := password()
	sh(t, 0, fmt.Sprintf(`printf '%%s\n' '%s' | robinmail -config %s user add %s`, pw, robin[0].config, user))

	login := func(node int, password string) reply {
		return auth(t, node, "POST", "/auth/login", "", "", map[string]string{"username": user, "password": password})
	}
	first := login(0, pw)
	cookie := first.cookies[sessionCookie]
	if first.status != 200 || cookie == "" {
		t.Fatalf("login on node 1: %d %s", first.status, first.body)
	}

	// --- An app password made on node 1 works on nodes 2 and 3 at once,
	// and the account password stops working for Basic auth there. ---
	for n := 1; n <= 2; n++ {
		if r := auth(t, n, "GET", "/jmap/session", "", user+":"+pw, nil); r.status != 200 {
			t.Fatalf("%s: Basic auth with the account password before any app password: %d", robin[n].service, r.status)
		}
	}
	var app struct{ ID, Secret string }
	if r := auth(t, 0, "POST", "/auth/app-passwords", cookie, "", map[string]any{"name": "Cluster test", "password": pw}); r.status != 201 {
		t.Fatalf("creating an app password: %d %s", r.status, r.body)
	} else {
		r.into(t, &app)
	}
	for n := 1; n <= 2; n++ {
		if r := auth(t, n, "GET", "/jmap/session", "", user+":"+app.Secret, nil); r.status != 200 {
			t.Errorf("%s: Basic auth with the app password right after it was made on node 1: %d", robin[n].service, r.status)
		}
		if r := auth(t, n, "GET", "/jmap/session", "", user+":"+pw, nil); r.status != 401 {
			t.Errorf("%s: Basic auth with the account password once an app password exists: %d", robin[n].service, r.status)
		}
	}

	// --- Revoking on node 1 ends what is open on the other nodes: an
	// event stream of the app password on node 2, and one of a session on
	// node 3 when that session is revoked. ---
	second := login(2, pw)
	other := second.cookies[sessionCookie]
	appStream := streamEnds(t, 1, "", user+":"+app.Secret)
	sessionStream := streamEnds(t, 2, other, "")
	keptStream := streamEnds(t, 1, cookie, "")
	time.Sleep(1500 * time.Millisecond) // the nodes' polls have seen the listeners

	start := time.Now()
	if r := auth(t, 0, "DELETE", "/auth/app-passwords/"+app.ID, cookie, "", nil); r.status != 204 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	ends(t, "app password revoked on node 1 -> its event stream on node 2 ended", appStream, start)
	for n := 0; n <= 2; n++ {
		if r := auth(t, n, "GET", "/jmap/session", "", user+":"+app.Secret, nil); r.status != 401 {
			t.Errorf("%s: Basic auth with the revoked app password: %d", robin[n].service, r.status)
		}
	}

	var list struct {
		List []struct {
			ID      string
			Current bool
		}
	}
	auth(t, 0, "GET", "/auth/sessions", cookie, "", nil).into(t, &list)
	if len(list.List) != 2 {
		t.Fatalf("sessions: %+v", list)
	}
	start = time.Now()
	for _, s := range list.List {
		if !s.Current {
			if r := auth(t, 0, "DELETE", "/auth/sessions/"+s.ID, cookie, "", nil); r.status != 204 {
				t.Fatalf("revoking the session: %d %s", r.status, r.body)
			}
		}
	}
	ends(t, "session revoked on node 1 -> its event stream on node 3 ended", sessionStream, start)
	for n := 0; n <= 2; n++ {
		if r := auth(t, n, "GET", "/auth/session", other, "", nil); r.status != 401 {
			t.Errorf("%s: the revoked session: %d", robin[n].service, r.status)
		}
	}
	select {
	case <-keptStream:
		t.Error("the event stream of the session that was not revoked ended too")
	default:
	}

	// --- TOTP: set up on node 1, confirmed on node 2; the login's first
	// step on node 2 and its second on node 3; the same code refused on
	// node 1. ---
	var enrol struct{ Secret string }
	if r := auth(t, 0, "POST", "/auth/totp/enrol", cookie, "", map[string]string{"password": pw}); r.status != 200 {
		t.Fatalf("enrol: %d %s", r.status, r.body)
	} else {
		r.into(t, &enrol)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrol.Secret)
	if err != nil {
		t.Fatal(err)
	}
	step := totp.Step(time.Now())
	if r := auth(t, 1, "POST", "/auth/totp/confirm", cookie, "", map[string]string{"code": totp.Code(secret, step)}); r.status != 200 {
		t.Fatalf("confirm on node 2: %d %s", r.status, r.body)
	}
	code := totp.Code(secret, step+1) // the next step's code: within the window, and not spent
	stepOne := func(node int) string {
		t.Helper()
		r := login(node, pw)
		if r.status != 200 || !strings.Contains(string(r.body), `"step":"totp"`) || r.cookies[pendingCookie] == "" || r.cookies[sessionCookie] != "" {
			t.Fatalf("%s: step one: %d %s %v", robin[node].service, r.status, r.body, r.cookies)
		}
		return r.cookies[pendingCookie]
	}
	pending := stepOne(1)
	if r := auth(t, 2, "POST", "/auth/login/totp", pending, "", map[string]string{"code": code}); r.status != 200 || r.cookies[sessionCookie] == "" {
		t.Fatalf("step one on node 2, the code on node 3: %d %s", r.status, r.body)
	}
	pending = stepOne(2)
	if r := auth(t, 0, "POST", "/auth/login/totp", pending, "", map[string]string{"code": code}); r.status != 401 {
		t.Errorf("the same code again, on node 1: %d %s", r.status, r.body)
	}

	// --- Wrong codes are counted for the cluster: one is counted (the
	// replay), four more over nodes 1 and 2, and node 3, which saw none of
	// them, refuses the right code. A node reads the counters from its
	// own rqlite node without asking the leader, so a follower may be a
	// moment behind: how long that moment is, is what is measured. ---
	for i := 0; i < 4; i++ {
		if r := auth(t, i%2, "POST", "/auth/login/totp", pending, "", map[string]string{"code": fmt.Sprintf("%06d", i)}); r.status != 401 {
			t.Fatalf("wrong code %d on %s: %d %s", i, robin[i%2].service, r.status, r.body)
		}
	}
	// The probe is a wrong code: where the limit is not seen yet it is one
	// more wrong code, and where it is seen, the answer is 429. (A right
	// code that a node behind the others still accepted would clear the
	// counter, as any success does.)
	took := waitFor(t, "node 3 to refuse codes after five wrong ones on nodes 1 and 2", 10*time.Second, func() bool {
		return auth(t, 2, "POST", "/auth/login/totp", pending, "", map[string]string{"code": "999999"}).status == 429
	})
	t.Logf("five wrong codes on nodes 1 and 2 -> node 3 refuses after %v", took.Round(time.Millisecond))
	if took > 2*time.Second {
		t.Errorf("node 3 took %v to see the limit", took)
	}
	right := map[string]string{"code": totp.Code(secret, totp.Step(time.Now())+1)}
	for n := 0; n <= 2; n++ {
		if r := auth(t, n, "POST", "/auth/login/totp", pending, "", right); r.status != 429 {
			t.Errorf("%s: the right code after five wrong ones: %d", robin[n].service, r.status)
		}
	}

	// --- The same for passwords at the web login: ten wrong ones over
	// nodes 1 and 2 close the address on node 3. ---
	for i := 0; i < 10; i++ {
		if r := login(i%2, fmt.Sprintf("wrong-%d", i)); r.status != 401 {
			t.Fatalf("wrong password %d on %s: %d", i, robin[i%2].service, r.status)
		}
	}
	took = waitFor(t, "node 3 to refuse the login after ten wrong passwords on nodes 1 and 2", 10*time.Second, func() bool {
		return login(2, "wrong-probe").status == 429
	})
	t.Logf("ten wrong passwords on nodes 1 and 2 -> node 3 refuses after %v", took.Round(time.Millisecond))
	if took > 2*time.Second {
		t.Errorf("node 3 took %v to see the limit", took)
	}
	for n := 0; n <= 2; n++ {
		if r := login(n, pw); r.status != 429 {
			t.Errorf("%s: the right password after ten wrong ones: %d", robin[n].service, r.status)
		}
	}
	// The session that was there all along still works, on every node.
	for n := 0; n <= 2; n++ {
		if r := auth(t, n, "GET", "/auth/session", cookie, "", nil); r.status != 200 {
			t.Errorf("%s: the session after all that: %d", robin[n].service, r.status)
		}
	}

	// The log of auth events, from a node that did none of it.
	audit := sh(t, 2, fmt.Sprintf(`robinmail -config %s audit %s 100`, robin[2].config, user))
	for _, want := range []string{"app-password-created", "app-password-revoked", "session-revoked", "totp-enabled", "web: password+totp", "code: used before", "login-failed"} {
		if !strings.Contains(audit, want) {
			t.Errorf("the audit log lacks %q:\n%s", want, audit)
		}
	}
	if strings.Contains(audit, app.Secret) || strings.Contains(audit, enrol.Secret) {
		t.Error("the audit log holds a secret")
	}
}
