package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/oidc/oidctest"
	"github.com/mikepage/robinmail/internal/queue"
	"github.com/mikepage/robinmail/internal/totp"
)

// authNode is one of two servers on the same database.
type authNode struct {
	*instance
	imapTLS string
}

// twoAuthNodes starts two "serve" in this process on one database, master
// key and blob directory, each with web, IMAP and submission listeners:
// two nodes of a cluster, as far as logins are concerned. extra is put
// into both configs.
func twoAuthNodes(t *testing.T, base, name, extra string) (a, b authNode) {
	t.Helper()
	const imapConfig = `"imap":{"listen_tls":"127.0.0.1:0","listen":"127.0.0.1:0"},`
	t.Cleanup(func() { onIMAPListen = nil })

	onIMAPListen = func(_, implicit net.Addr) { a.imapTLS = implicit.String() }
	a.instance = startInstanceWith(t, base, name, imapConfig+extra, func(*queue.Worker, *instance) {})

	dirB := t.TempDir()
	cert, key, _ := selfSigned(t, dirB, a.host)
	cfgB := filepath.Join(dirB, "robinmail.json")
	body := fmt.Sprintf(`{"hostname":%q,"data_dir":%q,"master_key_file":%q,"rqlite":{"url":%q},`+imapConfig+extra+`
		"tls":{"cert_file":%q,"key_file":%q},"smtp":{"listen":"127.0.0.1:0"},
		"submission":{"listen":"127.0.0.1:0","listen_tls":"127.0.0.1:0","undo_seconds":0},
		"web":{"http_listen":"127.0.0.1:0"}}`,
		a.host, filepath.Join(a.dir, "data"), filepath.Join(a.dir, "master.key"), base, cert, key)
	if err := os.WriteFile(cfgB, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b.instance = &instance{t: t, cli: &cli{t: t, dir: dirB, cfg: cfgB}, domain: a.domain, host: a.host, pool: a.pool}
	onIMAPListen = func(_, implicit net.Addr) { b.imapTLS = implicit.String() }
	onSubmissionListen = func(plain, implicit net.Addr) { b.submission, b.implicit = plain.String(), implicit.String() }
	t.Cleanup(func() { onSubmissionListen = nil })
	srvB := startServe(t, b.cli)
	b.smtp, b.web = srvB.addr, srvB.web
	if a.imapTLS == "" || b.imapTLS == "" || a.imapTLS == b.imapTLS || b.implicit == "" {
		t.Fatalf("listeners: imap %q %q, submission %q", a.imapTLS, b.imapTLS, b.implicit)
	}
	return a, b
}

// imapLogin tries a LOGIN on a new connection and returns the connection
// and the tagged answer.
func imapLogin(t *testing.T, name, addr, user, secret string) (*imapClient, string) {
	t.Helper()
	c := dialIMAP(t, name, addr)
	// The secret is quoted here and not logged by the server; the test
	// log shows it, and it is one this test made a moment ago.
	got := c.cmd(fmt.Sprintf(`LOGIN %s "%s"`, user, secret))
	return c, got[len(got)-1]
}

// smtpAuth tries AUTH PLAIN on the implicit TLS submission listener and
// returns the reply code.
func smtpAuth(t *testing.T, n authNode, user, secret string) int {
	t.Helper()
	s, code := smtpOpen(t, n, user, secret)
	s.conn.Close()
	return code
}

// smtpSession is a submission session that stays open after AUTH, as a
// mail client's does between two messages.
type smtpSession struct {
	conn net.Conn
	br   *bufio.Reader
}

func smtpOpen(t *testing.T, n authNode, user, secret string) (*smtpSession, int) {
	t.Helper()
	// Each node has a certificate of its own making; like dialIMAP, this
	// does not check it.
	conn, err := tls.Dial("tcp", n.implicit, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	s := &smtpSession{conn, bufio.NewReader(conn)}
	s.reply(t)
	fmt.Fprintf(conn, "EHLO client.example\r\n")
	s.reply(t)
	fmt.Fprintf(conn, "AUTH PLAIN %s\r\n", base64.StdEncoding.EncodeToString([]byte("\x00"+user+"\x00"+secret)))
	code, _ := s.reply(t)
	return s, code
}

func (s *smtpSession) reply(t *testing.T) (int, string) {
	t.Helper()
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			t.Fatalf("submission: %v", err)
		}
		if len(line) >= 4 && line[3] == ' ' {
			code := 0
			fmt.Sscanf(line[:3], "%d", &code)
			return code, line
		}
	}
}

// ended checks that the server ends the session unasked, with 421 4.7.0,
// and closes the connection.
func (s *smtpSession) ended(t *testing.T, what string) {
	t.Helper()
	s.conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := s.br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "421 4.7.0 ") || !strings.Contains(line, "login was withdrawn") {
		t.Errorf("%s: the open submission session got %q, %v", what, line, err)
		return
	}
	if _, err := s.br.ReadString('\n'); err != io.EOF {
		t.Errorf("%s: the submission session after the 421: %v, want the connection closed", what, err)
	}
}

// post sends JSON and decodes the answer into out when it is not nil.
func (c *jmapClient) post(method, url, body string, out any) int {
	c.t.Helper()
	resp, data := c.do(method, url, body, map[string]string{"Content-Type": "application/json"})
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, data)
		}
	}
	return resp.StatusCode
}

// TestAuthEndToEnd is the exit of phase 7 as far as it needs no browser
// and no provider: two nodes on one database, real IMAP, SMTP and HTTP.
//
//   - Until an account has an app password its password works for mail
//     clients. The first app password ends that: the open IMAP session
//     that logged in with the password is told BYE, on the other node, and
//     IMAP, SMTP and Basic auth refuse the password from then on while the
//     web login still takes it.
//
//   - An app password opens IMAP, SMTP and JMAP on both nodes, within its
//     scope. Revoking it on one node ends, within the second it takes the
//     other node to notice, the IDLE session that used it there, and
//     leaves the session of another app password alone.
//
//   - With TOTP the web login has a second step. A code works once across
//     the nodes, a recovery code too, and wrong codes are counted for
//     both nodes together.
//
//   - An administrator ends sessions (an open event stream on the other
//     node ends with them), turns TOTP off, resets the password and reads
//     the log of all of it.
//
//     ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 go test -v -run TestAuthEndToEnd ./cmd/robinmail
func TestAuthEndToEnd(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	a, b := twoAuthNodes(t, base, "auth", "")
	anna := a.user("anna")
	const password = "correct horse"
	const bye = "* BYE this login was revoked; log in again"

	// --- 1. Before any app password: the account password opens IMAP. ---
	withPassword, got := imapLogin(t, "pw", b.imapTLS, anna, password)
	if !strings.HasPrefix(got, "OK") {
		t.Fatalf("IMAP with the account password, no app passwords yet: %s", got)
	}
	withPassword.ok("SELECT INBOX")
	withPassword.write("idle0 IDLE\r\n")
	withPassword.want("+ idling", 10*time.Second)
	if code := smtpAuth(t, a, anna, password); code != 235 {
		t.Fatalf("SMTP AUTH with the account password: %d", code)
	}
	// A submission session that stays open, as Thunderbird keeps one
	// between messages.
	smtpWithPassword, authCode := smtpOpen(t, b, anna, password)
	if authCode != 235 {
		t.Fatalf("SMTP AUTH with the account password: %d", authCode)
	}

	// --- 2. The first app password, made in the webmail on node A. ---
	web := newJMAPClient(t, "http://"+a.web)
	if status := web.post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, anna, password), nil); status != 200 {
		t.Fatalf("web login: %d", status)
	}
	var phone, reader struct{ ID, Secret string }
	if status := web.post("POST", "/auth/app-passwords", `{"name":"Phone","password":"wrong"}`, nil); status != 403 {
		t.Fatalf("an app password with a wrong account password: %d", status)
	}
	start := time.Now()
	if status := web.post("POST", "/auth/app-passwords", fmt.Sprintf(`{"name":"Phone","password":%q}`, password), &phone); status != 201 || phone.Secret == "" {
		t.Fatalf("creating an app password: %d", status)
	}
	// The session on node B that logged in with the account password is
	// told to go: that password is no longer a way into IMAP.
	withPassword.want(bye, 10*time.Second)
	t.Logf("first app password on node A -> BYE for the password's IDLE session on node B after %v", time.Since(start).Round(time.Millisecond))
	// So is the submission session: it could send mail otherwise.
	smtpWithPassword.ended(t, "the first app password")

	// --- 3. The account password is for the web login only now. ---
	for name, n := range map[string]authNode{"A": a, "B": b} {
		if _, got := imapLogin(t, "old"+name, n.imapTLS, anna, password); !strings.HasPrefix(got, "NO [AUTHENTICATIONFAILED]") {
			t.Errorf("node %s: IMAP with the account password once an app password exists: %s", name, got)
		}
		if code := smtpAuth(t, n, anna, password); code != 535 {
			t.Errorf("node %s: SMTP AUTH with the account password: %d", name, code)
		}
		req, _ := http.NewRequest("GET", "http://"+n.web+"/jmap/session", nil)
		req.SetBasicAuth(anna, password)
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
			t.Errorf("node %s: Basic auth with the account password: %v %v", name, resp, err)
		} else {
			resp.Body.Close()
		}
		// The app password opens all three.
		if c, got := imapLogin(t, "app"+name, n.imapTLS, anna, phone.Secret); !strings.HasPrefix(got, "OK") {
			t.Errorf("node %s: IMAP with the app password: %s", name, got)
		} else {
			c.ok("LOGOUT")
		}
		if code := smtpAuth(t, n, anna, phone.Secret); code != 235 {
			t.Errorf("node %s: SMTP AUTH with the app password: %d", name, code)
		}
		req.SetBasicAuth(anna, strings.ToUpper(strings.ReplaceAll(phone.Secret, "-", "")))
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
			t.Errorf("node %s: Basic auth with the app password, typed without dashes and in capitals: %v %v", name, resp, err)
		} else {
			resp.Body.Close()
		}
	}
	other := newJMAPClient(t, "http://"+b.web)
	if status := other.post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, anna, password), nil); status != 200 {
		t.Errorf("web login with the account password on node B: %d", status)
	}
	if status := newJMAPClient(t, "http://"+b.web).post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, anna, phone.Secret), nil); status != 401 {
		t.Errorf("web login with an app password: %d", status)
	}

	// --- 4. Scope: an app password for IMAP only. ---
	if status := web.post("POST", "/auth/app-passwords", fmt.Sprintf(`{"name":"Reader","scopes":["imap"],"password":%q}`, password), &reader); status != 201 {
		t.Fatalf("creating the second app password: %d", status)
	}
	if code := smtpAuth(t, b, anna, reader.Secret); code != 535 {
		t.Errorf("SMTP AUTH with an app password for IMAP only: %d", code)
	}

	// --- 5. Revoke: the IDLE session on the other node ends. ---
	idle, got := imapLogin(t, "idle", b.imapTLS, anna, phone.Secret)
	if !strings.HasPrefix(got, "OK") {
		t.Fatalf("login for IDLE: %s", got)
	}
	idle.ok("SELECT INBOX")
	idle.write("idle1 IDLE\r\n")
	idle.want("+ idling", 10*time.Second)
	waiting, _ := imapLogin(t, "waiting", a.imapTLS, anna, phone.Secret) // sits between commands
	keeps, got := imapLogin(t, "keeps", b.imapTLS, anna, reader.Secret)  // another app password
	if !strings.HasPrefix(got, "OK") {
		t.Fatalf("login with the second app password: %s", got)
	}
	keeps.ok("SELECT INBOX")

	// The list shows the use.
	var list struct {
		List []struct {
			ID, Name, UsedIP string
			UsedAt           *time.Time
		}
	}
	if status := web.post("GET", "/auth/app-passwords", "", &list); status != 200 || len(list.List) != 2 {
		t.Fatalf("list: %d %+v", status, list)
	}
	for _, p := range list.List {
		if p.UsedAt == nil || p.UsedIP != "127.0.0.1" {
			t.Errorf("app password %s: last use %v from %q", p.Name, p.UsedAt, p.UsedIP)
		}
	}

	smtpPhone, authCode := smtpOpen(t, b, anna, phone.Secret)
	if authCode != 235 {
		t.Fatalf("SMTP AUTH with the app password: %d", authCode)
	}
	start = time.Now()
	if status := web.post("DELETE", "/auth/app-passwords/"+phone.ID, "", nil); status != 204 {
		t.Fatalf("revoke: %d", status)
	}
	smtpPhone.ended(t, "the revoke")
	idle.want(bye, 10*time.Second)
	t.Logf("revoke on node A -> BYE in IDLE on node B after %v", time.Since(start).Round(time.Millisecond))
	waiting.want(bye, 10*time.Second)
	// The connections are closed, not only told.
	for name, c := range map[string]*imapClient{"idle": idle, "waiting": waiting} {
		c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.br.ReadString('\n'); err != io.EOF {
			t.Errorf("session %q after BYE: %v, want the connection closed", name, err)
		}
	}
	// The other app password's session goes on, and a new login with the
	// revoked one is refused on both nodes.
	keeps.ok("NOOP")
	for name, n := range map[string]authNode{"A": a, "B": b} {
		if _, got := imapLogin(t, "revoked"+name, n.imapTLS, anna, phone.Secret); !strings.HasPrefix(got, "NO [AUTHENTICATIONFAILED]") {
			t.Errorf("node %s: IMAP with the revoked app password: %s", name, got)
		}
	}

	// --- 6. TOTP: set up on node A. ---
	var enrol struct {
		Secret, URI string
		QR          []string
	}
	if status := web.post("POST", "/auth/totp/enrol", fmt.Sprintf(`{"password":%q}`, password), &enrol); status != 200 || len(enrol.QR) < 21 {
		t.Fatalf("enrol: %d %+v", status, enrol)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrol.Secret)
	if err != nil || !strings.Contains(enrol.URI, "secret="+enrol.Secret) || !strings.HasPrefix(enrol.URI, "otpauth://totp/"+a.host+":") {
		t.Fatalf("enrolment: %v %s", err, enrol.URI)
	}
	// Codes of the steps to come: each login below spends one, and none
	// may be older than the last one spent. The server takes the current
	// step and one to either side, so the test waits for a step when it
	// has run ahead of the clock.
	step := totp.Step(time.Now())
	next := func() string {
		step++
		if wait := time.Until(time.Unix((step-1)*totp.Period, 0)); wait > 0 {
			t.Logf("waiting %v for time step %d", wait.Round(time.Second), step)
			time.Sleep(wait)
		}
		return totp.Code(secret, step)
	}
	var codes struct{ RecoveryCodes []string }
	if status := web.post("POST", "/auth/totp/confirm", `{"code":"000000"}`, nil); status != 401 {
		t.Fatalf("confirm with a wrong code: %d", status)
	}
	if status := web.post("POST", "/auth/totp/confirm", fmt.Sprintf(`{"code":%q}`, totp.Code(secret, step)), &codes); status != 200 || len(codes.RecoveryCodes) != 10 {
		t.Fatalf("confirm: %d %+v", status, codes)
	}

	// --- 7. The login in two steps, on node B. ---
	login := func(n authNode) *jmapClient {
		t.Helper()
		c := newJMAPClient(t, "http://"+n.web)
		var first struct{ Step, AccountID string }
		if status := c.post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, anna, password), &first); status != 200 || first.Step != "totp" || first.AccountID != "" {
			t.Fatalf("step one: %d %+v", status, first)
		}
		if status := c.post("POST", "/jmap/api", `{"using":[],"methodCalls":[]}`, nil); status != 401 {
			t.Fatalf("JMAP after the password alone: %d", status)
		}
		return c
	}
	// A wrong password says the same as for an account without TOTP.
	var wrong struct{ Error, Step string }
	if status := newJMAPClient(t, "http://"+b.web).post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"wrong"}`, anna), &wrong); status != 401 || wrong.Step != "" || wrong.Error != "wrong username or password" {
		t.Errorf("a wrong password for an account with TOTP: %d %+v", status, wrong)
	}
	c := login(b)
	if status := c.post("POST", "/auth/login/totp", `{"code":"000000"}`, nil); status != 401 {
		t.Errorf("a wrong code: %d", status)
	}
	code := next()
	if status := c.post("POST", "/auth/login/totp", fmt.Sprintf(`{"code":%q}`, code), nil); status != 200 {
		t.Fatalf("the right code on node B: %d", status)
	}
	if status := c.post("POST", "/jmap/api", `{"using":[],"methodCalls":[]}`, nil); status != 200 {
		t.Errorf("JMAP after the second step: %d", status)
	}
	// The same code again, in a new login on node A: RFC 6238 5.2, a code
	// is good for one login.
	replay := login(a)
	if status := replay.post("POST", "/auth/login/totp", fmt.Sprintf(`{"code":%q}`, code), nil); status != 401 {
		t.Errorf("the code a second time, on the other node: %d", status)
	}
	// A recovery code instead, once.
	if status := replay.post("POST", "/auth/login/totp", fmt.Sprintf(`{"recoveryCode":%q}`, codes.RecoveryCodes[0]), nil); status != 200 {
		t.Errorf("a recovery code on node A: %d", status)
	}
	again := login(b)
	if status := again.post("POST", "/auth/login/totp", fmt.Sprintf(`{"recoveryCode":%q}`, codes.RecoveryCodes[0]), nil); status != 401 {
		t.Errorf("the recovery code a second time, on node B: %d", status)
	}

	// --- 8. Wrong codes are counted for both nodes together. ---
	// A success forgets the wrong codes before it, so one is counted now:
	// the spent recovery code. Four more, in turn on the two nodes, reach
	// the limit of five, of which neither node saw more than three.
	onA := login(a)
	for i, cl := range []*jmapClient{again, onA, again, onA} {
		if status := cl.post("POST", "/auth/login/totp", fmt.Sprintf(`{"code":"%d%d%d%d%d%d"}`, i, i, i, i, i, i), nil); status != 401 {
			t.Errorf("wrong code %d: %d", i+2, status)
		}
	}
	for name, cl := range map[string]*jmapClient{"B": again, "A": onA} {
		resp, _ := cl.do("POST", "/auth/login/totp", fmt.Sprintf(`{"code":%q}`, totp.Code(secret, totp.Step(time.Now())+1)), nil)
		if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
			t.Errorf("node %s: the right code after five wrong ones in the cluster: %d", name, resp.StatusCode)
		}
	}

	// --- 9. The administrator. ---
	sessions := a.ok("", "user", "sessions", anna)
	a.expect(sessions, `password\+totp`, `password\+recovery`, `127\.0\.0\.1`)
	// An event stream of the session on node B, open while the session is
	// ended from the command line.
	stream := *c.hc
	stream.Timeout = 0
	ectx, stopStream := context.WithCancel(context.Background())
	defer stopStream()
	req, _ := http.NewRequestWithContext(ectx, "GET", "http://"+b.web+"/jmap/eventsource/?types=*&closeafter=no&ping=0", nil)
	resp, err := stream.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("event source: %v %v", resp, err)
	}
	defer resp.Body.Close()
	ended := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(ended) }()

	start = time.Now()
	a.expect(a.ok("", "user", "session-revoke", anna, "all"), `ended \d+ sessions of `+anna)
	select {
	case <-ended:
		t.Logf("session-revoke from the command line -> the event stream on node B ended after %v", time.Since(start).Round(time.Millisecond))
	case <-time.After(10 * time.Second):
		t.Error("the event stream of a revoked session is still open after 10 seconds")
	}
	for name, cl := range map[string]*jmapClient{"node A": web, "node B": c} {
		if status := cl.post("POST", "/jmap/api", `{"using":[],"methodCalls":[]}`, nil); status != 401 {
			t.Errorf("%s: a session after session-revoke all: %d", name, status)
		}
	}
	a.expect(a.ok("", "user", "sessions", anna), `has no sessions`)

	a.expect(a.ok("", "user", "app-passwords", anna), reader.ID+`\s+Reader\s+imap\s+created .* last used .* from 127\.0\.0\.1`)
	a.expect(a.ok("", "user", "app-password-revoke", anna, reader.ID), `revoked app password `+reader.ID)
	keeps.want(bye, 10*time.Second)
	if _, err := a.do("", "user", "app-password-revoke", anna, reader.ID); err == nil {
		t.Error("revoking an app password twice worked")
	}
	a.expect(a.ok("", "user", "app-passwords", anna), `has no app passwords`)

	a.expect(a.ok("", "user", "totp-disable", anna), `turned TOTP off for `+anna)
	a.expect(a.ok("", "user", "totp-disable", anna), `has no TOTP`)
	a.expect(a.ok("a new password\n", "user", "password", anna), `set a new password for `+anna)
	if _, err := a.do("short\n", "user", "password", anna); err == nil {
		t.Error("a password of five characters was accepted")
	}
	fresh := newJMAPClient(t, "http://"+b.web)
	if status := fresh.post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, anna, password), nil); status != 401 {
		t.Errorf("the old password after a reset: %d", status)
	}
	var who struct{ AccountID, Step string }
	if status := fresh.post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"a new password"}`, anna), &who); status != 200 || who.AccountID == "" || who.Step != "" {
		t.Errorf("the new password, TOTP off: %d %+v", status, who)
	}
	// No app password and no TOTP left: the password is back for IMAP.
	if _, got := imapLogin(t, "back", a.imapTLS, anna, "a new password"); !strings.HasPrefix(got, "OK") {
		t.Errorf("IMAP with the account password after the last app password went: %s", got)
	}

	// The log has it all, oldest first.
	audit := a.ok("", "audit", anna, "200")
	t.Logf("robinmail audit %s:\n%s", anna, audit)
	a.expect(audit,
		`login-failed\s+`+anna+`\s+127\.0\.0\.1\s+web: wrong password for a change of settings`,
		`app-password-created\s+`+anna+`\s+127\.0\.0\.1\s+Phone \(for all protocols\)`,
		`app-password-created\s+`+anna+`\s+127\.0\.0\.1\s+Reader \(for imap\)`,
		`login-failed\s+`+anna+`\s+127\.0\.0\.1\s+imap: the account password is not accepted here`,
		`login\s+`+anna+`\s+127\.0\.0\.1\s+imap: app password "Phone"`,
		`login-failed\s+`+anna+`\s+127\.0\.0\.1\s+smtp: app password "Reader" is not for this protocol`,
		`app-password-revoked\s+`+anna+`\s+127\.0\.0\.1\s+Phone`,
		`totp-enabled`,
		`login\s+`+anna+`\s+127\.0\.0\.1\s+web: password\+totp`,
		`code-failed\s+`+anna+`\s+127\.0\.0\.1\s+code: used before`,
		`recovery-code-used`,
		`session-revoked\s+`+anna+`\s+-\s+all sessions, by an administrator`,
		`app-password-revoked\s+`+anna+`\s+-\s+Reader, by an administrator`,
		`totp-disabled\s+`+anna+`\s+-\s+by an administrator`,
		`password-changed\s+`+anna+`\s+-\s+reset, all sessions ended, by an administrator`,
	)
	// No secret is in it.
	for _, s := range []string{phone.Secret, reader.Secret, enrol.Secret, codes.RecoveryCodes[0], password} {
		if strings.Contains(audit, s) {
			t.Errorf("the log of auth events holds a secret")
		}
	}
	if out, err := a.do("", "audit", "nobody@"+a.domain); err == nil {
		t.Errorf("audit of an unknown user: %s", out)
	}
	a.expect(a.ok("", "audit", "all", "3"), `\d{4}-\d\d-\d\d \d\d:\d\d:\d\d  \S+`)
}

// auth.require_app_passwords: the account password never opens a mail
// protocol, also for an account that has no app password yet.
func TestRequireAppPasswords(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	a, _ := twoAuthNodes(t, base, "require", `"auth":{"require_app_passwords":true},`)
	anna := a.user("anna")
	if _, got := imapLogin(t, "pw", a.imapTLS, anna, "correct horse"); !strings.HasPrefix(got, "NO [AUTHENTICATIONFAILED]") {
		t.Errorf("IMAP with the account password: %s", got)
	}
	if code := smtpAuth(t, a, anna, "correct horse"); code != 535 {
		t.Errorf("SMTP AUTH with the account password: %d", code)
	}
	web := newJMAPClient(t, "http://"+a.web)
	if status := web.post("POST", "/auth/login", fmt.Sprintf(`{"username":%q,"password":"correct horse"}`, anna), nil); status != 200 {
		t.Fatalf("web login: %d", status)
	}
	var app struct{ Secret string }
	web.post("POST", "/auth/app-passwords", `{"name":"Client","password":"correct horse"}`, &app)
	if _, got := imapLogin(t, "app", a.imapTLS, anna, app.Secret); !strings.HasPrefix(got, "OK") {
		t.Errorf("IMAP with an app password: %s", got)
	}
}

// TestOIDCLoginEndToEnd logs a browser in through an OpenID provider: the
// fake one of internal/oidc/oidctest, over HTTP, with the server as
// "serve" runs it and its config naming the provider. The session the
// callback on node A makes is good on node B.
func TestOIDCLoginEndToEnd(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	op := oidctest.New()
	defer op.Close()
	a, b := twoAuthNodes(t, base, "oidc", fmt.Sprintf(`"auth":{"oidc":[{"name":"test","label":"Test provider","issuer":%q,
		"client_id":%q,"client_secret":%q}]},`, op.Issuer, op.ClientID, op.ClientSecret))
	anna := a.user("anna")
	op.Lock()
	op.User = map[string]any{"sub": "anna-at-the-provider", "email": anna, "email_verified": true}
	op.Unlock()

	browser := newJMAPClient(t, "http://"+a.web)
	var cfg struct {
		Providers []struct{ Name, Label string }
	}
	if status := browser.post("GET", "/auth/config", "", &cfg); status != 200 || len(cfg.Providers) != 1 || cfg.Providers[0].Label != "Test provider" {
		t.Fatalf("config: %d %+v", status, cfg)
	}
	// The browser follows the redirects: to the provider, and back to the
	// callback, which answers with the page that goes on to the webmail.
	resp, page := browser.do("GET", "/auth/oidc/test/start", "", nil)
	if resp.StatusCode != 200 || resp.Request.URL.Path != "/auth/oidc/test/callback" || !strings.Contains(string(page), `content="0;url=/"`) {
		t.Fatalf("the login ended at %s with %d: %s", resp.Request.URL, resp.StatusCode, page)
	}
	op.Lock()
	sent := op.LastAuth
	op.Unlock()
	if sent.Get("code_challenge_method") != "S256" || sent.Get("redirect_uri") != "http://"+a.web+"/auth/oidc/test/callback" {
		t.Errorf("authorization request: %v", sent)
	}
	var who struct{ Username, Method string }
	if status := browser.post("GET", "http://"+b.web+"/auth/session", "", &who); status != 200 || who.Username != anna || who.Method != "oidc:test" {
		t.Fatalf("the session on node B: %d %+v", status, who)
	}
	if status := browser.post("POST", "http://"+b.web+"/jmap/api", `{"using":[],"methodCalls":[]}`, nil); status != 200 {
		t.Errorf("JMAP on node B with the session of the provider's login: %d", status)
	}

	// Somebody the provider knows and robinmail does not gets no session.
	op.Lock()
	op.User = map[string]any{"sub": "stranger", "email": "stranger@" + a.domain, "email_verified": true}
	op.Unlock()
	stranger := newJMAPClient(t, "http://"+a.web)
	if _, page := stranger.do("GET", "/auth/oidc/test/start", "", nil); !strings.Contains(string(page), "/login?error=oidc_no_account") {
		t.Errorf("a login without an account: %s", page)
	}
	if status := stranger.post("GET", "/auth/session", "", nil); status != 401 {
		t.Errorf("session of the stranger: %d", status)
	}
	// And the address of an account under another subject does not either.
	op.Lock()
	op.User = map[string]any{"sub": "somebody-else", "email": anna, "email_verified": true}
	op.Unlock()
	if _, page := newJMAPClient(t, "http://"+a.web).do("GET", "/auth/oidc/test/start", "", nil); !strings.Contains(string(page), "/login?error=oidc_no_account") {
		t.Errorf("the address under another subject: %s", page)
	}
	audit := a.ok("", "audit", "all", "20")
	a.expect(audit, `login\s+`+anna+`\s+127\.0\.0\.1\s+web: oidc:test`, `oidc-refused\s+stranger@`, `oidc-refused\s+`+anna+`\s+127\.0\.0\.1\s+test: the account is linked to another subject`)
	a.expect(a.ok("", "user", "oidc-unlink", anna), `forgot 1 provider subjects`)
	if _, page := newJMAPClient(t, "http://"+a.web).do("GET", "/auth/oidc/test/start", "", nil); !strings.Contains(string(page), `content="0;url=/"`) {
		t.Errorf("after oidc-unlink the new subject should be taken: %s", page)
	}
}
