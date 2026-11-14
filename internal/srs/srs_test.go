package srs

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func rewriter() (*Rewriter, *time.Time) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	return &Rewriter{Key: []byte("a key for the tests, 32 bytes!!!"), Now: func() time.Time { return now }}, &now
}

// The format of libsrs2 ("The Sender Rewriting Scheme", section 3.1 and
// 3.2; there is no RFC): SRS0=HHH=TT=hostname=local-part@forwarder.
func TestForwardFormat(t *testing.T) {
	r, _ := rewriter()
	got, ok := r.Forward("user@source.example", "forwarder.example")
	if !ok || !regexp.MustCompile(`^SRS0=[a-z2-7]{8}=[a-z2-7]{2}=source\.example=user@forwarder\.example$`).MatchString(got) {
		t.Fatalf("got %q %v", got, ok)
	}
	back, err := r.Reverse(got)
	if err != nil || back != "user@source.example" {
		t.Fatalf("Reverse(%q) = %q, %v", got, back, err)
	}
	// The timestamp is the day modulo 1024 in base32: 2026-11-14 is day
	// 20771, 20771 % 1024 = 291 = 9*32 + 3.
	if ts := strings.Split(got, "=")[2]; ts != "jd" {
		t.Errorf("timestamp %q, want jd", ts)
	}
	// A local part with "=" and other characters survives.
	for _, sender := range []string{"a=b=c@x.example", "Mixed.Case+tag@Source.Example", `"quoted part"@x.example`} {
		if strings.Contains(sender, " ") {
			if _, ok := r.Forward(sender, "f.example"); ok {
				t.Errorf("%q: rewritten, though it has white space", sender)
			}
			continue
		}
		fwd, ok := r.Forward(sender, "f.example")
		if !ok {
			t.Fatalf("%q not rewritten", sender)
		}
		if back, err := r.Reverse(fwd); err != nil || back != sender {
			t.Errorf("%q came back as %q, %v", sender, back, err)
		}
	}
}

// What cannot be rewritten is sent with the null sender by the caller.
func TestForwardRefuses(t *testing.T) {
	r, _ := rewriter()
	for _, sender := range []string{"", "nodomain", "@x.example", "user@", strings.Repeat("a", 50) + "@a-long-domain-name.example"} {
		if got, ok := r.Forward(sender, "f.example"); ok {
			t.Errorf("Forward(%q) = %q", sender, got)
		}
	}
	if _, ok := r.Forward("user@x.example", ""); ok {
		t.Error("rewritten without a domain of our own")
	}
}

// Section 3.3: an address that was rewritten once becomes SRS1, which
// keeps the first forwarder's address and points back at it; rewriting
// again does not grow it.
func TestSRS1(t *testing.T) {
	first, _ := rewriter()
	second := &Rewriter{Key: []byte("the second forwarder's own key.."), Now: first.Now}
	third := &Rewriter{Key: []byte("and a third forwarder's key....."), Now: first.Now}

	a, _ := first.Forward("user@source.example", "first.example")
	b, ok := second.Forward(a, "second.example")
	if !ok || !regexp.MustCompile(`^SRS1=[a-z2-7]{8}=first\.example==[a-z2-7]{8}=jd=source\.example=user@second\.example$`).MatchString(b) {
		t.Fatalf("SRS1: %q %v", b, ok)
	}
	c, ok := third.Forward(b, "third.example")
	if !ok || len(c)-len("third.example") != len(b)-len("second.example") || !strings.HasSuffix(c, "@third.example") {
		t.Fatalf("a third hop grew the address: %q", c)
	}
	// The bounce travels third -> first -> source.
	back, err := third.Reverse(c)
	if err != nil || back != a {
		t.Fatalf("third.Reverse = %q, %v; want %q", back, err, a)
	}
	back, err = second.Reverse(b)
	if err != nil || back != a {
		t.Fatalf("second.Reverse = %q, %v; want %q", back, err, a)
	}
	if orig, err := first.Reverse(back); err != nil || orig != "user@source.example" {
		t.Fatalf("first.Reverse = %q, %v", orig, err)
	}
	// The second forwarder's hash is its own.
	if _, err := third.Reverse(b); !errors.Is(err, ErrHash) {
		t.Errorf("another server's SRS1 address: %v", err)
	}
}

// The hash is what keeps the reverse path from being an open relay.
func TestReverseChecks(t *testing.T) {
	r, now := rewriter()
	good, _ := r.Forward("user@source.example", "f.example")
	other := &Rewriter{Key: []byte("some other server's key........."), Now: r.Now}
	forged, _ := other.Forward("victim@elsewhere.example", "f.example")
	tampered := strings.Replace(good, "source.example=user", "elsewhere.example=victim", 1)
	for addr, want := range map[string]error{
		good:                        nil,
		strings.ToLower(good):       nil, // a server on the way lowered the case
		strings.ToUpper(good):       nil,
		forged:                      ErrHash,
		tampered:                    ErrHash,
		"user@f.example":            ErrNotSRS,
		"SRS0=@f.example":           ErrNotSRS,
		"SRS0=abc@f.example":        ErrNotSRS,
		"SRS0=a=b=c=@f.example":     ErrNotSRS,
		"SRS1=abc=host@f.example":   ErrNotSRS,
		"SRS1=abc===rest@f.example": ErrNotSRS,
		"":                          ErrNotSRS,
	} {
		_, err := r.Reverse(addr)
		if !errors.Is(err, want) {
			t.Errorf("Reverse(%q): %v, want %v", addr, err, want)
		}
	}
	// Upper case comes back lowered: the user part is as the bounce had it.
	if back, _ := r.Reverse(strings.ToUpper(good)); back != "USER@SOURCE.EXAMPLE" {
		t.Errorf("upper case: %q", back)
	}

	// An address works for MaxAge and not after, also across the wrap of
	// the timestamp.
	*now = now.Add(MaxAge)
	if _, err := r.Reverse(good); err != nil {
		t.Errorf("after MaxAge exactly: %v", err)
	}
	*now = now.Add(24 * time.Hour)
	if _, err := r.Reverse(good); !errors.Is(err, ErrExpired) {
		t.Errorf("a day past MaxAge: %v", err)
	}
	*now = time.Date(2026, 11, 13, 23, 0, 0, 0, time.UTC) // a node whose clock is a day behind
	if _, err := r.Reverse(good); err != nil {
		t.Errorf("made a day ahead of this clock: %v", err)
	}
	*now = time.Unix(1023*86400, 0)
	wrap, _ := r.Forward("user@source.example", "f.example")
	*now = now.Add(3 * 24 * time.Hour)
	if _, err := r.Reverse(wrap); err != nil {
		t.Errorf("across the wrap of the timestamp: %v", err)
	}
}

func FuzzReverse(f *testing.F) {
	r, _ := rewriter()
	good, _ := r.Forward("user@source.example", "f.example")
	for _, s := range []string{good, "SRS1=x=y==z@f", "SRS0====@", "srs0=a=b=c=d", "\xff@\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		// Reverse never panics, and what Forward makes always comes back.
		r.Reverse(s)
		if fwd, ok := r.Forward(s, "f.example"); ok {
			back, err := r.Reverse(fwd)
			if err != nil {
				t.Fatalf("Forward(%q) = %q does not reverse: %v", s, fwd, err)
			}
			local, _, _ := split(s)
			if !IsSRS(local) && back != s {
				t.Fatalf("%q came back as %q", s, back)
			}
		}
	})
}

// A rotation of the master key changes Key. Addresses made under the old
// key are still out there, in mail that may bounce for days: they are read
// as long as the old key is in OldKeys, and new ones are made under Key.
func TestOldKeys(t *testing.T) {
	old, _ := rewriter()
	made, _ := old.Forward("user@source.example", "f.example")
	chained, _ := old.Forward("SRS0=abcd=jd=origin.example=someone@first.example", "f.example")

	now := &Rewriter{Key: []byte("the key after the rotation......"), OldKeys: [][]byte{old.Key}, Now: old.Now}
	for _, addr := range []string{made, chained} {
		if _, err := now.Reverse(addr); err != nil {
			t.Errorf("Reverse(%q) with the old key accepted: %v", addr, err)
		}
	}
	fresh, _ := now.Forward("user@source.example", "f.example")
	if fresh == made {
		t.Error("the new key makes the same address as the old one")
	}
	if _, err := old.Reverse(fresh); !errors.Is(err, ErrHash) {
		t.Errorf("the old key reads an address of the new one: %v", err)
	}
	// Once the old key is pruned its addresses are forgeries like any other.
	pruned := &Rewriter{Key: now.Key, Now: old.Now}
	for _, addr := range []string{made, chained} {
		if _, err := pruned.Reverse(addr); !errors.Is(err, ErrHash) {
			t.Errorf("Reverse(%q) without the old key: %v", addr, err)
		}
	}
	if back, err := pruned.Reverse(fresh); err != nil || back != "user@source.example" {
		t.Errorf("Reverse of a new address: %q %v", back, err)
	}
}
