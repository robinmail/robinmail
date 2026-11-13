package smtp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeHandler struct {
	mu   sync.Mutex
	msgs [][]byte
	envs []Envelope
}

func (h *fakeHandler) Rcpt(_ context.Context, _ *Envelope, to string) error {
	if strings.HasPrefix(to, "nobody@") {
		return &Error{550, "5.1.1", "No such user"}
	}
	return nil
}

func (h *fakeHandler) Deliver(_ context.Context, env *Envelope, msg []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, append([]byte(nil), msg...))
	h.envs = append(h.envs, *env)
	return nil
}

func start(t *testing.T, tc *tls.Config) (*fakeHandler, string) {
	t.Helper()
	h := &fakeHandler{}
	s := &Server{Hostname: "mx.test", Handler: h, TLSConfig: tc, MaxMessageBytes: 1000, MaxErrors: 20}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return h, l.Addr().String()
}

type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	cl := &client{t: t, conn: c, r: bufio.NewReader(c)}
	cl.expect("220")
	return cl
}

func (c *client) send(s string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(s)); err != nil {
		c.t.Fatal(err)
	}
}

// expect reads one (possibly multi-line) reply and checks its code.
func (c *client) expect(code string) string {
	c.t.Helper()
	var all strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("want %s, read error %v after %q", code, err, all.String())
		}
		all.WriteString(line)
		if len(line) < 4 || line[3] != '-' {
			if !strings.HasPrefix(line, code) {
				c.t.Fatalf("want %s, got %q", code, all.String())
			}
			return all.String()
		}
	}
}

func (c *client) cmd(line, code string) string {
	c.t.Helper()
	c.send(line + "\r\n")
	return c.expect(code)
}

func TestTransaction(t *testing.T) {
	h, addr := start(t, nil)
	c := dial(t, addr)
	ehlo := c.cmd("EHLO client.test", "250")
	for _, ext := range []string{"PIPELINING", "SIZE 1000", "8BITMIME", "SMTPUTF8", "ENHANCEDSTATUSCODES"} {
		if !strings.Contains(ehlo, ext) {
			t.Errorf("EHLO lacks %s", ext)
		}
	}
	if strings.Contains(ehlo, "STARTTLS") {
		t.Error("STARTTLS advertised without a TLS config")
	}
	c.cmd("MAIL FROM:<a@b.test> BODY=8BITMIME SIZE=100", "250")
	c.cmd("RCPT TO:<nobody@x.test>", "550 5.1.1")
	c.cmd("RCPT TO:<User@X.test>", "250")
	c.cmd("DATA", "354")
	c.cmd("Subject: hi\r\n\r\n..leading dot\r\nbody\r\n.", "250")
	c.cmd("QUIT", "221")

	if len(h.msgs) != 1 {
		t.Fatalf("got %d messages", len(h.msgs))
	}
	if want := "Subject: hi\r\n\r\n.leading dot\r\nbody\r\n"; string(h.msgs[0]) != want {
		t.Errorf("message %q, want %q", h.msgs[0], want)
	}
	env := h.envs[0]
	if env.From != "a@b.test" || len(env.Rcpts) != 1 || env.Rcpts[0] != "User@X.test" || env.Helo != "client.test" {
		t.Errorf("envelope %+v", env)
	}
}

func TestPipelining(t *testing.T) {
	h, addr := start(t, nil)
	c := dial(t, addr)
	c.send("EHLO c\r\nMAIL FROM:<>\r\nRCPT TO:<a@x.test>\r\nRCPT TO:<b@x.test>\r\nDATA\r\n")
	for _, code := range []string{"250", "250", "250", "250", "354"} {
		c.expect(code)
	}
	c.send("x\r\n.\r\nQUIT\r\n")
	c.expect("250")
	c.expect("221")
	if len(h.envs) != 1 || h.envs[0].From != "" || len(h.envs[0].Rcpts) != 2 {
		t.Errorf("envelopes %+v", h.envs)
	}
}

func TestSequence(t *testing.T) {
	_, addr := start(t, nil)
	c := dial(t, addr)
	c.cmd("MAIL FROM:<a@b.test>", "503")
	c.cmd("EHLO c", "250")
	c.cmd("RCPT TO:<a@b.test>", "503")
	c.cmd("DATA", "503")
	c.cmd("MAIL FROM:<a@b.test>", "250")
	c.cmd("MAIL FROM:<a@b.test>", "503")
	c.cmd("DATA", "554")
	c.cmd("RSET", "250")
	c.cmd("MAIL FROM:<a@b.test> FOO=1", "555")
	c.cmd("MAIL FROM:<a@b.test> SIZE=5000", "552")
}

func TestTooManyErrors(t *testing.T) {
	_, addr := start(t, nil)
	c := dial(t, addr)
	for i := 0; i < 19; i++ {
		c.cmd("BOGUS", "500")
	}
	c.cmd("BOGUS", "421")
}

func TestDataRejected(t *testing.T) {
	for _, tc := range []struct{ name, data, code string }{
		{"bare LF", "a\nb\r\n.\r\n", "550 5.6.0"},
		{"bare CR", "a\rb\r\n.\r\n", "550 5.6.0"},
		{"too big", strings.Repeat("x", 1200) + "\r\n.\r\n", "552"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, addr := start(t, nil)
			c := dial(t, addr)
			c.cmd("EHLO c", "250")
			c.cmd("MAIL FROM:<a@b.test>", "250")
			c.cmd("RCPT TO:<u@x.test>", "250")
			c.cmd("DATA", "354")
			c.send(tc.data)
			c.expect(tc.code)
			// The session is still in sync.
			c.cmd("NOOP", "250")
			if len(h.msgs) != 0 {
				t.Error("message was delivered")
			}
		})
	}
}

// TestSmuggling sends a second message hidden behind terminators that other
// servers accept. It must neither end the first message nor become its own.
func TestSmuggling(t *testing.T) {
	for _, end := range []string{"\n.\r\n", "\r\n.\n", "\n.\n", "\r.\r\n", "\r\n.\r"} {
		h, addr := start(t, nil)
		c := dial(t, addr)
		c.cmd("EHLO c", "250")
		c.cmd("MAIL FROM:<a@b.test>", "250")
		c.cmd("RCPT TO:<u@x.test>", "250")
		c.cmd("DATA", "354")
		c.send("Subject: one\r\n\r\nbody" + end +
			"MAIL FROM:<admin@x.test>\r\nRCPT TO:<victim@x.test>\r\nDATA\r\nSubject: smuggled\r\n\r\n.\r\n")
		c.expect("550 5.6.0")
		c.cmd("NOOP", "250")
		if len(h.msgs) != 0 {
			t.Errorf("%q: delivered %q", end, h.msgs)
		}
	}
}

func TestStartTLS(t *testing.T) {
	h, addr := start(t, testTLSConfig(t))
	c := dial(t, addr)
	if !strings.Contains(c.cmd("EHLO c", "250"), "STARTTLS") {
		t.Fatal("STARTTLS not advertised")
	}
	c.cmd("STARTTLS", "220")
	tc := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	c.conn, c.r = tc, bufio.NewReader(tc)
	c.cmd("MAIL FROM:<a@b.test>", "503") // EHLO again after TLS
	if strings.Contains(c.cmd("EHLO c", "250"), "STARTTLS") {
		t.Error("STARTTLS advertised inside TLS")
	}
	c.cmd("MAIL FROM:<a@b.test>", "250")
	c.cmd("RCPT TO:<u@x.test>", "250")
	c.cmd("DATA", "354")
	c.cmd("x\r\n.", "250")
	if len(h.envs) != 1 || h.envs[0].TLS == nil {
		t.Error("delivery did not record TLS")
	}
}

func TestStartTLSInjection(t *testing.T) {
	_, addr := start(t, testTLSConfig(t))
	c := dial(t, addr)
	c.cmd("EHLO c", "250")
	c.send("STARTTLS\r\nMAIL FROM:<evil@x.test>\r\n")
	c.expect("554")
}

func TestConnectionLimitPerIP(t *testing.T) {
	h := &fakeHandler{}
	s := &Server{Hostname: "mx.test", Handler: h, MaxConnsPerIP: 1}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.Shutdown(ctx)
	}()
	dial(t, l.Addr().String())
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.HasPrefix(line, "421") {
		t.Errorf("second connection got %q", line)
	}
}

func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mx.test"},
		DNSNames:     []string{"mx.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func TestParsePath(t *testing.T) {
	for _, tc := range []struct {
		in, addr string
		null, ok bool
		nparams  int
	}{
		{"<a@b.test>", "a@b.test", false, true, 0},
		{"<a@b.test> SIZE=1 BODY=8BITMIME", "a@b.test", false, true, 2},
		{"<>", "", true, true, 0},
		{"<>", "", false, false, 0},
		{"<@relay.test:a@b.test>", "a@b.test", false, true, 0},
		{"<\"a b\"@b.test>", "\"a b\"@b.test", false, true, 0},
		{"<Postmaster>", "Postmaster", false, true, 0},
		{"<Postmaster>", "", true, false, 0},
		{"a@b.test", "", false, false, 0},
		{"<a@b.test", "", false, false, 0},
		{"<a b@c.test>", "", false, false, 0},
		{"<a@b.test>x", "", false, false, 0},
		{"<@b.test>", "", false, false, 0},
		{"<a@>", "", false, false, 0},
		{"<a\x00@b.test>", "", false, false, 0},
	} {
		addr, params, err := parsePath(tc.in, !tc.null)
		if (err == nil) != tc.ok || (tc.ok && (addr != tc.addr || len(params) != tc.nparams)) {
			t.Errorf("parsePath(%q, forward=%v) = %q, %v, %v", tc.in, !tc.null, addr, params, err)
		}
	}
}

func FuzzParsePath(f *testing.F) {
	f.Add("<a@b.test> SIZE=1")
	f.Add("<@x:\"q\\\"\"@y>")
	f.Add("<@0:") // a source route and then nothing
	f.Fuzz(func(t *testing.T, s string) {
		for _, forward := range []bool{false, true} {
			addr, params, err := parsePath(s, forward)
			if err != nil {
				continue
			}
			if strings.ContainsAny(addr, "\r\n\x00") || !utf8.ValidString(addr) {
				t.Errorf("control character or broken UTF-8 in %q", addr)
			}
			if len(s) > maxPath+10*501 {
				t.Errorf("accepted %d bytes", len(s))
			}
			for _, p := range params {
				if p.key == "" || strings.ContainsAny(p.key+p.value, " =\r\n\x00") {
					t.Errorf("parameter %+v from %q", p, s)
				}
			}
		}
	})
}

func FuzzReadData(f *testing.F) {
	f.Add([]byte("a\r\n..b\r\n.\r\n"))
	f.Add([]byte("a\n.\r\nb\r\n.\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		msg, err := readData(bufio.NewReaderSize(bytes.NewReader(in), 64), 1<<20)
		if err != nil {
			return
		}
		// Accepted output contains only CRLF line endings.
		for i := range msg {
			if msg[i] == '\n' && (i == 0 || msg[i-1] != '\r') || msg[i] == '\r' && (i+1 >= len(msg) || msg[i+1] != '\n') {
				t.Fatalf("bare line ending in %q from %q", msg, in)
			}
		}
	})
}
