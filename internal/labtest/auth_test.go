package labtest

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAppPasswordInMailClients is the exit of phase 7 with programs that
// are not ours: mbsync and OfflineIMAP, each as a mail client of a user
// of its own.
//
//  1. While the account has no app password, the client logs in with the
//     account password, as before phase 7.
//  2. The user makes an app password in the webmail (here: the same HTTP
//     requests). From then on the client is refused with the account
//     password and syncs with the app password.
//  3. The app password is revoked: the client is refused again.
//
// What each client says when it is refused is logged: it is what a user
// will see.
func TestAppPasswordInMailClients(t *testing.T) {
	lab(t)
	for _, c := range syncClients {
		t.Run(c.name, func(t *testing.T) {
			user := fmt.Sprintf("app-%s-%d@%s", c.name, time.Now().UnixNano()%1_000_000_000, robinDomain)
			if out, err := execIn("robinmail", "", []byte(password()+"\n"), "robinmail", "-config", "/lab/robinmail.json", "user", "add", user); err != nil {
				t.Fatalf("user add: %v\n%s", err, out)
			}
			token := newToken()
			msg := crlf(rawMessage(token, "sender@"+robinDomain, user, nil, "A message for the mail client of "+user+"."))
			if out, err := execIn("robinmail", "", msg, "robinmail", "-config", "/lab/robinmail.json", "deliver", user); err != nil {
				t.Fatalf("deliver: %v\n%s", err, out)
			}

			// The client's config, for this user and a given secret.
			config := strings.ReplaceAll(c.config, c.user, user)
			run := func(secret string) (string, bool) {
				t.Helper()
				sh(t, []byte(strings.ReplaceAll(config, "PASSWORD", secret)),
					"rm -rf "+c.root+" /root/.mbsync /root/.offlineimap /root/.local/share/isync /root/.local/state/isync; mkdir -p "+c.root+
						"; cat > "+c.file+"; chmod 600 "+c.file)
				out, err := execIn(clients, "", nil, c.run...)
				// OfflineIMAP reports a refused login and still exits 0.
				refused := err != nil || strings.Contains(out, "AUTHENTICATIONFAILED") || strings.Contains(out, "ERROR")
				got := strings.Contains(sh(t, nil, "grep -rl "+token+" "+c.root+" 2>/dev/null | head -1; true"), c.root)
				if !refused && !got {
					t.Fatalf("%s ran without an error and did not fetch the message:\n%s", c.name, clip(out, 2000))
				}
				return out, !refused && got
			}
			said := func(out string) string {
				for _, line := range strings.Split(out, "\n") {
					if strings.Contains(line, "AUTHENTICATIONFAILED") || strings.Contains(line, "wrong name or password") {
						return strings.TrimSpace(line)
					}
				}
				return clip(strings.TrimSpace(out), 300)
			}

			// 1. No app password yet: the account password is the way in.
			if out, ok := run(password()); !ok {
				t.Fatalf("%s with the account password, before any app password:\n%s", c.name, clip(out, 2000))
			}

			// 2. An app password, made as the webmail makes it.
			web := newWeb(t, user)
			var app struct{ ID, Secret string }
			if status, data := web.do(t, "POST", "/auth/app-passwords", map[string]any{"name": c.name, "scopes": []string{"imap"}, "password": password()}); status != 201 {
				t.Fatalf("creating the app password: %d %s", status, data)
			} else if err := json.Unmarshal(data, &app); err != nil || app.Secret == "" {
				t.Fatalf("the answer: %s", data)
			}
			out, ok := run(password())
			if ok {
				t.Fatalf("%s still syncs with the account password once the account has an app password", c.name)
			}
			t.Logf("%s with the account password: %s", c.name, said(out))
			if !strings.Contains(out, "AUTHENTICATIONFAILED") {
				t.Errorf("%s was refused, but not by the login:\n%s", c.name, clip(out, 2000))
			}
			if out, ok := run(app.Secret); !ok {
				t.Fatalf("%s with the app password:\n%s", c.name, clip(out, 2000))
			}
			t.Logf("%s with the app password: synced", c.name)
			// As a person might type it into a client: without the dashes.
			if out, ok := run(strings.ReplaceAll(app.Secret, "-", "")); !ok {
				t.Errorf("%s with the app password without dashes:\n%s", c.name, clip(out, 2000))
			}

			// The settings show the use.
			var list struct {
				List []struct {
					Name, UsedIP string
					UsedAt       *time.Time
				}
			}
			if status, data := web.do(t, "GET", "/auth/app-passwords", nil); status != 200 || json.Unmarshal(data, &list) != nil ||
				len(list.List) != 1 || list.List[0].UsedAt == nil || list.List[0].UsedIP == "" {
				t.Errorf("the list of app passwords after the sync: %d %s", status, data)
			}

			// 3. Revoked.
			if status, data := web.do(t, "DELETE", "/auth/app-passwords/"+app.ID, nil); status != 204 {
				t.Fatalf("revoke: %d %s", status, data)
			}
			out, ok = run(app.Secret)
			if ok {
				t.Fatalf("%s still syncs with a revoked app password", c.name)
			}
			t.Logf("%s with the revoked app password: %s", c.name, said(out))
			// With the last app password gone the account password is back
			// (the account has no TOTP either).
			if out, ok := run(password()); !ok {
				t.Errorf("%s with the account password after the last app password was revoked:\n%s", c.name, clip(out, 2000))
			}
		})
	}
}

// webSession is a browser's login to the lab's webmail.
type webSession struct {
	hc     *http.Client
	cookie string
}

func newWeb(t testing.TB, user string) *webSession {
	t.Helper()
	w := &webSession{hc: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: robinHost, RootCAs: labRoots(t)},
		DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}}}
	body, _ := json.Marshal(map[string]string{"username": user, "password": password()})
	req, _ := http.NewRequest("POST", "https://"+gateHTTPS+"/auth/login", bytes.NewReader(body))
	req.Host = robinHost
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.hc.Do(req)
	if err != nil {
		t.Fatalf("web login: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-robinmail" {
			w.cookie = c.Name + "=" + c.Value
		}
	}
	if resp.StatusCode != 200 || w.cookie == "" {
		t.Fatalf("web login of %s: %d %s", user, resp.StatusCode, data)
	}
	return w
}

func (w *webSession) do(t testing.TB, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if method != "GET" {
		data, _ := json.Marshal(body)
		if body == nil {
			data = []byte("{}")
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "https://"+gateHTTPS+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = robinHost
	req.Header.Set("Cookie", w.cookie)
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}
