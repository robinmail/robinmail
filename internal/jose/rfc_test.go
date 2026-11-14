package jose

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
)

// The examples of the RFCs, cut from their text (testdata/rfc-vectors.json):
// RFC 7515 appendix A (JWS examples) and E, RFC 7517 appendix A (JWK Sets),
// RFC 7519 3.1, and RFC 7520 3 and 4 (the JOSE cookbook).
var vectors = func() map[string]json.RawMessage {
	data, err := os.ReadFile("testdata/rfc-vectors.json")
	if err != nil {
		panic(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		panic(err)
	}
	return m
}()

func vec(t *testing.T, name string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(vectors[name], &s); err != nil {
		t.Fatalf("vector %s: %v", name, err)
	}
	return s
}

// public strips the private members off an example key, which the RFCs
// print whole.
func public(t *testing.T, name string) Key {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(vectors[name], &m); err != nil {
		t.Fatalf("vector %s: %v", name, err)
	}
	for _, private := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		delete(m, private)
	}
	data, _ := json.Marshal(m)
	k, err := ParseKey(data)
	if err != nil {
		t.Fatalf("key %s: %v", name, err)
	}
	return k
}

const examplePayload = "{\"iss\":\"joe\",\r\n \"exp\":1300819380,\r\n \"http://example.com/is_root\":true}"

// RFC 7515 A.2, "Example JWS Using RSASSA-PKCS1-v1_5 SHA-256": the JWS
// validates with the public part of the key of A.2.1 (A.2.2).
func TestRFC7515A2_RS256(t *testing.T) {
	tok, err := Parse(vec(t, "rfc7515_a2_jws"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Header.Alg != "RS256" || string(tok.Payload) != examplePayload {
		t.Errorf("header %+v payload %q", tok.Header, tok.Payload)
	}
	key := public(t, "rfc7515_a2_jwk")
	if err := tok.Verify(key); err != nil {
		t.Errorf("the example does not verify: %v", err)
	}
	tampered(t, vec(t, "rfc7515_a2_jws"), key)
}

// RFC 7515 A.3, "Example JWS Using ECDSA P-256 SHA-256": the signature is
// R and S, 32 octets each (A.3.1), and validates with the key's x and y
// (A.3.2).
func TestRFC7515A3_ES256(t *testing.T) {
	tok, err := Parse(vec(t, "rfc7515_a3_jws"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Header.Alg != "ES256" || string(tok.Payload) != examplePayload || len(tok.signature) != 64 {
		t.Errorf("header %+v payload %q, signature of %d octets", tok.Header, tok.Payload, len(tok.signature))
	}
	key := public(t, "rfc7515_a3_jwk")
	if err := tok.Verify(key); err != nil {
		t.Errorf("the example does not verify: %v", err)
	}
	tampered(t, vec(t, "rfc7515_a3_jws"), key)
}

// tampered changes each part of a token that verifies and expects it not to.
func tampered(t *testing.T, compact string, key Key) {
	t.Helper()
	parts := strings.Split(compact, ".")
	flip := func(s string) string {
		b, _ := base64.RawURLEncoding.DecodeString(s)
		b[len(b)/2] ^= 1
		return base64.RawURLEncoding.EncodeToString(b)
	}
	cut := func(s string) string {
		b, _ := base64.RawURLEncoding.DecodeString(s)
		return base64.RawURLEncoding.EncodeToString(b[:len(b)-1])
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(examplePayload, "joe", "eve", 1)))
	for name, changed := range map[string]string{
		"another payload":            parts[0] + "." + payload + "." + parts[2],
		"a changed signature":        parts[0] + "." + parts[1] + "." + flip(parts[2]),
		"a cut signature":            parts[0] + "." + parts[1] + "." + cut(parts[2]),
		"no signature":               parts[0] + "." + parts[1] + ".",
		"the header spelled anew":    base64.RawURLEncoding.EncodeToString([]byte(`{"alg" : "`+algOf(parts[0])+`"}`)) + "." + parts[1] + "." + parts[2],
		"the signature of the other": parts[0] + "." + payload + "." + strings.Split(vec(t, "rfc7515_a2_jws"), ".")[2],
	} {
		tok, err := Parse(changed)
		if err == nil {
			err = tok.Verify(key)
		}
		if !errors.Is(err, ErrSignature) {
			t.Errorf("%s: %v, want ErrSignature", name, err)
		}
	}
}

func algOf(header string) string {
	b, _ := base64.RawURLEncoding.DecodeString(header)
	var h Header
	json.Unmarshal(b, &h)
	return h.Alg
}

// RFC 7515 A.5, "Example Unsecured JWS", and RFC 7518 3.6: "alg" "none",
// an empty signature. "Implementations that support Unsecured JWSs MUST
// NOT accept such objects as valid unless the application specifies that
// it is acceptable"; this one does not support them at all.
func TestRFC7515A5_NoneIsRefused(t *testing.T) {
	compact := vec(t, "rfc7515_a5_jws")
	if !strings.HasSuffix(compact, ".") || algOf(strings.Split(compact, ".")[0]) != "none" {
		t.Fatalf("the vector is not the unsecured JWS: %s", compact)
	}
	if tok, err := Parse(compact); !errors.Is(err, ErrAlgorithm) || tok != nil {
		t.Errorf("Parse = %v, %v", tok, err)
	}
	// The same in every spelling an attacker might hope is treated alike,
	// and with a signature taken from a real token.
	real := strings.Split(vec(t, "rfc7515_a2_jws"), ".")
	for _, alg := range []string{"none", "None", "NONE", "nOnE", "", " none", "none "} {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"` + alg + `"}`))
		for _, sig := range []string{"", real[2]} {
			tok, err := Parse(header + "." + real[1] + "." + sig)
			if err == nil || tok != nil {
				t.Errorf("alg %q with signature %q...: accepted", alg, sig[:min(8, len(sig))])
			}
		}
	}
}

// RFC 7515 A.1, "Example JWS Using HMAC SHA-256", which is also the JWT of
// RFC 7519 3.1, and RFC 7520 4.4: valid tokens of a kind this package does
// not take, because a verifier that takes HMAC can be talked into using a
// public key as the secret.
func TestHMACIsRefused(t *testing.T) {
	if vec(t, "rfc7515_a1_jws") != vec(t, "rfc7519_3_1_jwt") {
		t.Error("RFC 7515 A.1 and RFC 7519 3.1 should be the same token")
	}
	for _, name := range []string{"rfc7515_a1_jws", "rfc7520_4_4_jws"} {
		if tok, err := Parse(vec(t, name)); !errors.Is(err, ErrAlgorithm) || tok != nil {
			t.Errorf("%s: %v, %v", name, tok, err)
		}
	}
}

// Key confusion. The attacker has the provider's public key, as everybody
// does, and signs a token with HMAC-SHA-256 using that key's bytes as the
// secret, in each form a careless verifier might feed to its HMAC: the
// DER of the key, its PEM, the modulus. Nothing here computes an HMAC, so
// none of them gets as far as a comparison.
func TestKeyConfusion(t *testing.T) {
	key := public(t, "rfc7515_a2_jwk")
	der, _ := x509.MarshalPKIXPublicKey(key.RSA)
	pem := "-----BEGIN PUBLIC KEY-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END PUBLIC KEY-----\n"
	var jwkJSON map[string]string
	json.Unmarshal(vectors["rfc7515_a2_jwk"], &jwkJSON)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://issuer.example","sub":"attacker"}`))
	for name, secret := range map[string][]byte{"DER": der, "PEM": []byte(pem), "modulus": key.RSA.N.Bytes(), "n as text": []byte(jwkJSON["n"])} {
		for _, alg := range []string{"HS256", "HS384", "HS512"} {
			signed := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"`+alg+`"}`)) + "." + payload
			mac := hmac.New(sha256.New, secret)
			mac.Write([]byte(signed))
			forged := signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
			tok, err := Parse(forged)
			if !errors.Is(err, ErrAlgorithm) || tok != nil {
				t.Errorf("%s with the public key as %s for a secret: %v, %v", alg, name, tok, err)
			}
		}
	}
	// The other way around: a token that says RS256 offered to an EC key,
	// and one that says ES256 to an RSA key. The key's type decides.
	rs, _ := Parse(vec(t, "rfc7515_a2_jws"))
	es, _ := Parse(vec(t, "rfc7515_a3_jws"))
	if err := rs.Verify(public(t, "rfc7515_a3_jwk")); !errors.Is(err, ErrNoKey) {
		t.Errorf("an RS256 token against an EC key: %v", err)
	}
	if err := es.Verify(key); !errors.Is(err, ErrNoKey) {
		t.Errorf("an ES256 token against an RSA key: %v", err)
	}
	// A key that names its algorithm is for that one only.
	onlyES := public(t, "rfc7515_a2_jwk")
	onlyES.Alg = "ES256"
	if err := rs.Verify(onlyES); !errors.Is(err, ErrNoKey) {
		t.Errorf("a key whose alg is another: %v", err)
	}
}

// RFC 7518 3.1 lists the algorithms. Those besides RS256 and ES256 are
// refused by name: RFC 7515 A.4 and RFC 7520 4.3 (ES512), RFC 7520 4.2
// (PS384). A deliberate limit: an ID token is RS256 unless the client
// registered otherwise (OIDC Core 3.1.3.7 step 7).
func TestOtherAlgorithmsAreRefused(t *testing.T) {
	for _, name := range []string{"rfc7515_a4_jws", "rfc7520_4_2_jws", "rfc7520_4_3_jws"} {
		if _, err := Parse(vec(t, name)); !errors.Is(err, ErrAlgorithm) {
			t.Errorf("%s (alg %s): %v", name, algOf(strings.Split(vec(t, name), ".")[0]), err)
		}
	}
	for _, alg := range []string{"RS384", "RS512", "ES384", "PS256", "EdDSA", "ES256K", "rs256", "RS256 ", "A128KW", "dir"} {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"` + alg + `"}`))
		if _, err := Parse(header + ".e30.AAAA"); !errors.Is(err, ErrAlgorithm) {
			t.Errorf("alg %q: %v", alg, err)
		}
	}
}

// RFC 7520 3.3 and 4.1, "RSA v1.5 Signature": the cookbook's RS256
// example verifies with its RSA public key, found by "kid" in a set.
func TestRFC7520_4_1(t *testing.T) {
	tok, err := Parse(vec(t, "rfc7520_4_1_jws"))
	if err != nil {
		t.Fatal(err)
	}
	if tok.Header.Kid != "bilbo.baggins@hobbiton.example" || !strings.HasPrefix(string(tok.Payload), "It’s a dangerous business, Frodo") {
		t.Errorf("header %+v, payload %q", tok.Header, tok.Payload)
	}
	set, err := ParseSet([]byte(`{"keys":[` + string(vectors["rfc7520_3_1_jwk"]) + "," + string(vectors["rfc7520_3_3_jwk"]) + `]}`))
	if err != nil {
		t.Fatal(err)
	}
	// The P-521 key of 3.1 is left out: not a curve this package uses.
	if len(set.Keys) != 1 || set.Keys[0].ID != "bilbo.baggins@hobbiton.example" || set.Keys[0].RSA == nil {
		t.Fatalf("set: %+v", set.Keys)
	}
	if err := set.Verify(tok); err != nil {
		t.Errorf("the example does not verify: %v", err)
	}
	// Another key under the same id does not.
	wrong := Set{Keys: []Key{{ID: tok.Header.Kid, RSA: public(t, "rfc7515_a2_jwk").RSA}}}
	if err := wrong.Verify(tok); !errors.Is(err, ErrSignature) {
		t.Errorf("another key with the token's kid: %v", err)
	}
	// And the right key under another id is not found.
	renamed := Set{Keys: []Key{{ID: "frodo", RSA: set.Keys[0].RSA}}}
	if err := renamed.Verify(tok); !errors.Is(err, ErrNoKey) || renamed.Has(tok) {
		t.Errorf("the key under another kid: %v", err)
	}
}

// RFC 7517 A.1, "Example Public Keys": a set with an EC key marked for
// encryption and an RSA key for RS256. Only the second is one to verify
// with (4.2: "use" "enc" is not "sig").
func TestRFC7517A1(t *testing.T) {
	set, err := ParseSet(vectors["rfc7517_a1_jwks"])
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 1 || set.Keys[0].ID != "2011-04-29" || set.Keys[0].Alg != "RS256" || set.Keys[0].RSA == nil ||
		set.Keys[0].RSA.E != 65537 || set.Keys[0].RSA.N.BitLen() != 2048 {
		t.Fatalf("keys: %+v", set.Keys)
	}
	// The EC key of that set, were it for signatures, parses as P-256.
	var raw struct{ Keys []map[string]any }
	json.Unmarshal(vectors["rfc7517_a1_jwks"], &raw)
	if _, err := ParseKey(mustJSON(raw.Keys[0])); !errors.Is(err, ErrNotForSignatures) {
		t.Errorf("the key with use enc: %v", err)
	}
	raw.Keys[0]["use"] = "sig"
	k, err := ParseKey(mustJSON(raw.Keys[0]))
	if err != nil || k.EC == nil || k.EC.Curve != elliptic.P256() || k.ID != "1" {
		t.Errorf("the EC key as a signature key: %+v %v", k, err)
	}
}

// RFC 7517 A.2 and A.3: private and symmetric keys. A verifier has no use
// for either, and a provider that serves one has given its secret away.
func TestPrivateAndSymmetricKeysAreLeftOut(t *testing.T) {
	for _, name := range []string{"rfc7517_a2_jwks", "rfc7517_a3_jwks"} {
		set, err := ParseSet(vectors[name])
		if err != nil || len(set.Keys) != 0 {
			t.Errorf("%s: %d keys, %v", name, len(set.Keys), err)
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// RFC 7515 appendix E, "Negative Test Case for "crit" Header Parameter":
// a JWS whose header has a "crit" naming an extension the verifier does
// not understand "MUST be rejected by all implementations".
func TestRFC7515E_Crit(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString(vectors["rfc7515_e_header"])
	if _, err := Parse(header + ".RkFJTA."); err == nil {
		t.Error("the negative test case of appendix E was accepted")
	}
	// The same on an algorithm that is accepted otherwise.
	parts := strings.Split(vec(t, "rfc7515_a2_jws"), ".")
	header = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","crit":["exp"],"exp":1}`))
	if _, err := Parse(header + "." + parts[1] + "." + parts[2]); !errors.Is(err, ErrAlgorithm) {
		t.Errorf("crit with RS256: %v", err)
	}
}

// RFC 7515 7.1 and appendix C: three parts of base64url without padding.
func TestCompactForm(t *testing.T) {
	good := vec(t, "rfc7515_a2_jws")
	parts := strings.Split(good, ".")
	for name, bad := range map[string]string{
		"empty":                      "",
		"one part":                   parts[0],
		"two parts":                  parts[0] + "." + parts[1],
		"four parts":                 good + "." + parts[2],
		"five parts, as a JWE has":   good + ".a.b",
		"padding":                    parts[0] + "=." + parts[1] + "." + parts[2],
		"padded signature":           parts[0] + "." + parts[1] + "." + parts[2] + "==",
		"standard base64":            strings.ReplaceAll(strings.ReplaceAll(good, "-", "+"), "_", "/"),
		"a space":                    parts[0] + " ." + parts[1] + "." + parts[2],
		"a line break in the middle": parts[0] + "." + parts[1][:10] + "\n" + parts[1][10:] + "." + parts[2],
		"a trailing line break":      good + "\n",
		"header is not JSON":         "bm90IGpzb24." + parts[1] + "." + parts[2],
		"header is an array":         base64.RawURLEncoding.EncodeToString([]byte(`["RS256"]`)) + "." + parts[1] + "." + parts[2],
		"alg is a number":            base64.RawURLEncoding.EncodeToString([]byte(`{"alg":256}`)) + "." + parts[1] + "." + parts[2],
		"ALG in capitals":            base64.RawURLEncoding.EncodeToString([]byte(`{"ALG":"RS256"}`)) + "." + parts[1] + "." + parts[2],
		"the JSON serialization":     `{"payload":"` + parts[1] + `","protected":"` + parts[0] + `","signature":"` + parts[2] + `"}`,
		"too long":                   parts[0] + "." + strings.Repeat("A", maxToken) + "." + parts[2],
	} {
		if tok, err := Parse(bad); err == nil {
			t.Errorf("%s: accepted (%+v)", name, tok.Header)
		}
	}
	// "alg" twice: the last one counts, here and in the check after it,
	// so a "none" hidden before or after a real one never slips through.
	for _, header := range []string{`{"alg":"none","alg":"RS256"}`, `{"alg":"RS256","alg":"none"}`} {
		tok, err := Parse(base64.RawURLEncoding.EncodeToString([]byte(header)) + "." + parts[1] + "." + parts[2])
		if err == nil {
			err = tok.Verify(public(t, "rfc7515_a2_jwk"))
		}
		if err == nil {
			t.Errorf("header %s: verified", header)
		}
	}
}

// RFC 7518 6.2.1 and 6.3.1: what a key must look like.
func TestKeyParameters(t *testing.T) {
	var rsaKey, ecKey map[string]any
	json.Unmarshal(vectors["rfc7520_3_3_jwk"], &rsaKey)
	json.Unmarshal(vectors["rfc7515_a3_jwk"], &ecKey)
	delete(ecKey, "d")
	with := func(base map[string]any, k string, v any) []byte {
		m := map[string]any{}
		for key, val := range base {
			m[key] = val
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return mustJSON(m)
	}
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	b := base64.RawURLEncoding.EncodeToString
	x, _ := base64.RawURLEncoding.DecodeString(ecKey["x"].(string))
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"RSA of 1024 bits (RFC 7518 3.3)": {with(rsaKey, "n", b(small.N.Bytes())), ErrMalformed},
		"RSA without n":                   {with(rsaKey, "n", nil), ErrMalformed},
		"RSA with e = 1":                  {with(rsaKey, "e", "AQ"), ErrMalformed},
		"RSA with an even e":              {with(rsaKey, "e", "AQAA"), ErrMalformed},
		"RSA with a padded n":             {with(rsaKey, "n", rsaKey["n"].(string)+"="), ErrMalformed},
		"RSA for encryption":              {with(rsaKey, "use", "enc"), ErrNotForSignatures},
		"RSA for PS256":                   {with(rsaKey, "alg", "PS256"), ErrNotForSignatures},
		"RSA for HS256":                   {with(rsaKey, "alg", "HS256"), ErrNotForSignatures},
		"key_ops without verify":          {with(rsaKey, "key_ops", []string{"sign"}), ErrNotForSignatures},
		"a symmetric key":                 {[]byte(`{"kty":"oct","k":"AyM1SysPpbyDfgZld3umj1qzKObwVMkoqQ-EstJQLr_T-1qS0gZH75aKtMN3Yj0iPS4hcgUuTwjAzZr1Z9CAow"}`), ErrNotForSignatures},
		"an RSA key with a k too":         {with(rsaKey, "k", "c2VjcmV0"), ErrNotForSignatures},
		"an unknown type":                 {[]byte(`{"kty":"OKP","crv":"Ed25519","x":"11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"}`), ErrNotForSignatures},
		"no kty":                          {with(rsaKey, "kty", nil), ErrMalformed},
		"KTY in capitals":                 {[]byte(strings.Replace(string(with(rsaKey, "use", nil)), `"kty"`, `"KTY"`, 1)), ErrMalformed},
		"P-384":                           {with(ecKey, "crv", "P-384"), ErrNotForSignatures},
		"P-256 for ES512":                 {with(ecKey, "alg", "ES512"), ErrNotForSignatures},
		"x of 31 octets (6.2.1.2)":        {with(ecKey, "x", b(x[1:])), ErrMalformed},
		"a point off the curve":           {with(ecKey, "x", b(append([]byte{x[0] ^ 1}, x[1:]...))), ErrMalformed},
		"the point at infinity as zeros":  {with(with2(ecKey, "x", b(make([]byte, 32))), "y", b(make([]byte, 32))), ErrMalformed},
		"not JSON":                        {[]byte(`kty=RSA`), ErrMalformed},
	} {
		if _, err := ParseKey(tc.data); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if k, err := ParseKey(with(rsaKey, "key_ops", []string{"verify"})); err != nil || k.RSA == nil {
		t.Errorf("key_ops verify: %v", err)
	}
	for _, bad := range []string{``, `[]`, `{}`, `{"keys":"none"}`, `{"Keys":null}`} {
		if _, err := ParseSet([]byte(bad)); !errors.Is(err, ErrMalformed) {
			t.Errorf("set %q: %v", bad, err)
		}
	}
	// A set with keys of no use is an empty set, not an error.
	if s, err := ParseSet([]byte(`{"keys":[{"kty":"oct","k":"AA"},{"kty":"nope"},17]}`)); err != nil || len(s.Keys) != 0 {
		t.Errorf("a set of useless keys: %+v %v", s, err)
	}
}

func with2(base map[string]any, k string, v any) map[string]any {
	m := map[string]any{}
	for key, val := range base {
		m[key] = val
	}
	m[k] = v
	return m
}

// Tokens signed here, with keys made here: what this package accepts is
// what the standard library's signers produce, and RFC 7518 3.4's form of
// an ECDSA signature is the only one (the ASN.1 form of X.509 is not).
func TestRoundTrip(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	b := base64.RawURLEncoding.EncodeToString
	signed := func(alg string) string {
		return b([]byte(`{"alg":"`+alg+`","kid":"k1","typ":"JWT"}`)) + "." + b([]byte(`{"sub":"anna"}`))
	}
	sum := sha256.Sum256([]byte(signed("RS256")))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
	tok, err := Parse(signed("RS256") + "." + b(sig))
	if err != nil || tok.Verify(Key{RSA: &rsaKey.PublicKey}) != nil || tok.Header.Typ != "JWT" {
		t.Errorf("RS256 round trip: %v", err)
	}
	// RSASSA-PSS over the same input is another algorithm (PS256).
	pss, _ := rsa.SignPSS(rand.Reader, rsaKey, crypto.SHA256, sum[:], nil)
	if tok, _ := Parse(signed("RS256") + "." + b(pss)); tok.Verify(Key{RSA: &rsaKey.PublicKey}) == nil {
		t.Error("a PSS signature passed as RS256")
	}

	sum = sha256.Sum256([]byte(signed("ES256")))
	r, s, _ := ecdsa.Sign(rand.Reader, ecKey, sum[:])
	raw := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	tok, err = Parse(signed("ES256") + "." + b(raw))
	if err != nil || tok.Verify(Key{EC: &ecKey.PublicKey}) != nil {
		t.Errorf("ES256 round trip: %v", err)
	}
	der, _ := asn1.Marshal(struct{ R, S *big.Int }{r, s})
	if tok, _ := Parse(signed("ES256") + "." + b(der)); !errors.Is(tok.Verify(Key{EC: &ecKey.PublicKey}), ErrSignature) {
		t.Error("an ASN.1 ECDSA signature was accepted; RFC 7518 3.4 wants R || S")
	}
	// A key of another curve cannot be given to Verify by way of a JWK,
	// and one of the same curve but another owner does not verify.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if !errors.Is(tok.Verify(Key{EC: &other.PublicKey}), ErrSignature) {
		t.Error("another P-256 key verified")
	}

	// A set with several keys and a token without "kid": each fitting key
	// is tried.
	set := Set{Keys: []Key{{ID: "a", RSA: &rsaKey.PublicKey}, {ID: "b", EC: &other.PublicKey}, {ID: "c", EC: &ecKey.PublicKey}}}
	nokid := b([]byte(`{"alg":"ES256"}`)) + "." + b([]byte(`{}`))
	sum = sha256.Sum256([]byte(nokid))
	r, s, _ = ecdsa.Sign(rand.Reader, ecKey, sum[:])
	tok, _ = Parse(nokid + "." + b(append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)))
	if err := set.Verify(tok); err != nil {
		t.Errorf("a token without kid against a set: %v", err)
	}
	if err := (Set{}).Verify(tok); !errors.Is(err, ErrNoKey) {
		t.Errorf("an empty set: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	for _, name := range []string{"rfc7515_a1_jws", "rfc7515_a2_jws", "rfc7515_a3_jws", "rfc7515_a5_jws", "rfc7520_4_1_jws"} {
		var s string
		json.Unmarshal(vectors[name], &s)
		f.Add(s)
	}
	f.Add("..")
	f.Add("e30.e30.")
	key, _ := ParseKey(func() []byte {
		var m map[string]any
		json.Unmarshal(vectors["rfc7515_a2_jwk"], &m)
		for _, p := range []string{"d", "p", "q", "dp", "dq", "qi"} {
			delete(m, p)
		}
		return mustJSON(m)
	}())
	f.Fuzz(func(t *testing.T, compact string) {
		tok, err := Parse(compact)
		if err != nil {
			if tok != nil {
				t.Fatal("a token with an error")
			}
			return
		}
		if tok.Header.Alg != RS256 && tok.Header.Alg != ES256 {
			t.Fatalf("parsed a token with alg %q", tok.Header.Alg)
		}
		// Only the one RFC example verifies with this key.
		if tok.Verify(key) == nil && string(tok.Payload) != examplePayload {
			t.Fatalf("a token verified that is not the example: %q", compact)
		}
		ParseSet([]byte(compact))
	})
}
