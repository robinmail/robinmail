// Package tlsconf builds the TLS configuration shared by every listener.
package tlsconf

import (
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/mikepage/robinmail/internal/config"
)

// ErrNotConfigured means the config has neither certificate files nor ACME.
var ErrNotConfigured = errors.New("tls: no certificate configured")

// New returns a TLS config that serves either an ACME-managed certificate for
// hostname or the certificate files from the config.
//
// With ACME, certificates are validated over TLS-ALPN-01, so one listener
// using this config has to be reachable on port 443.
func New(c config.TLS, hostname, dataDir string) (*tls.Config, error) {
	if c.ACME {
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(hostname),
			Cache:      autocert.DirCache(filepath.Join(dataDir, "acme")),
			Email:      c.ACMEEmail,
		}
		tc := m.TLSConfig()
		tc.MinVersion = tls.VersionTLS12
		return tc, nil
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, ErrNotConfigured
	}
	r := &reloader{certFile: c.CertFile, keyFile: c.KeyFile}
	if _, err := r.get(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return r.get()
		},
	}, nil
}

// reloader serves a certificate from disk and picks up a renewed one when the
// file's modification time changes, without a restart.
type reloader struct {
	certFile, keyFile string

	mu   sync.Mutex
	mod  time.Time
	cert *tls.Certificate
}

func (r *reloader) get() (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fi, err := os.Stat(r.certFile)
	if err == nil && (r.cert == nil || !fi.ModTime().Equal(r.mod)) {
		var c tls.Certificate
		if c, err = tls.LoadX509KeyPair(r.certFile, r.keyFile); err == nil {
			r.cert, r.mod = &c, fi.ModTime()
		}
	}
	// A failed reload (for example a renewal caught halfway) keeps the old
	// certificate in service and is retried on the next handshake.
	if r.cert != nil {
		return r.cert, nil
	}
	return nil, err
}
