// Package imap is an IMAP server: IMAP4rev1 (RFC 3501) and, for a client
// that enables it, IMAP4rev2 (RFC 9051). It is protocol only: the mail is
// behind the Backend and User interfaces, and who may log in is the
// backend's to say.
//
// Extensions: IDLE (RFC 2177), NAMESPACE (2342), ID (2971), CHILDREN
// (3348), UNSELECT (3691), UIDPLUS (4315), ENABLE (5161), ESEARCH (4731),
// SEARCHRES (5182), LIST-EXTENDED (5258), LIST-STATUS (5819), SPECIAL-USE
// (6154), MOVE (6851), CONDSTORE (7162), LITERAL+ (7888), SASL-IR (4959),
// AUTH=PLAIN (4616), STARTTLS, the FETCH side of BINARY (3516), STATUS=SIZE
// (8438) and UTF8=ACCEPT (6855).
//
// How a mailbox is seen. A session that selected a mailbox holds its own
// list of the messages, in UID order: the message sequence numbers are
// positions in that list. The list only changes when the session tells the
// client: an untagged EXISTS when messages came, EXPUNGE when one went,
// FETCH when flags changed. That happens at the end of a command (not of
// FETCH, STORE or SEARCH for EXPUNGE, RFC 3501 7.4.1) and during IDLE, by
// reading the mailbox again and comparing; the backend's change channel
// says when that is worth doing.
//
// A password only ever crosses TLS: on a listener without TLS the server
// announces LOGINDISABLED and refuses LOGIN and AUTHENTICATE until
// STARTTLS.
package imap

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/mikepage/robinmail/internal/metrics"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Server serves IMAP. Fill the exported fields, then call Serve for every
// listener; a listener made with tls.NewListener has TLS from the first
// byte (port 993), any other gets STARTTLS (port 143).
type Server struct {
	Hostname string
	Backend  Backend
	// TLSConfig is for STARTTLS. Without it a plain listener cannot log
	// anybody in.
	TLSConfig *tls.Config
	Logger    *slog.Logger

	// MaxMessageBytes is the largest message APPEND takes [25 MiB].
	MaxMessageBytes int64
	// MaxConnections is how many connections there may be [1000],
	// MaxConnsPerIP from one address [20], MaxConnsPerUser logged in as
	// one user [20]. Mail clients open several: Thunderbird five.
	MaxConnections  int
	MaxConnsPerIP   int
	MaxConnsPerUser int
	// Trace logs every command (without its arguments for LOGIN and
	// AUTHENTICATE, and without literals) and every tagged reply.
	Trace bool

	// Timeouts. The zero values are the defaults: a client that has not
	// logged in gets a minute per command, one that has gets the 30
	// minutes of RFC 3501 5.4, also while it idles.
	LoginTimeout time.Duration
	IdleTimeout  time.Duration
	WriteTimeout time.Duration

	once      sync.Once
	mu        sync.Mutex
	listeners map[net.Listener]struct{}
	conns     map[*conn]struct{}
	perIP     map[string]int
	perUser   map[string]int
	closing   bool
	done      chan struct{} // closed when Shutdown starts
	wg        sync.WaitGroup
}

func (s *Server) init() {
	s.once.Do(func() {
		if s.Logger == nil {
			s.Logger = slog.New(slog.DiscardHandler)
		}
		if s.MaxMessageBytes <= 0 {
			s.MaxMessageBytes = 25 << 20
		}
		if s.MaxConnections <= 0 {
			s.MaxConnections = 1000
		}
		if s.MaxConnsPerIP <= 0 {
			s.MaxConnsPerIP = 20
		}
		if s.MaxConnsPerUser <= 0 {
			s.MaxConnsPerUser = 20
		}
		if s.LoginTimeout <= 0 {
			s.LoginTimeout = time.Minute
		}
		if s.IdleTimeout <= 0 {
			s.IdleTimeout = 30 * time.Minute
		}
		if s.WriteTimeout <= 0 {
			s.WriteTimeout = time.Minute
		}
		s.listeners = map[net.Listener]struct{}{}
		s.conns = map[*conn]struct{}{}
		s.perIP = map[string]int{}
		s.perUser = map[string]int{}
		s.done = make(chan struct{})
	})
}

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("imap: server closed")

// Serve accepts connections until the listener fails or Shutdown is
// called.
func (s *Server) Serve(l net.Listener) error {
	s.init()
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		l.Close()
		return ErrServerClosed
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, l)
		s.mu.Unlock()
	}()

	var delay time.Duration
	for {
		nc, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		metrics.IMAPConnections.Inc()
		ip := remoteIP(nc)
		s.mu.Lock()
		var refuse string
		switch {
		case s.closing:
			refuse = "the server is shutting down"
		case len(s.conns) >= s.MaxConnections:
			refuse = "too many connections"
		case s.perIP[ip] >= s.MaxConnsPerIP:
			refuse = "too many connections from your address"
		}
		var c *conn
		if refuse == "" {
			c = newConn(s, nc, ip)
			s.conns[c] = struct{}{}
			s.perIP[ip]++
			s.wg.Add(1)
		}
		s.mu.Unlock()
		if refuse != "" {
			go func() {
				// Under implicit TLS this handshakes first, so the client
				// can read why.
				nc.SetDeadline(time.Now().Add(10 * time.Second))
				nc.Write([]byte("* BYE " + refuse + "\r\n"))
				nc.Close()
			}()
			continue
		}
		go func() {
			defer s.wg.Done()
			c.serve()
			s.mu.Lock()
			delete(s.conns, c)
			if s.perIP[ip]--; s.perIP[ip] <= 0 {
				delete(s.perIP, ip)
			}
			if c.countedUser != "" {
				if s.perUser[c.countedUser]--; s.perUser[c.countedUser] <= 0 {
					delete(s.perUser, c.countedUser)
				}
			}
			s.mu.Unlock()
		}()
	}
}

// Shutdown stops accepting, tells every client BYE once the command it is
// in has been answered, and waits for the connections to end. When ctx is
// done first the rest are closed and its error returned.
func (s *Server) Shutdown(ctx context.Context) error {
	s.init()
	s.mu.Lock()
	if !s.closing {
		s.closing = true
		close(s.done)
	}
	for l := range s.listeners {
		l.Close()
	}
	// A session waiting for a command is woken; one in a command sees
	// the flag when it is done.
	for c := range s.conns {
		c.wake()
	}
	s.mu.Unlock()

	finished := make(chan struct{})
	go func() { s.wg.Wait(); close(finished) }()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.nc.Close()
		}
		s.mu.Unlock()
		<-finished
		return ctx.Err()
	}
}

func (s *Server) isClosing() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// userLoggedIn counts a login against the per-user limit.
func (s *Server) userLoggedIn(c *conn, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perUser[name] >= s.MaxConnsPerUser {
		return false
	}
	s.perUser[name]++
	c.countedUser = name
	return true
}

// Connections is how many connections are open, for tests and logs.
func (s *Server) Connections() int {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func remoteIP(c net.Conn) string {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP.String()
	}
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return c.RemoteAddr().String()
	}
	return host
}
