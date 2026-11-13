// Package blob stores immutable files, such as raw messages, under a key.
// There are two backends: S3-compatible storage for real use and a local
// directory for development.
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

// Store is a blob backend. Implementations are safe for concurrent use.
type Store interface {
	// Put stores data under key and returns once it is durable. Storing
	// the same key again replaces the blob.
	Put(ctx context.Context, key string, data []byte) error
	// Get opens the blob. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes the blob. A missing blob is not an error.
	Delete(ctx context.Context, key string) error
}

// Key is the key of a message or upload: "<account-id>/<sha256>".
func Key(accountID, sha256hex string) string { return accountID + "/" + sha256hex }

const maxKeyLen = 512

// checkKey accepts keys such as "acct/0a1b" or "acme/account.json": parts of
// letters, digits, "-", "_" and "." separated by "/", no part starting with
// a dot. Both backends rely on it: the local one to stay inside its
// directory, the S3 one to build a path that needs no escaping surprises.
func checkKey(key string) error {
	if key == "" || len(key) > maxKeyLen {
		return fmt.Errorf("blob: invalid key %q", key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part[0] == '.' {
			return fmt.Errorf("blob: invalid key %q", key)
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return fmt.Errorf("blob: invalid key %q", key)
			}
		}
	}
	return nil
}
