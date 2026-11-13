package users

import (
	"errors"
	"strings"
	"testing"
)

func TestHashVerify(t *testing.T) {
	if _, err := Hash("short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("Hash weak password: %v", err)
	}
	mike, err := Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	anna, err := Hash("battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(mike, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Errorf("hash = %q", mike)
	}
	if again, _ := Hash("correct horse"); again == mike {
		t.Error("two hashes of one password are equal: no salt")
	}

	if !Verify(mike, "correct horse") {
		t.Error("correct password rejected")
	}
	if Verify(mike, "battery staple") || Verify(anna, "correct horse") {
		t.Error("another user's password accepted")
	}
	if Verify("", "correct horse") || Verify("", "robinmail-dummy-password") {
		t.Error("unknown user accepted")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for _, h := range []string{
		"plaintext", "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=0,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=0$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=4294967295,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=4000000,p=2$c2FsdA$aGFzaA",
	} {
		if verifyPassword(h, "password") {
			t.Errorf("verifyPassword(%q) accepted", h)
		}
	}
}
