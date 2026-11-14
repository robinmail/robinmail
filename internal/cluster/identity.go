package cluster

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Identity is the key and self-signed certificate a node shows the other
// nodes, as server and as client. Nobody signs it: what counts is its
// fingerprint in the node table.
type Identity struct {
	Certificate tls.Certificate
	// Fingerprint is the SHA-256 of the certificate (DER), in hex.
	Fingerprint string
}

// Fingerprint is how a certificate is named in the node table.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// NewIdentity makes a new key (ECDSA P-256) and certificate.
func NewIdentity() (*Identity, error) {
	certPEM, keyPEM, err := generate()
	if err != nil {
		return nil, err
	}
	return parseIdentity(certPEM, keyPEM)
}

func generate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	// The dates are checked by nobody: a peer compares the fingerprint and
	// nothing else. They are set wide so that tools show something sane.
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "robinmail node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(100, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func parseIdentity(certPEM, keyPEM []byte) (*Identity, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &Identity{Certificate: cert, Fingerprint: Fingerprint(cert.Certificate[0])}, nil
}

// LoadOrCreateIdentity reads the node's key and certificate from
// <dir>/node.key and <dir>/node.crt, and makes them first if there are
// none. They are kept on disk so that the server and the commands run on
// the same node are the same node to the others, also after a restart. A
// node that lost them makes new ones and registers again.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	keyPath, certPath := filepath.Join(dir, "node.key"), filepath.Join(dir, "node.crt")
	for try := 0; try < 2; try++ {
		keyPEM, kerr := os.ReadFile(keyPath)
		certPEM, cerr := os.ReadFile(certPath)
		if kerr == nil && cerr == nil {
			id, err := parseIdentity(certPEM, keyPEM)
			if err != nil {
				return nil, fmt.Errorf("node identity in %s: %w", dir, err)
			}
			return id, nil
		}
		if !errors.Is(kerr, os.ErrNotExist) && kerr != nil {
			return nil, fmt.Errorf("node identity: %w", kerr)
		}
		if !errors.Is(cerr, os.ErrNotExist) && cerr != nil {
			return nil, fmt.Errorf("node identity: %w", cerr)
		}
		certPEM, keyPEM, err := generate()
		if err != nil {
			return nil, fmt.Errorf("node identity: %w", err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("node identity: %w", err)
		}
		// Both files are written under another name and renamed, the key
		// last: a certificate without a key is replaced on the next try,
		// and two processes that start together end with one pair or
		// retry.
		if err := writeFile(certPath, certPEM); err != nil {
			return nil, fmt.Errorf("node identity: %w", err)
		}
		if err := writeFile(keyPath, keyPEM); err != nil {
			return nil, fmt.Errorf("node identity: %w", err)
		}
		// Read back what is there now: it may be another process's pair.
	}
	return nil, fmt.Errorf("node identity: %s and %s do not belong together; remove both", keyPath, certPath)
}

func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-node-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
