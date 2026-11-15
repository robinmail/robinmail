package smtp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// manyAddrs gives every accepted connection a client address of its own,
// so that the limits per address do not hide the global bound.
type manyAddrs struct {
	net.Listener
	n atomic.Int32
}

type addrConn struct {
	net.Conn
	addr net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.addr }

func (l *manyAddrs) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	i := l.n.Add(1)
	return addrConn{c, &net.TCPAddr{IP: net.IPv4(10, 0, byte(i>>8), byte(i)), Port: 25}}, nil
}

// holdHandler keeps every message until release is closed, as a slow
// delivery (Rspamd, the database) does.
type holdHandler struct {
	held    atomic.Int32
	release chan struct{}
}

func (h *holdHandler) Rcpt(context.Context, *Envelope, string) error { return nil }

func (h *holdHandler) Deliver(ctx context.Context, _ *Envelope, msg []byte) error {
	h.held.Add(1)
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	runtime.KeepAlive(msg)
	return nil
}

// sendOne sends msg over a new connection and returns the reply to DATA.
func sendOne(addr string, msg []byte) (string, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Minute))
	r := bufio.NewReader(c)
	reply := func() (string, error) {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return "", err
			}
			if len(line) < 4 || line[3] != '-' {
				return strings.TrimSpace(line), nil
			}
		}
	}
	if _, err := reply(); err != nil {
		return "", err
	}
	for _, cmd := range []string{"EHLO c.test", "MAIL FROM:<a@c.test>", "RCPT TO:<b@mx.test>", "DATA"} {
		fmt.Fprintf(c, "%s\r\n", cmd)
		if _, err := reply(); err != nil {
			return "", err
		}
	}
	if _, err := c.Write(msg); err != nil {
		return "", err
	}
	return reply()
}

func heapAlloc() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestBudgetBoundsMemory sends many large messages at once from as many
// client addresses to a server whose delivery holds them. Without the
// budget every one of them is in memory at once (24 × 4 MiB here, several
// GiB with the defaults of 100 connections and 25 MiB, issue 43). With it
// the server holds at most Budget.Total, answers the others 452 4.3.1, and
// takes them when they come again.
func TestBudgetBoundsMemory(t *testing.T) {
	const (
		n       = 24
		maxSize = 4 << 20
	)
	t.Run("unbounded", func(t *testing.T) {
		// The behaviour before the budget, to show what it prevents.
		held, deferred, live, _ := flood(t, n, maxSize, &Budget{Total: 1 << 40, PerClient: 1 << 40})
		if held != n || deferred != 0 || live < n*maxSize*3/4 {
			t.Fatalf("held %d, deferred %d, %d MiB live: the flood did not take hold", held, deferred, live>>20)
		}
	})
	t.Run("bounded", func(t *testing.T) {
		total := int64(4 * maxSize) // the default for this message size
		held, _, live, peak := flood(t, n, maxSize, &Budget{Total: total, PerClient: maxSize})
		if held == 0 || held > int32(total/maxSize) {
			t.Fatalf("held %d messages, want 1 to %d", held, total/maxSize)
		}
		// What is live is what the budget allows plus a little; while
		// reading, the doubling buffers leave as much again for the
		// collector. n messages would be n times the size.
		if live > total+4<<20 {
			t.Fatalf("heap grew by %d MiB holding the messages, budget %d MiB", live>>20, total>>20)
		}
		if peak > 4*total {
			t.Fatalf("heap peaked %d MiB above base, budget %d MiB", peak>>20, total>>20)
		}
	})
}

// flood sends n messages of nearly maxSize at once, each from its own
// client address, to a server with budget whose delivery holds them. It
// returns how many were held and deferred and by how much the heap grew,
// with the messages held and at the peak. Then it lets them go, sends the
// deferred ones again, and checks that all are taken.
func flood(t *testing.T, n int, maxSize int64, budget *Budget) (held int32, deferred int, live, peakHeap int64) {
	h := &holdHandler{release: make(chan struct{})}
	s := &Server{Hostname: "mx.test", Handler: h, MaxMessageBytes: maxSize, MaxConnsPerIP: 1, Budget: budget}
	total := budget.Total
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(&manyAddrs{Listener: l})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	addr := l.Addr().String()

	// A message just under the limit, made once and sent by every client.
	var b bytes.Buffer
	b.WriteString("Subject: big\r\n\r\n")
	line := strings.Repeat("x", 998) + "\r\n"
	for int64(b.Len()+len(line)) < maxSize-16 {
		b.WriteString(line)
	}
	b.WriteString(".\r\n")
	msg := b.Bytes()

	runtime.GC()
	base := heapAlloc()

	// Sample the heap and the budget while the messages come in.
	var peak atomic.Uint64
	var over atomic.Bool
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			if hp := heapAlloc(); hp > peak.Load() {
				peak.Store(hp)
			}
			if budget.Used() > total {
				over.Store(true)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	type result struct {
		reply string
		err   error
	}
	results := make(chan result, n)
	for range n {
		go func() {
			r, err := sendOne(addr, msg)
			results <- result{r, err}
		}()
	}
	// Every client that is not held gets its answer; the held ones wait.
	var deferredR, other []string
	deadline := time.After(time.Minute)
	for int(h.held.Load())+len(deferredR)+len(other) < n {
		select {
		case r := <-results:
			switch {
			case r.err != nil:
				t.Fatal(r.err)
			case strings.HasPrefix(r.reply, "452 4.3.1"):
				deferredR = append(deferredR, r.reply)
			default:
				other = append(other, r.reply)
			}
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("held %d, deferred %d, other %v", h.held.Load(), len(deferredR), other)
		}
	}
	runtime.GC()
	heldHeap := heapAlloc()
	close(stop)
	<-sampled
	held, deferred = h.held.Load(), len(deferredR)
	live, peakHeap = int64(heldHeap)-int64(base), int64(peak.Load())-int64(base)
	t.Logf("held %d, deferred %d; heap above base: %d MiB held, %d MiB at the peak; budget %d MiB",
		held, deferred, live>>20, peakHeap>>20, total>>20)
	if len(other) > 0 {
		t.Fatalf("unexpected replies: %v", other)
	}
	if over.Load() {
		t.Fatal("the budget was exceeded")
	}

	close(h.release)
	for range held {
		if r := <-results; r.err != nil || !strings.HasPrefix(r.reply, "250") {
			t.Fatalf("held message: %q %v", r.reply, r.err)
		}
	}
	// The deferred ones are taken when they come again, one after another.
	for range deferred {
		if r, err := sendOne(addr, msg); err != nil || !strings.HasPrefix(r, "250") {
			t.Fatalf("retry: %q %v", r, err)
		}
	}
	waitFor(t, func() bool { return budget.Used() == 0 })
	return
}

// TestBudgetSharePerClient: one client address cannot hold more than its
// share, so it cannot defer the mail of others.
func TestBudgetSharePerClient(t *testing.T) {
	b := &Budget{Total: 100, PerClient: 30}
	if !b.take("a", 30) || b.take("a", 1) {
		t.Fatal("client a got past its share")
	}
	if !b.take("b", 30) || !b.take("c", 30) || b.take("d", 30) || !b.take("d", 10) {
		t.Fatal("total not kept")
	}
	b.give("a", 30)
	if !b.take("d", 20) {
		t.Fatal("what a gave back was not available")
	}
	for _, k := range []string{"b", "c", "d"} {
		b.give(k, 30)
	}
	if b.Used() != 0 || len(b.per) != 0 {
		t.Fatalf("used %d, per %v", b.Used(), b.per)
	}
}

// TestBudgetDeferThenAccept goes through a session: a message that does
// not fit is answered 452 4.3.1, the session stays usable, and the same
// message is taken once there is room.
func TestBudgetDeferThenAccept(t *testing.T) {
	h := &fakeHandler{}
	budget := &Budget{Total: 1 << 20, PerClient: 1 << 20}
	s := &Server{Hostname: "mx.test", Handler: h, MaxMessageBytes: 1 << 20, Budget: budget}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(l)
	t.Cleanup(func() { s.Shutdown(context.Background()) })

	budget.take("someone else", 1<<20-1000)
	c := dial(t, l.Addr().String())
	c.cmd("EHLO c.test", "250")
	body := strings.Repeat("y", 200) + "\r\n"
	data := "Subject: s\r\n\r\n" + strings.Repeat(body, 100) + ".\r\n"
	for _, want := range []string{"452 4.3.1", "250"} {
		c.cmd("MAIL FROM:<a@c.test>", "250")
		c.cmd("RCPT TO:<b@mx.test>", "250")
		c.cmd("DATA", "354")
		c.conn.Write([]byte(data))
		c.expect(want)
		if want == "452 4.3.1" {
			budget.give("someone else", 1<<20-1000)
		}
	}
	if len(h.msgs) != 1 {
		t.Fatalf("%d messages delivered", len(h.msgs))
	}
	if budget.Used() != 0 {
		t.Fatalf("%d bytes still held", budget.Used())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
