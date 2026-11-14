package smtp

import (
	"bufio"
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"unicode/utf8"
)

var errBadPath = errors.New("bad path")

// Size limits of RFC 5321 4.5.3.1.1 to 4.5.3.1.3, in octets. The RFC makes
// them the sizes every server has to accept; we also refuse anything larger.
const (
	maxLocalPart = 64
	maxDomain    = 255
	maxPath      = 256 // including the angle brackets and a source route
)

// param is one esmtp-param of a MAIL or RCPT command (RFC 5321 4.1.2).
type param struct {
	key      string // upper case
	value    string
	hasValue bool // "KEY=value" rather than "KEY"
}

// parsePath parses the argument of MAIL FROM: or RCPT TO: after the colon: a
// Reverse-path or Forward-path as RFC 5321 4.1.2 defines them, followed by
// parameters. It returns the mailbox without brackets and source route,
// exactly as the client spelled it.
//
// forward selects the Forward-path rules: the null path "<>" is refused and
// "<Postmaster>" without a domain is accepted (4.1.1.3). Non-ASCII
// characters are accepted where RFC 6531 3.3 extends the grammar; whether
// the client was allowed to use them is for the caller to decide.
//
// One deviation, for the Reverse-path only: an underscore is accepted in
// the domain of the sender. The grammar has none ("the underscore
// character is not permitted"), but DNS has such names, hosts are called
// "mail_1.example.com", and Postfix and OpenSMTPD both send from and
// accept such senders (Exim refuses them, as the RFC says). Refusing costs
// wanted mail and protects nothing: the name is looked up, never trusted.
// A recipient is one of our own domains, which have no underscore.
func parsePath(s string, forward bool) (addr string, params []param, err error) {
	if len(s) < 2 || s[0] != '<' {
		return "", nil, errBadPath
	}
	if !utf8.ValidString(s) {
		return "", nil, errBadPath
	}
	i := 1
	// A source route, "@a,@b:", has to be accepted and is ignored
	// (4.1.2, appendix C).
	if s[i] == '@' {
		for {
			if i >= len(s) || s[i] != '@' {
				return "", nil, errBadPath
			}
			end := domainEnd(s, i+1, false)
			if end < 0 || end >= len(s) {
				return "", nil, errBadPath
			}
			i = end + 1
			if s[end] == ':' {
				break
			}
			if s[end] != ',' {
				return "", nil, errBadPath
			}
		}
	}
	if i >= len(s) {
		return "", nil, errBadPath
	}
	start := i
	switch {
	case s[i] == '>':
		// The null reverse-path. "<@a:>" is not one.
		if forward || i != 1 {
			return "", nil, errBadPath
		}
	case forward && len(s) >= i+11 && strings.EqualFold(s[i:i+10], "postmaster") && s[i+10] == '>':
		i += 10
	default:
		if i = mailboxEnd(s, i, !forward); i < 0 || i >= len(s) || s[i] != '>' {
			return "", nil, errBadPath
		}
	}
	addr = s[start:i]
	if i+1 > maxPath {
		return "", nil, errBadPath
	}
	rest := s[i+1:]
	if rest != "" && rest[0] != ' ' {
		return "", nil, errBadPath
	}
	seen := map[string]bool{}
	for _, f := range strings.Fields(rest) {
		p, ok := parseParam(f)
		// No extension we know allows a parameter twice (RFC 1870 6,
		// RFC 6152 3).
		if !ok || seen[p.key] || len(params) == 10 {
			return "", nil, errBadPath
		}
		seen[p.key] = true
		params = append(params, p)
	}
	return addr, params, nil
}

// parseParam parses esmtp-keyword ["=" esmtp-value].
func parseParam(s string) (param, bool) {
	k, v, hasValue := strings.Cut(s, "=")
	if k == "" || len(s) > 500 || !alnum(k[0]) {
		return param{}, false
	}
	for i := 0; i < len(k); i++ {
		if !alnum(k[i]) && k[i] != '-' {
			return param{}, false
		}
	}
	if hasValue && v == "" {
		return param{}, false
	}
	for i := 0; i < len(v); i++ {
		// %d33-60 / %d62-126, and UTF8-non-ascii by RFC 6531.
		if c := v[i]; c < 33 || c == '=' || c == 127 {
			return param{}, false
		}
	}
	return param{key: strings.ToUpper(k), value: v, hasValue: hasValue}, true
}

// mailboxEnd parses a Mailbox starting at s[i] and returns the index after
// it, or -1.
//
//	Mailbox    = Local-part "@" ( Domain / address-literal )
//	Local-part = Dot-string / Quoted-string
//
// underscore also takes a domain with underscores in its labels.
func mailboxEnd(s string, i int, underscore bool) int {
	start := i
	if i < len(s) && s[i] == '"' {
		for i++; ; i++ {
			if i >= len(s) {
				return -1
			}
			c := s[i]
			if c == '"' {
				i++
				break
			}
			if c == '\\' {
				// quoted-pairSMTP = %d92 %d32-126
				if i++; i >= len(s) || s[i] < 32 || s[i] > 126 {
					return -1
				}
				continue
			}
			// qtextSMTP = %d32-33 / %d35-91 / %d93-126 / UTF8-non-ascii
			if c < 32 || c == 127 {
				return -1
			}
		}
	} else {
		// Dot-string = Atom *("." Atom)
		for {
			n := i
			for i < len(s) && atext(s[i]) {
				i++
			}
			if i == n {
				return -1
			}
			if i >= len(s) || s[i] != '.' {
				break
			}
			i++
		}
	}
	if i-start > maxLocalPart || i >= len(s) || s[i] != '@' {
		return -1
	}
	i++
	if i < len(s) && s[i] == '[' {
		end := strings.IndexByte(s[i:], ']')
		if end < 0 || !validAddressLiteral(s[i:i+end+1]) {
			return -1
		}
		return i + end + 1
	}
	return domainEnd(s, i, underscore)
}

// atext is RFC 5322 3.2.3's atext, plus UTF8-non-ascii (RFC 6531 3.3).
func atext(c byte) bool {
	return alnum(c) || c >= 0x80 || strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// domainEnd parses a Domain starting at s[i] and returns the index after it,
// or -1.
//
//	Domain     = sub-domain *("." sub-domain)
//	sub-domain = Let-dig [Ldh-str]
//
// So a label starts and ends with a letter or digit and has only letters,
// digits and hyphens in between; no underscore, no trailing dot. RFC 6531
// adds U-labels, taken here as non-ASCII characters in a label.
//
// underscore is the deviation described at parsePath: it lets "_" stand
// anywhere in a label, as in "mail_1.example.com" and "_x.example.com".
func domainEnd(s string, i int, underscore bool) int {
	start := i
	for {
		n := i
		for i < len(s) && (alnum(s[i]) || s[i] == '-' || s[i] >= 0x80 || underscore && s[i] == '_') {
			i++
		}
		if i == n || s[n] == '-' || s[i-1] == '-' {
			return -1
		}
		if i >= len(s) || s[i] != '.' {
			break
		}
		i++
	}
	if i-start > maxDomain {
		return -1
	}
	return i
}

// ValidDomain reports whether s is a Domain as RFC 5321 4.1.2 defines it.
func ValidDomain(s string) bool {
	return s != "" && ascii(s) && domainEnd(s, 0, false) == len(s)
}

// ValidHelo reports whether s is what RFC 5321 4.1.1.1 allows as the
// argument of EHLO: a Domain or an address literal.
func ValidHelo(s string) bool {
	return ValidDomain(s) || ascii(s) && validAddressLiteral(s)
}

// validAddressLiteral checks s, brackets included, against RFC 5321 4.1.3.
func validAddressLiteral(s string) bool {
	if len(s) < 3 || len(s) > maxDomain || s[0] != '[' || s[len(s)-1] != ']' {
		return false
	}
	s = s[1 : len(s)-1]
	if len(s) > 5 && strings.EqualFold(s[:5], "IPv6:") {
		a, err := netip.ParseAddr(s[5:])
		return err == nil && a.Is6() && a.Zone() == ""
	}
	// General-address-literal = Standardized-tag ":" 1*dcontent, where the
	// tag "MUST be specified in a Standards-Track RFC and registered with
	// IANA". IPv6 is the only tag registered, so anything else with a colon
	// is not a literal we know.
	if strings.Contains(s, ":") {
		return false
	}
	// IPv4-address-literal = Snum 3("." Snum), Snum = 1*3DIGIT, 0 to 255.
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

var (
	errTooBig          = errors.New("message too big")
	errBareLineEnding  = errors.New("bare CR or LF")
	errDataLineTooLong = errors.New("line too long")
)

// readData reads a message after DATA up to the terminating <CRLF>.<CRLF>
// and undoes dot-stuffing.
//
// Only CRLF ends a line. A bare CR or LF anywhere makes the whole message
// fail with errBareLineEnding, because servers disagree about what such a
// line ending means, and that disagreement is what SMTP smuggling exploits.
// The terminator is recognised only as "." on a line of its own after a real
// CRLF. On errTooBig, errBareLineEnding and errDataLineTooLong the input is
// still read up to the terminator, so the session stays in sync.
func readData(r *bufio.Reader, max int64) ([]byte, error) {
	var (
		buf       bytes.Buffer
		failed    error
		lineStart = true // the previous segment ended in CRLF
		size      int64
	)
	for {
		seg, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			if failed == nil {
				failed = errDataLineTooLong
			}
			lineStart = false
			continue
		}
		if err != nil {
			return nil, err
		}
		crlf := len(seg) >= 2 && seg[len(seg)-2] == '\r'
		if lineStart && crlf && len(seg) == 3 && seg[0] == '.' {
			break
		}
		if !crlf || bytes.IndexByte(seg[:len(seg)-2], '\r') >= 0 {
			if failed == nil || failed == errDataLineTooLong {
				failed = errBareLineEnding
			}
		}
		if lineStart && len(seg) > 0 && seg[0] == '.' {
			seg = seg[1:]
		}
		lineStart = crlf
		size += int64(len(seg))
		if size > max && failed == nil {
			failed = errTooBig
		}
		if failed == nil {
			buf.Write(seg)
		}
	}
	if failed != nil {
		return nil, failed
	}
	return buf.Bytes(), nil
}

// ValidMailbox reports whether s is a Mailbox of RFC 5321 4.1.2 (with the
// non-ASCII characters RFC 6531 adds): what may stand between the angle
// brackets of a RCPT TO we send. A domain with an underscore is taken, as
// it is in a sender (see parsePath): mail that came from such a domain
// can then be answered. What the other server makes of it is its call;
// Postfix and OpenSMTPD take it, Exim answers 501.
func ValidMailbox(s string) bool {
	return s != "" && len(s) <= maxPath-2 && utf8.ValidString(s) && mailboxEnd(s, 0, true) == len(s)
}
