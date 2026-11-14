// Package smtp is an ESMTP server, for inbound mail and for submission, and
// an ESMTP client (client.go). It speaks the protocol and knows nothing
// about storage: recipients and messages go to a Handler.
package smtp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Envelope is what the client told the server before DATA.
type Envelope struct {
	RemoteIP net.IP
	Helo     string
	TLS      *tls.ConnectionState // nil without STARTTLS
	From     string               // empty for the null reverse path <>
	Rcpts    []string
	UTF8     bool // SMTPUTF8 was requested
	// AuthUser is what the Authenticator returned for the client's
	// credentials; empty on a server without authentication.
	AuthUser string
}

// Authenticator checks the credentials of AUTH (RFC 4954). It returns the
// name the session then acts as, ErrAuthFailed for wrong credentials, or an
// *Error to answer with something else, such as a 454 when it cannot tell.
type Authenticator interface {
	Authenticate(ctx context.Context, ip net.IP, username, password string) (user string, err error)
}

// ErrAuthFailed is "535 5.7.8 Authentication credentials invalid".
var ErrAuthFailed = errors.New("smtp: authentication failed")

// MailChecker is a Handler that also wants to see MAIL FROM, for instance
// to refuse a sender the authenticated user may not use.
type MailChecker interface {
	Mail(ctx context.Context, env *Envelope) error
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
	Hostname  string
	TLSConfig *tls.Config // nil disables STARTTLS
	Handler   Handler
	// Auth makes this a submission server (RFC 6409): AUTH PLAIN and LOGIN
	// are offered once the connection is encrypted, and nothing is
	// accepted from a client that has not authenticated. The connection is
	// encrypted after STARTTLS, or from the start when Serve is given a
	// TLS listener (port 465, RFC 8314).
	Auth            Authenticator
	Logger          *slog.Logger
	MaxMessageBytes int64         // [25 MiB]
	MaxRecipients   int           // [100]
	MaxConnections  int           // [100]
	MaxConnsPerIP   int           // [10]
	CommandTimeout  time.Duration // [5m] per command, RFC 5321 4.5.3.2
	DataTimeout     time.Duration // [10m] for the whole message
	HandlerTimeout  time.Duration // [2m] for one Rcpt or Deliver call
	MaxErrors       int           // [10] bad commands before the connection is closed
	// Trace logs every command line and every reply line, for
	// development: never the message, and AUTH without its credentials.
	// A command line logged with more=true came in together with what
	// follows it, before its reply was written: a pipelining client.
	Trace bool

	mu       sync.Mutex
	perIP    map[string]int
	sessions map[*session]struct{}
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
		s.sessions = map[*session]struct{}{}
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

// Shutdown stops accepting connections and waits for the open ones to end.
// A session waiting for a command is told 421 right away (RFC 5321 3.8); one
// that is busy, for instance receiving or delivering a message, finishes
// that command, answers it, and is then told 421. Once ctx is done the
// connections still open are closed.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	for _, l := range s.listener {
		l.Close()
	}
	for sess := range s.sessions {
		if sess.idle {
			// Wakes the read in session.serve, which then sees closed.
			sess.conn.SetReadDeadline(time.Now())
		}
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for sess := range s.sessions {
			sess.conn.Close()
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
	var implicit *tls.ConnectionState
	if tc, ok := c.(*tls.Conn); ok {
		// Implicit TLS: the handshake comes before the greeting.
		tc.SetDeadline(time.Now().Add(time.Minute))
		if err := tc.Handshake(); err != nil {
			s.Logger.Debug("tls handshake failed", "remote", key, "err", err)
			return
		}
		tc.SetDeadline(time.Time{})
		st := tc.ConnectionState()
		implicit = &st
	}
	s.mu.Lock()
	if s.perIP[key] >= s.MaxConnsPerIP {
		s.mu.Unlock()
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		io.WriteString(c, "421 4.7.0 Too many connections from your address\r\n")
		return
	}
	sess := &session{srv: s, ip: ip, log: s.Logger.With("remote", key)}
	sess.setConn(c)
	sess.tls = implicit
	s.perIP[key]++
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.perIP[key]--; s.perIP[key] <= 0 {
			delete(s.perIP, key)
		}
		delete(s.sessions, sess)
		s.mu.Unlock()
	}()
	sess.serve()
	// The session's own connection, not c: after STARTTLS that is the TLS
	// layer, whose Close says goodbye first (close_notify, RFC 8446 6.1).
	// Without it the other side sees a connection cut off in the middle,
	// and Exim logs every delivery as a TLS error.
	s.mu.Lock()
	conn := sess.conn
	s.mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	conn.Close()
}

// session is one SMTP connection.
type session struct {
	srv *Server
	// conn changes at STARTTLS and idle with every command. Shutdown reads
	// both, so writes to them hold srv.mu.
	conn net.Conn
	idle bool // waiting for a command
	r    *bufio.Reader
	w    *bufio.Writer
	ip   net.IP
	log  *slog.Logger

	helo   string
	ehlo   bool
	user   string // set by a successful AUTH
	tls    *tls.ConnectionState
	env    *Envelope // non-nil between MAIL and the end of the transaction
	errors int
	quit   bool
	hold   bool // the reply just written may wait for the next one
}

func (s *session) setConn(c net.Conn) {
	s.conn = c
	s.r = bufio.NewReaderSize(c, maxData)
	s.w = bufio.NewWriter(c)
}

// maxReplyLine is the longest reply line, CRLF included (RFC 5321 4.5.3.1.5).
const maxReplyLine = 512

// reply writes a response into the output buffer; serve decides when it is
// sent. The text can come from outside (a handler passing on what Rspamd
// said), so it is made to fit RFC 5321 4.2: printable ASCII, and a line of
// at most 512 octets.
func (s *session) reply(code int, enhanced string, lines ...string) {
	for i, l := range lines {
		sep := " "
		if i < len(lines)-1 {
			sep = "-"
		}
		head := fmt.Sprintf("%d%s", code, sep)
		if enhanced != "" {
			head += enhanced + " "
		}
		l = strings.Map(func(r rune) rune {
			if r < 0x20 || r > 0x7e {
				return '?'
			}
			return r
		}, l)
		if max := maxReplyLine - 2 - len(head); len(l) > max {
			l = l[:max]
		}
		if s.srv.Trace {
			s.log.Info("smtp trace", "side", "server", "dir", "S", "line", head+l)
		}
		s.w.WriteString(head + l + "\r\n")
	}
}

// pendingLine reports whether a complete command line is waiting in the
// input buffer.
func (s *session) pendingLine() bool {
	buf, _ := s.r.Peek(s.r.Buffered())
	return bytes.IndexByte(buf, '\n') >= 0
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
		// PIPELINING (RFC 2920 3.2): replies to RSET, MAIL and RCPT may
		// wait for the reply to the next command of the group, all others
		// go out at once, and nothing is held back once no complete
		// command is left to read.
		if !s.hold || !s.pendingLine() {
			s.flush()
			if s.quit {
				return
			}
		}
		s.hold = false
		if !s.await() {
			s.reply(421, "4.3.2", s.srv.Hostname+" Service shutting down, closing transmission channel")
			return
		}
		line, err := s.readLine()
		s.srv.mu.Lock()
		s.idle = false
		closed := s.srv.closed
		s.srv.mu.Unlock()
		if err == errLineTooLong {
			s.bad(500, "5.5.2", "Line too long")
			continue
		}
		var ne net.Error
		switch {
		case err == nil:
			if s.srv.Trace {
				shown := line
				if len(shown) >= 5 && strings.EqualFold(shown[:5], "AUTH ") {
					mech, _, _ := strings.Cut(shown[5:], " ")
					shown = "AUTH " + mech + " ..."
				}
				s.log.Info("smtp trace", "side", "server", "dir", "C", "line", shown, "more", s.r.Buffered() > 0)
			}
			s.command(line)
		case errors.As(err, &ne) && ne.Timeout() && closed:
			s.reply(421, "4.3.2", s.srv.Hostname+" Service shutting down, closing transmission channel")
			return
		case errors.As(err, &ne) && ne.Timeout():
			s.reply(421, "4.4.2", s.srv.Hostname+" Timeout, closing transmission channel")
			return
		default:
			if !errors.Is(err, io.EOF) {
				s.log.Debug("read failed", "err", err)
			}
			return
		}
	}
}

// await marks the session as waiting for a command and starts the command
// timeout. It returns false when the server is shutting down. Marking and
// checking happen under the lock Shutdown holds, so either Shutdown sees the
// session idle and wakes it, or the session sees the server closed.
func (s *session) await() bool {
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	if s.srv.closed {
		return false
	}
	s.idle = true
	s.conn.SetReadDeadline(time.Now().Add(s.srv.CommandTimeout))
	return true
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
		// RFC 5321 4.3.2: commands without arguments answer 501 to one.
		if arg != "" {
			s.bad(501, "5.5.4", "Syntax: RSET")
			return
		}
		s.env = nil
		s.hold = true
		s.reply(250, "2.0.0", "OK")
	case "NOOP":
		// Its optional argument is ignored (4.1.1.9).
		s.reply(250, "2.0.0", "OK")
	case "QUIT":
		if arg != "" {
			s.bad(501, "5.5.4", "Syntax: QUIT")
			return
		}
		s.reply(221, "2.0.0", s.srv.Hostname+" Service closing transmission channel")
		s.quit = true
	case "VRFY":
		if arg == "" {
			s.bad(501, "5.5.4", "Syntax: VRFY address")
			return
		}
		// 3.5.3 and 7.3: a server may decline to verify, with 252.
		s.reply(252, "2.5.0", "Cannot VRFY user, but will accept message and attempt delivery")
	case "EXPN":
		// Recognised but not offered: 502, not 500 (4.2.4).
		s.bad(502, "5.5.1", "EXPN is not available")
	case "HELP":
		s.reply(214, "2.0.0", "See RFC 5321")
	case "AUTH":
		s.cmdAuth(arg)
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
	// RFC 4954 4: mechanisms that send the password are only offered on an
	// encrypted connection.
	if s.srv.Auth != nil && s.tls != nil {
		lines = append(lines, "AUTH PLAIN LOGIN")
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

// authLine reads one line of the client inside an AUTH exchange and
// decodes it. A line may be as long as a command line; RFC 4954 4 asks for
// 12288 octets, which no address and password need. "*" cancels (RFC 4954 4).
func (s *session) authLine(prompt string) (string, bool) {
	s.reply(334, "", base64.StdEncoding.EncodeToString([]byte(prompt)))
	s.flush()
	if s.quit {
		return "", false
	}
	s.conn.SetReadDeadline(time.Now().Add(s.srv.CommandTimeout))
	line, err := s.readLine()
	if err != nil {
		if err == errLineTooLong {
			s.bad(500, "5.5.6", "Authentication exchange line is too long")
		} else {
			s.quit = true
		}
		return "", false
	}
	if line == "*" {
		s.bad(501, "5.0.0", "Authentication cancelled")
		return "", false
	}
	return s.authDecode(line)
}

func (s *session) authDecode(b64 string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		s.bad(501, "5.5.2", "Cannot decode the authentication data")
		return "", false
	}
	return string(raw), true
}

// cmdAuth is AUTH (RFC 4954) with the mechanisms PLAIN (RFC 4616) and
// LOGIN (the same over two prompts, which old clients still use).
func (s *session) cmdAuth(arg string) {
	switch {
	case s.srv.Auth == nil:
		s.bad(502, "5.5.1", "Authentication is not available on this port")
		return
	case s.helo == "":
		s.bad(503, "5.5.1", "Send EHLO first")
		return
	case s.user != "":
		// "After an AUTH command has been successfully completed, no more
		// AUTH commands may be issued in the same session."
		s.bad(503, "5.5.1", "Already authenticated")
		return
	case s.env != nil:
		s.bad(503, "5.5.1", "AUTH is not permitted during a mail transaction")
		return
	case s.tls == nil:
		s.bad(538, "5.7.11", "Encryption required for requested authentication mechanism")
		return
	}
	mech, initial, hasInitial := strings.Cut(arg, " ")
	initial = strings.TrimSpace(initial)
	var username, password string
	switch strings.ToUpper(mech) {
	case "PLAIN":
		// message = [authzid] NUL authcid NUL passwd (RFC 4616 2)
		var msg string
		var ok bool
		switch {
		case !hasInitial:
			msg, ok = s.authLine("")
		case initial == "=":
			msg, ok = "", true // an empty initial response
		default:
			msg, ok = s.authDecode(initial)
		}
		if !ok {
			return
		}
		parts := strings.Split(msg, "\x00")
		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			s.bad(501, "5.5.2", "Malformed PLAIN authentication data")
			return
		}
		// Nobody may act as someone else here.
		if parts[0] != "" && parts[0] != parts[1] {
			s.bad(535, "5.7.8", "Authorization identity is not permitted")
			return
		}
		username, password = parts[1], parts[2]
	case "LOGIN":
		var ok bool
		if hasInitial && initial != "=" {
			username, ok = s.authDecode(initial)
		} else {
			username, ok = s.authLine("Username:")
		}
		if !ok {
			return
		}
		if password, ok = s.authLine("Password:"); !ok {
			return
		}
	default:
		s.bad(504, "5.5.4", "Unrecognized authentication type")
		return
	}
	if !utf8.ValidString(username) || !utf8.ValidString(password) || len(username) > 320 || len(password) > 1024 {
		s.bad(535, "5.7.8", "Authentication credentials invalid")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.srv.HandlerTimeout)
	defer cancel()
	user, err := s.srv.Auth.Authenticate(ctx, s.ip, username, password)
	var e *Error
	switch {
	case errors.Is(err, ErrAuthFailed):
		s.bad(535, "5.7.8", "Authentication credentials invalid")
	case errors.As(err, &e):
		s.errors++
		s.reply(e.Code, e.Enhanced, e.Msg)
	case err != nil || user == "":
		s.log.Error("authentication failed to run", "err", err)
		s.reply(454, "4.7.0", "Temporary authentication failure")
	default:
		s.user = user
		s.reply(235, "2.7.0", "Authentication successful")
	}
}

// swapConn swaps the connection for its TLS wrapper and keeps Shutdown able to close it.
func (s *session) swapConn(tc *tls.Conn) {
	s.srv.mu.Lock()
	defer s.srv.mu.Unlock()
	s.setConn(tc)
}

// sizeValue parses the value of the SIZE parameter. A number too large for
// 64 bits is a valid size-value, and larger than any limit.
func sizeValue(v string) (uint64, bool) {
	if len(v) == 0 || len(v) > 20 {
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return math.MaxUint64, true
	}
	return n, true
}

func (s *session) cmdMail(arg string) {
	s.hold = true
	if s.helo == "" {
		s.bad(503, "5.5.1", "Send EHLO first")
		return
	}
	if s.env != nil {
		s.bad(503, "5.5.1", "Nested MAIL command")
		return
	}
	if s.srv.Auth != nil {
		switch {
		case s.tls == nil:
			// RFC 3207 4
			s.bad(530, "5.7.0", "Must issue a STARTTLS command first")
			return
		case s.user == "":
			// RFC 4954 6
			s.bad(530, "5.7.0", "Authentication required")
			return
		}
	}
	rest, ok := cutPrefixFold(arg, "FROM:")
	if !ok {
		s.bad(501, "5.5.4", "Syntax: MAIL FROM:<address>")
		return
	}
	from, params, err := parsePath(rest, false)
	if err != nil {
		s.bad(501, "5.1.7", "Bad sender address")
		return
	}
	env := &Envelope{RemoteIP: s.ip, Helo: s.helo, TLS: s.tls, From: from, AuthUser: s.user}
	tooBig := false
	for _, p := range params {
		switch p.key {
		case "SIZE":
			// RFC 1870 3: size-value ::= 1*20DIGIT
			n, ok := sizeValue(p.value)
			if !ok {
				s.bad(501, "5.5.4", "Bad SIZE parameter")
				return
			}
			tooBig = n > uint64(s.srv.MaxMessageBytes)
		case "BODY":
			// RFC 6152 2: body-value ::= "7BIT" / "8BITMIME"
			if u := strings.ToUpper(p.value); u != "7BIT" && u != "8BITMIME" {
				s.bad(501, "5.5.4", "Bad BODY parameter")
				return
			}
		case "SMTPUTF8":
			// RFC 6531 3.4: the parameter takes no value.
			if p.hasValue {
				s.bad(501, "5.5.4", "Bad SMTPUTF8 parameter")
				return
			}
			env.UTF8 = true
		case "AUTH":
			// RFC 4954 5: who the message was first submitted by. Only an
			// authenticated client may send it, and we attach no meaning
			// to it: the session's own identity is what counts.
			if s.user == "" || !p.hasValue {
				s.bad(555, "5.5.4", "Unsupported parameter")
				return
			}
		default:
			// RFC 5321 4.1.1.11
			s.bad(555, "5.5.4", "Unsupported parameter")
			return
		}
	}
	if !env.UTF8 && !ascii(from) {
		// RFC 6531 3.5 gives MAIL 550 for this and RCPT 553.
		s.bad(550, "5.6.7", "Non-ASCII address needs SMTPUTF8")
		return
	}
	if tooBig {
		// RFC 1870 6.1
		s.reply(552, "5.3.4", "Message size exceeds fixed maximum message size")
		return
	}
	if mc, ok := s.srv.Handler.(MailChecker); ok {
		ctx, cancel := context.WithTimeout(context.Background(), s.srv.HandlerTimeout)
		defer cancel()
		if err := mc.Mail(ctx, env); err != nil {
			s.errors++
			s.replyErr(err)
			return
		}
	}
	s.env = env
	s.reply(250, "2.1.0", "OK")
}

func (s *session) cmdRcpt(arg string) {
	s.hold = true
	if s.env == nil {
		s.bad(503, "5.5.1", "Send MAIL first")
		return
	}
	rest, ok := cutPrefixFold(arg, "TO:")
	if !ok {
		s.bad(501, "5.5.4", "Syntax: RCPT TO:<address>")
		return
	}
	to, params, err := parsePath(rest, true)
	if err != nil {
		s.bad(501, "5.1.3", "Bad recipient address")
		return
	}
	if len(params) > 0 {
		s.bad(555, "5.5.4", "Unsupported parameter")
		return
	}
	if !s.env.UTF8 && !ascii(to) {
		// RFC 6531 3.5
		s.bad(553, "5.6.7", "Non-ASCII address needs SMTPUTF8")
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
