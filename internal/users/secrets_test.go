package users

import (
	"regexp"
	"strings"
	"testing"
)

func TestAppPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		p, err := NewAppPassword()
		if err != nil || !regexp.MustCompile(`^([a-z2-7]{4}-){7}[a-z2-7]{4}$`).MatchString(p) || seen[p] {
			t.Fatalf("app password %q %v", p, err)
		}
		seen[p] = true
		n, ok := NormalizeAppPassword(p)
		if !ok || len(n) != AppPasswordLen || strings.Contains(n, "-") {
			t.Fatalf("normalize(%q) = %q %v", p, n, ok)
		}
		// However a person or a client writes it down, it is the same.
		for _, typed := range []string{n, strings.ToUpper(p), strings.ReplaceAll(p, "-", " "), " " + p + " "} {
			if got, ok := NormalizeAppPassword(typed); !ok || got != n {
				t.Errorf("normalize(%q) = %q %v, want %q", typed, got, ok, n)
			}
		}
	}
	for _, not := range []string{"", "correct horse battery staple", "abcd-efgh", strings.Repeat("a", 31), strings.Repeat("a", 33),
		strings.Repeat("a", 31) + "1", strings.Repeat("a", 31) + "é", strings.Repeat("a", 31) + "\n", strings.Repeat("-", 500)} {
		if _, ok := NormalizeAppPassword(not); ok {
			t.Errorf("%q passes for an app password", not)
		}
	}
	a, b := HashAppPassword(strings.Repeat("a", 32)), HashAppPassword(strings.Repeat("b", 32))
	if len(a) != 64 || a == b {
		t.Errorf("hashes %q %q", a, b)
	}
}

func TestRecoveryCode(t *testing.T) {
	c, err := NewRecoveryCode()
	if err != nil || !regexp.MustCompile(`^([a-z2-7]{4}-){3}[a-z2-7]{4}$`).MatchString(c) {
		t.Fatalf("recovery code %q %v", c, err)
	}
	n, ok := NormalizeRecoveryCode(strings.ToUpper(c))
	if !ok || len(n) != RecoveryCodeLen {
		t.Fatalf("normalize: %q %v", n, ok)
	}
	// The same characters are not the same secret in the other role.
	if HashRecoveryCode(n) == HashAppPassword(n) {
		t.Error("a recovery code and an app password hash alike")
	}
	if _, ok := NormalizeRecoveryCode("123456"); ok {
		t.Error("a TOTP code passes for a recovery code")
	}
	if _, ok := NormalizeAppPassword(c); ok {
		t.Error("a recovery code passes for an app password")
	}
}
