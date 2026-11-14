package smtp

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// tap keeps what was read from a connection.
type tap struct {
	net.Conn
	read []byte
}

func (c *tap) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read = append(c.read, p[:n]...)
	return n, err
}

// RFC 8446 6.1 (RFC 5246 7.2.1 for TLS 1.2): each side sends close_notify
// before it closes. The server did not after QUIT: it closed the socket
// under the TLS layer, and Exim logged every delivery to it with "TLS
// error on connection (recv): The TLS connection was non-properly
// terminated".
//
// Go's own TLS client forgives a missing close_notify, so the test reads
// the records off the wire: with TLS 1.2 the type of a record is in the
// clear, and the last one the server sends must be an alert (21).
func TestQuitClosesTLS(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		srv := &Server{Handler: &recorder{}, TLSConfig: testTLSConfig(t), Hostname: "foo.com"}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		if implicit {
			// Port 465: TLS from the first byte.
			go srv.Serve(tls.NewListener(l, srv.TLSConfig))
		} else {
			go srv.Serve(l)
		}
		raw, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { raw.Close() })
		raw.SetDeadline(time.Now().Add(5 * time.Second))
		c := &client{t: t, conn: raw, r: bufio.NewReader(raw)}
		if !implicit {
			c.expect("220")
			c.cmd("EHLO mx.exim.test", "250")
			c.cmd("STARTTLS", "220")
		}
		wire := &tap{Conn: raw}
		tc := tls.Client(wire, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			t.Fatal(err)
		}
		c.conn, c.r = tc, bufio.NewReader(tc)
		if implicit {
			c.expect("220")
		}
		c.cmd("EHLO mx.exim.test", "250")
		c.cmd("QUIT", "221")
		if _, err := c.r.ReadByte(); err != io.EOF {
			t.Fatalf("implicit TLS %v: after QUIT: %v, want io.EOF", implicit, err)
		}
		last := byte(0)
		for rec := wire.read; len(rec) >= 5; {
			n := int(rec[3])<<8 | int(rec[4])
			if len(rec) < 5+n {
				t.Fatalf("a record cut short: %d of %d bytes", len(rec)-5, n)
			}
			last, rec = rec[0], rec[5+n:]
		}
		if last != 21 {
			t.Errorf("implicit TLS %v: the last TLS record from the server has type %d, want 21 (alert: close_notify)", implicit, last)
		}
	}
}
