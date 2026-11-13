// Package blob stores raw messages and uploads as immutable files on the
// node's disk, encrypted at rest.
//
// A blob is named by its account and the SHA-256 of its plaintext:
// "<account-id>/<sha256>". What is stored under that name is the encrypted
// file (crypt.go), sealed with a key of the account that is itself wrapped by
// the master key (keys.go). The Store interface only moves those encrypted
// files around and never sees a key, so copying a blob to another node
// (phase 4) is copying the file as it is.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrNotFound is returned by Get when no blob has the key.
var ErrNotFound = errors.New("blob not found")

// Store keeps encrypted blob files. Implementations are safe for concurrent
// use.
type Store interface {
	// Put stores what r yields under key and returns once it is durable.
	// Storing the same key again replaces the file.
	Put(ctx context.Context, key string, r io.Reader) error
	// Get opens the file. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the file. A missing one is not an error.
	Delete(ctx context.Context, key string) error
}

// Key is the key of a blob: "<account-id>/<sha256>", the hash in lowercase
// hex.
func Key(accountID, sha256hex string) string { return accountID + "/" + sha256hex }

// splitKey takes a key apart and checks both halves: an account id of 1 to
// 64 letters, digits, "-" or "_", and 64 lowercase hex digits. The local
// backend builds a path from them, so nothing else may get through.
func splitKey(key string) (account, sum string, err error) {
	account, sum, ok := strings.Cut(key, "/")
	if !ok || account == "" || len(account) > 64 || len(sum) != 64 {
		return "", "", fmt.Errorf("blob: invalid key %q", key)
	}
	for i := 0; i < len(account); i++ {
		c := account[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", "", fmt.Errorf("blob: invalid key %q", key)
		}
	}
	for i := 0; i < len(sum); i++ {
		c := sum[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", "", fmt.Errorf("blob: invalid key %q", key)
		}
	}
	return account, sum, nil
}
