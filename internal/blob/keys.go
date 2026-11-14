package blob

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// MasterKey wraps the account keys. Every node of a cluster has the same
// one, from a key file. It protects blobs on stolen disks and in backups:
// without the file the account keys in the database are useless. It does not
// protect against someone who controls a running server.
type MasterKey struct {
	aead cipher.AEAD
	// prk is what other keys are derived from (Derive).
	prk []byte
}

// wrapVersion marks how an account key is wrapped: XChaCha20-Poly1305 under
// the master key, with a random nonce in front and the account id as
// associated data, in base64.
const wrapVersion = "1:"

var keyEncoding = base64.RawStdEncoding

// NewMasterKey makes a master key from 32 raw bytes.
func NewMasterKey(raw []byte) (*MasterKey, error) {
	aead, err := chacha20poly1305.NewX(raw)
	if err != nil {
		return nil, errors.New("blob: a master key is 32 bytes")
	}
	prk, err := hkdf.Extract(sha256.New, raw, []byte("robinmail master key"))
	if err != nil {
		return nil, err
	}
	return &MasterKey{aead: aead, prk: prk}, nil
}

// Derive returns a key of n bytes for another purpose than wrapping account
// keys, such as the hash in rewritten sender addresses (internal/srs). The
// same master key and label give the same key on every node; different
// labels give unrelated keys (HKDF, RFC 5869).
func (m *MasterKey) Derive(label string, n int) []byte {
	key, err := hkdf.Expand(sha256.New, m.prk, label, n)
	if err != nil {
		panic("blob: " + err.Error()) // only for an absurd n
	}
	return key
}

// GenerateMasterKeyFile writes a new random master key to path, readable by
// the owner only. It never overwrites: losing a master key loses all mail.
func GenerateMasterKeyFile(path string) error {
	raw := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(keyEncoding.EncodeToString(raw) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LoadMasterKey reads a key file written by GenerateMasterKeyFile. It
// refuses a file that others than the owner can read.
func LoadMasterKey(path string) (*MasterKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("master key: %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("master key: %s has mode %04o, want 0600", path, perm)
	}
	data := make([]byte, 256)
	n, _ := f.Read(data)
	raw, err := keyEncoding.DecodeString(strings.TrimSpace(string(data[:n])))
	if err != nil || len(raw) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("master key: %s does not hold a key", path)
	}
	return NewMasterKey(raw)
}

func wrapAD(account string) []byte { return []byte("robinmail account key\x00" + account) }

// NewAccountKey makes the key that encrypts an account's blobs and returns
// it together with its wrapped form, which is what the account row stores.
func (m *MasterKey) NewAccountKey(account string) (key []byte, wrapped string, err error) {
	key = make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, "", err
	}
	nonce := make([]byte, m.aead.NonceSize(), m.aead.NonceSize()+KeySize+m.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", err
	}
	return key, wrapVersion + keyEncoding.EncodeToString(m.aead.Seal(nonce, nonce, key, wrapAD(account))), nil
}

// Unwrap recovers an account key. It fails for another account's key and
// under another master key.
func (m *MasterKey) Unwrap(account, wrapped string) ([]byte, error) {
	enc, ok := strings.CutPrefix(wrapped, wrapVersion)
	if !ok {
		return nil, errors.New("blob: unknown account key format")
	}
	data, err := keyEncoding.DecodeString(enc)
	if err != nil || len(data) != m.aead.NonceSize()+KeySize+m.aead.Overhead() {
		return nil, errors.New("blob: malformed account key")
	}
	key, err := m.aead.Open(nil, data[:m.aead.NonceSize()], data[m.aead.NonceSize():], wrapAD(account))
	if err != nil {
		return nil, errors.New("blob: account key does not unwrap: wrong master key?")
	}
	return key, nil
}
