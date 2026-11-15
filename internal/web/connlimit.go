package web

import (
	"net"
	"sync"

	"github.com/robinmail/robinmail/internal/ipkey"
)

// LimitListener bounds the connections open through l: total in all and
// perIP per client address (an IPv6 /64 counts as one, as for SMTP and
// IMAP). A connection past either bound is closed as soon as it is
// accepted, so a client with many connections cannot hold the memory and
// file descriptors of the process; the timeouts of the http.Server drop
// slow ones, this bounds many. Zero means no bound.
func LimitListener(l net.Listener, total, perIP int) net.Listener {
	return &limitListener{Listener: l, total: total, perIP: perIP, per: map[string]int{}}
}

type limitListener struct {
	net.Listener
	total, perIP int

	mu   sync.Mutex
	open int
	per  map[string]int
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		key := ""
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			key = ipkey.Of(a.IP.String())
		}
		l.mu.Lock()
		if (l.total > 0 && l.open >= l.total) || (l.perIP > 0 && l.per[key] >= l.perIP) {
			l.mu.Unlock()
			c.Close()
			continue
		}
		l.open++
		l.per[key]++
		l.mu.Unlock()
		return &limitConn{Conn: c, l: l, key: key}, nil
	}
}

// counts is the number of connections open in all and from key.
func (l *limitListener) counts(key string) (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open, l.per[key]
}

type limitConn struct {
	net.Conn
	l    *limitListener
	key  string
	once sync.Once
}

func (c *limitConn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		c.l.open--
		if c.l.per[c.key]--; c.l.per[c.key] <= 0 {
			delete(c.l.per, c.key)
		}
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}
