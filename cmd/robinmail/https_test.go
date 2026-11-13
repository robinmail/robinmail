package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSigned writes a certificate for the host and its key into dir.
func selfSigned(t *testing.T, dir, host string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// TestServeHTTPS runs the web side the way production does: over TLS with
// the certificate from the config, under the configured hostname. The
// session cookie then is Secure with the __Host- prefix, HSTS is sent, and
// the connection is HTTP/2.
func TestServeHTTPS(t *testing.T) {
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	const host = "mail.example.test" // the hostname of newCLI's config
	certFile, keyFile, pool := selfSigned(t, t.TempDir(), host)
	c := newCLI(t, base, fmt.Sprintf(`"smtp":{"listen":"127.0.0.1:0"},"web":{"listen":"127.0.0.1:0"},"tls":{"cert_file":%q,"key_file":%q},`,
		certFile, keyFile))
	domain := "tls-" + randomHex() + ".test"
	user := "mike@" + domain
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)
	c.ok("correct horse\n", "user", "add", user)
	srv := startServe(t, c)
	if srv.web == "" {
		t.Fatal("serve did not report a web listener")
	}
	_, port, _ := net.SplitHostPort(srv.web)

	// The client asks for https://mail.example.test:<port> and is sent to
	// the listener on 127.0.0.1.
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool},
		ForceAttemptHTTP2: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, srv.web)
		},
	}}
	url := "https://" + host + ":" + port

	resp, err := hc.Get(url + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.ProtoMajor != 2 || !strings.Contains(string(body), "<title>robinmail</title>") ||
		resp.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Errorf("GET /: %d %s, HSTS %q", resp.StatusCode, resp.Proto, resp.Header.Get("Strict-Transport-Security"))
	}

	resp, err = hc.Post(url+"/auth/login", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":"correct horse"}`, user)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var session *http.Cookie
	for _, ck := range resp.Cookies() {
		session = ck
	}
	if resp.StatusCode != 200 || session == nil || session.Name != "__Host-robinmail" || !session.Secure || !session.HttpOnly ||
		session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("login: %d, cookie %+v", resp.StatusCode, session)
	}
	req, _ := http.NewRequest("GET", url+"/.well-known/jmap", nil)
	req.AddCookie(session)
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"apiUrl":"`+url+`/jmap/api"`) {
		t.Errorf("session over TLS: %d %s", resp.StatusCode, body)
	}

	// Under another name the same listener answers 421: the certificate
	// check is off here to get that far.
	insecure := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err = insecure.Get("https://" + srv.web + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("by IP address: %d", resp.StatusCode)
	}
	// SMTP offers STARTTLS with the same certificate.
	s := dialSMTP(t, srv.addr)
	if ehlo := s.cmd("EHLO client.sender.example", 250); !strings.Contains(ehlo, "STARTTLS") {
		t.Errorf("no STARTTLS: %s", ehlo)
	}
	s.cmd("QUIT", 221)
	if err := srv.stop(); err != nil {
		t.Errorf("serve returned %v", err)
	}
}
