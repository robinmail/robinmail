package message

import (
	"encoding/base64"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
)

// charset finds the decoder for a charset label. enc is nil for UTF-8 and
// US-ASCII, which need none. A language suffix, as in "us-ascii*en" (RFC
// 2231 5), is ignored.
func charset(label string) (enc encoding.Encoding, known bool) {
	label = strings.ToLower(strings.TrimSpace(label))
	if star := strings.IndexByte(label, '*'); star >= 0 {
		label = label[:star]
	}
	switch label {
	case "utf-8", "utf8", "us-ascii", "ascii", "ansi_x3.4-1968":
		return nil, true
	case "":
		return nil, false
	}
	if e, err := htmlindex.Get(label); err == nil {
		return e, true
	}
	if e, err := ianaindex.MIME.Encoding(label); err == nil && e != nil {
		return e, true
	}
	return nil, false
}

// decodeCharset turns bytes in a charset into a string. known is false if
// the charset is not one we have; the bytes are then returned as they are.
// The result may be invalid UTF-8 when the input was.
func decodeCharset(b []byte, label string) (s string, known bool) {
	enc, known := charset(label)
	if !known || enc == nil {
		return string(b), known
	}
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return string(b), true
	}
	return string(out), true
}

// DecodeWords decodes the encoded words of RFC 2047 in a header value.
//
// White space between two encoded words is dropped (6.2). A word in a
// charset we do not have, or that is malformed, stays as written. Adjacent
// words in the same charset are decoded as one run of bytes, because
// senders split multi-byte characters over words although 5 (3) forbids it.
//
// Deviations from the RFC, on purpose: a word does not have to be
// delimited by white space, may be longer than 75 characters, and is also
// decoded inside a quoted string, where 5 (3) says it must not appear.
// Real mail does all three, and a reader is better served by the text.
func DecodeWords(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	var b strings.Builder
	var pending []byte // bytes of the run of words being collected
	pendingCharset := ""
	flush := func() {
		if pendingCharset != "" {
			text, _ := decodeCharset(pending, pendingCharset)
			b.WriteString(text)
			pending, pendingCharset = nil, ""
		}
	}
	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], "=?")
		if j < 0 {
			break
		}
		j += i
		cs, data, n, ok := encodedWord(s[j:])
		if !ok {
			flush()
			b.WriteString(s[i : j+2])
			i = j + 2
			continue
		}
		between := s[i:j]
		if pendingCharset != "" && strings.Trim(between, " \t\r\n") == "" {
			if !strings.EqualFold(cs, pendingCharset) {
				flush()
			}
		} else {
			flush()
			b.WriteString(between)
		}
		pending, pendingCharset = append(pending, data...), cs
		i = j + n
	}
	flush()
	b.WriteString(s[i:])
	return b.String()
}

// maxEncodedWord is the longest encoded text we look at. The RFC's limit is
// 75 characters for the whole word.
const maxEncodedWord = 4096

// encodedWord reads "=?charset?encoding?text?=" at the start of s and
// returns the charset, the decoded bytes and the length of the word.
func encodedWord(s string) (cs string, data []byte, n int, ok bool) {
	rest := s[2:]
	q := strings.IndexByte(rest, '?')
	if q <= 0 || q+2 >= len(rest) || rest[q+2] != '?' {
		return "", nil, 0, false
	}
	cs = rest[:q]
	if strings.ContainsAny(cs, " \t\r\n") {
		return "", nil, 0, false
	}
	if _, known := charset(cs); !known {
		return "", nil, 0, false
	}
	enc := rest[q+1]
	text := rest[q+3:]
	if len(text) > maxEncodedWord {
		text = text[:maxEncodedWord] // bounds the search for the end
	}
	end := strings.Index(text, "?=")
	if end < 0 {
		return "", nil, 0, false
	}
	text = text[:end]
	if strings.ContainsAny(text, " \t\r\n") {
		return "", nil, 0, false
	}
	switch enc {
	case 'b', 'B':
		data, ok = decodeBase64([]byte(text))
		if !ok {
			return "", nil, 0, false
		}
	case 'q', 'Q':
		data = make([]byte, 0, len(text))
		for k := 0; k < len(text); k++ {
			switch c := text[k]; {
			case c == '_':
				data = append(data, ' ')
			case c == '=' && k+2 < len(text) && isHex(text[k+1]) && isHex(text[k+2]):
				data = append(data, unhex(text[k+1])<<4|unhex(text[k+2]))
				k += 2
			default:
				data = append(data, c)
			}
		}
	default:
		return "", nil, 0, false
	}
	return cs, data, 2 + q + 3 + end + 2, true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// decodeBase64 is lenient, as mail needs: it skips everything outside the
// alphabet (line breaks, stray characters, RFC 2045 6.8) and stops at the
// first padding. ok is false if what is left cannot be base64.
func decodeBase64(src []byte) (out []byte, ok bool) {
	clean := make([]byte, 0, len(src))
	for _, c := range src {
		if c == '=' {
			break
		}
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' {
			clean = append(clean, c)
		}
	}
	ok = true
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1] // one character alone carries no byte
		ok = false
	}
	out = make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, err := base64.RawStdEncoding.Decode(out, clean)
	return out[:n], ok && err == nil
}

// decodeQuotedPrintable follows RFC 2045 6.7 and is lenient where the rule
// (2) note asks a decoder to be: an "=" that starts neither a soft line
// break nor two hex digits is kept as it is. White space at the end of a
// line is dropped (rule 3).
func decodeQuotedPrintable(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); {
		// One line at a time.
		end := i
		for end < len(src) && src[end] != '\n' {
			end++
		}
		line := src[i:end]
		hasLF := end < len(src)
		crlf := hasLF && len(line) > 0 && line[len(line)-1] == '\r'
		if crlf {
			line = line[:len(line)-1]
		}
		for len(line) > 0 && (line[len(line)-1] == ' ' || line[len(line)-1] == '\t') {
			line = line[:len(line)-1]
		}
		soft := len(line) > 0 && line[len(line)-1] == '='
		if soft {
			line = line[:len(line)-1]
		}
		for k := 0; k < len(line); k++ {
			if line[k] == '=' && k+2 < len(line) && isHex(line[k+1]) && isHex(line[k+2]) {
				out = append(out, unhex(line[k+1])<<4|unhex(line[k+2]))
				k += 2
				continue
			}
			out = append(out, line[k])
		}
		if hasLF && !soft {
			if crlf {
				out = append(out, '\r')
			}
			out = append(out, '\n')
		}
		i = end
		if hasLF {
			i++
		}
	}
	return out
}
