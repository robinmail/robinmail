package web

import (
	"bufio"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// otherAddrs gives each accepted connection a client address of its own.
type otherAddrs struct {
	net.Listener
	n atomic.Int32
}

type addrConn struct {
	net.Conn
	addr net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.addr }

func (l *otherAddrs) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	i := l.n.Add(1)
	return addrConn{c, &net.TCPAddr{IP: net.IPv4(10, 1, byte(i>>8), byte(i))}}, nil
}

// serveHold accepts through l, greets every connection it keeps and holds
// it until the client goes.
func serveHold(t *testing.T, l net.Listener) {
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				c.Write([]byte("hello\n"))
				c.Read(make([]byte, 1))
				c.Close()
			}()
		}
	}()
}

// open dials and reports whether the server kept the connection.
func open(t *testing.T, addr string) (net.Conn, bool) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	return c, err == nil && line == "hello\n"
}

// Review finding 14: the HTTPS listener had no bound on connections, so
// many clients could hold as many as they liked.
func TestLimitListenerPerIP(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := LimitListener(inner, 0, 3).(*limitListener)
	serveHold(t, l)
	var kept []net.Conn
	for i := range 3 {
		c, ok := open(t, inner.Addr().String())
		if !ok {
			t.Fatalf("connection %d refused", i)
		}
		kept = append(kept, c)
	}
	if _, ok := open(t, inner.Addr().String()); ok {
		t.Fatal("a fourth connection from one address was kept")
	}
	// One goes, and there is room again.
	kept[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, n := l.counts("127.0.0.1"); n < 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the closed connection was not given back")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := open(t, inner.Addr().String()); !ok {
		t.Fatal("no room after a connection closed")
	}
}

func TestLimitListenerTotal(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveHold(t, LimitListener(&otherAddrs{Listener: inner}, 5, 1))
	kept := 0
	for range 20 {
		if _, ok := open(t, inner.Addr().String()); ok {
			kept++
		}
	}
	if kept != 5 {
		t.Fatalf("%d connections kept from 20 addresses, want 5", kept)
	}
}
