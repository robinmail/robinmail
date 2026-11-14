package imap

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Modified UTF-7, the encoding of mailbox names in IMAP4rev1 (RFC 3501
// 5.1.3): printable ASCII stands for itself except "&", which is "&-";
// everything else is UTF-16 in a base64 that uses "," for "/" and has no
// padding, between "&" and "-".

var mbase64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

var errUTF7 = errors.New("imap: not modified UTF-7")

// encodeUTF7 writes a UTF-8 name in modified UTF-7.
func encodeUTF7(s string) string {
	direct := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e || s[i] == '&' {
			direct = false
			break
		}
	}
	if direct {
		return s
	}
	var b strings.Builder
	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		units := utf16.Encode(run)
		raw := make([]byte, 2*len(units))
		for i, u := range units {
			raw[2*i], raw[2*i+1] = byte(u>>8), byte(u)
		}
		b.WriteByte('&')
		b.WriteString(mbase64.EncodeToString(raw))
		b.WriteByte('-')
		run = run[:0]
	}
	for _, r := range s {
		switch {
		case r == '&':
			flush()
			b.WriteString("&-")
		case r >= 0x20 && r <= 0x7e:
			flush()
			b.WriteRune(r)
		default:
			run = append(run, r)
		}
	}
	flush()
	return b.String()
}

// decodeUTF7 reads a modified UTF-7 name. It accepts only what
// encodeUTF7 writes, so every name has one spelling: RFC 3501 5.1.3 forbids
// base64 for printable ASCII, a shift that ends where the next begins
// ("&AAA-&AAA-"), and a name that ends in the middle of a shift.
func decodeUTF7(s string) (string, error) {
	if strings.IndexByte(s, '&') < 0 {
		for i := 0; i < len(s); i++ {
			if s[i] < 0x20 || s[i] > 0x7e {
				return "", errUTF7
			}
		}
		return s, nil
	}
	var b strings.Builder
	afterShift := false
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return "", errUTF7
		}
		if c != '&' {
			b.WriteByte(c)
			afterShift = false
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '-')
		if end < 0 {
			return "", errUTF7
		}
		enc := s[i+1 : i+end]
		i += end + 1
		if enc == "" {
			b.WriteByte('&')
			afterShift = false
			continue
		}
		if afterShift {
			return "", errUTF7
		}
		raw, err := mbase64.Strict().DecodeString(enc)
		if err != nil || len(raw)%2 != 0 || len(raw) == 0 {
			return "", errUTF7
		}
		units := make([]uint16, len(raw)/2)
		for j := range units {
			units[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}
		for _, r := range utf16.Decode(units) {
			if r == utf8.RuneError || (r >= 0x20 && r <= 0x7e) {
				return "", errUTF7
			}
			b.WriteRune(r)
		}
		afterShift = true
	}
	// One spelling per name: what was read must be what would be written.
	// This also refuses what the base64 decoder lets pass (line breaks
	// inside a shift, unpaired surrogates).
	if out := b.String(); encodeUTF7(out) == s {
		return out, nil
	}
	return "", errUTF7
}
