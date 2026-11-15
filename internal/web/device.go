package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Known devices (issue 41). The limit of ten wrong passwords per address
// is what stops guessing from many client addresses, but it also lets
// anybody who knows an address keep its owner out of the web login. A
// browser that completed a login for an address gets a cookie that says
// so. With it, a password login for that address is counted under the
// cookie and the client address instead of under the address: strangers
// still use up the address's limit, the owner's own browser keeps working.
//
// The cookie is no credential: it only changes which counter a wrong
// password goes to. It holds the address, when it was made, and an HMAC
// under a key derived from the master key, so every node can check it and
// nobody can make one. One cookie has a limit of its own (as many failures
// as the address), so a stolen one is no licence to guess.

// DeviceTTL is how long a known-device cookie lasts. Every login renews it.
const DeviceTTL = 180 * 24 * time.Hour

func deviceCookie(r *http.Request) string { return sessionCookie(r) + "-device" }

// deviceKey is the key of the shared counter of one device cookie.
func deviceKey(tag string) string { return "d:" + tag }

func (s *Server) limitDevice(tag string) limit {
	return limit{s.byDevice, tag, deviceKey(tag), maxFailsPerAddr}
}

func (s *Server) deviceMAC(key []byte, address string, made int64) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("robinmail device\x00"))
	m.Write([]byte(address))
	m.Write([]byte{0})
	m.Write([]byte(strconv.FormatInt(made, 10)))
	return m.Sum(nil)[:16]
}

// newDevice is the cookie value for a browser that just logged in as
// address.
func (s *Server) newDevice(address string) string {
	made := s.Now().Unix()
	mac := s.deviceMAC(s.DeviceKeys[0], address, made)
	return strconv.FormatInt(made, 10) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(address)) + "." +
		base64.RawURLEncoding.EncodeToString(mac)
}

// knownDevice checks the device cookie of r for address and returns its
// tag, the name its failures are counted under; "" when r has no cookie
// for address that holds.
func (s *Server) knownDevice(r *http.Request, address string) string {
	if len(s.DeviceKeys) == 0 {
		return ""
	}
	c, err := r.Cookie(deviceCookie(r))
	if err != nil || len(c.Value) > 600 {
		return ""
	}
	madeS, rest, ok1 := strings.Cut(c.Value, ".")
	addrB, macB, ok2 := strings.Cut(rest, ".")
	if !ok1 || !ok2 {
		return ""
	}
	made, err := strconv.ParseInt(madeS, 10, 64)
	addr, err2 := base64.RawURLEncoding.DecodeString(addrB)
	mac, err3 := base64.RawURLEncoding.DecodeString(macB)
	if err != nil || err2 != nil || err3 != nil || string(addr) != address {
		return ""
	}
	if age := s.Now().Sub(time.Unix(made, 0)); age < -time.Minute || age > DeviceTTL {
		return ""
	}
	// Made under the current master key or one replaced since.
	for _, k := range s.DeviceKeys {
		if hmac.Equal(mac, s.deviceMAC(k, address, made)) {
			return hex.EncodeToString(mac)
		}
	}
	return ""
}

// rememberDevice gives the browser of a completed login a device cookie
// for the account's address.
func (s *Server) rememberDevice(w http.ResponseWriter, r *http.Request, address string) {
	if len(s.DeviceKeys) == 0 {
		return
	}
	// In the spelling the login counts failures under.
	key, _, _ := limiterKey(address)
	s.setCookie(w, r, deviceCookie(r), s.newDevice(key), DeviceTTL)
}
