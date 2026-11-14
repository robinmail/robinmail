package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/config"
)

func writeCert(t *testing.T, certFile, keyFile string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "mail.example.test"},
		DNSNames:     []string{"mail.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
}

func serialOf(t *testing.T, r *reloader) int64 {
	t.Helper()
	c, err := r.get()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.Int64()
}

func TestNotConfigured(t *testing.T) {
	if _, err := New(config.TLS{}, "mail.example.test", t.TempDir(), nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("New = %v, want ErrNotConfigured", err)
	}
}

func TestFilesAndReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeCert(t, certFile, keyFile, 1)

	tc, err := New(config.TLS{CertFile: certFile, KeyFile: keyFile}, "mail.example.test", dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tc.GetCertificate == nil {
		t.Fatal("GetCertificate not set")
	}

	r := &reloader{certFile: certFile, keyFile: keyFile}
	if got := serialOf(t, r); got != 1 {
		t.Fatalf("serial = %d, want 1", got)
	}

	// A renewal replaces both files; the new mtime triggers a reload.
	writeCert(t, certFile, keyFile, 2)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(certFile, later, later); err != nil {
		t.Fatal(err)
	}
	if got := serialOf(t, r); got != 2 {
		t.Errorf("serial after renewal = %d, want 2", got)
	}

	// A broken renewal keeps the last good certificate in service.
	if err := os.WriteFile(certFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := serialOf(t, r); got != 2 {
		t.Errorf("serial after broken renewal = %d, want 2", got)
	}
}
