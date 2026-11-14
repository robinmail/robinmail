// Package srs rewrites the envelope sender of forwarded mail: the Sender
// Rewriting Scheme. There is no RFC for it; the format is the one of
// libsrs2 and its paper (Shevek, "The Sender Rewriting Scheme", 2004),
// which Postfix (postsrsd), Exim and others use:
//
//	SRS0=HHH=TT=source.example=user@forwarder.example
//	SRS1=HHH=first.example==HHH=TT=source.example=user@second.example
//
// A message from user@source.example that forwarder.example sends on
// leaves with the first address as its envelope sender. The next server
// checks SPF against forwarder.example, which is honest: that is who is
// sending. A bounce comes back to the forwarder, which finds the original
// sender in the address and passes the bounce on.
//
// HHH is a keyed hash over the rest, so nobody else can make an address
// that this server would pass mail on to: without it the reverse path
// would be an open relay. TT is the day the address was made, so an
// address stops working after MaxAge. SRS1 is for mail that was already
// rewritten by another forwarder: it keeps the first forwarder's address
// as it is and points back at that forwarder, so chains do not grow.
//
// Where this differs from libsrs2: the hash is HMAC-SHA-256, 8 characters
// of lower-case base32 (libsrs2: HMAC-SHA-1, 4 characters of base64, of
// which it ignores the case when comparing). Only this server reads its
// own hashes, so that changes nothing for anyone else, and an address that
// a server on the way has folded to lower case still verifies.
package srs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
	"time"
)

// MaxAge is how long a rewritten address is accepted: bounces come within
// days. libsrs2 has 21 days.
const MaxAge = 21 * 24 * time.Hour

// maxLocal is the longest local part of RFC 5321 4.5.3.1.1. A sender that
// does not fit in it rewritten is not rewritten.
const maxLocal = 64

const (
	hashLen = 8
	// A timestamp is the day since the epoch modulo 2^10 in two base32
	// characters, as in libsrs2: it wraps every 1024 days.
	tsMod = 1 << 10
)

var (
	ErrNotSRS  = errors.New("srs: not a rewritten address")
	ErrHash    = errors.New("srs: the address was not made by this server")
	ErrExpired = errors.New("srs: the address is too old")
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

const tsAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// Rewriter makes and reads rewritten addresses. Every node of a cluster
// needs the same Key.
type Rewriter struct {
	Key []byte
	// OldKeys are keys that addresses were made under before Key replaced
	// them (a master key rotation). Addresses made under them are still
	// read until they are too old anyway; new ones are made under Key.
	OldKeys [][]byte
	Now     func() time.Time // time.Now when nil
}

func (r *Rewriter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Rewriter) hash(parts ...string) string { return hashWith(r.Key, parts...) }

// hashOK reports whether hash is the hash of the parts under the key or
// one of the old keys.
func (r *Rewriter) hashOK(hash string, parts ...string) bool {
	ok := false
	for _, key := range append([][]byte{r.Key}, r.OldKeys...) {
		// Every key is tried, so the time taken does not tell which matched.
		if hmac.Equal([]byte(strings.ToLower(hash)), []byte(hashWith(key, parts...))) {
			ok = true
		}
	}
	return ok
}

func hashWith(key []byte, parts ...string) string {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write([]byte(strings.ToLower(p)))
		h.Write([]byte{0})
	}
	return strings.ToLower(b32.EncodeToString(h.Sum(nil)))[:hashLen]
}

func timestamp(t time.Time) string {
	day := int(t.Unix()/86400) % tsMod
	return string([]byte{tsAlphabet[day>>5], tsAlphabet[day&31]})
}

func parseTimestamp(s string) (int, bool) {
	if len(s) != 2 {
		return 0, false
	}
	hi := strings.IndexByte(tsAlphabet, s[0]|0x20)
	lo := strings.IndexByte(tsAlphabet, s[1]|0x20)
	if s[0] >= '2' && s[0] <= '7' {
		hi = strings.IndexByte(tsAlphabet, s[0])
	}
	if s[1] >= '2' && s[1] <= '7' {
		lo = strings.IndexByte(tsAlphabet, s[1])
	}
	if hi < 0 || lo < 0 {
		return 0, false
	}
	return hi<<5 | lo, true
}

// IsSRS reports whether a local part has the form of a rewritten address.
func IsSRS(local string) bool {
	return len(local) > 5 && (strings.EqualFold(local[:5], "SRS0=") || strings.EqualFold(local[:5], "SRS1="))
}

func split(addr string) (local, domain string, ok bool) {
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return "", "", false
	}
	return addr[:at], addr[at+1:], true
}

// Forward returns the envelope sender for a message from sender that this
// server, as domain, sends on. ok is false when there is nothing to
// rewrite to: the null sender, an address without a domain, or one whose
// rewritten form would not fit a local part. The caller then sends with
// the null sender, to which nothing bounces.
func (r *Rewriter) Forward(sender, domain string) (rewritten string, ok bool) {
	local, host, valid := split(sender)
	// "=" separates the parts; no domain name has one.
	if !valid || domain == "" || strings.ContainsAny(sender, " \t\r\n") || strings.Contains(host, "=") {
		return "", false
	}
	var out string
	switch {
	case len(local) > 5 && strings.EqualFold(local[:5], "SRS0="):
		// Rewritten once, elsewhere: point back at that forwarder and
		// keep what it made.
		rest := local[5:]
		out = "SRS1=" + r.hash(host, rest) + "=" + host + "==" + rest
	case len(local) > 5 && strings.EqualFold(local[:5], "SRS1="):
		// Rewritten more than once: the first forwarder stays, the hash
		// becomes ours.
		_, after, found := strings.Cut(local[5:], "=")
		first, rest, found2 := strings.Cut(after, "==")
		if !found || !found2 || first == "" || rest == "" {
			return "", false
		}
		out = "SRS1=" + r.hash(first, rest) + "=" + first + "==" + rest
	default:
		ts := timestamp(r.now())
		out = "SRS0=" + r.hash(ts, host, local) + "=" + ts + "=" + host + "=" + local
	}
	if len(out) > maxLocal {
		return "", false
	}
	return out + "@" + domain, true
}

// Reverse returns where a bounce to a rewritten address goes: the original
// sender, or for mail that came through another forwarder first, that
// forwarder's rewritten address. It fails for an address this server did
// not make, or made too long ago.
func (r *Rewriter) Reverse(rcpt string) (string, error) {
	local, _, ok := split(rcpt)
	if !ok {
		local = rcpt
	}
	if !IsSRS(local) {
		return "", ErrNotSRS
	}
	if strings.EqualFold(local[:5], "SRS1=") {
		hash, after, found := strings.Cut(local[5:], "=")
		first, rest, found2 := strings.Cut(after, "==")
		if !found || !found2 || first == "" || rest == "" {
			return "", ErrNotSRS
		}
		if !r.hashOK(hash, first, rest) {
			return "", ErrHash
		}
		return "SRS0=" + rest + "@" + first, nil
	}
	parts := strings.SplitN(local[5:], "=", 4)
	if len(parts) != 4 || parts[2] == "" || parts[3] == "" {
		return "", ErrNotSRS
	}
	hash, ts, host, user := parts[0], parts[1], parts[2], parts[3]
	if !r.hashOK(hash, ts, host, user) {
		return "", ErrHash
	}
	day, ok := parseTimestamp(ts)
	if !ok {
		return "", ErrNotSRS
	}
	today := int(r.now().Unix()/86400) % tsMod
	age := (today - day + tsMod) % tsMod
	// A day ahead is a clock that differs between nodes, not an old address.
	if age > int(MaxAge/(24*time.Hour)) && age != tsMod-1 {
		return "", ErrExpired
	}
	return user + "@" + host, nil
}
