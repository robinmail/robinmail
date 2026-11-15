package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// server is a "robinmail serve" running inside the test.
type server struct {
	addr string       // SMTP
	web  string       // the web listener, if the config has one
	stop func() error // what a SIGTERM does; returns what serve returned
}

// startServe runs serve on a free port with the given extra config.
func startServe(t *testing.T, c *cli) *server {
	t.Helper()
	listening := make(chan net.Addr, 1)
	onListen = func(a net.Addr) { listening <- a }
	webAddr := make(chan net.Addr, 1)
	onWebListen = func(a net.Addr) { webAddr <- a }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.run(ctx, "", "serve")
		done <- err
	}()
	s := &server{}
	stopped := false
	s.stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(shutdownGrace + 5*time.Second):
			return fmt.Errorf("serve did not stop")
		}
	}
	t.Cleanup(func() { s.stop(); onListen, onWebListen = nil, nil })
	select {
	case a := <-listening:
		s.addr = a.String()
		// serve reports the web listener before the SMTP one.
		select {
		case w := <-webAddr:
			s.web = w.String()
		default:
		}
	case err := <-done:
		stopped = true
		t.Fatalf("serve: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not start listening")
	}
	return s
}

// smtpClient speaks SMTP by hand and logs the dialogue, so "go test -v"
// shows what swaks would.
type smtpClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dialSMTP(t *testing.T, addr string) *smtpClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(90 * time.Second)) // an Rspamd scan looks things up in DNS
	c := &smtpClient{t: t, conn: conn, r: bufio.NewReader(conn)}
	c.read(220)
	return c
}

// read reads one reply, which may have several lines, and checks its code.
func (c *smtpClient) read(want int) string {
	c.t.Helper()
	var all strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("want %d, read error %v after %q", want, err, all.String())
		}
		c.t.Logf("S: %s", strings.TrimRight(line, "\r\n"))
		all.WriteString(line)
		if len(line) >= 4 && line[3] == '-' {
			continue
		}
		if !strings.HasPrefix(line, fmt.Sprint(want)) {
			c.t.Fatalf("want %d, got %q", want, all.String())
		}
		return all.String()
	}
}

func (c *smtpClient) cmd(line string, want int) string {
	c.t.Helper()
	c.t.Logf("C: %s", line)
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatal(err)
	}
	return c.read(want)
}

// send runs one transaction and returns the reply to the end of DATA.
func (c *smtpClient) send(from, to, msg string, want int) string {
	c.t.Helper()
	c.cmd("MAIL FROM:<"+from+">", 250)
	c.cmd("RCPT TO:<"+to+">", 250)
	c.cmd("DATA", 354)
	c.t.Logf("C: (%d bytes of message)", len(msg))
	if _, err := c.conn.Write([]byte(msg + ".\r\n")); err != nil {
		c.t.Fatal(err)
	}
	return c.read(want)
}

func message(to, id, subject, body string) string {
	return "Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"From: Anna <anna@sender.example>\r\n" +
		"To: Mike <" + to + ">\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <" + id + "@sender.example>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + body + "\r\n"
}

// The GTUBE string makes any spam filter reject a message. Rspamd's variant
// with a Y in front asks for "add header" instead; contrib/compose turns it on.
const (
	gtube       = "XJS*C4JDBQADN1.NSBN3*2IDNEN*GTUBE-STANDARD-ANTI-UBE-TEST-EMAIL*C.34X"
	gtubeHeader = "YJS*C4JDBQADN1.NSBN3*2IDNEN*GTUBE-STANDARD-ANTI-UBE-TEST-EMAIL*C.34X"
)

// TestServeEndToEnd is phase 2's exit: a message sent over SMTP is scanned
// by a real Rspamd and lands in the Inbox with score headers, GTUBE is
// refused with 550, and spam goes to Junk. It needs rqlite and Rspamd:
//
//	docker compose -f contrib/compose/compose.yaml up -d
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RSPAMD=http://127.0.0.1:11333 \
//	  go test -v -run TestServeEndToEnd ./cmd/robinmail
func TestServeEndToEnd(t *testing.T) {
	scanner := os.Getenv("ROBINMAIL_TEST_RSPAMD")
	if scanner == "" {
		t.Skip("ROBINMAIL_TEST_RSPAMD is not set")
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	if resp, err := hc.Get(scanner + "/ping"); err != nil {
		t.Skipf("Rspamd is not reachable: %v", err)
	} else {
		resp.Body.Close()
	}

	c := newCLI(t, fmt.Sprintf(`"smtp":{"listen":"127.0.0.1:0"},"rspamd":{"url":%q,"timeout_seconds":60},`, scanner))
	domain := "e2e-" + randomHex() + ".test"
	user, pm := "mike@"+domain, "postmaster@"+domain
	c.ok("", "key", "generate")
	c.ok("", "domain", "add", domain)
	c.ok("correct horse\n", "user", "add", user)
	c.ok("correct horse\n", "user", "add", pm)
	srv := startServe(t, c)

	s := dialSMTP(t, srv.addr)
	ehlo := s.cmd("EHLO client.sender.example", 250)
	for _, ext := range []string{"PIPELINING", "SIZE ", "8BITMIME", "SMTPUTF8", "ENHANCEDSTATUSCODES"} {
		if !strings.Contains(ehlo, ext) {
			t.Errorf("EHLO lacks %s", ext)
		}
	}

	// A normal message lands in the Inbox with Rspamd's score headers.
	s.send("anna@sender.example", user, message(user, "1", "Lunch on Tuesday", "Hi Mike, are you free for lunch on Tuesday? Anna"), 250)
	c.expect(c.ok("", "list", user), `Inbox \(m1\): 1 emails`, `(?m)^e1 +uid 1 +t1 +m1 .*Anna.*Lunch on Tuesday`)
	stored := c.ok("", "cat", user, "e1")
	head, body, _ := strings.Cut(stored, "\r\n\r\n")
	t.Logf("stored in the Inbox:\n%s", strings.ReplaceAll(head, "\r\n", "\n"))
	c.expect(head,
		`^Return-Path: <anna@sender\.example>\r\nReceived: from client\.sender\.example \(.*\[127\.0\.0\.1\]\)\r\n\tby mail\.example\.test \(robinmail\) with ESMTP id [0-9A-F]{16}\r\n\tfor <`+regexp.QuoteMeta(user)+`>;\r\n`,
		`(?m)^X-Spamd-Result: default: False \[-?\d+\.\d+ / \d+\.\d+\];\r$`,
		`(?m)^X-Rspamd-Action: no action\r$`,
		`(?m)^X-Rspamd-Server: `,
		`(?m)^Subject: Lunch on Tuesday\r$`)
	if body != "Hi Mike, are you free for lunch on Tuesday? Anna\r\n" {
		t.Errorf("body = %q", body)
	}
	if strings.Contains(head, "X-Spam: ") {
		t.Errorf("a clean message was marked as spam")
	}

	// GTUBE is refused, and nothing is stored.
	if reply := s.send("anna@sender.example", user, message(user, "2", "Buy now", gtube), 550); !strings.HasPrefix(reply, "550 5.7.1 ") {
		t.Errorf("GTUBE reply %q", reply)
	}
	c.expect(c.ok("", "list", user), `Inbox \(m1\): 1 emails`)

	// "add header" means spam: delivered, but to Junk, marked X-Spam.
	s.send("anna@sender.example", user, message(user, "3", "Cheap watches", gtubeHeader), 250)
	c.expect(c.ok("", "list", user), `Inbox \(m1\): 1 emails`)
	c.expect(c.ok("", "list", user, "junk"), `Junk \(m4\): 1 emails`, `(?m)^e2 +uid 1 +t2 +m4 .*Cheap watches`)
	c.expect(c.ok("", "cat", user, "e2"), `(?m)^X-Spam: Yes\r$`)

	// Plus-addressing reaches the base account; <Postmaster> needs no domain.
	s.send("anna@sender.example", "mike+lists@"+domain, message(user, "4", "To a tag", "Hello tag."), 250)
	c.expect(c.ok("", "list", user), `Inbox \(m1\): 2 emails`, `(?m)^e3 +uid 2 .*To a tag`)
	s.cmd("MAIL FROM:<>", 250)
	s.cmd("RCPT TO:<Postmaster>", 250)
	s.cmd("RSET", 250)

	// No relaying, no unknown users.
	s.cmd("MAIL FROM:<anna@sender.example>", 250)
	if reply := s.cmd("RCPT TO:<someone@elsewhere.example>", 550); !strings.HasPrefix(reply, "550 5.7.1 ") {
		t.Errorf("relay attempt: %q", reply)
	}
	if reply := s.cmd("RCPT TO:<nobody@"+domain+">", 550); !strings.HasPrefix(reply, "550 5.1.1 ") {
		t.Errorf("unknown user: %q", reply)
	}
	s.cmd("DATA", 554)

	// SIGTERM: the open session is told 421, and serve returns.
	done := make(chan error, 1)
	go func() { done <- srv.stop() }()
	s.read(421)
	if err := <-done; err != nil {
		t.Errorf("serve returned %v", err)
	}
	if conn, err := net.DialTimeout("tcp", srv.addr, time.Second); err == nil {
		conn.Close()
		t.Error("still listening after shutdown")
	}
	// What was accepted is still there, and readable.
	c.expect(c.ok("", "verify", user), `3 emails checked, 0 failed`)
}

// When Rspamd cannot be reached the sender is told to retry (451) and
// nothing is stored, unless fail_open says to deliver unscanned.
func TestServeRspamdDown(t *testing.T) {
	// A port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	l.Close()

	for _, failOpen := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail_open=%v", failOpen), func(t *testing.T) {
			c := newCLI(t, fmt.Sprintf(`"smtp":{"listen":"127.0.0.1:0"},"rspamd":{"url":%q,"fail_open":%v,"timeout_seconds":5},`, dead, failOpen))
			domain := "down-" + randomHex() + ".test"
			user := "mike@" + domain
			c.ok("", "key", "generate")
			c.ok("", "domain", "add", domain)
			c.ok("correct horse\n", "user", "add", user)
			srv := startServe(t, c)

			s := dialSMTP(t, srv.addr)
			s.cmd("EHLO client.sender.example", 250)
			if failOpen {
				s.send("anna@sender.example", user, message(user, "1", "Unscanned", "Hello."), 250)
				c.expect(c.ok("", "list", user), `Inbox \(m1\): 1 emails`)
				if out := c.ok("", "cat", user, "e1"); strings.Contains(out, "X-Spamd-Result") {
					t.Error("an unscanned message has score headers")
				}
			} else {
				if reply := s.send("anna@sender.example", user, message(user, "1", "Deferred", "Hello."), 451); !strings.HasPrefix(reply, "451 4.7.0 ") {
					t.Errorf("reply %q", reply)
				}
				c.expect(c.ok("", "list", user), `Inbox \(m1\): 0 emails`)
			}
			s.cmd("QUIT", 221)
			if err := srv.stop(); err != nil {
				t.Errorf("serve returned %v", err)
			}
		})
	}
}


// freeAddr returns a loopback address whose port was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// nodeSection is the node member of a config for a node on loopback,
// followed by a comma.
func nodeSection(t *testing.T, id string) string {
	addr := freeAddr(t)
	return fmt.Sprintf(`"node":{"id":%q,"listen":%q,"address":%q},`, id, addr, addr)
}

// joinNode admits the node of b to the cluster of a, whose server runs,
// the way an operator does: "node id" on b, "node add" on a, "node join"
// on b. Start b's server next.
func joinNode(t *testing.T, a, b *cli) {
	t.Helper()
	f := strings.Fields(b.ok("", "node", "id"))
	if len(f) != 3 {
		t.Fatalf("node id: %v", f)
	}
	out := a.ok("", "node", "add", f[0], f[1], f[2])
	m := regexp.MustCompile(`node join (\S+) (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("node add said %q", out)
	}
	b.expect(b.ok("", "node", "join", m[1], m[2]), `^ready to join 2 members`)
}
