package main

import (
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What status and serve say about the connection to rqlite, case by case.
// Whoever reaches rqlite's HTTP port without a login owns every account;
// the one setup without a login that draws no warning is rqlite on this
// machine.
func TestRqliteLinkWords(t *testing.T) {
	for _, c := range []struct {
		link    rqliteLink
		says    string
		problem string
	}{
		{rqliteLink{encrypted: true, credentials: true, required: true}, "authenticated, encrypted (https), over the network", ""},
		{rqliteLink{credentials: true, required: true, local: true}, "authenticated, not encrypted (http), on this machine", ""},
		{rqliteLink{local: true}, "rqlite asks for no login, not encrypted (http), on this machine", ""},
		{rqliteLink{}, "rqlite asks for no login, not encrypted (http), over the network", "rqlite answers without a login|the connection is not encrypted"},
		{rqliteLink{credentials: true, required: true}, "authenticated, not encrypted (http), over the network", "the connection is not encrypted"},
		{rqliteLink{encrypted: true}, "rqlite asks for no login, encrypted (https), over the network", "rqlite answers without a login"},
		{rqliteLink{encrypted: true, credentials: true}, "credentials are sent, but rqlite asks for none: it answers anybody, encrypted (https), over the network", "rqlite answers without a login"},
		{rqliteLink{credentials: true, probeErr: errors.New("x")}, "could not ask whether rqlite wants a login; credentials are sent, not encrypted (http), over the network", "the connection is not encrypted"},
	} {
		if got := c.link.String(); got != c.says {
			t.Errorf("%+v says %q, want %q", c.link, got, c.says)
		}
		got := c.link.problem()
		if (c.problem == "") != (got == "") {
			t.Errorf("%+v: problem %q", c.link, got)
		}
		for _, part := range strings.Split(c.problem, "|") {
			if !strings.Contains(got, part) {
				t.Errorf("%+v: problem %q lacks %q", c.link, got, part)
			}
		}
	}
}

// The config's rqlite.auth_file and rqlite.ca_file reach the client: a
// stand-in for an rqlite with an auth file and HTTPS is only talked to
// with both, and the commands say what is wrong without the password.
func TestRqliteAuthFromConfig(t *testing.T) {
	const secret = "pw-of-this-test-0nly"
	var withAuth, without int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if u, p, _ := r.BasicAuth(); u != "robinmail" || p != secret {
			without++
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		withAuth++
		// Enough of rqlite for Init to fail on: the point here is what
		// reaches the server.
		io.WriteString(w, `{"results":[{"error":"stand-in"}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	auth := filepath.Join(dir, "rqlite.auth")
	os.WriteFile(auth, []byte("robinmail:"+secret+"\n"), 0o600)

	run := func(extra string) error {
		c := newCLI(t, srv.URL, "")
		c.ok("", "key", "generate")
		cfg := string(mustRead(t, c.cfg))
		// newCLI writes "rqlite":{"url":...}; the files go next to it.
		cfg = strings.Replace(cfg, `"rqlite":{"url":`, `"rqlite":{`+extra+`"url":`, 1)
		os.WriteFile(c.cfg, []byte(cfg), 0o600)
		_, err := c.do("", "domain", "list")
		return err
	}
	// Without the CA: the certificate is unknown, nothing is sent.
	err := run(`"auth_file":"` + auth + `",`)
	if err == nil || withAuth+without != 0 {
		t.Errorf("without ca_file: %v, %d requests", err, withAuth+without)
	}
	// With the CA and no credentials: refused, and the error says where
	// to look.
	err = run(`"ca_file":"` + ca + `",`)
	if err == nil || !strings.Contains(err.Error(), "rqlite.auth_file") || withAuth != 0 || without == 0 {
		t.Errorf("without auth_file: %v (%d with, %d without)", err, withAuth, without)
	}
	// With both, the request arrives with the login.
	err = run(`"ca_file":"` + ca + `","auth_file":"` + auth + `",`)
	if err == nil || !strings.Contains(err.Error(), "stand-in") || withAuth == 0 {
		t.Errorf("with both: %v (%d with)", err, withAuth)
	}
	// A wrong password, and a credentials file others can read.
	os.WriteFile(auth, []byte("robinmail:not-"+secret+"\n"), 0o600)
	err = run(`"ca_file":"` + ca + `","auth_file":"` + auth + `",`)
	if err == nil || !strings.Contains(err.Error(), "refused the credentials") || strings.Contains(err.Error(), secret) {
		t.Errorf("a wrong password: %v", err)
	}
	os.Chmod(auth, 0o644)
	err = run(`"ca_file":"` + ca + `","auth_file":"` + auth + `",`)
	if err == nil || !strings.Contains(err.Error(), "0600") || strings.Contains(err.Error(), secret) {
		t.Errorf("a credentials file others can read: %v", err)
	}
}
