// Package smtp is an ESMTP server for inbound mail. It speaks the protocol
// and knows nothing about storage: recipients and messages go to a Handler.
package smtp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// Envelope is what the client told the server before DATA.
type Envelope struct {
	RemoteIP net.IP
	Helo     string
	TLS      *tls.ConnectionState // nil without STARTTLS
	From     string               // empty for the null reverse path <>
	Rcpts    []string
	UTF8     bool // SMTPUTF8 was requested
}

// Handler decides about recipients and takes delivery of messages.
// Returning an *Error controls the reply; any other error becomes a 451.
type Handler interface {
	Rcpt(ctx context.Context, env *Envelope, to string) error
	Deliver(ctx context.Context, env *Envelope, msg []byte) error
}

// Error is an SMTP reply with an enhanced status code.
type Error struct {
	Code     int    // 451, 550, ...
	Enhanced string // "5.1.1"
	Msg      string
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s %s", e.Code, e.Enhanced, e.Msg) }

// Server is an SMTP listener. Zero limits get the defaults in brackets.
type Server struct {
	Hostname        string
	TLSConfig       *tls.Config // nil disables STARTTLS
	Handler         Handler
	Logger          *slog.Logger
	MaxMessageBytes int64         // [25 MiB]
	MaxRecipients   int           // [100]
	MaxConnections  int           // [100]
	MaxConnsPerIP   int           // [10]
	CommandTimeout  time.Duration // [5m] per command, RFC 5321 4.5.3.2
	DataTimeout     time.Duration // [10m] for the whole message
	HandlerTimeout  time.Duration // [2m] for one Rcpt or Deliver call
	MaxErrors       int           // [10] bad commands before the connection is closed

	mu       sync.Mutex
	perIP    map[string]int
	conns    map[net.Conn]struct{}
	listener []net.Listener
	closed   bool
	wg       sync.WaitGroup
	sem      chan struct{}
}

const (
	maxLine = 4096  // command lines; RFC 5321 asks for 512
	maxData = 65536 // one line inside DATA
)

func (s *Server) defaults() {
	if s.MaxMessageBytes == 0 {
		s.MaxMessageBytes = 25 << 20
	}
	if s.MaxRecipients == 0 {
		s.MaxRecipients = 100
	}
	if s.MaxConnections == 0 {
		s.MaxConnections = 100
	}
	if s.MaxConnsPerIP == 0 {
		s.MaxConnsPerIP = 10
	}
	if s.CommandTimeout == 0 {
		s.CommandTimeout = 5 * time.Minute
	}
	if s.DataTimeout == 0 {
		s.DataTimeout = 10 * time.Minute
	}
	if s.HandlerTimeout == 0 {
		s.HandlerTimeout = 2 * time.Minute
	}
	if s.MaxErrors == 0 {
		s.MaxErrors = 10
	}
	if s.Logger == nil {
		s.Logger = slog.New(slog.DiscardHandler)
	}
}

// ErrServerClosed is returned by Serve after Shutdown.
var ErrServerClosed = errors.New("smtp: server closed")

// Serve accepts connections on l until Shutdown.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrServerClosed
	}
	if s.sem == nil {
		s.defaults()
		s.sem = make(chan struct{}, s.MaxConnections)
		s.perIP = map[string]int{}
		s.conns = map[net.Conn]struct{}{}
	}
	s.listener = append(s.listener, l)
	s.mu.Unlock()

	for {
		c, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
}

// Shutdown stops accepting connections, closes the open ones once ctx is
// done, and waits for them to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	for _, l := range s.listener {
		l.Close()
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func remoteIP(c net.Conn) net.IP {
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	return nil
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	ip := remoteIP(c)
	key := ip.String()

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(c, "421 4.7.0 Too many connections, try again later\r\n")
		return
	}
	s.mu.Lock()
	if s.perIP[key] >= s.MaxConnsPerIP {
		s.mu.Unlock()
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(c, "421 4.7.0 Too many connections from your address\r\n")
		return
	}
	s.perIP[key]++
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.perIP[key]--; s.perIP[key] <= 0 {
			delete(s.perIP, key)
		}
		delete(s.conns, c)
		s.mu.Unlock()
	}()

	sess := &session{srv: s, conn: c, ip: ip}
	sess.setConn(c)
	sess.log = s.Logger.With("remote", key)
	sess.serve()
}

// session is one SMTP connection.
type session struct {
	srv  *Server
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	ip   net.IP
	log  *slog.Logger

	helo   string
	ehlo   bool
	tls    *tls.ConnectionState
	env    *Envelope // non-nil between MAIL and the end of the transaction
	errors int
	quit   bool
}

func (s *session) setConn(c net.Conn) {
	s.conn = c
	s.r = bufio.NewReaderSize(c, maxData)
	s.w = bufio.NewWriter(c)
}

// reply writes a response. Writes are only flushed when the client has no
// further pipelined commands waiting, as PIPELINING (RFC 2920) allows.
func (s *session) reply(code int, enhanced string, lines ...string) {
	for i, l := range lines {
		sep := " "
		if i < len(lines)-1 {
			sep = "-"
		}
		if enhanced != "" {
			fmt.Fprintf(s.w, "%d%s%s %s\r\n", code, sep, enhanced, l)
		} else {
			fmt.Fprintf(s.w, "%d%s%s\r\n", code, sep, l)
		}
	}
	if s.r.Buffered() == 0 {
		s.flush()
	}
}

func (s *session) flush() {
	s.conn.SetWriteDeadline(time.Now().Add(s.srv.CommandTimeout))
	if err := s.w.Flush(); err != nil {
		s.quit = true
	}
}

func (s *session) replyErr(err error) {
	var e *Error
	if errors.As(err, &e) {
		s.reply(e.Code, e.Enhanced, e.Msg)
		return
	}
	s.log.Error("handler failed", "err", err)
	s.reply(451, "4.3.0", "Temporary failure, try again later")
}

func (s *session) serve() {
	defer s.flush()
	s.reply(220, "", s.srv.Hostname+" ESMTP robinmail")
	for !s.quit {
		s.conn.SetReadDeadline(time.Now().Add(s.srv.CommandTimeout))
		line, err := s.readLine()
		if err == errLineTooLong {
			s.bad(500, "5.5.2", "Line too long")
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("read failed", "err", err)
			}
			return
		}
		s.command(line)
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads one command line. A line must end in CRLF; a bare LF is
// accepted for commands (not for DATA) because it cannot smuggle anything.
func (s *session) readLine() (string, error) {
	line, err := s.r.ReadSlice('\n')
	if err == bufio.ErrBufferFull || (err == nil && len(line) > maxLine) {
		// Skip the rest of the line.
		for err == bufio.ErrBufferFull {
			_, err = s.r.ReadSlice('\n')
		}
		if err != nil {
			return "", err
		}
		return "", errLineTooLong
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// bad replies with an error and counts it; too many errors end the session.
func (s *session) bad(code int, enhanced, msg string) {
	s.errors++
	if s.errors >= s.srv.MaxErrors {
		s.reply(421, "4.7.0", "Too many errors, closing connection")
		s.quit = true
		return
	}
	s.reply(code, enhanced, msg)
}

func (s *session) command(line string) {
	verb, arg, _ := strings.Cut(line, " ")
	verb = strings.ToUpper(verb)
	arg = strings.TrimSpace(arg)

	switch verb {
	case "EHLO", "HELO":
		s.cmdHelo(verb, arg)
	case "STARTTLS":
		s.cmdStartTLS(arg)
	case "MAIL":
		s.cmdMail(arg)
	case "RCPT":
		s.cmdRcpt(arg)
	case "DATA":
		s.cmdData(arg)
	case "RSET":
		s.env = nil
		s.reply(250, "2.0.0", "OK")
	case "NOOP":
		s.reply(250, "2.0.0", "OK")
	case "QUIT":
		s.reply(221, "2.0.0", "Bye")
		s.quit = true
	case "VRFY":
		s.reply(252, "2.5.0", "Cannot VRFY user, but will accept message and attempt delivery")
	case "HELP":
		s.reply(214, "2.0.0", "See RFC 5321")
	case "AUTH":
		s.bad(503, "5.5.1", "Authentication is not available on this port")
	default:
		// Clients sometimes send HTTP to port 25 to probe it.
		if verb == "GET" || verb == "POST" || verb == "CONNECT" {
			s.reply(421, "4.7.0", "This is an SMTP server")
			s.quit = true
			return
		}
		s.bad(500, "5.5.2", "Command not recognized")
	}
}

func (s *session) cmdHelo(verb, arg string) {
	if arg == "" || len(arg) > 255 || !printable(arg) {
		s.bad(501, "5.5.4", "Syntax: "+verb+" hostname")
		return
	}
	s.helo, s.ehlo, s.env = arg, verb == "EHLO", nil
	if !s.ehlo {
		s.reply(250, "", s.srv.Hostname)
		return
	}
	lines := []string{
		s.srv.Hostname,
		"PIPELINING",
		fmt.Sprintf("SIZE %d", s.srv.MaxMessageBytes),
		"8BITMIME",
		"SMTPUTF8",
		"ENHANCEDSTATUSCODES",
	}
	if s.srv.TLSConfig != nil && s.tls == nil {
		lines = append(lines, "STARTTLS")
	}
	s.reply(250, "", lines...)
}

func (s *session) cmdStartTLS(arg string) {
	switch {
	case s.srv.TLSConfig == nil:
		s.bad(502, "5.5.1", "STARTTLS not available")
		return
	case s.tls != nil:
		s.bad(503, "5.5.1", "Already in TLS")
		return
	case arg != "":
		s.bad(501, "5.5.4", "Syntax: STARTTLS")
		return
	}
	// Commands pipelined after STARTTLS were sent in plaintext and must not
	// be executed inside the TLS session (CVE-2011-0411).
	if s.r.Buffered() > 0 {
		s.reply(554, "5.5.0", "Pipelining after STARTTLS is not allowed")
		s.quit = true
		return
	}
	s.reply(220, "2.0.0", "Ready to start TLS")
	s.flush()
	tc := tls.Server(s.conn, s.srv.TLSConfig)
	tc.SetDeadline(time.Now().Add(time.Minute))
	if err := tc.Handshake(); err != nil {
		s.log.Debug("tls handshake failed", "err", err)
		s.quit = true
		return
	}
	st := tc.ConnectionState()
	s.swapConn(tc)
	s.tls = &st
	// RFC 3207: forget everything learned before TLS.
	s.helo, s.ehlo, s.env = "", false, nil
}

// swapConn swaps the connection for its TLS wrapper and keeps Shutdown able to close it.
func (s *session) swapConn(tc *tls.Conn) {
	srv := s.srv
	srv.mu.Lock()
	delete(srv.conns, s.conn)
	srv.conns[tc] = struct{}{}
	srv.mu.Unlock()
	s.setConn(tc)
}

func (s *session) cmdMail(arg string) {
	if s.helo == "" {
		s.bad(503, "5.5.1", "Send EHLO first")
		return
	}
	if s.env != nil {
		s.bad(503, "5.5.1", "Nested MAIL command")
		return
	}
	rest, ok := cutPrefixFold(arg, "FROM:")
	if !ok {
		s.bad(501, "5.5.4", "Syntax: MAIL FROM:<address>")
		return
	}
	from, params, err := parsePath(rest, true)
	if err != nil {
		s.bad(501, "5.1.7", "Bad sender address")
		return
	}
	env := &Envelope{RemoteIP: s.ip, Helo: s.helo, TLS: s.tls, From: from}
	for _, p := range params {
		k, v, _ := strings.Cut(p, "=")
		switch strings.ToUpper(k) {
		case "SIZE":
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
				s.bad(501, "5.5.4", "Bad SIZE parameter")
				return
			}
			if n > s.srv.MaxMessageBytes {
				s.reply(552, "5.3.4", "Message too big")
				return
			}
		case "BODY":
			if u := strings.ToUpper(v); u != "7BIT" && u != "8BITMIME" {
				s.bad(501, "5.5.4", "Bad BODY parameter")
				return
			}
		case "SMTPUTF8":
			if v != "" {
				s.bad(501, "5.5.4", "Bad SMTPUTF8 parameter")
				return
			}
			env.UTF8 = true
		default:
			s.bad(555, "5.5.4", "Unsupported parameter")
			return
		}
	}
	if !env.UTF8 && !ascii(from) {
		s.bad(553, "5.6.7", "Non-ASCII address needs SMTPUTF8")
		return
	}
	s.env = env
	s.reply(250, "2.1.0", "OK")
}

func (s *session) cmdRcpt(arg string) {
	if s.env == nil {
		s.bad(503, "5.5.1", "Send MAIL first")
		return
	}
	rest, ok := cutPrefixFold(arg, "TO:")
	if !ok {
		s.bad(501, "5.5.4", "Syntax: RCPT TO:<address>")
		return
	}
	to, params, err := parsePath(rest, false)
	if err != nil {
		s.bad(501, "5.1.3", "Bad recipient address")
		return
	}
	if len(params) > 0 {
		s.bad(555, "5.5.4", "Unsupported parameter")
		return
	}
	if len(s.env.Rcpts) >= s.srv.MaxRecipients {
		s.reply(452, "4.5.3", "Too many recipients")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.srv.HandlerTimeout)
	defer cancel()
	if err := s.srv.Handler.Rcpt(ctx, s.env, to); err != nil {
		s.errors++
		s.replyErr(err)
		return
	}
	s.env.Rcpts = append(s.env.Rcpts, to)
	s.reply(250, "2.1.5", "OK")
}

func (s *session) cmdData(arg string) {
	switch {
	case arg != "":
		s.bad(501, "5.5.4", "Syntax: DATA")
		return
	case s.env == nil:
		s.bad(503, "5.5.1", "Send MAIL first")
		return
	case len(s.env.Rcpts) == 0:
		s.bad(554, "5.5.1", "No valid recipients")
		return
	}
	s.reply(354, "", "End data with <CR><LF>.<CR><LF>")
	s.flush()

	s.conn.SetReadDeadline(time.Now().Add(s.srv.DataTimeout))
	msg, err := readData(s.r, s.srv.MaxMessageBytes)
	env := s.env
	s.env = nil
	switch {
	case errors.Is(err, errTooBig):
		s.reply(552, "5.3.4", "Message too big")
		return
	case errors.Is(err, errBareLineEnding):
		s.log.Info("rejected bare CR or LF in DATA", "from", env.From)
		s.reply(550, "5.6.0", "Bare CR or LF in message, use CRLF line endings")
		return
	case errors.Is(err, errDataLineTooLong):
		s.reply(500, "5.5.2", "Line too long")
		return
	case err != nil:
		s.log.Debug("data read failed", "err", err)
		s.quit = true
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.srv.HandlerTimeout)
	defer cancel()
	if err := s.srv.Handler.Deliver(ctx, env, msg); err != nil {
		s.replyErr(err)
		return
	}
	s.reply(250, "2.0.0", "OK, message accepted")
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimLeft(s[len(prefix):], " "), true
}

func printable(s string) bool {
	for _, r := range s {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
