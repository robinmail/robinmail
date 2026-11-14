package smtp

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the client, written from RFC 5321 (the sending side) and the
// RFCs of the extensions it uses. Most run against a scripted server, so
// the test decides what the client is told; some against our own Server.

// fakeServer is the other end of a client connection. For every command
// line it asks reply what to answer; an empty answer is no answer. After a
// 354 it reads the message and asks reply("<data>").
type fakeServer struct {
	t        *testing.T
	greeting string
	reply    func(line string) string
	// batch makes it read that many lines, from MAIL on, before answering
	// any of them: a client that waits for an answer in between gets
	// stuck.
	batch int

	mu    sync.Mutex
	lines []string
	data  []string
}

func (f *fakeServer) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

func (f *fakeServer) message() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.data) == 0 {
		return ""
	}
	return f.data[len(f.data)-1]
}

// start runs the server on one end of a pipe and returns a Client on the
// other, greeted and with short timeouts.
func (f *fakeServer) start() (*Client, error) {
	f.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		if f.greeting == "" {
			f.greeting = "220 mx.example.net ESMTP\r\n"
		}
		conn.Write([]byte(f.greeting))
		for {
			var batch []string
			for n := 1; len(batch) < n; {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				batch = append(batch, strings.TrimRight(line, "\r\n"))
				if f.batch > 0 && strings.HasPrefix(line, "MAIL") {
					n = f.batch
				}
			}
			for _, line := range batch {
				f.mu.Lock()
				f.lines = append(f.lines, line)
				f.mu.Unlock()
				rep := f.reply(line)
				conn.Write([]byte(rep))
				if strings.HasPrefix(rep, "354") {
					var msg strings.Builder
					for {
						l, err := r.ReadString('\n')
						if err != nil {
							return
						}
						if l == ".\r\n" {
							break
						}
						msg.WriteString(l)
					}
					f.mu.Lock()
					f.data = append(f.data, msg.String())
					f.mu.Unlock()
					conn.Write([]byte(f.reply("<data>")))
				}
				if strings.HasPrefix(rep, "221") || strings.HasPrefix(rep, "421") {
					return
				}
			}
		}
	}()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close() })
	short := 3 * time.Second
	return NewClient(conn, "client.example.org", Timeouts{short, short, short, short, short, short, short})
}

// esmtp answers like a server with the given extensions.
func esmtp(ext ...string) func(string) string {
	return func(line string) string {
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO":
			out := "250-mx.example.net greets client.example.org\r\n"
			for _, e := range ext {
				out += "250-" + e + "\r\n"
			}
			return out + "250 HELP\r\n"
		case "MAIL", "RCPT", "RSET":
			return "250 2.1.0 OK\r\n"
		case "DATA":
			return "354 go ahead\r\n"
		case "<DATA>":
			return "250 2.0.0 OK queued as 12345\r\n"
		case "QUIT":
			return "221 bye\r\n"
		}
		return "500 what\r\n"
	}
}

func hello(t *testing.T, f *fakeServer) *Client {
	t.Helper()
	c, err := f.start()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Hello(); err != nil {
		t.Fatal(err)
	}
	return c
}

// RFC 5321 4.2 and 4.2.1: a reply is a three-digit code and text; in a
// multiline reply every line but the last has a hyphen after the code, and
// all lines have the same code.
func TestRFC5321ClientReplies(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		code      int
		enhanced  string
		lines     int
		bad       bool
	}{
		// The example of 4.2.1.
		{"multiline", "250-First line\r\n250-Second line\r\n250-234 Text beginning with numbers\r\n250 The last line\r\n", 250, "", 4, false},
		{"one line", "250 OK\r\n", 250, "", 1, false},
		{"code only", "250\r\n", 250, "", 1, false}, // 4.2: text is optional for a client to accept
		{"enhanced code", "550 5.1.1 No such user here\r\n", 550, "5.1.1", 1, false},
		{"enhanced code of another class is not one", "550 2.1.1 odd\r\n", 550, "", 1, false},
		{"three digits in a status", "451 4.7.651 try later\r\n", 451, "4.7.651", 1, false},
		{"bare LF", "250 OK\n", 250, "", 1, false},
		{"codes differ", "250-one\r\n251 two\r\n", 0, "", 0, true},
		{"not a code", "abc hello\r\n", 0, "", 0, true},
		{"code out of range", "199 hello\r\n", 0, "", 0, true},
		{"no separator", "250OK\r\n", 0, "", 0, true},
		{"too short", "25\r\n", 0, "", 0, true},
		{"endless", strings.Repeat("250-more\r\n", 200), 0, "", 0, true},
		{"a line without end", "250 " + strings.Repeat("x", 10000) + "\r\n", 0, "", 0, true},
	} {
		f := &fakeServer{t: t, reply: func(string) string { return tc.raw }}
		c, err := f.start()
		if err != nil {
			t.Fatal(err)
		}
		rep, err := c.cmd("NOOP", time.Second)
		if tc.bad {
			if !errors.Is(err, ErrProtocol) || !c.Broken() {
				t.Errorf("%s: err %v, broken %v; want ErrProtocol and a broken connection", tc.name, err, c.Broken())
			}
			continue
		}
		if err != nil || rep.Code != tc.code || rep.Enhanced != tc.enhanced || len(rep.Lines) != tc.lines {
			t.Errorf("%s: %+v, %v", tc.name, rep, err)
		}
	}
	rep := Reply{Code: 550, Lines: []string{"5.1.1 No such", "user"}}
	if rep.String() != "550 5.1.1 No such user" || !rep.Permanent() || rep.Temporary() || rep.Positive() {
		t.Errorf("Reply: %s", rep)
	}
}

// RFC 5321 3.1: "The SMTP protocol allows a server to formally reject a
// mail session [...] with a 554 response"; the client then quits.
// 4.2.3 and 3.8: 421 means the server is closing the channel.
func TestRFC5321ClientGreeting(t *testing.T) {
	f := &fakeServer{t: t, greeting: "554 No SMTP service here\r\n", reply: esmtp()}
	if _, err := f.start(); err == nil || !strings.Contains(err.Error(), "554 No SMTP service here") {
		t.Errorf("554 greeting: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := f.seen(); len(got) != 1 || got[0] != "QUIT" {
		t.Errorf("after a 554 greeting the client sent %q, want QUIT", got)
	}
	f = &fakeServer{t: t, greeting: "220-mx.example.net\r\n220 ready\r\n", reply: func(string) string { return "421 4.3.2 shutting down\r\n" }}
	c, err := f.start()
	if err != nil {
		t.Fatal(err)
	}
	var rep Reply
	if err := c.Hello(); !errors.As(err, &rep) || rep.Code != 421 || !c.Broken() {
		t.Errorf("421 to EHLO: %v, broken %v", err, c.Broken())
	}
	if _, err := c.Send("a@b.example", []string{"c@d.example"}, []byte("x\r\n")); err == nil {
		t.Error("a transaction on a connection that got 421")
	}
}

// RFC 5321 3.2 and 4.1.1.1: EHLO first; "if the SMTP server [...] returns
// 'command not recognized', the client SHOULD fall back to HELO". The
// keywords of the EHLO reply are the extensions.
func TestRFC5321ClientHello(t *testing.T) {
	f := &fakeServer{t: t, reply: esmtp("8BITMIME", "SIZE 10240000", "pipelining", "STARTTLS")}
	c := hello(t, f)
	if got := f.seen(); got[0] != "EHLO client.example.org" {
		t.Errorf("sent %q", got)
	}
	if v, ok := c.Extension("size"); !ok || v != "10240000" {
		t.Errorf("SIZE: %q %v", v, ok)
	}
	// 4.1.1.1: keywords are case-insensitive.
	if _, ok := c.Extension("PIPELINING"); !ok {
		t.Error("pipelining in lower case is not seen")
	}
	if _, ok := c.Extension("SMTPUTF8"); ok {
		t.Error("an extension that was not offered")
	}
	// The domain in the first line is not an extension.
	if _, ok := c.Extension("mx.example.net"); ok {
		t.Error("the greeting line is taken for a keyword")
	}

	old := &fakeServer{t: t, reply: func(line string) string {
		if strings.HasPrefix(line, "EHLO") {
			return "500 Command not recognized\r\n"
		}
		if strings.HasPrefix(line, "HELO") {
			return "250 mx.example.net\r\n"
		}
		return esmtp()(line)
	}}
	c = hello(t, old)
	if got := old.seen(); len(got) != 2 || got[1] != "HELO client.example.org" {
		t.Errorf("no HELO after a refused EHLO: %q", got)
	}
	// Without extensions: no parameters on MAIL, and no pipelining.
	res, err := c.Send("a@b.example", []string{"c@d.example"}, []byte("Subject: x\r\n\r\nhi\r\n"))
	if err != nil || !res[0].Delivered() {
		t.Fatalf("%v %v", res, err)
	}
	if got := old.seen(); got[2] != "MAIL FROM:<a@b.example>" {
		t.Errorf("MAIL to a server without extensions: %q", got[2])
	}
}

// RFC 5321 3.3 and appendix D.1: a transaction with a recipient the server
// refuses. Each recipient has its own answer; the message goes to the
// ones accepted.
func TestRFC5321ClientTransaction(t *testing.T) {
	f := &fakeServer{t: t}
	f.reply = func(line string) string {
		if line == "RCPT TO:<Green@foo.com>" {
			return "550 No such user here\r\n"
		}
		return esmtp()(line)
	}
	c := hello(t, f)
	msg := "Date: Thu, 21 May 1998 05:33:29 -0700\r\nFrom: John Q. Public <JQP@bar.com>\r\nSubject: The Next Meeting of the Board\r\n\r\nBill:\r\n" +
		".\r\n..two dots\r\n.leading dot\r\nThe next meeting of the board of directors will be\r\non Tuesday.\r\n"
	res, err := c.Send("Smith@bar.com", []string{"Jones@foo.com", "Green@foo.com", "Brown@foo.com"}, []byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"MAIL FROM:<Smith@bar.com>", "RCPT TO:<Jones@foo.com>", "RCPT TO:<Green@foo.com>", "RCPT TO:<Brown@foo.com>", "DATA"}
	if got := f.seen()[1:]; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commands %q, want %q", got, want)
	}
	if len(res) != 3 || !res[0].Delivered() || res[1].Delivered() || !res[2].Delivered() ||
		res[1].Reply.Code != 550 || res[0].Reply.String() != "250 2.0.0 OK queued as 12345" || res[0].Addr != "Jones@foo.com" {
		t.Errorf("results %+v", res)
	}
	// 4.5.2: a line that starts with a period gets one more, and the
	// server sees the message as it was.
	if got := f.message(); !strings.Contains(got, "Bill:\r\n..\r\n...two dots\r\n..leading dot\r\nThe next") || len(got) != len(msg)+3 {
		t.Errorf("on the wire:\n%q", got)
	}

	// No recipient accepted: no DATA (3.3: "If no recipients were accepted
	// [...] the client must not send DATA"), and the session is reset so
	// that the connection can be used again.
	none := &fakeServer{t: t}
	none.reply = func(line string) string {
		if strings.HasPrefix(line, "RCPT") {
			return "550 5.1.1 unknown\r\n"
		}
		return esmtp()(line)
	}
	c = hello(t, none)
	res, err = c.Send("a@b.example", []string{"x@foo.com"}, []byte("x\r\n"))
	if err != nil || res[0].Reply.Enhanced != "5.1.1" {
		t.Fatalf("%v %v", res, err)
	}
	if got := none.seen(); got[len(got)-1] != "RSET" || strings.Contains(strings.Join(got, " "), "DATA") {
		t.Errorf("commands %q", got)
	}

	// MAIL refused: every recipient has that answer.
	refused := &fakeServer{t: t}
	refused.reply = func(line string) string {
		if strings.HasPrefix(line, "MAIL") {
			return "451 4.7.1 greylisted, come back later\r\n"
		}
		return esmtp()(line)
	}
	c = hello(t, refused)
	res, _ = c.Send("a@b.example", []string{"x@foo.com", "y@foo.com"}, []byte("x\r\n"))
	if !res[0].Reply.Temporary() || res[1].Reply.Enhanced != "4.7.1" || len(refused.seen()) != 2 {
		t.Errorf("results %+v, commands %q", res, refused.seen())
	}

	// A message without a final line end still ends in CRLF "." CRLF.
	f2 := &fakeServer{t: t, reply: esmtp()}
	c = hello(t, f2)
	c.Send("a@b.example", []string{"c@d.example"}, []byte("no line end"))
	if got := f2.message(); got != "no line end\r\n" {
		t.Errorf("message %q", got)
	}
	// Two transactions on one connection.
	if res, err := c.Send("a@b.example", []string{"c@d.example"}, []byte("second\r\n")); err != nil || !res[0].Delivered() || c.Sent != 2 {
		t.Errorf("second transaction: %v %v", res, err)
	}
	if err := c.Quit(); err != nil {
		t.Error(err)
	}
}

// RFC 2920 3.1: with PIPELINING the client may send MAIL, RCPT and DATA
// without waiting. The server here answers nothing before it has read all
// five commands, as the example of section 4 does.
func TestRFC2920ClientPipelining(t *testing.T) {
	f := &fakeServer{t: t}
	f.reply = func(line string) string {
		if line == "RCPT TO:<green@example.com>" {
			return "550 remote mail to <green@example.com> not allowed\r\n"
		}
		return esmtp("PIPELINING")(line)
	}
	f.batch = 5
	c := hello(t, f)
	res, err := c.Send("mrose@dbc.mtview.ca.us", []string{"ned@innosoft.com", "dan@innosoft.com", "green@example.com"}, []byte("x\r\n"))
	if err != nil {
		t.Fatalf("the client did not pipeline: %v", err)
	}
	if !res[0].Delivered() || !res[1].Delivered() || res[2].Reply.Code != 550 {
		t.Errorf("results %+v", res)
	}

	// 3.1: "If none of the RCPT TO commands succeed [and] the DATA
	// command is accepted, the client SHOULD send a single dot." Its own
	// example has the server refuse DATA with 554; both work.
	for _, dataReply := range []string{"554 no valid recipients given\r\n", "354 enter mail\r\n"} {
		f := &fakeServer{t: t}
		f.reply = func(line string) string {
			switch {
			case strings.HasPrefix(line, "RCPT"):
				return "550 5.1.1 no\r\n"
			case line == "DATA":
				return dataReply
			case line == "<data>":
				return "554 5.5.1 no recipients\r\n"
			}
			return esmtp("PIPELINING")(line)
		}
		c := hello(t, f)
		res, err := c.Send("a@b.example", []string{"x@foo.com", "y@foo.com"}, []byte("secret\r\n"))
		if err != nil || res[0].Reply.Enhanced != "5.1.1" || res[1].Reply.Code != 550 {
			t.Errorf("%q: %+v %v", dataReply, res, err)
		}
		if f.message() != "" {
			t.Errorf("%q: the message was sent to no recipient: %q", dataReply, f.message())
		}
		if c.Broken() {
			t.Errorf("%q: the connection is not usable afterwards", dataReply)
		}
	}
}

// RFC 1870 6.1 and 6.2: the client declares the size; a message larger
// than the server's declared limit is not offered.
func TestRFC1870ClientSize(t *testing.T) {
	f := &fakeServer{t: t, reply: esmtp("SIZE 1000")}
	c := hello(t, f)
	msg := []byte("Subject: x\r\n\r\n" + strings.Repeat("a", 500) + "\r\n")
	if _, err := c.Send("a@b.example", []string{"c@d.example"}, msg); err != nil {
		t.Fatal(err)
	}
	if got := f.seen()[1]; got != "MAIL FROM:<a@b.example> SIZE=516" {
		t.Errorf("MAIL %q", got)
	}
	before := len(f.seen())
	res, err := c.Send("a@b.example", []string{"c@d.example"}, append(msg, msg...))
	if err != nil || res[0].Reply.Code != 552 || res[0].Reply.Enhanced != "5.3.4" || !res[0].Reply.Local {
		t.Errorf("too large: %+v %v", res, err)
	}
	if len(f.seen()) != before {
		t.Errorf("a message over the limit was offered: %q", f.seen()[before:])
	}
	// "SIZE" without a number, or 0, is no fixed limit (RFC 1870 4).
	for _, ext := range []string{"SIZE", "SIZE 0"} {
		c := hello(t, &fakeServer{t: t, reply: esmtp(ext)})
		if res, _ := c.Send("a@b.example", []string{"c@d.example"}, append(msg, msg...)); !res[0].Delivered() {
			t.Errorf("%s: %+v", ext, res)
		}
	}
}

// RFC 6152 2 and 3: 8-bit content is declared with BODY=8BITMIME, and
// "a sender SMTP MUST NOT send 8bit data" to a server without the
// extension: it converts the message or returns it. We do not convert.
func TestRFC6152Client8BitMIME(t *testing.T) {
	eight := []byte("Subject: x\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\ncaf\xc3\xa9\r\n")
	f := &fakeServer{t: t, reply: esmtp("8BITMIME")}
	c := hello(t, f)
	c.Send("a@b.example", []string{"c@d.example"}, eight)
	if got := f.seen()[1]; got != "MAIL FROM:<a@b.example> BODY=8BITMIME" {
		t.Errorf("MAIL %q", got)
	}
	c.Send("a@b.example", []string{"c@d.example"}, []byte("Subject: x\r\n\r\nplain\r\n"))
	if got := f.seen(); got[len(got)-3] != "MAIL FROM:<a@b.example>" {
		t.Errorf("7-bit message: MAIL %q", got[len(got)-3])
	}
	f = &fakeServer{t: t, reply: esmtp()}
	c = hello(t, f)
	res, err := c.Send("a@b.example", []string{"c@d.example"}, eight)
	if err != nil || res[0].Reply.Enhanced != "5.6.3" || !res[0].Reply.Permanent() || len(f.seen()) != 1 {
		t.Errorf("8-bit to a 7-bit server: %+v %v, commands %q", res, err, f.seen())
	}
}

// RFC 6531 3.2 to 3.4: SMTPUTF8 is asked for when an address or the header
// needs it. A server without the extension cannot have such a message.
// The one thing that can be downgraded is a domain: it has an ASCII form
// (A-labels), a local part does not.
func TestRFC6531ClientSMTPUTF8(t *testing.T) {
	plain := []byte("Subject: x\r\n\r\nhi\r\n")
	f := &fakeServer{t: t, reply: esmtp("SMTPUTF8", "8BITMIME")}
	c := hello(t, f)
	c.Send("a@b.example", []string{"δοκιμή@παράδειγμα.δοκιμή", "c@d.example"}, plain)
	got := f.seen()
	if got[1] != "MAIL FROM:<a@b.example> SMTPUTF8" || got[2] != "RCPT TO:<δοκιμή@xn--hxajbheg2az3al.xn--jxalpdlp>" {
		t.Errorf("commands %q", got)
	}
	// A header with UTF-8 (RFC 6532) needs the extension too.
	c.Send("a@b.example", []string{"c@d.example"}, []byte("Subject: caf\xc3\xa9\r\n\r\nhi\r\n"))
	if got := f.seen(); got[len(got)-3] != "MAIL FROM:<a@b.example> BODY=8BITMIME SMTPUTF8" {
		t.Errorf("UTF-8 header: MAIL %q", got[len(got)-3])
	}

	f = &fakeServer{t: t, reply: esmtp("8BITMIME")}
	c = hello(t, f)
	// Only the domain is not ASCII: sent as A-labels, without SMTPUTF8.
	res, _ := c.Send("a@b.example", []string{"info@bücher.example"}, plain)
	if got := f.seen(); !res[0].Delivered() || got[1] != "MAIL FROM:<a@b.example>" || got[2] != "RCPT TO:<info@xn--bcher-kva.example>" {
		t.Errorf("IDN downgrade: %+v, commands %q", res, got)
	}
	// One recipient with a non-ASCII local part: it fails alone.
	res, _ = c.Send("a@b.example", []string{"zoë@d.example", "c@d.example"}, plain)
	if res[0].Reply.Enhanced != "5.6.7" || !res[0].Reply.Local || !res[1].Delivered() {
		t.Errorf("mixed recipients: %+v", res)
	}
	for _, r := range f.seen() {
		if strings.Contains(r, "zoë") {
			t.Errorf("a non-ASCII address was sent to a server without SMTPUTF8: %q", r)
		}
	}
	// A sender or a header that needs it: nobody gets the message.
	before := len(f.seen())
	for name, try := range map[string]func() ([]RcptResult, error){
		"sender": func() ([]RcptResult, error) { return c.Send("zoë@b.example", []string{"c@d.example"}, plain) },
		"header": func() ([]RcptResult, error) {
			return c.Send("a@b.example", []string{"c@d.example"}, []byte("Subject: caf\xc3\xa9\r\n\r\nhi\r\n"))
		},
	} {
		if res, err := try(); err != nil || res[0].Reply.Enhanced != "5.6.7" {
			t.Errorf("%s: %+v %v", name, res, err)
		}
	}
	if len(f.seen()) != before {
		t.Errorf("commands sent for a message that cannot go: %q", f.seen()[before:])
	}
}

// RFC 5321 4.5.3.2: the timeouts a client "SHOULD" have at least, and that
// one that runs out ends the attempt.
func TestRFC5321ClientTimeouts(t *testing.T) {
	want := Timeouts{Greeting: 5 * time.Minute, Mail: 5 * time.Minute, Rcpt: 5 * time.Minute, DataInit: 2 * time.Minute,
		DataBlock: 3 * time.Minute, DataDone: 10 * time.Minute, Other: 5 * time.Minute}
	if RFCTimeouts != want || (Timeouts{}).withDefaults() != want {
		t.Errorf("timeouts %+v", RFCTimeouts)
	}
	f := &fakeServer{t: t}
	f.reply = func(line string) string {
		if line == "<data>" {
			return "" // never answers the end of the message
		}
		return esmtp()(line)
	}
	c := hello(t, f)
	c.Timeouts.DataDone = 200 * time.Millisecond
	start := time.Now()
	_, err := c.Send("a@b.example", []string{"c@d.example"}, []byte("x\r\n"))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || time.Since(start) > 2*time.Second || !c.Broken() {
		t.Errorf("no reply after the message: %v after %v", err, time.Since(start))
	}
	// A server that says nothing at all.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		if conn, err := l.Accept(); err == nil {
			time.Sleep(2 * time.Second)
			conn.Close()
		}
	}()
	conn, _ := net.Dial("tcp", l.Addr().String())
	if _, err := NewClient(conn, "client.example.org", Timeouts{Greeting: 100 * time.Millisecond}); !errors.As(err, &ne) || !ne.Timeout() {
		t.Errorf("no greeting: %v", err)
	}
}

// RFC 3207 4 and 4.2: after STARTTLS the client "MUST discard any
// knowledge obtained from the server" and say EHLO again. Against our own
// server, which offers AUTH only inside TLS.
func TestRFC3207ClientStartTLS(t *testing.T) {
	h := &recorder{}
	addr := serveWith(t, &Server{Handler: h, Hostname: "mx.example.net", TLSConfig: testTLSConfig(t)})
	connect := func() *Client {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c, err := NewClient(conn, "client.example.org", Timeouts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Hello(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := connect()
	if _, ok := c.Extension("STARTTLS"); !ok || c.TLS() != nil {
		t.Fatal("STARTTLS is not offered")
	}
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Extension("STARTTLS"); ok || c.TLS() == nil || c.TLS().Version < tls.VersionTLS12 {
		t.Errorf("after STARTTLS: extensions not read again, or no TLS state")
	}
	msg := "Subject: over TLS\r\n\r\n.dot\r\nbody\r\n"
	res, err := c.Send("a@b.example", []string{"c@d.example"}, []byte(msg))
	if err != nil || !res[0].Delivered() {
		t.Fatalf("%v %v", res, err)
	}
	if env, got := h.last(t); env.TLS == nil || string(got) != msg {
		t.Errorf("the server got %q, TLS %v", got, env.TLS != nil)
	}
	c.Quit()

	// A certificate that is checked and does not pass: no session.
	c = connect()
	if err := c.StartTLS(&tls.Config{ServerName: "mx.example.net"}); err == nil || !c.Broken() {
		t.Errorf("a self-signed certificate passed verification: %v", err)
	}

	// The client side of CVE-2011-0411: text that follows the 220 in the
	// same packet was not sent inside TLS and must not be believed.
	f := &fakeServer{t: t}
	f.reply = func(line string) string {
		if line == "STARTTLS" {
			return "220 go ahead\r\n250-injected\r\n250 AUTH PLAIN\r\n"
		}
		return esmtp("STARTTLS")(line)
	}
	c = hello(t, f)
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); !errors.Is(err, ErrProtocol) {
		t.Errorf("plaintext after the 220 of STARTTLS: %v", err)
	}
	// A server that refuses STARTTLS (3207 4: 454).
	f = &fakeServer{t: t}
	f.reply = func(line string) string {
		if line == "STARTTLS" {
			return "454 TLS not available due to temporary reason\r\n"
		}
		return esmtp("STARTTLS")(line)
	}
	c = hello(t, f)
	var rep Reply
	if err := c.StartTLS(&tls.Config{InsecureSkipVerify: true}); !errors.As(err, &rep) || rep.Code != 454 {
		t.Errorf("454 to STARTTLS: %v", err)
	}
}

// The client against our own server: what one writes the other reads,
// with every extension both have.
func TestClientAgainstServer(t *testing.T) {
	h := &recorder{rcpt: func(to string) error {
		if strings.HasPrefix(to, "nobody@") {
			return &Error{Code: 550, Enhanced: "5.1.1", Msg: "No such user"}
		}
		return nil
	}}
	addr := serveWith(t, &Server{Handler: h, Hostname: "mx.example.net", MaxMessageBytes: 1 << 20})
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(conn, "client.example.org", Timeouts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Hello(); err != nil {
		t.Fatal(err)
	}
	msg := "Subject: caf\xc3\xa9\r\n\r\n.\r\n..\r\nbody with \xc3\xa9\r\n"
	res, err := c.Send("zoë@b.example", []string{"mike@d.example", "nobody@d.example"}, []byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Delivered() || res[0].Reply.Enhanced != "2.0.0" || res[1].Reply.Code != 550 || res[1].Reply.Enhanced != "5.1.1" {
		t.Errorf("results %+v", res)
	}
	env, got := h.last(t)
	if string(got) != msg || env.From != "zoë@b.example" || !env.UTF8 || len(env.Rcpts) != 1 {
		t.Errorf("the server got %q with %+v", got, env)
	}
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'a'
		if i%70 == 68 {
			big[i], big[i+1] = '\r', '\n'
		}
	}
	if res, _ := c.Send("a@b.example", []string{"mike@d.example"}, big); res[0].Reply.Enhanced != "5.3.4" || !res[0].Reply.Local {
		t.Errorf("over the server's SIZE: %+v", res)
	}
	if err := c.Reset(); err != nil {
		t.Error(err)
	}
	c.Quit()
}
