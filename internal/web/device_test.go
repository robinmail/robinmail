package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) { r.AddCookie(c) }
}

// Issue 41: anybody who knows an address could keep its owner out of the
// web login with ten wrong passwords every 15 minutes. A browser that
// logged in before keeps working; everybody else is still stopped, and a
// device cookie is no licence to guess.
func TestKnownDeviceIsNotLockedOut(t *testing.T) {
	st := newSite(t)
	st.s.DeviceKeys = [][]byte{[]byte("current key 0123456789abcdef0123")}
	const name = "__Host-robinmail-device"

	rec := st.login(tlsBase, "Anna@Example.Test", "correct horse", from("203.0.113.9:1000"))
	device := cookieNamed(rec, name)
	if rec.Code != 200 || device == nil {
		t.Fatalf("login: %d, device cookie %v", rec.Code, device)
	}
	if !device.HttpOnly || !device.Secure || device.SameSite != http.SameSiteStrictMode || device.MaxAge < int(DeviceTTL/time.Second)-1 {
		t.Errorf("device cookie: %+v", device)
	}

	// Strangers use up the address's limit.
	for i := range maxFailsPerAddr {
		if rec := st.login(tlsBase, "anna@example.test", "guess", from(fmt.Sprintf("198.51.100.%d:1000", i))); rec.Code != 401 {
			t.Fatalf("stranger %d: %d", i, rec.Code)
		}
	}
	// Without the cookie the address is locked, for strangers and for the
	// owner on a new browser alike.
	if rec := st.login(tlsBase, "anna@example.test", "correct horse", from("203.0.113.9:1000")); rec.Code != 429 {
		t.Fatalf("without the cookie: %d", rec.Code)
	}
	// With it, from anywhere, the owner gets in.
	rec = st.login(tlsBase, "anna@example.test", "correct horse", from("192.0.2.44:1000"), withCookie(device))
	if rec.Code != 200 {
		t.Fatalf("known device: %d", rec.Code)
	}
	if renewed := cookieNamed(rec, name); renewed == nil {
		t.Error("the login did not renew the device cookie")
	}
	// The cookie is for anna only: bob, locked out too, is not let in by it.
	for i := range maxFailsPerAddr {
		st.login(tlsBase, "bob@example.test", "guess", from(fmt.Sprintf("198.51.100.%d:1000", 100+i)))
	}
	if rec := st.login(tlsBase, "bob@example.test", "battery staple", from("192.0.2.44:1000"), withCookie(device)); rec.Code != 429 {
		t.Errorf("anna's cookie for bob: %d", rec.Code)
	}
	// A cookie with the address changed is no cookie.
	forged := *device
	parts := strings.SplitN(forged.Value, ".", 3)
	forged.Value = parts[0] + ".Ym9iQGV4YW1wbGUudGVzdA." + parts[2] // bob@example.test
	if rec := st.login(tlsBase, "bob@example.test", "battery staple", from("192.0.2.44:1000"), withCookie(&forged)); rec.Code != 429 {
		t.Errorf("forged cookie: %d", rec.Code)
	}

	// Somebody who has the cookie (a stolen one) gets the limit of one
	// address: ten wrong passwords, then 429, also from other client
	// addresses. The owner's own failures do not lock the address for
	// others' known devices either way.
	for i := range maxFailsPerAddr {
		if rec := st.login(tlsBase, "anna@example.test", "guess", from(fmt.Sprintf("192.0.2.%d:1000", 100+i)), withCookie(device)); rec.Code != 401 {
			t.Fatalf("guess %d with the cookie: %d", i, rec.Code)
		}
	}
	checks := st.verified.Load()
	if rec := st.login(tlsBase, "anna@example.test", "correct horse", from("192.0.2.200:1000"), withCookie(device)); rec.Code != 429 {
		t.Errorf("after %d failures with the cookie: %d", maxFailsPerAddr, rec.Code)
	}
	if st.verified.Load() != checks {
		t.Error("a refused login still cost a password check")
	}
}

// A device cookie expires, and one made under a master key that has been
// replaced since still holds while that key is listed.
func TestKnownDeviceKeysAndExpiry(t *testing.T) {
	st := newSite(t)
	oldKey, newKey := []byte("old key 0123456789abcdef01234567"), []byte("new key 0123456789abcdef01234567")
	st.s.DeviceKeys = [][]byte{oldKey}
	rec := st.login(tlsBase, "anna@example.test", "correct horse")
	device := cookieNamed(rec, "__Host-robinmail-device")
	if device == nil {
		t.Fatal("no device cookie")
	}
	req := func() *http.Request {
		r := httptest.NewRequest("POST", tlsBase+"/auth/login", nil)
		r.AddCookie(device)
		return r
	}
	if st.s.knownDevice(req(), "anna@example.test") == "" {
		t.Fatal("fresh cookie refused")
	}
	st.s.DeviceKeys = [][]byte{newKey, oldKey}
	if st.s.knownDevice(req(), "anna@example.test") == "" {
		t.Error("cookie under the previous key refused")
	}
	st.s.DeviceKeys = [][]byte{newKey}
	if st.s.knownDevice(req(), "anna@example.test") != "" {
		t.Error("cookie under a key no longer listed taken")
	}
	st.s.DeviceKeys = [][]byte{oldKey}
	st.clock = st.clock.Add(DeviceTTL + time.Hour)
	if st.s.knownDevice(req(), "anna@example.test") != "" {
		t.Error("expired cookie taken")
	}
	// Without keys there are no device cookies.
	st.s.DeviceKeys = nil
	if c := cookieNamed(st.login(tlsBase, "anna@example.test", "correct horse"), "__Host-robinmail-device"); c != nil {
		t.Error("device cookie without keys")
	}
}
