package users

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// App passwords and recovery codes are made by the server, not chosen by
// people, and that decides how they are stored.
//
// An app password is 160 random bits, a recovery code 80. Argon2id exists
// to make each guess at a password a person chose expensive, because such
// a password is one of perhaps a billion likely ones; against 2^160 or
// 2^80 equally likely values no price per guess matters, and a single
// SHA-256 is as good a lock. It is also what the login needs: a mail
// client opens several IMAP connections every few minutes and logs in on
// each, and Argon2id as configured here (64 MiB, three passes, about a
// tenth of a second of four cores) on every one of them would be the
// largest cost of serving mail and an easy way to exhaust a server. A
// SHA-256 and one indexed read cost microseconds. Recovery codes are
// shorter because a person types them; every wrong one is counted against
// the login limits, which is what bounds guessing there.
//
// Both are written in base32 (a to z, 2 to 7) in groups of four: no
// letters that look alike in both cases, and whether a client or a person
// adds or drops the dashes, spaces or capitals does not matter.

var secretAlphabet = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

const (
	appPasswordBytes  = 20 // 32 characters
	recoveryCodeBytes = 10 // 16 characters
	// AppPasswordLen and RecoveryCodeLen are the lengths without dashes.
	AppPasswordLen  = 32
	RecoveryCodeLen = 16
)

func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretAlphabet.EncodeToString(b), nil
}

// group writes a secret in groups of four with dashes between them.
func group(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(s[i:min(i+4, len(s))])
	}
	return b.String()
}

// normalize drops dashes and spaces and lowers capitals, and reports
// whether what is left has the given length and only base32 characters.
func normalize(s string, length int) (string, bool) {
	if len(s) > 4*length {
		return "", false
	}
	b := make([]byte, 0, length)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' || c == ' ':
			continue
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		}
		if !(c >= 'a' && c <= 'z') && !(c >= '2' && c <= '7') {
			return "", false
		}
		b = append(b, c)
	}
	return string(b), len(b) == length
}

// NewAppPassword makes an app password and returns it as it is shown to
// the user, once: "abcd-efgh-...".
func NewAppPassword() (string, error) {
	s, err := randomSecret(appPasswordBytes)
	return group(s), err
}

// NormalizeAppPassword reports whether s has the form of an app password,
// however it was typed, and returns the form its hash is taken of.
func NormalizeAppPassword(s string) (string, bool) { return normalize(s, AppPasswordLen) }

// HashAppPassword is what the database holds of an app password. The
// argument is the normalized form.
func HashAppPassword(normalized string) string {
	sum := sha256.Sum256([]byte("robinmail app password\x00" + normalized))
	return hex.EncodeToString(sum[:])
}

// NewRecoveryCode makes a recovery code as it is shown to the user.
func NewRecoveryCode() (string, error) {
	s, err := randomSecret(recoveryCodeBytes)
	return group(s), err
}

// NormalizeRecoveryCode is NormalizeAppPassword for recovery codes.
func NormalizeRecoveryCode(s string) (string, bool) { return normalize(s, RecoveryCodeLen) }

// HashRecoveryCode is what the database holds of a recovery code.
func HashRecoveryCode(normalized string) string {
	sum := sha256.Sum256([]byte("robinmail recovery code\x00" + normalized))
	return hex.EncodeToString(sum[:])
}
