package delivery

import (
	"bytes"
	"mime"
	"strings"
)

// field is one header field, raw, including continuation lines and the
// final CRLF.
type field struct {
	name string
	raw  []byte
}

// splitHeader cuts a message into its header fields and the rest. The rest
// starts at the blank line that ends the header, or at the first line that
// is not a valid header field, and is passed through untouched.
func splitHeader(msg []byte) (fields []field, rest []byte) {
	for len(msg) > 0 {
		end := bytes.Index(msg, []byte("\r\n"))
		if end < 0 {
			end = len(msg)
		} else {
			end += 2
		}
		line := msg[:end]
		switch {
		case line[0] == ' ' || line[0] == '\t':
			if len(fields) == 0 {
				return fields, msg
			}
			f := &fields[len(fields)-1]
			f.raw = append(f.raw, line...)
		default:
			colon := bytes.IndexByte(line, ':')
			if colon <= 0 || !validName(string(line[:colon])) {
				return fields, msg
			}
			fields = append(fields, field{name: string(line[:colon]), raw: append([]byte(nil), line...)})
		}
		msg = msg[end:]
	}
	return fields, msg
}

// validName accepts printable ASCII without spaces or colons (RFC 5322 2.2).
func validName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || c == ':' {
			return false
		}
	}
	return true
}

// removeHeader removes occurrences of name with Rspamd's semantics:
// 0 removes all, n > 0 the n-th from the top, n < 0 the n-th from the bottom.
func removeHeader(fields []field, name string, n int) []field {
	var idx []int
	for i, f := range fields {
		if strings.EqualFold(f.name, name) {
			idx = append(idx, i)
		}
	}
	drop := map[int]bool{}
	switch {
	case n == 0:
		for _, i := range idx {
			drop[i] = true
		}
	case n > 0 && n <= len(idx):
		drop[idx[n-1]] = true
	case n < 0 && -n <= len(idx):
		drop[idx[len(idx)+n]] = true
	}
	out := fields[:0]
	for i, f := range fields {
		if !drop[i] {
			out = append(out, f)
		}
	}
	return out
}

// newField builds a header field from an untrusted value. Line breaks in
// the value become folding (CRLF plus a tab), other control characters are
// dropped, and non-ASCII text is encoded as a MIME word.
func newField(name, value string) (field, bool) {
	if !validName(name) {
		return field{}, false
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	var parts []string
	for _, p := range strings.Split(value, "\n") {
		p = strings.Map(func(r rune) rune {
			if r == '\t' || r >= 0x20 && r != 0x7f {
				return r
			}
			return -1
		}, p)
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	value = strings.Join(parts, "\r\n\t")
	if !isASCII(value) {
		value = mime.QEncoding.Encode("utf-8", strings.Join(parts, " "))
	}
	return field{name: name, raw: []byte(name + ": " + value + "\r\n")}, true
}

// setHeader replaces the first occurrence of name, or adds it at the end.
func setHeader(fields []field, f field) []field {
	for i := range fields {
		if strings.EqualFold(fields[i].name, f.name) {
			fields[i] = f
			return fields
		}
	}
	return append(fields, f)
}

func joinHeader(fields []field, rest []byte) []byte {
	var b bytes.Buffer
	for _, f := range fields {
		b.Write(f.raw)
	}
	b.Write(rest)
	return b.Bytes()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
