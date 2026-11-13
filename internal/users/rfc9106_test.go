package users

// Tests written from RFC 9106 (Argon2) and from the PHC string format that
// stores its output (github.com/P-H-C/phc-string-format).

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// RFC 9106 section 4, "Parameter Choice": "If much less memory is
// available, a uniformly safe option is Argon2id with t=3 iterations, p=4
// lanes, m=2^(16) (64 MiB of RAM), 128-bit salt, and 256-bit tag size. This
// is the SECOND RECOMMENDED option." New hashes use exactly that, and the
// version is 0x13 (19), the one the RFC specifies.
func TestRFC9106_4_ParameterChoice(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=4" {
		t.Fatalf("hash %q", h)
	}
	salt, err1 := b64.DecodeString(parts[4])
	tag, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(salt)*8 != 128 || len(tag)*8 != 256 {
		t.Errorf("salt of %d bits, tag of %d bits (%v, %v)", len(salt)*8, len(tag)*8, err1, err2)
	}
	if argon2.Version != 0x13 {
		t.Errorf("x/crypto implements Argon2 version %#x", argon2.Version)
	}
}

// The test vector of RFC 9106 5.3 (Argon2id) has a secret value K and
// associated data X. x/crypto's exported argon2.IDKey takes neither, as a
// password hash does not use them, so that vector cannot be run from here;
// x/crypto runs it in its own tests (TestArgon2 in argon2_test.go, through
// the unexported deriveKey: "go test golang.org/x/crypto/argon2").
//
// What we can check is the path we use, without K and X, against the test
// vectors of the reference implementation that the RFC describes
// (phc-winner-argon2, src/test.c, version 0x13). Each is a raw tag and its
// PHC string; the PHC strings go through Verify.
func TestRFC9106_ReferenceVectors(t *testing.T) {
	for _, v := range []struct {
		t, mLog2    uint32
		p           uint8
		pass, salt  string
		tag, encTag string
	}{
		{2, 16, 1, "password", "somesalt", "09316115d5cf24ed5a15a31a3ba326e5cf32edc24702987c02b6566f61913cf7", "CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"},
		{2, 18, 1, "password", "somesalt", "78fe1ec91fb3aa5657d72e710854e4c3d9b9198c742f9616c2f085bed95b2e8c", "eP4eyR+zqlZX1y5xCFTkw9m5GYx0L5YWwvCFvtlbLow"},
		{2, 8, 1, "password", "somesalt", "9dfeb910e80bad0311fee20f9c0e2b12c17987b4cac90c2ef54d5b3021c68bfe", "nf65EOgLrQMR/uIPnA4rEsF5h7TKyQwu9U1bMCHGi/4"},
		{2, 8, 2, "password", "somesalt", "6d093c501fd5999645e0ea3bf620d7b8be7fd2db59c20d9fff9539da2bf57037", "bQk8UB/VmZZF4Oo79iDXuL5/0ttZwg2f/5U52iv1cDc"},
		{1, 16, 1, "password", "somesalt", "f6a5adc1ba723dddef9b5ac1d464e180fcd9dffc9d1cbf76cca2fed795d9ca98", "9qWtwbpyPd3vm1rB1GThgPzZ3/ydHL92zKL+15XZypg"},
		{4, 16, 1, "password", "somesalt", "9025d48e68ef7395cca9079da4c4ec3affb3c8911fe4f86d1a2520856f63172c", "kCXUjmjvc5XMqQedpMTsOv+zyJEf5PhtGiUghW9jFyw"},
		{2, 16, 1, "differentpassword", "somesalt", "0b84d652cf6b0c4beaef0dfe278ba6a80df6696281d7e0d2891b817d8c458fde", "C4TWUs9rDEvq7w3+J4umqA32aWKB1+DSiRuBfYxFj94"},
		{2, 16, 1, "password", "diffsalt", "bdf32b05ccc42eb15d58fd19b1f856b113da1e9a5874fdcc544308565aa8141c", "vfMrBczELrFdWP0ZsfhWsRPaHppYdP3MVEMIVlqoFBw"},
	} {
		m := uint32(1) << v.mLog2
		got := argon2.IDKey([]byte(v.pass), []byte(v.salt), v.t, m, v.p, 32)
		if hex.EncodeToString(got) != v.tag {
			t.Errorf("t=%d m=2^%d p=%d %q %q: tag %x, want %s", v.t, v.mLog2, v.p, v.pass, v.salt, got, v.tag)
		}
		encoded := fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", m, v.t, v.p, b64.EncodeToString([]byte(v.salt)), v.encTag)
		if !verifyPassword(encoded, v.pass) {
			t.Errorf("the reference PHC string %s does not verify", encoded)
		}
		if verifyPassword(encoded, v.pass+"x") {
			t.Errorf("%s verifies a wrong password", encoded)
		}
	}
}

// phcArgon2id is the PHC string format for Argon2id as the reference
// implementation writes it: the parameters m, t, p in that order as decimal
// numbers, then salt and tag in base64 without padding.
var phcArgon2id = regexp.MustCompile(`^\$argon2id\$v=19\$m=[1-9][0-9]*,t=[1-9][0-9]*,p=[1-9][0-9]*\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$`)

// The PHC string format: what Hash stores follows it, and Verify reads it
// back.
func TestPHCStringFormat(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !phcArgon2id.MatchString(h) {
		t.Errorf("%q is not a PHC string", h)
	}
	// 16 and 32 bytes in base64 without padding are 22 and 43 characters.
	parts := strings.Split(h, "$")
	if len(parts[4]) != 22 || len(parts[5]) != 43 {
		t.Errorf("salt of %d and tag of %d characters", len(parts[4]), len(parts[5]))
	}
	if !Verify(h, "correct horse battery staple") {
		t.Error("the stored string does not verify")
	}
}

// What is not a PHC string for Argon2id version 19, or is outside the
// parameter ranges of RFC 9106 3.1, never verifies. The base is a reference
// vector, so each case fails for the one thing that is changed.
func TestPHCStringRejects(t *testing.T) {
	const salt, tag = "c29tZXNhbHQ", "nf65EOgLrQMR/uIPnA4rEsF5h7TKyQwu9U1bMCHGi/4"
	good := "$argon2id$v=19$m=256,t=2,p=1$" + salt + "$" + tag
	if !verifyPassword(good, "password") {
		t.Fatal("the base string does not verify")
	}
	for name, h := range map[string]string{
		"argon2i":                  "$argon2i$v=19$m=256,t=2,p=1$" + salt + "$" + tag,
		"argon2d":                  "$argon2d$v=19$m=256,t=2,p=1$" + salt + "$" + tag,
		"upper case":               "$ARGON2ID$v=19$m=256,t=2,p=1$" + salt + "$" + tag,
		"version 16":               "$argon2id$v=16$m=256,t=2,p=1$" + salt + "$" + tag,
		"version with junk":        "$argon2id$v=19x$m=256,t=2,p=1$" + salt + "$" + tag,
		"no version":               "$argon2id$m=256,t=2,p=1$" + salt + "$" + tag,
		"parameters out of order":  "$argon2id$v=19$t=2,m=256,p=1$" + salt + "$" + tag,
		"a parameter missing":      "$argon2id$v=19$m=256,t=2$" + salt + "$" + tag,
		"a parameter too many":     "$argon2id$v=19$m=256,t=2,p=1,x=1$" + salt + "$" + tag,
		"leading zero":             "$argon2id$v=19$m=0256,t=2,p=1$" + salt + "$" + tag,
		"plus sign":                "$argon2id$v=19$m=+256,t=2,p=1$" + salt + "$" + tag,
		"hexadecimal":              "$argon2id$v=19$m=0x100,t=2,p=1$" + salt + "$" + tag,
		"trailing junk in a value": "$argon2id$v=19$m=256,t=2,p=1x$" + salt + "$" + tag,
		"space":                    "$argon2id$v=19$m=256, t=2,p=1$" + salt + "$" + tag,
		"padded salt":              "$argon2id$v=19$m=256,t=2,p=1$" + salt + "=$" + tag,
		"padded tag":               "$argon2id$v=19$m=256,t=2,p=1$" + salt + "$" + tag + "=",
		"URL-safe base64":          "$argon2id$v=19$m=256,t=2,p=1$" + salt + "$" + strings.ReplaceAll(tag, "/", "_"),
		"no tag":                   "$argon2id$v=19$m=256,t=2,p=1$" + salt + "$",
		"no salt":                  "$argon2id$v=19$m=256,t=2,p=1$$" + tag,
		"trailing field":           good + "$",
		"trailing newline":         good + "\n",
		"leading text":             "x" + good,
		// RFC 9106 3.1: t from 1, p from 1, m from 8*p, a tag of 4 bytes
		// or more.
		"t=0":            "$argon2id$v=19$m=256,t=0,p=1$" + salt + "$" + tag,
		"p=0":            "$argon2id$v=19$m=256,t=2,p=0$" + salt + "$" + tag,
		"p=256":          "$argon2id$v=19$m=4096,t=2,p=256$" + salt + "$" + tag,
		"m below 8p":     "$argon2id$v=19$m=7,t=2,p=1$" + salt + "$" + tag,
		"tag of 3 bytes": "$argon2id$v=19$m=256,t=2,p=1$" + salt + "$AAAA",
		// Our own ceilings: a stored hash must not be able to ask for
		// gigabytes or minutes.
		"m of 4 TiB":     "$argon2id$v=19$m=4294967295,t=2,p=1$" + salt + "$" + tag,
		"t of 4 million": "$argon2id$v=19$m=256,t=4000000,p=1$" + salt + "$" + tag,
		"m overflowing":  "$argon2id$v=19$m=4294967552,t=2,p=1$" + salt + "$" + tag,
	} {
		if verifyPassword(h, "password") {
			t.Errorf("%s: %q verifies", name, h)
		}
	}
}

func FuzzVerifyPassword(f *testing.F) {
	f.Add("$argon2id$v=19$m=256,t=2,p=1$c29tZXNhbHQ$nf65EOgLrQMR/uIPnA4rEsF5h7TKyQwu9U1bMCHGi/4")
	f.Add("$argon2id$v=19$m=8,t=1,p=1$AAAAAAAAAAA$AAAAAA")
	f.Fuzz(func(t *testing.T, s string) {
		// Keep the fuzzer from spending its time in Argon2 itself.
		if strings.Contains(s, "m=") && !strings.Contains(s, "m=8,") && !strings.Contains(s, "m=256,") {
			return
		}
		if verifyPassword(s, "password") && !phcArgon2id.MatchString(s) {
			t.Errorf("%q verifies but is not a PHC string", s)
		}
	})
}
