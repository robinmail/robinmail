// Package users hashes and verifies account passwords with Argon2id. The
// hashes are PHC strings and live in the account table (internal/store).
package users

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

var ErrWeakPassword = fmt.Errorf("password must be at least %d characters", MinPasswordLen)

const MinPasswordLen = 8

// Argon2id parameters for new hashes: RFC 9106 section 4's "SECOND
// RECOMMENDED option", t=3 passes, p=4 lanes, m=2^16 KiB (64 MiB), a
// 128-bit salt and a 256-bit tag. Existing hashes carry their own.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Limits on the parameters of a stored hash, so a corrupt or planted row
// cannot make one login eat all memory. The lower bounds are RFC 9106 3.1's
// (a tag of at least 4 bytes, memory of at least 8 KiB per lane) and the
// PHC string format's (a salt of at least 8 bytes for Argon2).
const (
	maxArgonMemory = 1024 * 1024 // KiB
	maxArgonTime   = 16
	maxArgonKeyLen = 128
	minArgonKeyLen = 4
	minArgonSalt   = 8
	maxArgonSalt   = 64
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

// verifyPassword checks password against an Argon2id hash in the PHC string
// format:
//
//	$argon2id$v=19$m=<KiB>,t=<passes>,p=<lanes>$<salt>$<tag>
//
// It accepts exactly what the format defines and what the reference
// implementation writes: the parameters in this order, as decimal numbers
// without sign or leading zeros, and salt and tag in standard base64
// without padding.
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false
	}
	memory, ok1 := phcNumber(params[0], "m=")
	iterations, ok2 := phcNumber(params[1], "t=")
	threads, ok3 := phcNumber(params[2], "p=")
	if !ok1 || !ok2 || !ok3 || iterations == 0 || iterations > maxArgonTime || threads == 0 || threads > 255 ||
		memory > maxArgonMemory || memory < 8*threads {
		return false
	}
	salt, ok := phcBase64(parts[4])
	if !ok || len(salt) < minArgonSalt || len(salt) > maxArgonSalt {
		return false
	}
	want, ok := phcBase64(parts[5])
	if !ok || len(want) < minArgonKeyLen || len(want) > maxArgonKeyLen {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// phcBase64 decodes standard base64 without padding. Go's decoder skips CR
// and LF, which the format does not allow, so those are refused first.
func phcBase64(s string) ([]byte, bool) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, false
	}
	b, err := b64.Strict().DecodeString(s)
	return b, err == nil
}

// phcNumber parses "<prefix><decimal>" the way the PHC string format writes
// numbers: digits only, no leading zero.
func phcNumber(s, prefix string) (uint32, bool) {
	s, ok := strings.CutPrefix(s, prefix)
	if !ok || s == "" || len(s) > 10 || (s[0] == '0' && len(s) > 1) {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

var dummyHash = sync.OnceValue(func() string {
	h, err := hashPassword("robinmail-dummy-password")
	if err != nil {
		panic(err)
	}
	return h
})
