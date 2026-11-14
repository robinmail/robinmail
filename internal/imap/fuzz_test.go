package imap

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// discardConn is a connection whose output goes nowhere.
type discardConn struct{ net.Conn }

func (discardConn) Write(p []byte) (int, error)      { return len(p), nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) SetReadDeadline(time.Time) error  { return nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }
func (discardConn) SetDeadline(time.Time) error      { return nil }
func (discardConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (discardConn) Read([]byte) (int, error)         { return 0, io.EOF }

// fuzzSession is a session that is logged in over "TLS", fed from input.
func fuzzSession(t *testing.T, st *memStore, input []byte) {
	srv := &Server{Hostname: "imap.example.test", Backend: st}
	srv.init()
	c := newConn(srv, discardConn{}, "127.0.0.1")
	c.tls = true
	defer c.cancel()
	br := bufio.NewReader(bytes.NewReader(input))
	reader := &commandReader{br: br, continuation: func() error { return nil }, literalLimit: c.literalLimit}
	for i := 0; i < 200 && c.state != stateLogout; i++ {
		parts, _, err := reader.read()
		if err != nil {
			break
		}
		c.handle(parts)
	}
	if c.stopChanges != nil {
		c.stopChanges()
	}
}

// FuzzCommands feeds arbitrary input to a session with mail: the reader of
// commands and literals, the parser, and every command. Nothing may panic
// or hang, whatever a client sends.
func FuzzCommands(f *testing.F) {
	login := "a LOGIN fred@example.test secret\r\nb SELECT INBOX\r\n"
	for _, seed := range []string{
		"c FETCH 1:* (FLAGS ENVELOPE BODYSTRUCTURE BODY.PEEK[HEADER.FIELDS (FROM TO)] BODY[1.2.MIME]<0.10>)\r\n",
		"c UID FETCH 1:*,3,$ (BINARY[1] BINARY.SIZE[2] RFC822 MODSEQ) (CHANGEDSINCE 3)\r\n",
		"c SEARCH RETURN (MIN MAX COUNT SAVE) CHARSET UTF-8 OR FROM x (NOT TEXT {3+}\r\nabc SINCE 1-Feb-1994) MODSEQ \"/flags/x\" all 3\r\n",
		"c STORE 1:2 (UNCHANGEDSINCE 5) +FLAGS.SILENT (\\Seen $x)\r\nd EXPUNGE\r\ne UID EXPUNGE 1:*\r\n",
		"c COPY 1 Sent\r\nd UID MOVE 2:* \"Trash\"\r\ne CLOSE\r\n",
		"c APPEND INBOX (\\Seen) \" 7-Feb-1994 22:43:04 -0800\" {10+}\r\nSubject: x\r\nd APPEND {5}\r\nINBOX UTF8 (~{1+}\r\nx)\r\n",
		"c LIST (SUBSCRIBED RECURSIVEMATCH) \"\" (\"%\" \"*/x\") RETURN (CHILDREN STATUS (MESSAGES SIZE))\r\nd LSUB \"\" *\r\n",
		"c CREATE a/b/&AOk-\r\nd RENAME a/b c\r\ne DELETE c\r\nf SUBSCRIBE INBOX\r\ng STATUS INBOX (MESSAGES HIGHESTMODSEQ)\r\n",
		"c ENABLE CONDSTORE IMAP4rev2 UTF8=ACCEPT\r\nd ID (\"a\" \"b\")\r\ne NAMESPACE\r\nf IDLE\r\nDONE\r\ng UNSELECT\r\nh LOGOUT\r\n",
		"c AUTHENTICATE PLAIN =\r\nd STARTTLS\r\n\r\n* x\r\nc FETCH 4294967296 FLAGS\r\nd FETCH 1 BODY[" + strings.Repeat("1.", 60) + "1]\r\n",
	} {
		f.Add([]byte(login + seed))
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4000 {
			return
		}
		st := newMemStore()
		st.add("INBOX", partsMessage, "$seen")
		st.add("INBOX", sampleMessage)
		st.add("INBOX", "\r\n")
		done := make(chan struct{})
		go func() { defer close(done); fuzzSession(t, st, input) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("the session did not finish: %q", input)
		}
	})
}

// FuzzMIME reads arbitrary bytes as a message: the structure, the
// envelope and every section must come out without a panic, and every
// part must lie inside the message.
func FuzzMIME(f *testing.F) {
	for _, seed := range []string{partsMessage, sampleMessage, "", "\r\n", "x", "Content-Type: multipart/mixed; boundary=\r\n\r\n----\r\n",
		"Content-Type: multipart/x; boundary=b\r\n\r\n--b\r\n--b\r\n\r\n--b--", "Content-Type: message/rfc822\r\n\r\nContent-Type: message/rfc822\r\n\r\nx",
		"From: a@b, G: c@d;, =?x?q?y?= <e@f>\r\nContent-Type: text/plain; name*0=a; name*1=b\r\nContent-Disposition: ;;;\r\nContent-Language: ,,\r\n\r\n",
		"Content-Type: multipart/digest; boundary=\"a b\"\r\n\r\n--a b\r\n\r\nSubject: in\r\n\r\nbody\r\n--a b--\r\n"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 20000 {
			return
		}
		root := parseEntity(raw, 0, len(raw), 0, "text/plain")
		var check func(e *entity, depth int)
		check = func(e *entity, depth int) {
			if e.start < 0 || e.start > e.bodyStart || e.bodyStart > e.end || e.end > len(raw) {
				t.Fatalf("a part at %d..%d..%d of %d octets", e.start, e.bodyStart, e.end, len(raw))
			}
			if depth > maxMIMEDepth {
				t.Fatalf("nested %d deep", depth)
			}
			e.decoded()
			rawFields(e.header())
			headerSubset(e.header(), []string{"from", "x"}, depth%2 == 0)
			for _, p := range e.parts {
				if p.start < e.bodyStart || p.end > e.end {
					t.Fatalf("a part outside its multipart")
				}
				check(p, depth+1)
			}
			if e.inner != nil {
				check(e.inner, depth+1)
			}
		}
		check(root, 0)
		for _, utf8ok := range []bool{false, true} {
			bs := root.bodyStructure(true, utf8ok)
			if !balanced(bs) || !balanced(root.bodyStructure(false, utf8ok)) || !balanced(root.envelope(utf8ok)) {
				t.Fatalf("parentheses do not balance: %q", bs)
			}
		}
		for _, path := range [][]int{{1}, {2}, {1, 1}, {1, 2, 1}, {3, 1, 1, 1}} {
			for _, text := range []string{"", "HEADER", "TEXT", "MIME"} {
				it := fetchItem{kind: "SECTION", path: path, text: text, partial: true, offset: 3, count: 5}
				if data, ok := it.section(root); ok {
					it.cut(data)
				}
			}
		}
	})
}

// balanced checks the parentheses of a response, outside strings and
// literals.
func balanced(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '{':
			end := strings.IndexByte(s[i:], '}')
			n := 0
			for _, d := range s[i+1 : i+end] {
				n = n*10 + int(d-'0')
			}
			i += end + 2 + n
		case '(':
			depth++
		case ')':
			if depth--; depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// FuzzUTF7: every name has one spelling. What decodes encodes back to the
// same octets, and every UTF-8 string survives the round trip.
func FuzzUTF7(f *testing.F) {
	for _, seed := range []string{"~peter/mail/&U,BTFw-/&ZeVnLIqe-", "&-", "&", "&AGE-", "Entwürfe", "a&b", "&2D3eAA-", "\xff", "&U,B-", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if dec, err := decodeUTF7(s); err == nil {
			if !utf8.ValidString(dec) {
				t.Fatalf("decode(%q) = %q is not UTF-8", s, dec)
			}
			if enc := encodeUTF7(dec); enc != s {
				t.Fatalf("decode(%q) = %q, which encodes to %q", s, dec, enc)
			}
		}
		if utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f\x7f") {
			enc := encodeUTF7(s)
			if dec, err := decodeUTF7(enc); err != nil || dec != s {
				t.Fatalf("encode(%q) = %q, which decodes to %q, %v", s, enc, dec, err)
			}
		}
	})
}

// FuzzListPattern compares the pattern matcher with the plain recursive
// definition of the wildcards.
func FuzzListPattern(f *testing.F) {
	f.Add("a/%", "a/b")
	f.Add("*%*", "x/y/z")
	f.Add("%/%c", "a/bc")
	var slow func(p, n string) bool
	slow = func(p, n string) bool {
		if p == "" {
			return n == ""
		}
		switch p[0] {
		case '*':
			for i := 0; i <= len(n); i++ {
				if slow(p[1:], n[i:]) {
					return true
				}
			}
			return false
		case '%':
			for i := 0; ; i++ {
				if slow(p[1:], n[i:]) {
					return true
				}
				if i >= len(n) || n[i] == '/' {
					return false
				}
			}
		}
		return n != "" && n[0] == p[0] && slow(p[1:], n[1:])
	}
	f.Fuzz(func(t *testing.T, pattern, name string) {
		if len(pattern) > 8 || len(name) > 12 {
			return
		}
		if got, want := matchPattern(pattern, name), slow(pattern, name); got != want {
			t.Fatalf("match(%q, %q) = %v, want %v", pattern, name, got, want)
		}
	})
}
