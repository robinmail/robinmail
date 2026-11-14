// Package jose verifies signed JSON Web Tokens: the compact serialization
// of JWS (RFC 7515), public keys as JWK and JWK Sets (RFC 7517), and the
// two signature algorithms robinmail accepts (RFC 7518): RS256 and ES256.
//
// It is the part of an OpenID Connect login that decides whether an ID
// token is the provider's, written here on the standard library. It only
// verifies; nothing in robinmail signs or encrypts a token.
//
// What it refuses, whatever key is offered:
//
//   - "none" (RFC 7515 A.5, RFC 7518 3.6): a token nobody signed.
//   - HS256 and its family: an HMAC under a secret both sides share. An
//     attacker who gets a verifier to take the provider's public key, which
//     everybody has, for that secret can sign anything ("key confusion").
//     No public key is ever used as an HMAC key here because there is no
//     HMAC here.
//   - every other algorithm, known or not. RS256 is the one every OpenID
//     provider must support (OIDC Core 15.1) and ES256 is what the ones
//     that prefer elliptic curves use; a third can be added when a
//     provider needs it.
//   - a key whose type does not fit the algorithm in the token, or whose
//     own "alg", "use" or "key_ops" say it is for something else.
//   - a "crit" header (RFC 7515 4.1.11): it names extensions the verifier
//     must understand, and this one understands none.
//   - the JSON serializations and JWE (five parts): not what an ID token is.
package jose

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Algorithms (RFC 7518 3.1).
const (
	RS256 = "RS256" // RSASSA-PKCS1-v1_5 using SHA-256
	ES256 = "ES256" // ECDSA using P-256 and SHA-256
)

var (
	// ErrMalformed is a token or key that is not what its RFC describes.
	ErrMalformed = errors.New("jose: malformed")
	// ErrAlgorithm is an algorithm this package does not accept.
	ErrAlgorithm = errors.New("jose: algorithm not accepted")
	// ErrNoKey is no key in the set that could have signed the token.
	ErrNoKey = errors.New("jose: no key for the token")
	// ErrSignature is a signature that does not verify.
	ErrSignature = errors.New("jose: signature does not verify")
)

// Limits. A token arrives from the provider's token endpoint and a key set
// from its jwks_uri; neither is the user's to choose, but both are input.
const (
	maxToken     = 64 << 10
	minRSABits   = 2048 // RFC 7518 3.3: "A key of size 2048 bits or larger MUST be used"
	maxRSABits   = 8192
	maxKeysTried = 8
)

// b64 decodes base64url without padding (RFC 7515 2, appendix C), and
// nothing else: no padding, no characters outside the alphabet, no line
// breaks (which Go's decoder would skip), no bits left over.
func b64(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("%w: line break in base64url", ErrMalformed)
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: base64url: %v", ErrMalformed, err)
	}
	return b, nil
}

// Header is the JOSE header of a token (RFC 7515 4.1), as far as this
// package looks at it.
type Header struct {
	Alg  string          `json:"alg"`
	Kid  string          `json:"kid"`
	Typ  string          `json:"typ"`
	Crit json.RawMessage `json:"crit"`
}

// Token is a JWS in compact serialization, taken apart and not yet
// verified. Nothing in Payload may be believed before Verify succeeds.
type Token struct {
	Header    Header
	Payload   []byte
	signed    []byte // what the signature is over: ASCII(header "." payload)
	signature []byte
}

// Parse takes a compact JWS apart (RFC 7515 7.1):
//
//	BASE64URL(UTF8(JWS Protected Header)) || '.' ||
//	BASE64URL(JWS Payload) || '.' ||
//	BASE64URL(JWS Signature)
//
// It refuses an algorithm this package does not verify, so that no caller
// reads the payload of a token signed with "none".
func Parse(compact string) (*Token, error) {
	if len(compact) > maxToken {
		return nil, fmt.Errorf("%w: token of %d bytes", ErrMalformed, len(compact))
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: %d parts, a signed token has 3", ErrMalformed, len(parts))
	}
	rawHeader, err := b64(parts[0])
	if err != nil {
		return nil, err
	}
	t := &Token{signed: []byte(compact[:len(parts[0])+1+len(parts[1])])}
	// RFC 7515 5.2 step 4: a header with a name twice is refused or the
	// last one wins; encoding/json takes the last, also for names that
	// differ in case only, and so do the checks below.
	if err := json.Unmarshal(rawHeader, &t.Header); err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrMalformed, err)
	}
	if !exactMember(rawHeader, "alg", t.Header.Alg) {
		return nil, fmt.Errorf("%w: header has no \"alg\" spelled so", ErrMalformed)
	}
	switch t.Header.Alg {
	case RS256, ES256:
	case "":
		return nil, fmt.Errorf("%w: no \"alg\"", ErrMalformed)
	default:
		return nil, fmt.Errorf("%w: %q", ErrAlgorithm, t.Header.Alg)
	}
	if len(t.Header.Crit) > 0 {
		return nil, fmt.Errorf("%w: \"crit\" header %s", ErrAlgorithm, t.Header.Crit)
	}
	if t.Payload, err = b64(parts[1]); err != nil {
		return nil, err
	}
	if t.signature, err = b64(parts[2]); err != nil {
		return nil, err
	}
	return t, nil
}

// exactMember reports whether the JSON object has a member with exactly
// that name holding exactly that string. encoding/json matches names
// without regard to case; "ALG" is not "alg" (RFC 7515 4.1: names are
// case-sensitive).
func exactMember(object []byte, name, want string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(object, &m) != nil {
		return false
	}
	var got string
	return json.Unmarshal(m[name], &got) == nil && got == want
}

// Key is a public key from a JWK (RFC 7517 4, RFC 7518 6).
type Key struct {
	ID string
	// Alg is the key's own "alg", if it has one: the only algorithm it is
	// for.
	Alg string
	RSA *rsa.PublicKey
	EC  *ecdsa.PublicKey
}

// fits reports whether the key may verify a token with that header.
func (k Key) fits(h Header) bool {
	if k.Alg != "" && k.Alg != h.Alg {
		return false
	}
	switch h.Alg {
	case RS256:
		return k.RSA != nil
	case ES256:
		return k.EC != nil
	}
	return false
}

// Verify checks the token's signature with the key.
func (t *Token) Verify(k Key) error {
	if !k.fits(t.Header) {
		return fmt.Errorf("%w: the key is not for %s", ErrNoKey, t.Header.Alg)
	}
	sum := sha256.Sum256(t.signed)
	switch t.Header.Alg {
	case RS256:
		// RFC 7518 3.3: RSASSA-PKCS1-v1_5 with SHA-256 (RFC 8017 8.2.2).
		if rsa.VerifyPKCS1v15(k.RSA, crypto.SHA256, sum[:], t.signature) != nil {
			return ErrSignature
		}
	case ES256:
		// RFC 7518 3.4: the signature is R and S as two 32-byte
		// big-endian integers, one after the other; not the ASN.1 form.
		if len(t.signature) != 64 {
			return ErrSignature
		}
		r, s := new(big.Int).SetBytes(t.signature[:32]), new(big.Int).SetBytes(t.signature[32:])
		if !ecdsa.Verify(k.EC, sum[:], r, s) {
			return ErrSignature
		}
	default:
		return fmt.Errorf("%w: %q", ErrAlgorithm, t.Header.Alg)
	}
	return nil
}

// Set is a JWK Set (RFC 7517 5): the keys of it this package can use.
type Set struct {
	Keys []Key
}

// Verify checks the token's signature with the key of the set that the
// token names. With a "kid" in the token only keys with that id are
// tried; without one every key that fits the algorithm (RFC 7515 4.1.4
// leaves the matching to the application; OIDC Core 10.1 wants a "kid"
// when the set has more than one key).
func (s Set) Verify(t *Token) error {
	tried := 0
	for _, k := range s.Keys {
		if !k.fits(t.Header) || (t.Header.Kid != "" && k.ID != t.Header.Kid) {
			continue
		}
		if tried++; tried > maxKeysTried {
			break
		}
		if t.Verify(k) == nil {
			return nil
		}
	}
	if tried == 0 {
		return fmt.Errorf("%w (alg %s, kid %q)", ErrNoKey, t.Header.Alg, t.Header.Kid)
	}
	return ErrSignature
}

// Has reports whether the set has a key the token could be checked with.
func (s Set) Has(t *Token) bool {
	for _, k := range s.Keys {
		if k.fits(t.Header) && (t.Header.Kid == "" || k.ID == t.Header.Kid) {
			return true
		}
	}
	return false
}

type jwk struct {
	Kty    string   `json:"kty"`
	Use    string   `json:"use"`
	KeyOps []string `json:"key_ops"`
	Alg    string   `json:"alg"`
	Kid    string   `json:"kid"`
	// RSA (RFC 7518 6.3.1)
	N string `json:"n"`
	E string `json:"e"`
	// EC (RFC 7518 6.2.1)
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	// Private and symmetric members: a key that has one is refused. A
	// provider that publishes one has published its secret.
	D string `json:"d"`
	K string `json:"k"`
}

// ErrNotForSignatures is returned by ParseKey for a well-formed key that
// is of no use for verifying: for encryption, of an unknown type or curve.
var ErrNotForSignatures = errors.New("jose: key is not one this package verifies with")

// ParseKey reads one JWK holding an RSA or a P-256 public key for
// signatures.
func ParseKey(data []byte) (Key, error) {
	var j jwk
	if err := json.Unmarshal(data, &j); err != nil {
		return Key{}, fmt.Errorf("%w: JWK: %v", ErrMalformed, err)
	}
	if !exactMember(data, "kty", j.Kty) || j.Kty == "" {
		return Key{}, fmt.Errorf("%w: JWK without \"kty\"", ErrMalformed)
	}
	if j.D != "" || j.K != "" {
		return Key{}, fmt.Errorf("%w: a private or symmetric key", ErrNotForSignatures)
	}
	// RFC 7517 4.2 and 4.3: "use" and "key_ops" say what the key is for.
	if j.Use != "" && j.Use != "sig" {
		return Key{}, fmt.Errorf("%w: use %q", ErrNotForSignatures, j.Use)
	}
	if j.KeyOps != nil {
		verify := false
		for _, op := range j.KeyOps {
			verify = verify || op == "verify"
		}
		if !verify {
			return Key{}, fmt.Errorf("%w: key_ops %v", ErrNotForSignatures, j.KeyOps)
		}
	}
	k := Key{ID: j.Kid, Alg: j.Alg}
	switch j.Kty {
	case "RSA":
		if j.Alg != "" && j.Alg != RS256 {
			return Key{}, fmt.Errorf("%w: an RSA key for %q", ErrNotForSignatures, j.Alg)
		}
		n, err := b64(j.N)
		if err != nil {
			return Key{}, err
		}
		e, err := b64(j.E)
		if err != nil {
			return Key{}, err
		}
		// RFC 7518 6.3.1.1 and 2: unsigned big-endian integers.
		mod, exp := new(big.Int).SetBytes(n), new(big.Int).SetBytes(e)
		if mod.BitLen() < minRSABits || mod.BitLen() > maxRSABits {
			return Key{}, fmt.Errorf("%w: RSA modulus of %d bits (RFC 7518 3.3 wants 2048 or more)", ErrMalformed, mod.BitLen())
		}
		if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31-1 || exp.Bit(0) == 0 || mod.Bit(0) == 0 {
			return Key{}, fmt.Errorf("%w: RSA key with an exponent or modulus that cannot be one", ErrMalformed)
		}
		k.RSA = &rsa.PublicKey{N: mod, E: int(exp.Int64())}
	case "EC":
		if j.Crv != "P-256" {
			return Key{}, fmt.Errorf("%w: curve %q", ErrNotForSignatures, j.Crv)
		}
		if j.Alg != "" && j.Alg != ES256 {
			return Key{}, fmt.Errorf("%w: a P-256 key for %q", ErrNotForSignatures, j.Alg)
		}
		x, err := b64(j.X)
		if err != nil {
			return Key{}, err
		}
		y, err := b64(j.Y)
		if err != nil {
			return Key{}, err
		}
		// RFC 7518 6.2.1.2: "The length of this octet string MUST be the
		// full size of a coordinate for the curve": 32 octets for P-256.
		if len(x) != 32 || len(y) != 32 {
			return Key{}, fmt.Errorf("%w: P-256 coordinates of %d and %d octets", ErrMalformed, len(x), len(y))
		}
		// The uncompressed point of SEC 1 2.3.3; parsing it checks that
		// the point is on the curve.
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return Key{}, fmt.Errorf("%w: not a point on P-256", ErrMalformed)
		}
		k.EC = pub
	default:
		return Key{}, fmt.Errorf("%w: key type %q", ErrNotForSignatures, j.Kty)
	}
	return k, nil
}

// ParseSet reads a JWK Set. Keys this package cannot verify with are left
// out, as RFC 7517 5 asks ("Implementations SHOULD ignore JWKs within a
// JWK Set that use "kty" values that are not understood by them, that are
// missing required members, or for which values are out of the supported
// ranges"); a set that is not a set is an error.
func ParseSet(data []byte) (Set, error) {
	var raw struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Set{}, fmt.Errorf("%w: JWK Set: %v", ErrMalformed, err)
	}
	if raw.Keys == nil {
		return Set{}, fmt.Errorf("%w: JWK Set without \"keys\"", ErrMalformed)
	}
	var s Set
	for _, r := range raw.Keys {
		if k, err := ParseKey(r); err == nil {
			s.Keys = append(s.Keys, k)
		}
	}
	return s, nil
}
