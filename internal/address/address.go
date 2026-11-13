// Package address parses and validates mailbox addresses.
//
// Addresses end up as directory names in the store, so validation here is
// also what keeps a hostile address from escaping the data directory.
package address

import (
	"errors"
	"strings"
)

// Address is a validated, lower-cased mailbox address.
type Address struct {
	Local  string
	Domain string
}

const maxAddress = 256 - 2

// ErrInvalid is returned for any address that fails validation.
var ErrInvalid = errors.New("invalid address")

// Parse validates s and returns it in canonical (lower-case) form.
func Parse(s string) (Address, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	at := strings.LastIndexByte(s, '@')
	// RFC 5321 4.5.3.1.3: a path is at most 256 octets with its angle
	// brackets. A longer address could never be a recipient.
	if at <= 0 || at == len(s)-1 || len(s) > maxAddress {
		return Address{}, ErrInvalid
	}
	a := Address{Local: s[:at], Domain: s[at+1:]}
	if !validLocal(a.Local) || !ValidDomain(a.Domain) {
		return Address{}, ErrInvalid
	}
	return a, nil
}

func (a Address) String() string { return a.Local + "@" + a.Domain }

func validLocal(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	if s[0] == '.' || s[len(s)-1] == '.' || strings.Contains(s, "..") {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// ValidDomain reports whether s is a lower-case ASCII host name.
func ValidDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
