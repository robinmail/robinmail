package totp

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"hash"
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 4226 Appendix D, "HOTP Algorithm: Test Values": the secret
// "12345678901234567890" and the HOTP values for counters 0 to 9.
func TestRFC4226AppendixD(t *testing.T) {
	secret := []byte("12345678901234567890")
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for count, w := range want {
		if got := HOTP(sha1.New, secret, uint64(count), 6); got != w {
			t.Errorf("count %d: %s, want %s", count, got, w)
		}
	}
}

// RFC 4226 5.4, "Example of HOTP Computation for Digit = 6": the worked
// example's HMAC ends in 0x5a, so the offset is 10 and the four bytes
// there are 0x50ef7f19, which is 1357872921 and gives 872921. The key
// that produces that HMAC is not given, so this test checks the
// truncation on the vectors of Appendix D instead, whose table lists the
// truncated values in decimal: for count 0 it is 1284755224.
func TestRFC4226Truncation(t *testing.T) {
	secret := []byte("12345678901234567890")
	// Nine digits keep all but the first of the ten decimal digits of the
	// 31-bit value.
	for count, dec := range []string{"1284755224", "1094287082", "137359152", "1726969429", "1640338314",
		"868254676", "1918287922", "82162583", "673399871", "645520489"} {
		want := dec
		for len(want) < 9 {
			want = "0" + want
		}
		want = want[len(want)-9:]
		if got := HOTP(sha1.New, secret, uint64(count), 9); got != want {
			t.Errorf("count %d with 9 digits: %s, want %s", count, got, want)
		}
	}
}

// RFC 6238 Appendix B, "Test Vectors": eight digits, T0 = 0, a step of 30
// seconds, for SHA-1, SHA-256 and SHA-512. The seeds are the ASCII string
// "12345678901234567890" repeated to 20, 32 and 64 bytes, as in the
// reference implementation of Appendix A (the erratum to the RFC says so
// too; the text alone suggests one seed for all three).
func TestRFC6238AppendixB(t *testing.T) {
	seed := func(n int) []byte { return []byte(strings.Repeat("1234567890", 7)[:n]) }
	modes := []struct {
		name string
		h    func() hash.Hash
		key  []byte
	}{{"SHA1", sha1.New, seed(20)}, {"SHA256", sha256.New, seed(32)}, {"SHA512", sha512.New, seed(64)}}
	vectors := []struct {
		unix int64
		step string // the value of T in the table, hex
		want [3]string
	}{
		{59, "0000000000000001", [3]string{"94287082", "46119246", "90693936"}},
		{1111111109, "00000000023523EC", [3]string{"07081804", "68084774", "25091201"}},
		{1111111111, "00000000023523ED", [3]string{"14050471", "67062674", "99943326"}},
		{1234567890, "000000000273EF07", [3]string{"89005924", "91819424", "93441116"}},
		{2000000000, "0000000003F940AA", [3]string{"69279037", "90698825", "38618901"}},
		{20000000000, "0000000027BC86AA", [3]string{"65353130", "77737706", "47863826"}},
	}
	for _, v := range vectors {
		step := Step(time.Unix(v.unix, 0))
		if got := strings.ToUpper(hex16(step)); got != v.step {
			t.Errorf("time %d: T = %s, want %s", v.unix, got, v.step)
		}
		for i, m := range modes {
			if got := HOTP(m.h, m.key, uint64(step), 8); got != v.want[i] {
				t.Errorf("time %d %s: %s, want %s", v.unix, m.name, got, v.want[i])
			}
		}
	}
}

func hex16(n int64) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		b[i] = digits[n&15]
		n >>= 4
	}
	return string(b)
}

// What robinmail uses is the SHA-1 column cut to six digits.
func TestCode(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 2000000000: "279037"} {
		if got := Code(secret, Step(time.Unix(unix, 0))); got != want {
			t.Errorf("time %d: %s, want %s", unix, got, want)
		}
	}
}

// RFC 6238 5.2: a code is accepted for the current step and, for network
// delay and clocks that are off, one step to either side; not further.
func TestMatch(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	cur := Step(now)
	for d := int64(-3); d <= 3; d++ {
		step, ok := Match(secret, Code(secret, cur+d), now)
		if wantOK := d >= -Skew && d <= Skew; ok != wantOK || (ok && step != cur+d) {
			t.Errorf("the code of step %+d: step %d ok %v", d, step-cur, ok)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "05047 ", " 050471", "050471\n"} {
		if _, ok := Match(secret, bad, now); ok {
			t.Errorf("%q matched", bad)
		}
	}
	if _, ok := Match([]byte("another secret 12345"), Code(secret, cur), now); ok {
		t.Error("a code matched under another secret")
	}
	// The first step there is: nothing before time 0.
	if step, ok := Match(secret, Code(secret, 0), time.Unix(10, 0)); !ok || step != 0 {
		t.Errorf("step 0: %d %v", step, ok)
	}
}

// The otpauth URI, as the Key Uri Format describes it: type totp, a label
// "issuer:account", the secret in base32 without padding, the issuer again
// as a parameter.
func TestURI(t *testing.T) {
	secret := []byte("12345678901234567890")
	got := URI("mail.example.org", "anna@example.org", secret)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/mail.example.org:anna@example.org" ||
		q.Get("secret") != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" || q.Get("issuer") != "mail.example.org" ||
		q.Get("algorithm") != "SHA1" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Errorf("URI: %s", got)
	}
	if strings.Contains(got, "=&") || strings.Contains(q.Get("secret"), "=") {
		t.Errorf("padding in the secret: %s", got)
	}
	// A label with characters that mean something in a URI.
	u, err = url.Parse(URI("Mail & Co", "a b@example.org", secret))
	if err != nil || u.Path != "/Mail & Co:a b@example.org" || u.Query().Get("issuer") != "Mail & Co" {
		t.Errorf("escaping: %v %v", u, err)
	}
}

func TestNewSecret(t *testing.T) {
	a, err := NewSecret()
	b, _ := NewSecret()
	if err != nil || len(a) != SecretSize || string(a) == string(b) {
		t.Errorf("secrets: %x %x %v", a, b, err)
	}
	if got := EncodeSecret(a); len(got) != 32 {
		t.Errorf("a 20-byte secret in base32: %q", got)
	}
}
