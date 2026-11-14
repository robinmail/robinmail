package message

import (
	"sort"
	"strconv"
	"strings"
)

// parseParams reads a structured field of the Content-Type kind (RFC 2045
// 5.1): a value followed by ";"-separated parameters. The value comes back
// in lowercase without comments; parameter names are lowercase.
//
// Parameter values follow RFC 2231: continuations ("name*0", "name*1") are
// joined in order, and extended values ("name*", "name*0*") are
// percent-decoded and converted from their charset. A plain value that
// holds RFC 2047 encoded words is decoded too: RFC 2231 says such words
// must not be used there, but senders do, mostly for file names.
func parseParams(v string) (value string, params map[string]string) {
	s := unfold(v)
	params = map[string]string{}
	var sk skipper
	semi := indexOutsideQuotes(&sk, s, ';')
	if semi < 0 {
		return cleanValue(s), params
	}
	value = cleanValue(s[:semi])
	s = s[semi+1:]

	type section struct {
		n        int
		extended bool
		value    string
	}
	sections := map[string][]section{}
	plain := map[string]string{}
	count := 0
	for len(s) > 0 && count < MaxParams {
		semi := indexOutsideQuotes(&sk, s, ';')
		item := s
		if semi >= 0 {
			item, s = s[:semi], s[semi+1:]
		} else {
			s = ""
		}
		eq := strings.IndexByte(item, '=')
		if eq < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(stripComments(item[:eq])))
		val := strings.TrimSpace(item[eq+1:])
		if strings.HasPrefix(val, `"`) {
			if end := skipQuoted(val, 0); end > 0 {
				val = unquotePairs(val[1 : end-1])
			} else {
				val = unquotePairs(val[1:])
			}
		} else {
			val = strings.TrimSpace(stripComments(val))
		}
		if name == "" {
			continue
		}
		count++
		star := strings.IndexByte(name, '*')
		if star < 0 {
			if _, dup := plain[name]; !dup {
				plain[name] = val
			}
			continue
		}
		base, rest := name[:star], name[star+1:]
		sec := section{value: val}
		if strings.HasSuffix(rest, "*") {
			sec.extended = true
			rest = rest[:len(rest)-1]
		} else if rest == "" {
			sec.extended = true // "name*=": one extended value
		}
		if rest != "" {
			n, err := strconv.Atoi(rest)
			if err != nil || n < 0 || n > MaxParams {
				continue
			}
			sec.n = n
		}
		if base != "" {
			sections[base] = append(sections[base], sec)
		}
	}

	for name, val := range plain {
		params[name] = strings.ToValidUTF8(DecodeWords(val), "\uFFFD")
	}
	// An RFC 2231 value wins over a plain one of the same name: senders give
	// both, the plain one as a fallback for old readers.
	for name, secs := range sections {
		sort.SliceStable(secs, func(i, j int) bool { return secs[i].n < secs[j].n })
		cs := ""
		var raw []byte
		for i, sec := range secs {
			val := sec.value
			if !sec.extended {
				raw = append(raw, val...)
				continue
			}
			if i == 0 {
				// charset'language'text
				if q1 := strings.IndexByte(val, '\''); q1 >= 0 {
					if q2 := strings.IndexByte(val[q1+1:], '\''); q2 >= 0 {
						cs = val[:q1]
						val = val[q1+1+q2+1:]
					}
				}
			}
			raw = append(raw, percentDecode(val)...)
		}
		text := string(raw)
		if cs != "" {
			if decoded, known := decodeCharset(raw, cs); known {
				text = decoded
			}
		}
		params[name] = strings.ToValidUTF8(text, "\uFFFD")
	}
	return value, params
}

// cleanValue is the value in front of the parameters: no comments, no white
// space, lowercase.
func cleanValue(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(stripComments(s)), ""))
}

// indexOutsideQuotes finds c outside quoted strings and comments.
func indexOutsideQuotes(sk *skipper, s string, c byte) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if end := sk.quoted(s, i); end > 0 {
				i = end - 1
			}
		case '(':
			if end := sk.comment(s, i); end > 0 {
				i = end - 1
			}
		case c:
			return i
		}
	}
	return -1
}

func percentDecode(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

// Params reads a field of the Content-Type kind (RFC 2045 5.1): the value
// in lowercase, and its parameters by lowercase name with RFC 2231
// continuations joined and extended values decoded. IMAP's BODYSTRUCTURE
// is built from it.
func Params(v string) (value string, params map[string]string) { return parseParams(v) }
