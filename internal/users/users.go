// Package users hashes and verifies account passwords with Argon2id. The
// hashes are PHC strings and live in the account table (internal/store).
package users

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

var ErrWeakPassword = fmt.Errorf("password must be at least %d characters", MinPasswordLen)

const MinPasswordLen = 8

// Argon2id parameters for new hashes. Existing hashes carry their own.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Limits on the parameters of a stored hash, so a corrupt or planted row
// cannot make one login eat all memory.
const (
	maxArgonMemory = 1024 * 1024 // KiB
	maxArgonTime   = 16
	maxArgonKeyLen = 128
)

var b64 = base64.RawStdEncoding

// Hash returns the PHC string for a new password. It fails with
// ErrWeakPassword if the password is too short.
func Hash(password string) (string, error) {
	if len(password) < MinPasswordLen {
		return "", ErrWeakPassword
	}
	return hashPassword(password)
}

// Verify checks a password against the hash stored for an account. Pass an
// empty hash for an unknown account: that costs the same time as a wrong
// password, so response time does not reveal which addresses exist.
func Verify(hash, password string) bool {
	if hash == "" {
		verifyPassword(dummyHash(), password)
		return false
	}
	return verifyPassword(hash, password)
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword checks password against a PHC-format Argon2id hash.
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false
	}
	var version int
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	if iterations == 0 || threads == 0 || iterations > maxArgonTime || memory > maxArgonMemory {
		return false
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > maxArgonKeyLen {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var dummyHash = sync.OnceValue(func() string {
	h, err := hashPassword("robinmail-dummy-password")
	if err != nil {
		panic(err)
	}
	return h
})
