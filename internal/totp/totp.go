// Package totp computes one-time passwords: HOTP (RFC 4226) from a counter
// and TOTP (RFC 6238) from the time. It knows nothing about accounts; which
// time steps were used already is the caller's to remember.
//
// What robinmail uses is what every authenticator app understands and
// what an otpauth URI means when it says nothing else: HMAC-SHA-1, six
// digits, a step of 30 seconds. SHA-1 is not a weakness here: HMAC does
// not depend on the collision resistance that SHA-1 lost (RFC 6238 1.2,
// RFC 4226 B.2).
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"hash"
	"net/url"
	"strconv"
	"time"
)

const (
	// Period is the length of a time step in seconds (RFC 6238 4.1's X;
	// 5.2 recommends 30).
	Period = 30
	// Digits is the length of a code.
	Digits = 6
	// SecretSize is the length of a new secret in bytes: the output
	// length of SHA-1, which RFC 4226 4 (R6) recommends.
	SecretSize = 20
	// Skew is how many time steps before and after the current one a code
	// is accepted for. RFC 6238 5.2: "We RECOMMEND that at most one time
	// step is allowed as the network delay."
	Skew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret makes a random secret.
func NewSecret() ([]byte, error) {
	s := make([]byte, SecretSize)
	_, err := rand.Read(s)
	return s, err
}

// EncodeSecret writes a secret the way authenticator apps take it by
// hand: base32 without padding.
func EncodeSecret(secret []byte) string { return b32.EncodeToString(secret) }

// HOTP is RFC 4226 5.3: the HMAC of the counter, truncated dynamically to
// 31 bits and then to the given number of decimal digits (6 to 9), with
// leading zeros kept.
func HOTP(newHash func() hash.Hash, key []byte, counter uint64, digits int) string {
	mac := hmac.New(newHash, key)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], counter)
	mac.Write(c[:])
	sum := mac.Sum(nil)
	// 5.3: the low four bits of the last byte say where the four bytes
	// start; the top bit of those is dropped.
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	s := strconv.FormatUint(uint64(bin%mod), 10)
	for len(s) < digits {
		s = "0" + s
	}
	return s
}

// Step is the time step a moment falls in: RFC 6238 4.2's
// T = (Current Unix time - T0) / X with T0 = 0.
func Step(t time.Time) int64 { return t.Unix() / Period }

// Code is the code of a time step.
func Code(secret []byte, step int64) string {
	return HOTP(sha1.New, secret, uint64(step), Digits)
}

// Match looks for the time step, among the current one and Skew steps to
// either side, whose code is the given one. Every candidate is computed
// and compared in constant time, so the time taken says nothing about
// which one matched or how nearly.
func Match(secret []byte, code string, now time.Time) (step int64, ok bool) {
	if len(code) != Digits {
		return 0, false
	}
	cur := Step(now)
	for d := int64(-Skew); d <= Skew; d++ {
		if cur+d < 0 {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(Code(secret, cur+d)), []byte(code)) == 1 {
			// The newest match wins, so a code is spent together with
			// everything before it.
			step, ok = cur+d, true
		}
	}
	return step, ok
}

// URI is the otpauth URI an authenticator app reads from a QR code (the
// "Key Uri Format" of Google Authenticator, which the other apps follow):
//
//	otpauth://totp/<issuer>:<account>?secret=<base32>&issuer=<issuer>&algorithm=SHA1&digits=6&period=30
//
// The parameters that are defaults are written out: an app that ignores
// them does the same as one that reads them.
func URI(issuer, account string, secret []byte) string {
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(Digits))
	q.Set("period", strconv.Itoa(Period))
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + issuer + ":" + account, RawQuery: q.Encode()}
	return u.String()
}
