package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/config"
	"github.com/robinmail/robinmail/internal/tlsconf"
)

// The development instance of contrib/dev/seed.sh is one a mail client
// can use: its config has TLS, IMAP and submission listeners on loopback
// next to the plain webmail, and the certificate seed.sh makes for it
// (contrib/dev/cert.sh) is good for 127.0.0.1, which is what the comments
// in seed.sh tell a user to type into Thunderbird.
func TestDevConfigAndCertificate(t *testing.T) {
	src, err := os.ReadFile("../../contrib/dev/robinmail.dev.json")
	if err != nil {
		t.Fatal(err)
	}
	// As seed.sh lays it out: the config and the certificate in one
	// directory, the paths in the config relative to it.
	dir := t.TempDir()
	path := filepath.Join(dir, "robinmail.json")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the dev config does not load: %v", err)
	}
	for name, got := range map[string][2]string{
		"web http_listen":       {cfg.Web.HTTPListen, "127.0.0.1:8080"},
		"web listen":            {cfg.Web.Listen, ""},
		"smtp listen":           {cfg.SMTP.Listen, "127.0.0.1:2525"},
		"imap listen_tls":       {cfg.IMAP.ListenTLS, "127.0.0.1:1993"},
		"imap listen":           {cfg.IMAP.Listen, "127.0.0.1:1143"},
		"submission listen_tls": {cfg.Submission.ListenTLS, "127.0.0.1:1465"},
		"submission listen":     {cfg.Submission.Listen, "127.0.0.1:1587"},
		"tls cert_file":         {cfg.TLS.CertFile, filepath.Join(dir, "tls.crt")},
		"tls key_file":          {cfg.TLS.KeyFile, filepath.Join(dir, "tls.key")},
		"outbound route":        {cfg.Outbound.Routes["*"], "127.0.0.1:1025"},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", name, got[0], got[1])
		}
	}
	// The dev account keeps its account password for mail clients until
	// it has an app password: nothing in the config takes that away.
	if cfg.Auth.RequireAppPasswords {
		t.Error("the dev config requires app passwords; seed.sh says a client logs in with the account password")
	}
	// Every listener is on loopback.
	for _, addr := range []string{cfg.Web.HTTPListen, cfg.SMTP.Listen, cfg.IMAP.ListenTLS, cfg.IMAP.Listen, cfg.Submission.ListenTLS, cfg.Submission.Listen} {
		host, _, err := net.SplitHostPort(addr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			t.Errorf("listener %q is not on a loopback address", addr)
		}
	}

	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl: the certificate of contrib/dev/cert.sh is not made here")
	}
	script, _ := filepath.Abs("../../contrib/dev/cert.sh")
	if out, err := exec.Command(script, dir).CombinedOutput(); err != nil {
		t.Fatalf("cert.sh: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(cfg.TLS.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	// A second run leaves the pair alone: the exception a mail client
	// made for the certificate stays good.
	if out, err := exec.Command(script, dir).CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("cert.sh a second time: %v %q", err, out)
	}
	if again, _ := os.ReadFile(cfg.TLS.CertFile); string(again) != string(raw) {
		t.Error("cert.sh replaced an existing certificate")
	}
	if info, err := os.Stat(cfg.TLS.KeyFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the key file: %v %v", info, err)
	}

	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("tls.crt is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	// Self-signed: it is its own, and only, authority.
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	for _, name := range []string{"127.0.0.1", "localhost", "mail.robinmail.test", "::1"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("the certificate is not accepted for %s: %v", name, err)
		}
	}
	for _, name := range []string{"192.0.2.1", "example.org", "robinmail.test"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err == nil {
			t.Errorf("the certificate is accepted for %s", name)
		}
	}
	if left := time.Until(cert.NotAfter); left < 5*365*24*time.Hour {
		t.Errorf("the certificate ends in %v", left)
	}

	// And the server takes the pair: a client that trusts the certificate
	// and dials 127.0.0.1, as a mail client does after its one exception,
	// completes the handshake.
	tc, err := tlsconf.New(cfg.TLS, cfg.Hostname, cfg.DataDir, nil)
	if err != nil {
		t.Fatalf("the server does not load the pair: %v", err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	// No ServerName: Go then checks the certificate against the address
	// dialed, 127.0.0.1.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", l.Addr().String(), &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("TLS to 127.0.0.1 with the dev certificate: %v", err)
	}
	conn.Close()
}
