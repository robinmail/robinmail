package smtp

// Tests written from RFC 5321, not from what the server happens to do. Each
// names the section it checks. Where the server deviates on purpose the test
// says "Deviation" and why.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a Handler that keeps what it was given. rcpt decides about a
// recipient; nil accepts all.
type recorder struct {
	rcpt func(to string) error

	mu    sync.Mutex
	rcpts []string
	msgs  [][]byte
	envs  []Envelope
}

func (h *recorder) Rcpt(_ context.Context, _ *Envelope, to string) error {
	h.mu.Lock()
	h.rcpts = append(h.rcpts, to)
	h.mu.Unlock()
	if h.rcpt != nil {
		return h.rcpt(to)
	}
	return nil
}

func (h *recorder) Deliver(_ context.Context, env *Envelope, msg []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, bytes.Clone(msg))
	e := *env
	e.Rcpts = append([]string(nil), env.Rcpts...)
	h.envs = append(h.envs, e)
	return nil
}

func (h *recorder) last(t *testing.T) (Envelope, []byte) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.msgs) == 0 {
		t.Fatal("nothing was delivered")
	}
	return h.envs[len(h.envs)-1], h.msgs[len(h.msgs)-1]
}

func (h *recorder) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

// serveWith starts srv, with defaults for what a test leaves out.
func serveWith(t *testing.T, srv *Server) string {
	t.Helper()
	if srv.Hostname == "" {
		srv.Hostname = "foo.com"
	}
	if srv.MaxErrors == 0 {
		srv.MaxErrors = 1000
	}
	if srv.MaxConnsPerIP == 0 {
		srv.MaxConnsPerIP = 100
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return l.Addr().String()
}

// fooCom is the host of the RFC's examples: it has mailboxes for Jones,
// Brown and Admin.MRC and none for Green (appendix D).
func fooCom(t *testing.T) (*recorder, *client) {
	t.Helper()
	h := &recorder{rcpt: func(to string) error {
		if strings.HasPrefix(to, "Green@") {
			return &Error{550, "5.1.1", "No such user here"}
		}
		return nil
	}}
	return h, dial(t, serveWith(t, &Server{Handler: h}))
}

// code returns the reply code of a reply as read by client.expect.
func code(reply string) string { return reply[:3] }

// any sends a command and returns the reply, whatever its code.
func (c *client) any(line string) string {
	c.t.Helper()
	c.send(line + "\r\n")
	return c.expect("")
}

// stuff is the client side of RFC 5321 4.5.2: a period is put in front of
// every line that starts with one, and the end of mail data follows.
func stuff(msg string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(msg, "\r\n") {
		if strings.HasPrefix(line, ".") {
			b.WriteByte('.')
		}
		b.WriteString(line)
	}
	return b.String() + ".\r\n"
}

// RFC 5321 4.1.2: Reverse-path, Forward-path, Path, Mailbox, Local-part,
// Domain and address-literal, with 4.1.3 for the literals. The valid cases
// include the RFC's own examples.
func TestRFC5321_4_1_2_PathGrammar(t *testing.T) {
	both := "both"
	for _, tc := range []struct {
		path  string
		valid string // "both", "reverse", "forward" or "" for neither
		addr  string
	}{
		// Dot-string local parts.
		{"<Smith@bar.com>", both, "Smith@bar.com"}, // appendix D.1
		{"<Admin.MRC@foo.com>", both, "Admin.MRC@foo.com"},
		{"<a@b>", both, "a@b"}, // one sub-domain is a Domain
		{"<user@1.example>", both, "user@1.example"},
		{"<user@a-b.example>", both, "user@a-b.example"},
		{"<user@a--b.example>", both, "user@a--b.example"},
		{"<user@123.example>", both, "user@123.example"},
		// Every atext character (RFC 5322 3.2.3).
		{"<!#$%&'*+-/=?^_`{|}~@example.com>", both, "!#$%&'*+-/=?^_`{|}~@example.com"},
		// Quoted-string local parts; "Joe\,Smith" is the RFC's example.
		{`<"Joe\,Smith"@example.com>`, both, `"Joe\,Smith"@example.com`},
		{`<"a b"@example.com>`, both, `"a b"@example.com`},
		{`<"a@b"@example.com>`, both, `"a@b"@example.com`},
		{`<"a>b"@example.com>`, both, `"a>b"@example.com`},
		{`<"a\"b"@example.com>`, both, `"a\"b"@example.com`},
		{`<"a\\"@example.com>`, both, `"a\\"@example.com`},
		{`<"..."@example.com>`, both, `"..."@example.com`},
		{`<""@example.com>`, both, `""@example.com`}, // *QcontentSMTP
		// Address literals (4.1.3); [123.255.37.2] is the RFC's example.
		{"<user@[123.255.37.2]>", both, "user@[123.255.37.2]"},
		{"<user@[0.0.0.0]>", both, "user@[0.0.0.0]"},
		{"<user@[IPv6:2001:db8::1]>", both, "user@[IPv6:2001:db8::1]"},
		{"<user@[IPv6:2001:db8:0:0:0:0:0:1]>", both, "user@[IPv6:2001:db8:0:0:0:0:0:1]"},
		{"<user@[IPv6:::ffff:192.0.2.1]>", both, "user@[IPv6:::ffff:192.0.2.1]"},
		// Source routes MUST be accepted and SHOULD be ignored; these are
		// the RFC's examples (appendix C and F.2).
		{"<@hosta.int,@jkl.org:userc@d.bar.org>", both, "userc@d.bar.org"},
		{"<@jkl.org:userc@d.bar.org>", both, "userc@d.bar.org"},
		{"<@a,@b:user@d>", both, "user@d"},
		// The null reverse-path, and Postmaster without a domain (4.1.1.3).
		{"<>", "reverse", ""},
		{"<Postmaster>", "forward", "Postmaster"},
		{"<postmaster>", "forward", "postmaster"},
		{"<POSTMASTER>", "forward", "POSTMASTER"},
		{"<Postmaster@foo.com>", both, "Postmaster@foo.com"},

		// Not a Path.
		{"Smith@bar.com", "", ""},
		{"<Smith@bar.com", "", ""},
		{"Smith@bar.com>", "", ""},
		{"<<Smith@bar.com>>", "", ""},
		{"<Smith@bar.com>>", "", ""},
		{"< Smith@bar.com>", "", ""},
		{"<Smith@bar.com >", "", ""},
		{"", "", ""},
		{"<", "", ""},
		// Not a Mailbox.
		{"<Smith>", "", ""},
		{"<@bar.com>", "", ""},
		{"<Smith@>", "", ""},
		{"<Smith@bar.com@baz.com>", "", ""},
		// Not a Dot-string.
		{"<.Smith@bar.com>", "", ""},
		{"<Smith.@bar.com>", "", ""},
		{"<Sm..ith@bar.com>", "", ""},
		{"<Sm ith@bar.com>", "", ""},
		{"<Sm,ith@bar.com>", "", ""},
		{"<Sm(i)th@bar.com>", "", ""},
		{"<Sm;ith@bar.com>", "", ""},
		{"<Sm[i]th@bar.com>", "", ""},
		{"<Sm\\ith@bar.com>", "", ""},
		// Not a Quoted-string.
		{`<"Smith@bar.com>`, "", ""},
		{`<Sm"i"th@bar.com>`, "", ""},
		{`<"Sm"ith"@bar.com>`, "", ""},
		{`<"Smith"x@bar.com>`, "", ""},
		{"<\"Sm\tith\"@bar.com>", "", ""},   // qtextSMTP starts at %d32
		{"<\"Sm\\\tith\"@bar.com>", "", ""}, // quoted-pairSMTP too
		// Control characters MUST NOT be used in MAIL or RCPT.
		{"<Sm\x00ith@bar.com>", "", ""},
		{"<Sm\x7fith@bar.com>", "", ""},
		{"<Smith@bar\x01.com>", "", ""},
		// Not a Domain. "In particular, the underscore character is not
		// permitted."
		{"<Smith@bar_baz.com>", "", ""},
		{"<Smith@-bar.com>", "", ""},
		{"<Smith@bar-.com>", "", ""},
		{"<Smith@bar..com>", "", ""},
		{"<Smith@.bar.com>", "", ""},
		{"<Smith@bar.com.>", "", ""},
		{"<Smith@bar.c*m>", "", ""},
		{"<Smith@bar com>", "", ""},
		// Not an address-literal.
		{"<user@[123.255.37.256]>", "", ""},
		{"<user@[123.255.37]>", "", ""},
		{"<user@[123.255.37.2.1]>", "", ""},
		{"<user@[1234.255.37.2]>", "", ""},
		{"<user@[2001:db8::1]>", "", ""}, // IPv6 needs its tag
		{"<user@[IPv6:2001:db8::zz]>", "", ""},
		{"<user@[IPv6:192.0.2.1]>", "", ""},
		{"<user@[IPv6:fe80::1%eth0]>", "", ""},
		{"<user@[]>", "", ""},
		{"<user@[123.255.37.2>", "", ""},
		{"<user@[123.255.37.2]x>", "", ""},
		// A General-address-literal needs a tag that is "specified in a
		// Standards-Track RFC and registered with IANA". IPv6 is the only
		// one, so no other tag is valid.
		{"<user@[tag:content]>", "", ""},
		{"<user@[tag:]>", "", ""},
		// Not a source route.
		{"<@hosta.int:>", "", ""},
		{"<@hosta.int:", "", ""},
		{"<@hosta.int,", "", ""},
		{"<@", "", ""},
		{"<@hosta.int>", "", ""},
		{"<@hosta.int,userc@d.bar.org>", "", ""},
		{"<@hosta.int,:userc@d.bar.org>", "", ""},
		{"<@hosta_x.int:userc@d.bar.org>", "", ""},
		{"<@:userc@d.bar.org>", "", ""},
		{"<@hosta.int userc@d.bar.org>", "", ""},
	} {
		for _, forward := range []bool{false, true} {
			want := tc.valid == both || tc.valid == "forward" && forward || tc.valid == "reverse" && !forward
			addr, _, err := parsePath(tc.path, forward)
			if (err == nil) != want {
				t.Errorf("parsePath(%q, forward=%v): err = %v, want valid = %v", tc.path, forward, err, want)
			} else if want && addr != tc.addr {
				t.Errorf("parsePath(%q, forward=%v) = %q, want %q", tc.path, forward, addr, tc.addr)
			}
		}
	}
}

// RFC 5321 4.1.2: Mail-parameters and Rcpt-parameters.
//
//	esmtp-param   = esmtp-keyword ["=" esmtp-value]
//	esmtp-keyword = (ALPHA / DIGIT) *(ALPHA / DIGIT / "-")
//	esmtp-value   = 1*(%d33-60 / %d62-126)
func TestRFC5321_4_1_2_Parameters(t *testing.T) {
	for in, want := range map[string][]param{
		"<a@b.example>":                {},
		"<a@b.example> SIZE=100":       {{"SIZE", "100", true}},
		"<a@b.example> size=100":       {{"SIZE", "100", true}},
		"<a@b.example> KEY":            {{"KEY", "", false}},
		"<a@b.example> 8BIT-x":         {{"8BIT-X", "", false}},
		"<a@b.example> K=!\"#<>~ L=v2": {{"K", "!\"#<>~", true}, {"L", "v2", true}},
		"<> BODY=8BITMIME SIZE=1":      {{"BODY", "8BITMIME", true}, {"SIZE", "1", true}},
	} {
		_, got, err := parsePath(in, false)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("parsePath(%q) params = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{
		"<a@b.example> =v",      // no keyword
		"<a@b.example> -K=v",    // keyword starts with ALPHA or DIGIT
		"<a@b.example> K_L=v",   // "_" is not in a keyword
		"<a@b.example> K=",      // esmtp-value is 1*
		"<a@b.example> K=a=b",   // "=" is excluded from the value
		"<a@b.example> K=a\x01", // control characters are too
		"<a@b.example> K=a\x7f",
		"<a@b.example>SIZE=100", // SP separates path and parameters
	} {
		if _, got, err := parsePath(in, false); err == nil {
			t.Errorf("parsePath(%q) accepted, params %v", in, got)
		}
	}
}

// RFC 5321 4.5.3.1.1 to 4.5.3.1.3: a local-part of 64 octets, a domain of
// 255 and a path of 256 have to be accepted.
//
// Deviation: the RFC allows a server to take longer ones; we refuse them
// (501, as 4.5.3.1.9 suggests), so nothing downstream sees an oversized
// address.
func TestRFC5321_4_5_3_1_AddressLimits(t *testing.T) {
	label := strings.Repeat("d", 63)
	domain := func(n int) string { // a domain of n octets, n >= 4
		s := ""
		for len(s)+64 <= n-2 {
			s += label + "."
		}
		return s + strings.Repeat("e", n-len(s))
	}
	if d := domain(255); len(d) != 255 || !ValidDomain(d) {
		t.Errorf("a domain of 255 octets is not valid (4.5.3.1.2)")
	}
	if d := domain(256); len(d) != 256 || ValidDomain(d) {
		t.Errorf("a domain of 256 octets is valid")
	}

	local64, local65 := strings.Repeat("l", 64), strings.Repeat("l", 65)
	if _, _, err := parsePath("<"+local64+"@bar.com>", true); err != nil {
		t.Errorf("a local-part of 64 octets: %v (4.5.3.1.1)", err)
	}
	if _, _, err := parsePath("<"+local65+"@bar.com>", true); err == nil {
		t.Errorf("a local-part of 65 octets was accepted")
	}
	// "<" local "@" domain ">" of 256 and 257 octets in all.
	path256 := "<" + local64 + "@" + domain(256-64-3) + ">"
	path257 := "<" + local64 + "@" + domain(257-64-3) + ">"
	if len(path256) != 256 || len(path257) != 257 {
		t.Fatalf("test paths are %d and %d octets", len(path256), len(path257))
	}
	if _, _, err := parsePath(path256, true); err != nil {
		t.Errorf("a path of 256 octets: %v (4.5.3.1.3)", err)
	}
	if _, _, err := parsePath(path257, true); err == nil {
		t.Errorf("a path of 257 octets was accepted")
	}

	// And through the server: accepted, respectively 501.
	_, c := fooCom(t)
	c.cmd("EHLO "+domain(255), "250")
	c.cmd("MAIL FROM:"+path256, "250")
	c.cmd("RCPT TO:"+path256, "250")
	c.cmd("RCPT TO:"+path257, "501")
	c.cmd("RCPT TO:<"+local65+"@bar.com>", "501")
}

// RFC 5321 2.4: command verbs are case-insensitive, and so is the "FROM:"
// and "TO:" that follows them.
func TestRFC5321_2_4_CommandsCaseInsensitive(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("ehlo bar.com", "250")
	c.cmd("mail from:<Smith@bar.com>", "250")
	c.cmd("rCpT tO:<Jones@foo.com>", "250")
	c.cmd("Data", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	c.cmd("noop", "250")
	c.cmd("rset", "250")
	c.cmd("quit", "221")
	if h.count() != 1 {
		t.Errorf("%d messages delivered", h.count())
	}
}

// RFC 5321 2.4: "The local-part of a mailbox MUST BE treated as case
// sensitive. Therefore, SMTP implementations MUST take care to preserve the
// case of mailbox local-parts." The SMTP layer hands addresses on exactly as
// they came; folding them is delivery's decision.
func TestRFC5321_2_4_LocalPartCasePreserved(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<SmItH@Bar.Com>", "250")
	c.cmd("RCPT TO:<JoNeS@Foo.Com>", "250")
	c.cmd(`RCPT TO:<"Jo Nes"@foo.com>`, "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	env, _ := h.last(t)
	if env.From != "SmItH@Bar.Com" || strings.Join(env.Rcpts, " ") != `JoNeS@Foo.Com "Jo Nes"@foo.com` {
		t.Errorf("envelope %q -> %q", env.From, env.Rcpts)
	}
}

// RFC 5321 4.1.1.1: EHLO takes a Domain or an address-literal, HELO a
// Domain. The EHLO reply's first line starts with the server's domain, the
// rest are ehlo-lines: a keyword and optional parameters.
func TestRFC5321_4_1_1_1_EHLO(t *testing.T) {
	_, c := fooCom(t)
	for _, arg := range []string{"bar.com", "[123.255.37.2]", "[IPv6:2001:db8::1]", "localhost"} {
		reply := c.cmd("EHLO "+arg, "250")
		lines := strings.Split(strings.TrimSuffix(reply, "\r\n"), "\r\n")
		if len(lines) < 2 {
			t.Fatalf("EHLO reply %q", reply)
		}
		if !regexp.MustCompile(`^250-foo\.com( .*)?$`).MatchString(lines[0]) {
			t.Errorf("first EHLO line %q does not start with the domain", lines[0])
		}
		for i, l := range lines[1:] {
			sep := "-"
			if i == len(lines)-2 {
				sep = " "
			}
			// ehlo-keyword = (ALPHA / DIGIT) *(ALPHA / DIGIT / "-"),
			// ehlo-param = 1*(%d33-126)
			if !regexp.MustCompile(`^250` + sep + `[A-Za-z0-9][A-Za-z0-9-]*( [!-~]+)*$`).MatchString(l) {
				t.Errorf("EHLO line %q is not an ehlo-line", l)
			}
		}
	}
	// HELO: "250 Domain".
	if reply := c.cmd("HELO bar.com", "250"); !regexp.MustCompile(`^250 foo\.com( .*)?\r\n$`).MatchString(reply) {
		t.Errorf("HELO reply %q", reply)
	}
	// Both need their argument.
	c.cmd("EHLO", "501")
	c.cmd("HELO", "501")
	c.cmd("EHLO ", "501")
}

// Deviation from RFC 5321 4.1.1.1: an EHLO argument that is neither a Domain
// nor an address-literal is accepted. Real clients send names with
// underscores or a trailing dot, and 4.1.4 forbids refusing mail because the
// name does not check out. The name is not trusted for anything: the
// Received line only quotes it when it is well-formed (see delivery).
func TestRFC5321_4_1_1_1_Deviation_AnyEHLOArgument(t *testing.T) {
	_, c := fooCom(t)
	for _, arg := range []string{"my_host", "bar.com.", "-bar.com", "[192.0.2.999]", "[2001:db8::1]"} {
		if ValidHelo(arg) {
			t.Errorf("ValidHelo(%q) is true", arg)
		}
		c.cmd("EHLO "+arg, "250")
	}
	// An IPv4 address without brackets is, syntactically, a Domain.
	for _, arg := range []string{"bar.com", "[192.0.2.1]", "[IPv6:2001:db8::1]", "192.0.2.1"} {
		if !ValidHelo(arg) {
			t.Errorf("ValidHelo(%q) is false", arg)
		}
	}
}

// RFC 5321 4.1.4: order of commands. A session starts with EHLO or HELO;
// MAIL, RCPT, DATA go in that order; NOOP, RSET, QUIT, HELP and VRFY work at
// any time; a second EHLO resets like RSET.
func TestRFC5321_4_1_4_OrderOfCommands(t *testing.T) {
	h, c := fooCom(t)
	// "NOOP, HELP, EXPN, VRFY, and RSET commands can be used at any time
	// during a session, or without previously initializing a session."
	c.cmd("NOOP", "250")
	c.cmd("RSET", "250")
	c.cmd("HELP", "214")
	c.cmd("VRFY Crispin", "252")
	// "A session that will contain mail transactions MUST first be
	// initialized by the use of the EHLO command."
	c.cmd("MAIL FROM:<Smith@bar.com>", "503")
	c.cmd("HELO bar.com", "250") // 4.1.1.1: HELO MUST be supported
	c.cmd("RCPT TO:<Jones@foo.com>", "503")
	c.cmd("DATA", "503")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	// "MAIL ... MUST NOT be sent if a mail transaction is already open."
	c.cmd("MAIL FROM:<Smith@bar.com>", "503")
	// 3.3: DATA without a valid recipient is 503 or 554.
	if got := code(c.any("DATA")); got != "503" && got != "554" {
		t.Errorf("DATA without recipients: %s", got)
	}
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	// RSET aborts the transaction: the recipient is gone, MAIL is needed.
	c.cmd("RSET", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "503")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	// "An EHLO command MAY be issued by a client later in the session. If
	// it is issued after the session begins ... the SMTP server MUST clear
	// all buffers and reset the state exactly as if a RSET command had
	// been issued."
	c.cmd("EHLO bar.com", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "503")
	c.cmd("DATA", "503")
	// A complete transaction ends the transaction: 3.3, "the mail
	// transaction is complete" and the buffers are cleared.
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	c.cmd("RCPT TO:<Brown@foo.com>", "503")
	c.cmd("DATA", "503")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250") // and a new one can start
	c.cmd("QUIT", "221")
	if h.count() != 1 {
		t.Errorf("%d messages delivered, want 1", h.count())
	}
	// 4.1.1.10: after QUIT the server closes the channel.
	if _, err := c.r.ReadByte(); err == nil {
		t.Error("connection open after QUIT")
	}
}

// RFC 5321 4.3.2, with 4.1.1.5, 4.1.1.9 and 4.1.1.10: DATA, RSET and QUIT
// take no argument and "SHOULD return a 501 message if arguments are
// supplied". NOOP and HELP take an optional string; VRFY and EXPN need one.
func TestRFC5321_4_3_2_Arguments(t *testing.T) {
	_, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("RSET now", "501")
	c.cmd("QUIT now", "501")
	c.cmd("MAIL", "501")
	c.cmd("MAIL FROM", "501")
	c.cmd("MAIL TO:<Smith@bar.com>", "501")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT", "501")
	c.cmd("RCPT FROM:<Jones@foo.com>", "501")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA now", "501")
	c.cmd("NOOP", "250")
	c.cmd("NOOP anything at all", "250")
	c.cmd("HELP", "214")
	c.cmd("HELP MAIL", "214")
	c.cmd("VRFY", "501")
	// The transaction survived all of that.
	c.cmd("DATA", "354")
	c.send(".\r\n")
	c.expect("250")
}

// RFC 5321 4.2.4 and 4.3.2: 500 is for a command that is not recognised,
// 502 for one that is recognised but not implemented. "Producing a 'command
// not recognized' error in response to the required subset of these
// commands is a violation" (4.5.1 lists the subset).
func TestRFC5321_4_2_4_UnknownAndUnimplemented(t *testing.T) {
	_, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("FROB", "500")
	c.cmd("", "500")
	c.cmd("EXPN Example-People", "502")
	// 4.5.1: the minimum implementation.
	for _, cmd := range []string{"EHLO bar.com", "HELO bar.com", "MAIL FROM:<>", "RCPT TO:<Jones@foo.com>", "DATA",
		"RSET", "NOOP", "VRFY Jones", "QUIT"} {
		reply := c.any(cmd)
		if code(reply) == "500" || code(reply) == "502" {
			t.Errorf("%s: %s", cmd, reply)
		}
		if code(reply) == "354" {
			c.send(".\r\n")
			c.expect("250")
		}
	}
}

// RFC 5321 4.1.1.3 and 4.5.1: "RCPT TO:<Postmaster>" without a domain MUST
// be accepted, in any case; with a domain it is an ordinary mailbox.
func TestRFC5321_4_5_1_Postmaster(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	for _, to := range []string{"Postmaster", "postmaster", "POSTMASTER", "Postmaster@foo.com"} {
		c.cmd("RCPT TO:<"+to+">", "250")
	}
	// Any other name needs a domain, and so does the reverse-path.
	c.cmd("RCPT TO:<Jones>", "501")
	c.cmd("RSET", "250")
	c.cmd("MAIL FROM:<Postmaster>", "501")
	if got := strings.Join(h.rcpts, " "); got != "Postmaster postmaster POSTMASTER Postmaster@foo.com" {
		t.Errorf("handler saw %q", got)
	}
}

// RFC 5321 3.6.3 and 4.5.5: a notification is sent with a null reverse-path,
// "MAIL FROM:<>", which every server has to accept. RCPT has no null path.
func TestRFC5321_4_5_5_NullReversePath(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<>", "250")
	c.cmd("RCPT TO:<>", "501")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send("bounce\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.From != "" {
		t.Errorf("From = %q for the null reverse-path", env.From)
	}
}

// RFC 5321 appendix C and F.2: source routes "MUST be accepted" and "SHOULD
// be ignored": the message goes to the mailbox at the end of the route.
func TestRFC5321_AppendixC_SourceRoutes(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<@a,@b:user@d>", "250")
	c.cmd("RCPT TO:<@hosta.int,@jkl.org:userc@d.bar.org>", "250")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); env.From != "user@d" || env.Rcpts[0] != "userc@d.bar.org" {
		t.Errorf("envelope %q -> %q", env.From, env.Rcpts)
	}
}

// RFC 5321 4.5.2, transparency: the client doubles a leading period, the
// server removes it again, and a line with only a period ends the data. The
// message arrives exactly as the user wrote it.
func TestRFC5321_4_5_2_Transparency(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	for _, msg := range []string{
		"Subject: dots\r\n\r\n.\r\n..\r\n...\r\n.leading\r\nmid.dle\r\ntrailing.\r\n. \r\n",
		".\r\n",              // a message that is one line with one period
		".first line\r\n",    // a period at the very start of the data
		"..\r\n.\r\n..\r\n",  // periods all the way down
		"no periods\r\n",     // nothing to do
		"",                   // the end of data right after DATA's CRLF
		"\r\n\r\n",           // empty lines
		"a\r\n\r\n.\r\n\r\n", // a lone period between empty lines
	} {
		c.cmd("MAIL FROM:<Smith@bar.com>", "250")
		c.cmd("RCPT TO:<Jones@foo.com>", "250")
		c.cmd("DATA", "354")
		c.send(stuff(msg))
		c.expect("250")
		if _, got := h.last(t); string(got) != msg {
			t.Errorf("sent %q, delivered %q", msg, got)
		}
	}
}

// RFC 5321 4.1.1.4: "the mail data ... are terminated by a line containing
// only a period, that is, the character sequence "<CRLF>.<CRLF>", where the
// first <CRLF> is actually the terminator of the previous line". A period
// followed by anything else, or at the end of a line, ends nothing.
func TestRFC5321_4_1_1_4_EndOfData(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send("a.\r\n. \r\n.b\r\nc\r\n.\r\n")
	c.expect("250")
	if _, got := h.last(t); string(got) != "a.\r\n \r\nb\r\nc\r\n" {
		t.Errorf("delivered %q", got)
	}
	// What follows the end of data is a command again.
	c.cmd("NOOP", "250")
}

// Deviation from RFC 5321 2.3.8 and 4.1.1.4. The RFC says only CRLF ends a
// line and "<LF>.<LF>" MUST NOT end the data, but lets a server pass a bare
// CR or LF through as data. We reject the whole message with 550 instead:
// servers disagree about what such a line ending means, and that
// disagreement is what SMTP smuggling exploits. The data is still read up to
// the real end, so the session stays in sync and nothing after a fake end of
// data runs as a command.
func TestRFC5321_2_3_8_Deviation_BareLineEndingsInData(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	for _, data := range []string{
		"line\nline\r\n.\r\n",
		"line\rline\r\n.\r\n",
		// Two attempts to end the data early and start a second message.
		"a\r\n\n.\nMAIL FROM:<evil@bar.com>\r\nRCPT TO:<Brown@foo.com>\r\nDATA\r\nsmuggled\r\n.\r\n",
		"a\n.\r\nMAIL FROM:<evil@bar.com>\r\nRCPT TO:<Brown@foo.com>\r\nDATA\r\nsmuggled\r\n.\r\n",
	} {
		c.cmd("MAIL FROM:<Smith@bar.com>", "250")
		c.cmd("RCPT TO:<Jones@foo.com>", "250")
		c.cmd("DATA", "354")
		c.send(data)
		c.expect("550")
		c.cmd("NOOP", "250")
	}
	if h.count() != 0 {
		t.Errorf("%d messages delivered, want none", h.count())
	}
}

// Deviation from RFC 5321 2.3.8 for command lines: one that ends in a bare
// LF is taken as if it ended in CRLF. That lets a person type a session by
// hand, and unlike in DATA there is no second reader who might see the line
// differently.
func TestRFC5321_2_3_8_Deviation_BareLFEndsACommand(t *testing.T) {
	_, c := fooCom(t)
	c.send("EHLO bar.com\n")
	c.expect("250")
	c.send("NOOP\n")
	c.expect("250")
}

// RFC 5321 4.5.3.1.4: a command line of 512 octets including CRLF has to be
// accepted. 4.3.2: a longer one may get 500 "line too long"; the session
// goes on with the next line.
func TestRFC5321_4_5_3_1_4_CommandLineLength(t *testing.T) {
	_, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	line := "NOOP " + strings.Repeat("x", 512-2-5)
	if len(line)+2 != 512 {
		t.Fatal("test line is not 512 octets")
	}
	c.cmd(line, "250")
	// Far beyond any limit: refused, and the rest of the line is not read
	// as commands.
	c.cmd("NOOP "+strings.Repeat("x", 100000), "500")
	c.cmd("NOOP", "250")
}

// RFC 5321 4.5.3.1.5: a reply line is at most 512 octets with its CRLF, and
// 4.2 limits its text to HT, SP and printable ASCII. A handler may pass on
// text from elsewhere (Rspamd's reason for a rejection); it is made to fit.
func TestRFC5321_4_5_3_1_5_ReplyLine(t *testing.T) {
	h := &recorder{rcpt: func(string) error {
		return &Error{550, "5.7.1", "Rejected: \x00\x1b[31m café \r\n250 2.0.0 OK " + strings.Repeat("spam ", 400)}
	}}
	c := dial(t, serveWith(t, &Server{Handler: h}))
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	reply := c.cmd("RCPT TO:<Jones@foo.com>", "550")
	if len(reply) > 512 || !regexp.MustCompile(`^550 5\.7\.1 [\t -~]+\r\n$`).MatchString(reply) {
		t.Errorf("reply of %d octets: %q", len(reply), reply)
	}
	// Exactly one reply came: the CRLF in the text did not start another.
	c.cmd("NOOP", "250")
}

// RFC 5321 4.5.3.1.6: a text line of 1000 octets including CRLF has to be
// accepted, not counting a period added for transparency.
func TestRFC5321_4_5_3_1_6_TextLineLength(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	msg := strings.Repeat("a", 998) + "\r\n" + "." + strings.Repeat("b", 997) + "\r\n"
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send(stuff(msg))
	c.expect("250")
	if _, got := h.last(t); string(got) != msg {
		t.Errorf("delivered %d octets, want %d", len(got), len(msg))
	}
}

// RFC 5321 4.5.3.1.7: a message content of 64K octets has to be accepted.
// The server's default limit is far above that.
func TestRFC5321_4_5_3_1_7_MessageContent(t *testing.T) {
	h, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	msg := strings.Repeat(strings.Repeat("m", 62)+"\r\n", 1024) // 64 KiB
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
	c.cmd("DATA", "354")
	c.send(msg + ".\r\n")
	c.expect("250")
	if _, got := h.last(t); len(got) != 64<<10 || string(got) != msg {
		t.Errorf("delivered %d octets, want %d", len(got), len(msg))
	}
}

// RFC 5321 4.5.3.1.8 and 4.5.3.1.10: 100 recipients have to be accepted.
// One over the limit gets 452, and the ones accepted before stay.
func TestRFC5321_4_5_3_1_8_Recipients(t *testing.T) {
	h, c := fooCom(t) // the default limit
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	for i := 1; i <= 100; i++ {
		c.cmd(fmt.Sprintf("RCPT TO:<user%d@foo.com>", i), "250")
	}
	c.cmd("RCPT TO:<user101@foo.com>", "452")
	c.cmd("DATA", "354")
	c.send("x\r\n.\r\n")
	c.expect("250")
	if env, _ := h.last(t); len(env.Rcpts) != 100 || env.Rcpts[0] != "user1@foo.com" || env.Rcpts[99] != "user100@foo.com" {
		t.Errorf("%d recipients delivered", len(env.Rcpts))
	}
}

// RFC 5321 4.1.1.11: "If the server SMTP does not recognize or cannot
// implement one or more of the parameters associated with a particular MAIL
// FROM or RCPT TO command, it will return code 555."
func TestRFC5321_4_1_1_11_UnknownParameters(t *testing.T) {
	_, c := fooCom(t)
	c.cmd("EHLO bar.com", "250")
	c.cmd("MAIL FROM:<Smith@bar.com> FROB=1", "555")
	c.cmd("MAIL FROM:<Smith@bar.com> FROB", "555")
	c.cmd("MAIL FROM:<Smith@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@foo.com> FROB=1", "555")
	// A parameter that is not even an esmtp-param is a syntax error.
	c.cmd("RCPT TO:<Jones@foo.com> FROB=", "501")
	c.cmd("RCPT TO:<Jones@foo.com>", "250")
}

// replyLine is RFC 5321 4.2's Reply-line for one line, with the code limited
// as 4.3.2 demands: three digits, the first between 2 and 5.
var replyLine = regexp.MustCompile(`^[2-5][0-5][0-9]([ -][\t -~]*)?\r\n$`)

// tour runs a session that reaches most of the server's replies and returns
// each command with its reply. The command "." stands for a message.
func tour(t *testing.T, c *client, greeting string) [][2]string {
	t.Helper()
	out := [][2]string{{"(connect)", greeting}}
	for _, cmd := range []string{
		"NOOP", "MAIL FROM:<Smith@bar.com>", "FROB", "EHLO", "EHLO bar.com", "HELO bar.com", "EHLO bar.com",
		"RCPT TO:<Jones@foo.com>", "DATA", "VRFY Crispin", "VRFY", "EXPN Example-People", "HELP", "STARTTLS",
		"AUTH PLAIN", "MAIL FROM:Smith@bar.com", "MAIL FROM:<Smith@bar_baz.com>", "MAIL FROM:<Smith@bar.com> FROB=1",
		"MAIL FROM:<Smith@bar.com> SIZE=x", "MAIL FROM:<Smith@bar.com> SIZE=99999999999",
		"MAIL FROM:<Smït@bar.com>", "MAIL FROM:<Smith@bar.com> BODY=8BITMIME SIZE=100",
		"MAIL FROM:<Smith@bar.com>", "DATA", "RCPT TO:<Green@foo.com>", "RCPT TO:<Jonés@foo.com>",
		"RCPT TO:Jones@foo.com", "RCPT TO:<Jones@foo.com> FROB=1", "RCPT TO:<Later@foo.com>", "RCPT TO:<Jones@foo.com>",
		"DATA now", "DATA", ".", "RSET", "RSET now", "MAIL FROM:<>", "RCPT TO:<Postmaster>", "DATA", "bare\nLF\r\n.",
		"NOOP " + strings.Repeat("x", 5000), "QUIT now", "QUIT",
	} {
		line := cmd
		if cmd == "." {
			line = "Subject: x\r\n\r\nbody\r\n."
		}
		out = append(out, [2]string{cmd, c.any(line)})
	}
	return out
}

// tourServer is fooCom plus a mailbox that is busy.
func tourServer(t *testing.T) *client {
	t.Helper()
	h := &recorder{rcpt: func(to string) error {
		switch {
		case strings.HasPrefix(to, "Green@"):
			return &Error{550, "5.1.1", "No such user here"}
		case strings.HasPrefix(to, "Later@"):
			return &Error{451, "4.3.0", "Try again later"}
		}
		return nil
	}}
	conn, err := net.Dial("tcp", serveWith(t, &Server{Handler: h, MaxMessageBytes: 100000}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return &client{t: t, conn: conn, r: bufio.NewReader(conn)}
}

// RFC 5321 4.2 and 4.2.1: every reply line is "code SP text CRLF", or
// "code-text CRLF" for all but the last line of a multiline reply; all lines
// of one reply carry the same code; codes have three digits and start with 2
// to 5 (4.3.2); a line is at most 512 octets (4.5.3.1.5).
func TestRFC5321_4_2_ReplySyntax(t *testing.T) {
	c := tourServer(t)
	for _, step := range tour(t, c, c.expect("220")) {
		cmd, reply := step[0], step[1]
		lines := strings.SplitAfter(reply, "\r\n")
		lines = lines[:len(lines)-1]
		for i, l := range lines {
			if !replyLine.MatchString(l) || len(l) > 512 {
				t.Errorf("%s: reply line %q is not a Reply-line", cmd, l)
				continue
			}
			if l[:3] != lines[0][:3] {
				t.Errorf("%s: codes differ within one reply: %q", cmd, reply)
			}
			if last := i == len(lines)-1; (l[3] == ' ') != last {
				t.Errorf("%s: line %d of %d has separator %q", cmd, i+1, len(lines), l[3])
			}
		}
	}
}

// RFC 5321 4.3.2: the replies each command may give. 500, 501 and 421 are
// open to every command; 4.1.1.11 adds 555 for MAIL and RCPT, which the
// table lists too.
func TestRFC5321_4_3_2_CommandReplySequences(t *testing.T) {
	allowed := map[string]string{
		"(connect)": "220 554",
		"EHLO":      "250 504 550 502",
		"HELO":      "250 504 550 502",
		"MAIL":      "250 552 451 452 550 553 503 455 555",
		"RCPT":      "250 251 550 551 552 553 450 451 452 503 455 555",
		"DATA":      "354 503 554",
		"(data)":    "250 552 554 451 452 450 550",
		"RSET":      "250",
		"VRFY":      "250 251 252 550 551 553 502 504",
		"EXPN":      "250 252 550 500 502 504",
		"HELP":      "211 214 502 504",
		"NOOP":      "250",
		"QUIT":      "221",
	}
	c := tourServer(t)
	seen := map[string]bool{}
	for _, step := range tour(t, c, c.expect("220")) {
		cmd, got := step[0], code(step[1])
		verb, _, _ := strings.Cut(cmd, " ")
		if cmd == "." || strings.HasPrefix(cmd, "bare\n") {
			verb = "(data)"
		}
		seen[verb+" "+got] = true
		if got == "500" || got == "501" || got == "421" {
			continue
		}
		set, known := allowed[verb]
		if !known {
			// FROB, STARTTLS without TLS, AUTH: not commands of RFC
			// 5321 here, so 500 or 502.
			if got != "502" {
				t.Errorf("%s: %s for a command that is not offered, want 500 or 502", cmd, got)
			}
			continue
		}
		if !strings.Contains(set, got) {
			t.Errorf("%s: reply %s, RFC 5321 4.3.2 allows %s", cmd, got, set)
		}
	}
	// The tour did reach the replies it is meant to check.
	for _, want := range []string{"MAIL 250", "MAIL 503", "MAIL 501", "MAIL 555", "MAIL 552", "MAIL 550", "RCPT 250",
		"RCPT 550", "RCPT 553", "RCPT 451", "RCPT 555", "RCPT 503", "DATA 354", "DATA 503", "DATA 501", "(data) 250",
		"(data) 550", "VRFY 252", "EXPN 502", "HELP 214", "NOOP 250", "NOOP 500", "RSET 250", "QUIT 221", "EHLO 501"} {
		if !seen[want] {
			t.Errorf("the tour never got %s", want)
		}
	}
}

// RFC 5321 4.2 and 4.3.1: the greeting is "220 Domain ..." and the reply to
// QUIT "221 Domain ...", as in every example of appendix D.
func TestRFC5321_4_3_1_GreetingAndClosing(t *testing.T) {
	c := tourServer(t)
	if greeting := c.expect("220"); !strings.HasPrefix(greeting, "220 foo.com ") {
		t.Errorf("greeting %q", greeting)
	}
	if bye := c.cmd("QUIT", "221"); !strings.Contains(bye, " foo.com ") {
		t.Errorf("closing %q", bye)
	}
}

// replay replays the client's side of an RFC example, one line at a time,
// and checks the reply codes against the server's side of the example.
func replay(t *testing.T, c *client, script string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(script), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		who, text, _ := strings.Cut(line, ":")
		text = strings.TrimPrefix(text, " ")
		switch who {
		case "C":
			c.send(text + "\r\n")
		case "S":
			if len(text) > 3 && text[3] == '-' {
				continue // our multiline replies have other lines
			}
			if i == 0 {
				continue // the greeting was read by dial
			}
			c.expect(text[:3])
		default:
			t.Fatalf("bad script line %q", line)
		}
	}
}

// RFC 5321 appendix D.1, "A Typical SMTP Transaction Scenario": mail from
// Smith at bar.com to Jones, Green and Brown at foo.com. Green has no
// mailbox. The client's lines are the RFC's; so are the reply codes.
func TestRFC5321_AppendixD1_TypicalTransaction(t *testing.T) {
	h, c := fooCom(t)
	replay(t, c, `
		S: 220 foo.com Simple Mail Transfer Service Ready
		C: EHLO bar.com
		S: 250-foo.com greets bar.com
		S: 250-8BITMIME
		S: 250-SIZE
		S: 250-DSN
		S: 250 HELP
		C: MAIL FROM:<Smith@bar.com>
		S: 250 OK
		C: RCPT TO:<Jones@foo.com>
		S: 250 OK
		C: RCPT TO:<Green@foo.com>
		S: 550 No such user here
		C: RCPT TO:<Brown@foo.com>
		S: 250 OK
		C: DATA
		S: 354 Start mail input; end with <CRLF>.<CRLF>
		C: Blah blah blah...
		C: ...etc. etc. etc.
		C: .
		S: 250 OK
		C: QUIT
		S: 221 foo.com Service closing transmission channel`)
	env, msg := h.last(t)
	if env.From != "Smith@bar.com" || env.Helo != "bar.com" || strings.Join(env.Rcpts, " ") != "Jones@foo.com Brown@foo.com" {
		t.Errorf("envelope %+v", env)
	}
	// The example's second line starts with a period on the wire, so by
	// 4.5.2 one is removed.
	if string(msg) != "Blah blah blah...\r\n..etc. etc. etc.\r\n" {
		t.Errorf("message %q", msg)
	}
}

// RFC 5321 appendix D.2, "Aborted SMTP Transaction Scenario".
func TestRFC5321_AppendixD2_AbortedTransaction(t *testing.T) {
	h, c := fooCom(t)
	replay(t, c, `
		S: 220 foo.com Simple Mail Transfer Service Ready
		C: EHLO bar.com
		S: 250-foo.com greets bar.com
		S: 250 HELP
		C: MAIL FROM:<Smith@bar.com>
		S: 250 OK
		C: RCPT TO:<Jones@foo.com>
		S: 250 OK
		C: RCPT TO:<Green@foo.com>
		S: 550 No such user here
		C: RSET
		S: 250 OK
		C: QUIT
		S: 221 foo.com Service closing transmission channel`)
	if h.count() != 0 {
		t.Errorf("an aborted transaction delivered %d messages", h.count())
	}
}

// RFC 5321 appendix D.3, "Relayed Mail Scenario", both steps as the
// receiving side sees them. Whether to relay is the handler's decision; the
// protocol is the same. Step 2 carries a Received line with a folded date.
func TestRFC5321_AppendixD3_RelayedMail(t *testing.T) {
	h, c := fooCom(t)
	replay(t, c, `
		S: 220 foo.com Simple Mail Transfer Service Ready
		C: EHLO bar.com
		S: 250-foo.com greets bar.com
		S: 250 HELP
		C: MAIL FROM:<JQP@bar.com>
		S: 250 OK
		C: RCPT TO:<Jones@XYZ.COM>
		S: 250 OK
		C: DATA
		S: 354 Start mail input; end with <CRLF>.<CRLF>
		C: Date: Thu, 21 May 1998 05:33:29 -0700
		C: From: John Q. Public <JQP@bar.com>
		C: Subject: The Next Meeting of the Board
		C: To: Jones@xyz.com
		C:
		C: Bill:
		C: The next meeting of the board of directors will be
		C: on Tuesday.
		C: John.
		C: .
		S: 250 OK
		C: QUIT
		S: 221 foo.com Service closing transmission channel`)
	env, msg := h.last(t)
	if env.From != "JQP@bar.com" || env.Rcpts[0] != "Jones@XYZ.COM" {
		t.Errorf("envelope %+v", env)
	}
	want := "Date: Thu, 21 May 1998 05:33:29 -0700\r\nFrom: John Q. Public <JQP@bar.com>\r\n" +
		"Subject: The Next Meeting of the Board\r\nTo: Jones@xyz.com\r\n\r\nBill:\r\n" +
		"The next meeting of the board of directors will be\r\non Tuesday.\r\nJohn.\r\n"
	if string(msg) != want {
		t.Errorf("message %q", msg)
	}

	// Step 2. The script helper trims lines, so this one is sent by hand
	// to keep the folded header and the indented signature.
	c = dial(t, serveWith(t, &Server{Handler: h, Hostname: "xyz.com"}))
	c.cmd("EHLO foo.com", "250")
	c.cmd("MAIL FROM:<JQP@bar.com>", "250")
	c.cmd("RCPT TO:<Jones@XYZ.COM>", "250")
	c.cmd("DATA", "354")
	step2 := "Received: from bar.com by foo.com ; Thu, 21 May 1998\r\n    05:33:29 -0700\r\n" +
		"Date: Thu, 21 May 1998 05:33:22 -0700\r\nFrom: John Q. Public <JQP@bar.com>\r\n" +
		"Subject:  The Next Meeting of the Board\r\nTo: Jones@xyz.com\r\n\r\nBill:\r\n" +
		"The next meeting of the board of directors will be\r\non Tuesday.\r\n                        John.\r\n"
	c.send(step2 + ".\r\n")
	c.expect("250")
	c.cmd("QUIT", "221")
	if _, msg := h.last(t); string(msg) != step2 {
		t.Errorf("step 2 message %q", msg)
	}
}

// RFC 5321 appendix D.4, "Verifying and Sending Scenario".
//
// Deviation: the example's server answers VRFY with 250 and the mailbox. We
// answer 252, which 3.5.3 and 7.3 allow a server that does not want to
// confirm which addresses exist; the rest of the scenario is unchanged.
func TestRFC5321_AppendixD4_VerifyingAndSending(t *testing.T) {
	h, c := fooCom(t)
	replay(t, c, `
		S: 220 foo.com Simple Mail Transfer Service Ready
		C: EHLO bar.com
		S: 250-foo.com greets bar.com
		S: 250 HELP
		C: VRFY Crispin
		S: 252 (the RFC's server says: 250 Mark Crispin <Admin.MRC@foo.com>)
		C: MAIL FROM:<EAK@bar.com>
		S: 250 OK
		C: RCPT TO:<Admin.MRC@foo.com>
		S: 250 OK
		C: DATA
		S: 354 Start mail input; end with <CRLF>.<CRLF>
		C: Blah blah blah...
		C: ...etc. etc. etc.
		C: .
		S: 250 OK
		C: QUIT
		S: 221 foo.com Service closing transmission channel`)
	if env, _ := h.last(t); env.From != "EAK@bar.com" || env.Rcpts[0] != "Admin.MRC@foo.com" {
		t.Errorf("envelope %+v", env)
	}
}
