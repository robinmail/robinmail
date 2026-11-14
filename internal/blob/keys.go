package blob

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// MasterKey wraps the account keys. Every node of a cluster has the same
// one, from a key file. It protects blobs on stolen disks and in backups:
// without the file the account keys in the database are useless. It does not
// protect against someone who controls a running server.
//
// A key file may hold several keys, one per line. The first is the current
// one: new account keys are wrapped under it. The others are still accepted
// for unwrapping, which is what lets the key be replaced while servers run
// ("robinmail key add", "key rotate", "key prune").
type MasterKey struct {
	keys []masterKey // the current one first
}

type masterKey struct {
	raw  []byte
	aead cipher.AEAD
	// prk is what other keys are derived from (Derive).
	prk []byte
}

// wrapVersion marks how an account key is wrapped: XChaCha20-Poly1305 under
// the master key, with a random nonce in front and the account id as
// associated data, in base64.
const wrapVersion = "1:"

var keyEncoding = base64.RawStdEncoding

// MaxMasterKeys is how many keys a key file may hold.
const MaxMasterKeys = 8

// NewMasterKey makes a master key from 32 raw bytes, and from older keys
// that are still accepted.
func NewMasterKey(raw []byte, older ...[]byte) (*MasterKey, error) {
	m := &MasterKey{}
	for _, r := range append([][]byte{raw}, older...) {
		aead, err := chacha20poly1305.NewX(r)
		if err != nil {
			return nil, errors.New("blob: a master key is 32 bytes")
		}
		prk, err := hkdf.Extract(sha256.New, r, []byte("robinmail master key"))
		if err != nil {
			return nil, err
		}
		k := masterKey{raw: append([]byte(nil), r...), aead: aead, prk: prk}
		for _, have := range m.keys {
			if have.fingerprint() == k.fingerprint() {
				return nil, errors.New("blob: the same master key twice")
			}
		}
		m.keys = append(m.keys, k)
	}
	if len(m.keys) > MaxMasterKeys {
		return nil, fmt.Errorf("blob: more than %d master keys", MaxMasterKeys)
	}
	return m, nil
}

func (k masterKey) derive(label string, n int) []byte {
	key, err := hkdf.Expand(sha256.New, k.prk, label, n)
	if err != nil {
		panic("blob: " + err.Error()) // only for an absurd n
	}
	return key
}

// fingerprint names a key without giving anything of it away: 16 hex
// digits derived from it.
func (k masterKey) fingerprint() string {
	return fmt.Sprintf("%x", k.derive("robinmail master key fingerprint", 8))
}

// Derive returns a key of n bytes for another purpose than wrapping account
// keys, such as the hash in rewritten sender addresses (internal/srs). The
// same master key and label give the same key on every node; different
// labels give unrelated keys (HKDF, RFC 5869). It is derived from the
// current key.
func (m *MasterKey) Derive(label string, n int) []byte { return m.keys[0].derive(label, n) }

// DeriveAll is Derive for every key of the file, the current one first:
// for checking what was made under a key that has since been replaced.
func (m *MasterKey) DeriveAll(label string, n int) [][]byte {
	out := make([][]byte, len(m.keys))
	for i, k := range m.keys {
		out[i] = k.derive(label, n)
	}
	return out
}

// Fingerprints names the keys, the current one first.
func (m *MasterKey) Fingerprints() []string {
	out := make([]string, len(m.keys))
	for i, k := range m.keys {
		out[i] = k.fingerprint()
	}
	return out
}

// With returns a master key with one more key, accepted and not current.
func (m *MasterKey) With(raw []byte) (*MasterKey, error) {
	older := [][]byte{}
	for _, k := range m.keys[1:] {
		older = append(older, k.raw)
	}
	return NewMasterKey(m.keys[0].raw, append(older, raw)...)
}

// Promote returns a master key whose current key is the one with the given
// fingerprint; the others stay accepted, in their order.
func (m *MasterKey) Promote(fingerprint string) (*MasterKey, error) {
	var first []byte
	var rest [][]byte
	for _, k := range m.keys {
		if k.fingerprint() == fingerprint {
			first = k.raw
		} else {
			rest = append(rest, k.raw)
		}
	}
	if first == nil {
		return nil, fmt.Errorf("blob: no master key %s", fingerprint)
	}
	return NewMasterKey(first, rest...)
}

// Only returns a master key with the keys of the given fingerprints, in
// the order they have now. The current key cannot be left out.
func (m *MasterKey) Only(fingerprints map[string]bool) (*MasterKey, error) {
	if !fingerprints[m.keys[0].fingerprint()] {
		return nil, errors.New("blob: the current master key cannot be dropped")
	}
	var rest [][]byte
	for _, k := range m.keys[1:] {
		if fingerprints[k.fingerprint()] {
			rest = append(rest, k.raw)
		}
	}
	return NewMasterKey(m.keys[0].raw, rest...)
}

func randomKey() ([]byte, error) {
	raw := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// NewRandomKey returns 32 random bytes for With.
func NewRandomKey() ([]byte, error) { return randomKey() }

func (m *MasterKey) fileText() string {
	var b strings.Builder
	for _, k := range m.keys {
		b.WriteString(keyEncoding.EncodeToString(k.raw) + "\n")
	}
	return b.String()
}

// GenerateMasterKeyFile writes a new random master key to path, readable by
// the owner only. It never overwrites: losing a master key loses all mail.
func GenerateMasterKeyFile(path string) error {
	raw, err := randomKey()
	if err != nil {
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

// WriteFile replaces the key file at path by the keys of m: written to a
// temporary file next to it, synced and renamed, so the file is the old
// one or the new one, also after a crash. The file must exist: this
// changes a key file, it does not make one.
func (m *MasterKey) WriteFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".master-key-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.WriteString(m.fileText()); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// LoadMasterKey reads a key file written by GenerateMasterKeyFile or
// WriteFile. It refuses a file that others than the owner can read.
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
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	var raws [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw, err := keyEncoding.DecodeString(line)
		if err != nil || len(raw) != chacha20poly1305.KeySize {
			return nil, fmt.Errorf("master key: %s does not hold a key", path)
		}
		raws = append(raws, raw)
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("master key: %s does not hold a key", path)
	}
	m, err := NewMasterKey(raws[0], raws[1:]...)
	if err != nil {
		return nil, fmt.Errorf("master key: %s: %w", path, err)
	}
	return m, nil
}

func wrapAD(account string) []byte { return []byte("robinmail account key\x00" + account) }

// NewAccountKey makes the key that encrypts an account's blobs and returns
// it together with its wrapped form, which is what the account row stores.
func (m *MasterKey) NewAccountKey(account string) (key []byte, wrapped string, err error) {
	key = make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, "", err
	}
	wrapped, err = m.keys[0].wrap(account, key)
	return key, wrapped, err
}

func (k masterKey) wrap(account string, key []byte) (string, error) {
	nonce := make([]byte, k.aead.NonceSize(), k.aead.NonceSize()+KeySize+k.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return wrapVersion + keyEncoding.EncodeToString(k.aead.Seal(nonce, nonce, key, wrapAD(account))), nil
}

// Unwrap recovers an account key. It fails for another account's key and
// under a master key that is not in the file.
func (m *MasterKey) Unwrap(account, wrapped string) ([]byte, error) {
	key, _, err := m.unwrap(account, wrapped)
	return key, err
}

// WrappedBy returns the fingerprint of the key an account key is wrapped
// under.
func (m *MasterKey) WrappedBy(account, wrapped string) (string, error) {
	_, i, err := m.unwrap(account, wrapped)
	if err != nil {
		return "", err
	}
	return m.keys[i].fingerprint(), nil
}

// Rewrap returns the account key wrapped under the current master key.
// changed is false when it already is.
func (m *MasterKey) Rewrap(account, wrapped string) (out string, changed bool, err error) {
	key, i, err := m.unwrap(account, wrapped)
	if err != nil {
		return "", false, err
	}
	if i == 0 {
		return wrapped, false, nil
	}
	out, err = m.keys[0].wrap(account, key)
	return out, err == nil, err
}

func (m *MasterKey) unwrap(account, wrapped string) ([]byte, int, error) {
	enc, ok := strings.CutPrefix(wrapped, wrapVersion)
	if !ok {
		return nil, 0, errors.New("blob: unknown account key format")
	}
	data, err := keyEncoding.DecodeString(enc)
	n := m.keys[0].aead.NonceSize()
	if err != nil || len(data) != n+KeySize+m.keys[0].aead.Overhead() {
		return nil, 0, errors.New("blob: malformed account key")
	}
	for i, k := range m.keys {
		if key, err := k.aead.Open(nil, data[:n], data[n:], wrapAD(account)); err == nil {
			return key, i, nil
		}
	}
	return nil, 0, errors.New("blob: account key does not unwrap: wrong master key?")
}
