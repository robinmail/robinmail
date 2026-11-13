package smtp

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
)

var errBadPath = errors.New("bad path")

// parsePath parses "<address> PARAM=value ..." from MAIL FROM or RCPT TO.
// It returns the address without brackets and source route. The null path
// <> is only allowed when allowNull is set.
func parsePath(s string, allowNull bool) (addr string, params []string, err error) {
	if !strings.HasPrefix(s, "<") {
		return "", nil, errBadPath
	}
	end, quoted := -1, false
	for i := 1; i < len(s) && end < 0; i++ {
		switch c := s[i]; {
		case c == '\\' && quoted:
			i++
		case c == '"':
			quoted = !quoted
		case c == '>' && !quoted:
			end = i
		case c < 0x20 || c == 0x7f || (c == ' ' && !quoted) || (c == '<' && !quoted):
			return "", nil, errBadPath
		}
	}
	if end < 0 {
		return "", nil, errBadPath
	}
	addr = s[1:end]
	// A source route such as <@a,@b:user@c> is ignored (RFC 5321 4.1.1.3).
	if strings.HasPrefix(addr, "@") {
		_, after, ok := strings.Cut(addr, ":")
		if !ok {
			return "", nil, errBadPath
		}
		addr = after
	}
	switch {
	case addr == "":
		if !allowNull {
			return "", nil, errBadPath
		}
	case len(addr) > 320:
		return "", nil, errBadPath
	case !strings.Contains(addr, "@"):
		// RFC 5321 requires <Postmaster> without a domain to be accepted.
		if allowNull || !strings.EqualFold(addr, "postmaster") {
			return "", nil, errBadPath
		}
	default:
		at := strings.LastIndexByte(addr, '@')
		if at == 0 || at == len(addr)-1 {
			return "", nil, errBadPath
		}
	}
	rest := s[end+1:]
	if rest != "" && rest[0] != ' ' {
		return "", nil, errBadPath
	}
	for _, p := range strings.Fields(rest) {
		if !printable(p) || len(p) > 500 {
			return "", nil, errBadPath
		}
		params = append(params, p)
	}
	if len(params) > 10 {
		return "", nil, errBadPath
	}
	return addr, params, nil
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
