package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// The states of a connection (RFC 3501 3).
const (
	stateNotAuth = iota
	stateAuth
	stateSelected
	stateLogout
)

const (
	// maxBadCommands is how many commands in a row may be BAD before the
	// client is dropped; maxFailedLogins how many logins may fail on one
	// connection.
	maxBadCommands  = 20
	maxFailedLogins = 3
)

// conn is one session.
type conn struct {
	srv *Server
	nc  net.Conn
	ip  string
	br  *bufio.Reader
	bw  *bufio.Writer
	tls bool

	state       int
	user        User
	countedUser string // the name counted against MaxConnsPerUser
	changes     <-chan struct{}
	stopChanges func()
	// revoked is closed when the login was withdrawn (Revocable); nil for
	// a user whose login cannot be.
	revoked     <-chan struct{}
	stopRevoked func()
	sel         *selected

	// What the client enabled (RFC 5161): CONDSTORE (RFC 7162), IMAP4rev2
	// (RFC 9051), UTF8=ACCEPT (RFC 6855).
	condstore bool
	rev2      bool
	utf8      bool
	// usesUIDs: the client has sent a UID command.
	usesUIDs bool
	// saved is the result of the last SEARCH with SAVE, as UIDs (RFC
	// 5182). nil is no result.
	saved      []uint32
	savedValid bool

	ctx    context.Context
	cancel context.CancelFunc

	bad          int // BAD commands in a row
	failedLogins int

	// dmu guards shut and the setting of read deadlines against wake.
	dmu  sync.Mutex
	shut bool
}

var errShutdown = errors.New("imap: server is shutting down")

// byeRevoked ends a session whose login was withdrawn. RFC 3501 7.1.5:
// BYE is how a server says it is about to close the connection; RFC 5530
// gives no response code that fits better than none.
const byeRevoked = "* BYE this login was revoked; log in again"

func (c *conn) isRevoked() bool {
	select {
	case <-c.revoked:
		return c.revoked != nil
	default:
		return false
	}
}

func newConn(s *Server, nc net.Conn, ip string) *conn {
	c := &conn{srv: s, nc: nc, ip: ip}
	_, c.tls = nc.(*tls.Conn)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.setConn(nc)
	return c
}

func (c *conn) setConn(nc net.Conn) {
	c.nc = nc
	c.br = bufio.NewReaderSize(connReader{c}, 16<<10)
	c.bw = bufio.NewWriterSize(connWriter{c}, 32<<10)
}

// connReader gives every read its deadline: how long the client may be
// silent.
type connReader struct{ c *conn }

func (r connReader) Read(p []byte) (int, error) {
	c := r.c
	c.dmu.Lock()
	if c.shut {
		c.dmu.Unlock()
		return 0, errShutdown
	}
	timeout := c.srv.LoginTimeout
	if c.state != stateNotAuth {
		timeout = c.srv.IdleTimeout
	}
	c.nc.SetReadDeadline(time.Now().Add(timeout))
	c.dmu.Unlock()
	n, err := c.nc.Read(p)
	if err != nil {
		c.dmu.Lock()
		shut := c.shut
		c.dmu.Unlock()
		if shut {
			return n, errShutdown
		}
	}
	return n, err
}

type connWriter struct{ c *conn }

func (w connWriter) Write(p []byte) (int, error) {
	w.c.nc.SetWriteDeadline(time.Now().Add(w.c.srv.WriteTimeout))
	return w.c.nc.Write(p)
}

// wake ends a read that is waiting for the client: the server is stopping.
func (c *conn) wake() {
	c.dmu.Lock()
	c.shut = true
	c.nc.SetReadDeadline(time.Unix(1, 0))
	c.dmu.Unlock()
}

// printf writes one response line.
func (c *conn) printf(format string, a ...any) {
	if len(a) == 0 {
		c.bw.WriteString(format)
	} else {
		fmt.Fprintf(c.bw, format, a...)
	}
	c.bw.WriteString("\r\n")
}

func (c *conn) flush() error { return c.bw.Flush() }

// reply is the tagged answer to a command.
type reply struct {
	status string // OK, NO or BAD
	code   string // response code, without brackets
	text   string
}

func ok(text string) reply           { return reply{"OK", "", text} }
func okCode(code, text string) reply { return reply{"OK", code, text} }
func no(text string) reply           { return reply{"NO", "", text} }
func noCode(code, text string) reply { return reply{"NO", code, text} }
func (r reply) line(tag string) string {
	if r.code != "" {
		return tag + " " + r.status + " [" + r.code + "] " + r.text
	}
	return tag + " " + r.status + " " + r.text
}

// failure turns a backend error into the answer for the client.
func (c *conn) failure(what string, err error) reply {
	switch {
	case errors.Is(err, ErrNoMailbox):
		return noCode("NONEXISTENT", "no such mailbox")
	case errors.Is(err, errNotUTF7):
		return noCode("CANNOT", "the mailbox name is not modified UTF-7")
	case errors.Is(err, ErrExists):
		return noCode("ALREADYEXISTS", "a mailbox with that name exists")
	case errors.Is(err, ErrHasChildren):
		return no("the mailbox has child mailboxes; delete those first")
	case errors.Is(err, ErrSpecial):
		return noCode("NOPERM", "this mailbox cannot be deleted")
	case errors.Is(err, ErrName):
		return noCode("CANNOT", "not a name a mailbox can have")
	case errors.Is(err, ErrLoop):
		return noCode("CANNOT", "a mailbox cannot be moved under itself")
	case errors.Is(err, ErrFlag):
		return noCode("CANNOT", "not a flag a message can have")
	case errors.Is(err, ErrTooLarge):
		return noCode("TOOBIG", "the message is too large")
	case errors.Is(err, ErrNoMessage):
		return noCode("EXPUNGEISSUED", "the message is gone")
	}
	if c.ctx.Err() == nil {
		c.srv.Logger.Error("imap: "+what+" failed", "user", c.username(), "err", err)
	}
	return noCode("UNAVAILABLE", "the server cannot do that right now, try again later")
}

func (c *conn) username() string {
	if c.user == nil {
		return ""
	}
	return c.user.Username()
}

// capabilities is the capability list for the state the connection is in
// (RFC 3501 7.2.1; RFC 9051 7.2.2: it may differ before and after login).
func (c *conn) capabilities() string {
	caps := []string{"IMAP4rev1", "IMAP4rev2", "LITERAL+", "SASL-IR", "ID", "ENABLE"}
	if c.state == stateNotAuth {
		if c.tls {
			caps = append(caps, "AUTH=PLAIN")
		} else {
			if c.srv.TLSConfig != nil {
				caps = append(caps, "STARTTLS")
			}
			// RFC 3501 6.2.3: LOGIN is refused without TLS, and said so.
			caps = append(caps, "LOGINDISABLED")
		}
		return strings.Join(caps, " ")
	}
	return strings.Join(append(caps, "IDLE", "NAMESPACE", "UNSELECT", "UIDPLUS", "MOVE", "SPECIAL-USE", "CHILDREN",
		"LIST-EXTENDED", "LIST-STATUS", "CONDSTORE", "ESEARCH", "SEARCHRES", "BINARY", "STATUS=SIZE", "UTF8=ACCEPT"), " ")
}

// command is one entry of the command table.
type command struct {
	state int // the lowest state it is valid in
	// only is the one state it is valid in, when not 0... see table.
	notAuthOnly bool
	// noExpunge: no EXPUNGE response may be sent while answering it (RFC
	// 3501 7.4.1: FETCH, STORE and SEARCH; not their UID forms).
	noExpunge bool
	run       func(c *conn, tag string, p *parser) (reply, error)
}

var commands map[string]command

func init() {
	commands = map[string]command{
		"CAPABILITY":   {run: (*conn).cmdCapability},
		"NOOP":         {run: (*conn).cmdNoop},
		"LOGOUT":       {run: (*conn).cmdLogout},
		"ID":           {run: (*conn).cmdID},
		"STARTTLS":     {notAuthOnly: true, run: (*conn).cmdStartTLS},
		"LOGIN":        {notAuthOnly: true, run: (*conn).cmdLogin},
		"AUTHENTICATE": {notAuthOnly: true, run: (*conn).cmdAuthenticate},

		"ENABLE":      {state: stateAuth, run: (*conn).cmdEnable},
		"SELECT":      {state: stateAuth, run: (*conn).cmdSelect},
		"EXAMINE":     {state: stateAuth, run: (*conn).cmdExamine},
		"CREATE":      {state: stateAuth, run: (*conn).cmdCreate},
		"DELETE":      {state: stateAuth, run: (*conn).cmdDelete},
		"RENAME":      {state: stateAuth, run: (*conn).cmdRename},
		"SUBSCRIBE":   {state: stateAuth, run: (*conn).cmdSubscribe},
		"UNSUBSCRIBE": {state: stateAuth, run: (*conn).cmdUnsubscribe},
		"LIST":        {state: stateAuth, run: (*conn).cmdList},
		"LSUB":        {state: stateAuth, run: (*conn).cmdLsub},
		"NAMESPACE":   {state: stateAuth, run: (*conn).cmdNamespace},
		"STATUS":      {state: stateAuth, run: (*conn).cmdStatus},
		"APPEND":      {state: stateAuth, run: (*conn).cmdAppend},
		"IDLE":        {state: stateAuth, run: (*conn).cmdIdle},

		"CHECK":       {state: stateSelected, run: (*conn).cmdCheck},
		"CLOSE":       {state: stateSelected, run: (*conn).cmdClose},
		"UNSELECT":    {state: stateSelected, run: (*conn).cmdUnselect},
		"EXPUNGE":     {state: stateSelected, run: (*conn).cmdExpunge},
		"SEARCH":      {state: stateSelected, noExpunge: true, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdSearch(tag, p, false) }},
		"FETCH":       {state: stateSelected, noExpunge: true, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdFetch(tag, p, false) }},
		"STORE":       {state: stateSelected, noExpunge: true, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdStore(tag, p, false) }},
		"COPY":        {state: stateSelected, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdCopy(tag, p, false, false) }},
		"MOVE":        {state: stateSelected, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdCopy(tag, p, false, true) }},
		"UID SEARCH":  {state: stateSelected, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdSearch(tag, p, true) }},
		"UID FETCH":   {state: stateSelected, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdFetch(tag, p, true) }},
		"UID STORE":   {state: stateSelected, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdStore(tag, p, true) }},
		"UID COPY":    {state: stateSelected, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdCopy(tag, p, true, false) }},
		"UID MOVE":    {state: stateSelected, run: func(c *conn, tag string, p *parser) (reply, error) { return c.cmdCopy(tag, p, true, true) }},
		"UID EXPUNGE": {state: stateSelected, run: (*conn).cmdUIDExpunge},
	}
}

// serve runs the session until the client leaves or is told to.
func (c *conn) serve() {
	defer func() {
		c.cancel()
		if c.stopChanges != nil {
			c.stopChanges()
		}
		if c.stopRevoked != nil {
			c.stopRevoked()
		}
		c.nc.Close()
	}()
	if tc, isTLS := c.nc.(*tls.Conn); isTLS {
		tc.SetDeadline(time.Now().Add(c.srv.LoginTimeout))
		if err := tc.HandshakeContext(c.ctx); err != nil {
			return
		}
		tc.SetDeadline(time.Time{})
	}
	c.printf("* OK [CAPABILITY %s] %s robinmail IMAP ready", c.capabilities(), c.srv.Hostname)

	reader := &commandReader{br: c.br,
		continuation: func() error { c.printf("+ OK"); return c.flush() },
		literalLimit: c.literalLimit}
	for c.state != stateLogout {
		if c.flush() != nil {
			return
		}
		if c.srv.isClosing() {
			c.printf("* BYE the server is shutting down")
			c.flush()
			return
		}
		if c.isRevoked() {
			c.printf(byeRevoked)
			c.flush()
			return
		}
		reader.br = c.br
		parts, skip, err := reader.read()
		if err != nil {
			c.readFailed(parts, skip, err)
			if !errors.Is(err, errTooLarge) || skip > 0 {
				return
			}
			continue
		}
		c.handle(parts)
	}
	c.flush()
}

// readFailed answers a command that could not be read.
func (c *conn) readFailed(parts []part, skip int64, err error) {
	tag := "*"
	if len(parts) > 0 {
		if t, _, found := strings.Cut(parts[0].text, " "); found && validTag(t) {
			tag = t
		}
	}
	var ne net.Error
	switch {
	case errors.Is(err, errShutdown) && c.isRevoked() && !c.srv.isClosing():
		c.printf(byeRevoked)
	case errors.Is(err, errShutdown):
		c.printf("* BYE the server is shutting down")
	case errors.Is(err, errLineTooLong):
		c.printf("* BYE command line too long")
	case errors.Is(err, errTooLarge) && skip > 0:
		// A non-synchronizing literal is on its way and will not be read
		// (RFC 7888 4: "the server ... MUST ... close the connection").
		c.printf("* BYE [TOOBIG] literal too large")
	case errors.Is(err, errTooLarge):
		if tag == "*" {
			c.printf("* BAD literal too large")
		} else {
			c.printf("%s NO [TOOBIG] too large: a message may have %d octets, any other literal %d", tag, c.srv.MaxMessageBytes, maxLiteral)
		}
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, os.ErrDeadlineExceeded):
		c.printf("* BYE autologout: idle for too long")
	default:
		return // the client went away
	}
	c.flush()
}

// literalLimit decides how large the literal that comes next may be: the
// message of an APPEND may be a whole message, anything else is a name or
// a search string.
func (c *conn) literalLimit(parts []part) int64 {
	if c.state < stateAuth {
		return maxLiteral
	}
	p := &parser{parts: parts, utf8: true}
	p.takeWhile(func(b byte) bool { return b != ' ' })
	if p.space() != nil {
		return maxLiteral
	}
	if name, err := p.word(); err != nil || name != "APPEND" || p.space() != nil {
		return maxLiteral
	}
	pending := func() bool { return p.i == len(p.parts)-1 && p.pos == len(p.parts[p.i].text) }
	if pending() {
		return maxLiteral // the mailbox name is the literal
	}
	if _, err := p.astring(); err != nil || p.space() != nil {
		return maxLiteral
	}
	if p.peek() == '(' {
		if _, err := p.flagList(false); err != nil || p.space() != nil {
			return maxLiteral
		}
	}
	if p.peek() == '"' {
		if _, err := p.dateTime(); err != nil || p.space() != nil {
			return maxLiteral
		}
	}
	if strings.HasPrefix(strings.ToUpper(p.cur()), "UTF8 (") {
		p.pos += len("UTF8 (")
	}
	if pending() {
		return c.srv.MaxMessageBytes
	}
	return maxLiteral
}

// handle runs one command.
func (c *conn) handle(parts []part) {
	p := &parser{parts: parts, utf8: c.utf8}
	tag := p.takeWhile(func(b byte) bool { return b != ' ' })
	if !validTag(tag) {
		c.printf("* BAD a command starts with a tag")
		c.countBad()
		return
	}
	name := ""
	if err := p.space(); err == nil {
		name, _ = p.word()
	}
	if name == "UID" {
		sub := ""
		if err := p.space(); err == nil {
			sub, _ = p.word()
		}
		name = "UID " + sub
		c.usesUIDs = true
	}
	cmd, known := commands[name]
	if c.srv.Trace {
		args := p.cur()
		if name == "LOGIN" || name == "AUTHENTICATE" {
			args = " ..."
		}
		c.srv.Logger.Info("imap <", "ip", c.ip, "user", c.username(), "cmd", tag+" "+name+clip(args, 200), "literals", len(parts)/2)
	}
	var r reply
	switch {
	case !known:
		r = reply{"BAD", "", "unknown command"}
	case cmd.notAuthOnly && c.state != stateNotAuth:
		r = reply{"BAD", "", "already logged in"}
	case c.state < cmd.state && c.state == stateNotAuth:
		r = reply{"BAD", "", "log in first"}
	case c.state < cmd.state:
		r = reply{"BAD", "", "select a mailbox first"}
	default:
		var err error
		r, err = cmd.run(c, tag, p)
		var pe *parseError
		switch {
		case errors.As(err, &pe):
			r = reply{"BAD", "", pe.msg}
		case err != nil:
			r = c.failure(name, err)
		}
		if c.state == stateSelected {
			if err := c.poll(!cmd.noExpunge); err != nil && r.status == "OK" {
				r = c.failure(name, err)
			}
		}
	}
	if r.status == "BAD" {
		c.countBad()
	} else {
		c.bad = 0
	}
	if r.status != "" {
		c.printf("%s", r.line(tag))
	}
	if c.srv.Trace {
		c.srv.Logger.Info("imap >", "ip", c.ip, "user", c.username(), "reply", clip(r.line(tag), 200))
	}
}

func (c *conn) countBad() {
	if c.bad++; c.bad >= maxBadCommands {
		c.printf("* BYE too many commands that are not IMAP")
		c.state = stateLogout
	}
}

func (c *conn) cmdCapability(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	c.printf("* CAPABILITY %s", c.capabilities())
	return ok("CAPABILITY completed"), nil
}

func (c *conn) cmdNoop(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if c.sel != nil {
		c.sel.dirty = true // NOOP is how a client asks for news
	}
	return ok("NOOP completed"), nil
}

func (c *conn) cmdLogout(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	c.printf("* BYE %s robinmail IMAP closing the connection", c.srv.Hostname)
	c.sel = nil
	c.state = stateLogout
	return ok("LOGOUT completed"), nil
}

// cmdID is RFC 2971: the client says what it is, the server what it is.
func (c *conn) cmdID(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	var fields []string
	if p.peek() == '(' {
		p.pos++
		for !p.skip(')') {
			if len(fields) > 0 {
				if err := p.space(); err != nil {
					return reply{}, err
				}
			}
			// RFC 2971 3.3: a field is a string, a value an nstring.
			var s string
			var err error
			if len(fields)%2 == 1 && strings.HasPrefix(strings.ToUpper(p.cur()), "NIL") {
				p.pos += 3
			} else if s, err = p.str(); err != nil {
				return reply{}, err
			}
			fields = append(fields, s)
			if len(fields) > 60 {
				return reply{}, bad("ID takes at most 30 fields")
			}
		}
		if len(fields)%2 != 0 {
			return reply{}, bad("ID takes pairs of field and value")
		}
	} else if w, _ := p.word(); w != "NIL" {
		return reply{}, bad("ID takes a list or NIL")
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if len(fields) > 0 {
		for i := range fields {
			fields[i] = clip(fields[i], 80)
		}
		c.srv.Logger.Info("imap: client id", "ip", c.ip, "user", c.username(), "id", strings.Join(fields, " "))
	}
	c.printf(`* ID ("name" "robinmail")`)
	return ok("ID completed"), nil
}

// cmdStartTLS is RFC 3501 6.2.1.
func (c *conn) cmdStartTLS(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if c.tls {
		return reply{"BAD", "", "TLS is already on"}, nil
	}
	if c.srv.TLSConfig == nil {
		return no("TLS is not available"), nil
	}
	// Whatever the client sent after STARTTLS without waiting was not
	// protected: it is not taken as a command (RFC 3501 6.2.1, and the
	// command injection of CVE-2011-0411).
	if c.br.Buffered() > 0 {
		c.printf("%s BAD nothing may follow STARTTLS before the handshake", tag)
		c.flush()
		c.state = stateLogout
		return reply{}, nil
	}
	c.printf("%s OK begin TLS negotiation now", tag)
	if c.flush() != nil {
		c.state = stateLogout
		return reply{}, nil
	}
	tc := tls.Server(c.nc, c.srv.TLSConfig)
	tc.SetDeadline(time.Now().Add(c.srv.LoginTimeout))
	if err := tc.HandshakeContext(c.ctx); err != nil {
		c.state = stateLogout
		return reply{}, nil
	}
	tc.SetDeadline(time.Time{})
	c.dmu.Lock()
	c.setConn(tc)
	c.dmu.Unlock()
	c.tls = true
	return reply{}, nil
}

// cmdLogin is RFC 3501 6.2.3.
func (c *conn) cmdLogin(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	name, err := p.astring()
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	password, err := p.astring()
	if err != nil {
		return reply{}, err
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if !c.tls {
		// The password has crossed the network in the clear already; all
		// that is left is not to reward it.
		return noCode("PRIVACYREQUIRED", "LOGIN is disabled without TLS; use STARTTLS first"), nil
	}
	return c.login(name, password), nil
}

// cmdAuthenticate is RFC 3501 6.2.2 with SASL PLAIN (RFC 4616) and an
// initial response (RFC 4959).
func (c *conn) cmdAuthenticate(tag string, p *parser) (reply, error) {
	if err := p.space(); err != nil {
		return reply{}, err
	}
	mech, err := p.word()
	if err != nil {
		return reply{}, err
	}
	initial, hasInitial := "", false
	if p.peek() == ' ' {
		p.pos++
		// RFC 4959 3: base64, or "=" for an empty response.
		initial = p.takeWhile(func(b byte) bool { return b > ' ' && b < 0x7f })
		hasInitial = true
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if !c.tls {
		return noCode("PRIVACYREQUIRED", "AUTHENTICATE is disabled without TLS; use STARTTLS first"), nil
	}
	if mech != "PLAIN" {
		return no("unsupported authentication mechanism"), nil
	}
	if !hasInitial {
		c.printf("+ ")
		if c.flush() != nil {
			c.state = stateLogout
			return reply{}, nil
		}
		line, err := readLine(c.br, maxLine)
		if err != nil {
			c.state = stateLogout
			return reply{}, nil
		}
		initial = line
	}
	if initial == "*" {
		return reply{"BAD", "", "authentication cancelled"}, nil
	}
	if initial == "=" {
		initial = ""
	}
	raw, err := base64.StdEncoding.DecodeString(initial)
	if err != nil {
		return reply{"BAD", "", "the response is not base64"}, nil
	}
	// RFC 4616 2: [authzid] NUL authcid NUL passwd.
	fields := strings.Split(string(raw), "\x00")
	if len(fields) != 3 || fields[1] == "" {
		return reply{"BAD", "", "not a PLAIN response"}, nil
	}
	if fields[0] != "" && !strings.EqualFold(fields[0], fields[1]) {
		c.failedLogins++
		return noCode("AUTHORIZATIONFAILED", "you can only log in as yourself"), nil
	}
	return c.login(fields[1], fields[2]), nil
}

// login asks the backend and moves to the authenticated state.
func (c *conn) login(name, password string) reply {
	user, err := c.srv.Backend.Login(c.ctx, c.ip, name, password)
	switch {
	case errors.Is(err, ErrAuth):
		if c.failedLogins++; c.failedLogins >= maxFailedLogins {
			c.printf("* BYE too many failed logins")
			c.state = stateLogout
		}
		return noCode("AUTHENTICATIONFAILED", "wrong name or password")
	case errors.Is(err, ErrLimited):
		return noCode("UNAVAILABLE", "too many failed logins; try again later")
	case err != nil:
		return c.failure("login", err)
	}
	if !c.srv.userLoggedIn(c, strings.ToLower(user.Username())) {
		return noCode("UNAVAILABLE", "too many connections for this user")
	}
	c.user = user
	c.changes, c.stopChanges = user.Changes()
	if r, ok := user.(Revocable); ok {
		// A session waiting for a command, or in IDLE, is woken the way a
		// shutdown wakes it; one in a command sees it when that is done.
		c.revoked, c.stopRevoked = r.Revoked()
		go func(revoked <-chan struct{}) {
			select {
			case <-revoked:
				c.wake()
			case <-c.ctx.Done():
			}
		}(c.revoked)
	}
	c.state = stateAuth
	c.srv.Logger.Info("imap: login", "user", user.Username(), "ip", c.ip)
	return okCode("CAPABILITY "+c.capabilities(), "logged in")
}

// enableCondstore turns CONDSTORE on for the session. RFC 7162 3.1.2.1:
// when a "CONDSTORE enabling command" does that while a mailbox is
// selected, the client is told the mailbox's HIGHESTMODSEQ.
func (c *conn) enableCondstore() {
	if c.condstore {
		return
	}
	c.condstore = true
	if c.sel != nil {
		c.printf("* OK [HIGHESTMODSEQ %d] the highest modseq", c.sel.modseq)
	}
}

// cmdEnable is RFC 5161.
func (c *conn) cmdEnable(tag string, p *parser) (reply, error) {
	var enabled []string
	for p.peek() == ' ' {
		p.pos++
		name, err := p.atom()
		if err != nil {
			return reply{}, err
		}
		// RFC 5161 3.1: unknown names are ignored.
		switch strings.ToUpper(name) {
		case "CONDSTORE":
			c.condstore = true
			enabled = append(enabled, "CONDSTORE")
		case "IMAP4REV2":
			// RFC 9051 6.3.1 and appendix A: from here on the session
			// follows IMAP4rev2, mailbox names in UTF-8 included.
			c.rev2, c.utf8 = true, true
			enabled = append(enabled, "IMAP4rev2")
		case "UTF8=ACCEPT":
			c.utf8 = true
			enabled = append(enabled, "UTF8=ACCEPT")
		}
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	if c.state == stateSelected {
		return reply{"BAD", "", "ENABLE before a mailbox is selected"}, nil
	}
	if len(enabled) == 0 {
		c.printf("* ENABLED")
	} else {
		c.printf("* ENABLED %s", strings.Join(enabled, " "))
	}
	return ok("ENABLE completed"), nil
}

// cmdIdle is RFC 2177: the client waits, the server says what happens.
func (c *conn) cmdIdle(tag string, p *parser) (reply, error) {
	if err := p.end(); err != nil {
		return reply{}, err
	}
	c.printf("+ idling")
	if c.sel != nil {
		c.sel.dirty = true
		if err := c.poll(true); err != nil {
			return reply{}, err
		}
	}
	if c.flush() != nil {
		c.state = stateLogout
		return reply{}, nil
	}
	// One line is expected from the client: DONE. It is read on the
	// side, so that this side can talk meanwhile.
	type lineResult struct {
		line string
		err  error
	}
	got := make(chan lineResult, 1)
	go func() {
		line, err := readLine(c.br, maxLine)
		got <- lineResult{line, err}
	}()
	for {
		select {
		case r := <-got:
			if r.err != nil {
				c.readFailed(nil, 0, r.err)
				c.state = stateLogout
				return reply{}, nil
			}
			if !strings.EqualFold(r.line, "DONE") {
				return reply{"BAD", "", "IDLE ends with DONE"}, nil
			}
			return ok("IDLE completed"), nil
		case <-c.changes:
			if c.sel == nil {
				continue
			}
			c.sel.dirty = true
			if err := c.poll(true); err != nil {
				// The mailbox is gone or the store is away: say so and
				// let the client out of IDLE by its own DONE.
				c.srv.Logger.Warn("imap: reading the mailbox during IDLE failed", "user", c.username(), "err", err)
				continue
			}
			if c.flush() != nil {
				c.nc.Close()
				<-got
				c.state = stateLogout
				return reply{}, nil
			}
		case <-c.srv.done:
			c.wake()
			<-got
			c.printf("* BYE the server is shutting down")
			c.state = stateLogout
			return reply{}, nil
		case <-c.revoked:
			c.wake()
			<-got
			c.printf(byeRevoked)
			c.state = stateLogout
			return reply{}, nil
		}
	}
}
