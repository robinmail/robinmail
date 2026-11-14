package smtp

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"
)

// Reply is what a server answered to a command: one code, one or more
// lines of text (RFC 5321 4.2).
type Reply struct {
	Code int
	// Enhanced is the enhanced status code of RFC 2034 at the start of the
	// text, such as "5.1.1", or empty.
	Enhanced string
	// Lines is the text of every line, without code and separator.
	Lines []string
	// Local is true for a reply this client made up because it could not
	// send at all: the message is too big for the server, or needs an
	// extension the server does not offer.
	Local bool
}

// Positive reports a 2yz reply.
func (r Reply) Positive() bool { return r.Code/100 == 2 }

// Temporary reports a 4yz reply: try again later.
func (r Reply) Temporary() bool { return r.Code/100 == 4 }

// Permanent reports a 5yz reply.
func (r Reply) Permanent() bool { return r.Code/100 == 5 }

// String is the reply on one line, as it would stand in a report.
func (r Reply) String() string {
	return strconv.Itoa(r.Code) + " " + strings.Join(r.Lines, " ")
}

func (r Reply) Error() string { return "smtp: " + r.String() }

func localReply(code int, enhanced, text string) Reply {
	return Reply{Code: code, Enhanced: enhanced, Lines: []string{enhanced + " " + text}, Local: true}
}

// Timeouts are how long a client waits, per command. RFC 5321 4.5.3.2 gives
// minimums, which RFCTimeouts are.
type Timeouts struct {
	Greeting  time.Duration // for the 220 after connecting
	Mail      time.Duration
	Rcpt      time.Duration
	DataInit  time.Duration // for the 354 after DATA
	DataBlock time.Duration // for each write of a piece of the message
	DataDone  time.Duration // for the reply after the final dot
	Other     time.Duration // EHLO, STARTTLS, RSET, QUIT
}

// RFCTimeouts are the values of RFC 5321 4.5.3.2.1 to 4.5.3.2.6.
var RFCTimeouts = Timeouts{
	Greeting:  5 * time.Minute,
	Mail:      5 * time.Minute,
	Rcpt:      5 * time.Minute,
	DataInit:  2 * time.Minute,
	DataBlock: 3 * time.Minute,
	DataDone:  10 * time.Minute,
	Other:     5 * time.Minute,
}

func (t Timeouts) withDefaults() Timeouts {
	d := RFCTimeouts
	for _, f := range []struct{ v, def *time.Duration }{{&t.Greeting, &d.Greeting}, {&t.Mail, &d.Mail}, {&t.Rcpt, &d.Rcpt},
		{&t.DataInit, &d.DataInit}, {&t.DataBlock, &d.DataBlock}, {&t.DataDone, &d.DataDone}, {&t.Other, &d.Other}} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	return t
}

// Limits on what a server may send us.
const (
	maxReplyLines   = 100
	maxReplyLineLen = 4096 // RFC 5321 4.5.3.1.5 says 512; servers send more
	maxRcptsPerMail = 100  // RFC 5321 4.5.3.1.8: what every server accepts
)

// ErrProtocol is returned when the server sends something that is not SMTP.
var ErrProtocol = errors.New("smtp: malformed reply")

// Client is the sending side of one SMTP connection: EHLO, STARTTLS and
// any number of mail transactions. It is not safe for concurrent use.
type Client struct {
	// LocalName is what the client calls itself in EHLO.
	LocalName string
	Timeouts  Timeouts

	conn   net.Conn
	r      *bufio.Reader
	ext    map[string]string // EHLO keywords in upper case -> parameters
	esmtp  bool
	tls    *tls.ConnectionState
	broken bool
	// Sent counts the transactions done on the connection.
	Sent int
}

// NewClient takes a connection that was just made and reads the greeting.
// A server that does not want us answers 554 (RFC 5321 3.1); that reply, or
// any other than 220, comes back as the error.
func NewClient(conn net.Conn, localName string, t Timeouts) (*Client, error) {
	c := &Client{LocalName: localName, Timeouts: t.withDefaults(), conn: conn, r: bufio.NewReaderSize(conn, maxReplyLineLen)}
	rep, err := c.read(c.Timeouts.Greeting)
	if err != nil {
		c.Close()
		return nil, err
	}
	if rep.Code != 220 {
		c.Quit()
		return nil, rep
	}
	return c, nil
}

// Broken reports that the connection cannot be used for another
// transaction: an error happened on it, or the server said 421.
func (c *Client) Broken() bool { return c.broken }

// TLS is the state of the TLS session, nil without one.
func (c *Client) TLS() *tls.ConnectionState { return c.tls }

// Extension reports whether the server offered an extension in its EHLO
// reply, and the parameters it gave with it.
func (c *Client) Extension(name string) (string, bool) {
	p, ok := c.ext[strings.ToUpper(name)]
	return p, ok
}

// read reads one reply. Every line has the same code; all but the last
// have a hyphen after it (RFC 5321 4.2.1).
func (c *Client) read(timeout time.Duration) (Reply, error) {
	c.conn.SetReadDeadline(time.Now().Add(timeout))
	var rep Reply
	for n := 0; ; n++ {
		line, err := c.r.ReadSlice('\n')
		if err != nil {
			c.broken = true
			if err == bufio.ErrBufferFull {
				return Reply{}, ErrProtocol
			}
			return Reply{}, fmt.Errorf("smtp: reading the reply: %w", err)
		}
		s := strings.TrimRight(string(line), "\r\n")
		if n >= maxReplyLines || len(s) < 3 {
			c.broken = true
			return Reply{}, ErrProtocol
		}
		code, err := strconv.Atoi(s[:3])
		if err != nil || code < 200 || code > 599 || (n > 0 && code != rep.Code) || strings.ContainsAny(s[:3], "+- ") {
			c.broken = true
			return Reply{}, ErrProtocol
		}
		rep.Code = code
		last := true
		text := ""
		if len(s) > 3 {
			switch s[3] {
			case '-':
				last = false
			case ' ':
			default:
				c.broken = true
				return Reply{}, ErrProtocol
			}
			text = s[4:]
		}
		rep.Lines = append(rep.Lines, strings.ToValidUTF8(text, "?"))
		if last {
			break
		}
	}
	rep.Enhanced = enhancedCode(rep.Code, rep.Lines[0])
	if rep.Code == 421 {
		// "The service is closing the transmission channel" (4.2.2).
		c.broken = true
	}
	return rep, nil
}

// enhancedCode returns the status code at the start of a reply text if it
// is one and its class matches the reply code (RFC 2034 4).
func enhancedCode(code int, text string) string {
	first, _, _ := strings.Cut(text, " ")
	parts := strings.Split(first, ".")
	if len(parts) != 3 || parts[0] != strconv.Itoa(code/100) {
		return ""
	}
	for _, p := range parts[1:] {
		if len(p) < 1 || len(p) > 3 {
			return ""
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return ""
			}
		}
	}
	return first
}

func (c *Client) write(data []byte, timeout time.Duration) error {
	c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := c.conn.Write(data); err != nil {
		c.broken = true
		return fmt.Errorf("smtp: writing: %w", err)
	}
	return nil
}

func (c *Client) cmd(line string, timeout time.Duration) (Reply, error) {
	if err := c.write([]byte(line+"\r\n"), timeout); err != nil {
		return Reply{}, err
	}
	return c.read(timeout)
}

// Hello sends EHLO and records the extensions. A server that does not know
// EHLO gets HELO (RFC 5321 3.2), and then has no extensions.
func (c *Client) Hello() error {
	rep, err := c.cmd("EHLO "+c.LocalName, c.Timeouts.Other)
	if err != nil {
		return err
	}
	c.ext, c.esmtp = map[string]string{}, false
	if rep.Positive() {
		c.esmtp = true
		// The first line is the greeting; each other line is a keyword
		// and its parameters (4.1.1.1).
		for _, l := range rep.Lines[1:] {
			kw, params, _ := strings.Cut(l, " ")
			if kw != "" {
				c.ext[strings.ToUpper(kw)] = strings.TrimSpace(params)
			}
		}
		return nil
	}
	if rep.Code == 421 || rep.Temporary() {
		return rep
	}
	rep, err = c.cmd("HELO "+c.LocalName, c.Timeouts.Other)
	if err != nil {
		return err
	}
	if !rep.Positive() {
		return rep
	}
	return nil
}

// StartTLS upgrades the connection (RFC 3207) and says EHLO again, as the
// extensions of before must be forgotten. cfg decides whether the
// certificate is checked. After an error the connection is closed.
func (c *Client) StartTLS(cfg *tls.Config) error {
	rep, err := c.cmd("STARTTLS", c.Timeouts.Other)
	if err != nil {
		c.Close()
		return err
	}
	if rep.Code != 220 {
		c.Close()
		return rep
	}
	// Anything the server sent after its 220 came in plaintext and is not
	// part of the TLS session: refuse it (the client side of CVE-2011-0411).
	if c.r.Buffered() > 0 {
		c.Close()
		return ErrProtocol
	}
	tc := tls.Client(c.conn, cfg)
	tc.SetDeadline(time.Now().Add(time.Minute))
	if err := tc.Handshake(); err != nil {
		c.Close()
		return fmt.Errorf("smtp: TLS handshake: %w", err)
	}
	tc.SetDeadline(time.Time{})
	st := tc.ConnectionState()
	c.conn, c.r, c.tls = tc, bufio.NewReaderSize(tc, maxReplyLineLen), &st
	if err := c.Hello(); err != nil {
		c.Close()
		return err
	}
	return nil
}

// RcptResult is what became of one recipient of a transaction.
type RcptResult struct {
	Addr string
	// Reply is the reply that decided it: the one to RCPT if the server
	// refused the recipient, the one to MAIL or DATA if it refused the
	// transaction, otherwise the one after the message.
	Reply Reply
}

// Delivered reports that the server took responsibility for the message
// for this recipient.
func (r RcptResult) Delivered() bool { return r.Reply.Positive() }

func isASCII(s string) bool { return ascii(s) }

// asciiDomain returns the address with a domain in ASCII: an
// internationalized domain is written as A-labels, which needs no
// extension (RFC 6531 3.2 allows that for the domain, never for the
// local part).
func asciiDomain(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || isASCII(addr[at+1:]) {
		return addr
	}
	d, err := idna.Lookup.ToASCII(addr[at+1:])
	if err != nil {
		return addr
	}
	return addr[:at+1] + d
}

// headerEnd returns the length of the header section of msg.
func headerEnd(msg []byte) int {
	if i := bytes.Index(msg, []byte("\r\n\r\n")); i >= 0 {
		return i
	}
	return len(msg)
}

func has8bit(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return false
}

// Send runs one mail transaction: MAIL, RCPT for every recipient, DATA and
// the message. msg has CRLF line ends. The result has one entry per
// recipient, in order. An error means the connection failed before every
// recipient had an answer; the recipients are then all undecided.
//
// Extensions:
//   - PIPELINING (RFC 2920): MAIL, the RCPTs and DATA go out together.
//   - SIZE (RFC 1870): the size is declared, and a message larger than the
//     server's limit is not offered at all.
//   - 8BITMIME (RFC 6152): a message with 8-bit content is declared as
//     such. To a server without the extension it is not sent: the RFC
//     wants it converted or returned, and we do not convert.
//   - SMTPUTF8 (RFC 6531): asked for when an address has a non-ASCII local
//     part or the header has non-ASCII text. A non-ASCII domain alone is
//     sent as A-labels instead. To a server without the extension such a
//     message cannot go (3.2): a recipient with a non-ASCII local part
//     fails alone, a sender or header that needs it fails everyone.
func (c *Client) Send(from string, rcpts []string, msg []byte) ([]RcptResult, error) {
	if c.broken {
		return nil, errors.New("smtp: the connection is not usable")
	}
	results := make([]RcptResult, len(rcpts))
	for i, r := range rcpts {
		results[i].Addr = r
	}
	all := func(rep Reply) ([]RcptResult, error) {
		for i := range results {
			results[i].Reply = rep
		}
		return results, nil
	}
	if len(rcpts) == 0 {
		return results, nil
	}

	from = asciiDomain(from)
	to := make([]string, len(rcpts))
	for i, r := range rcpts {
		to[i] = asciiDomain(r)
	}
	_, utf8ok := c.Extension("SMTPUTF8")
	_, eightOK := c.Extension("8BITMIME")
	needUTF8 := !isASCII(from) || has8bit(msg[:headerEnd(msg)])
	if needUTF8 && !utf8ok {
		return all(localReply(550, "5.6.7", "The message needs SMTPUTF8, which the server does not offer"))
	}
	eight := has8bit(msg)
	if eight && !eightOK {
		return all(localReply(554, "5.6.3", "The message has 8-bit content and the server does not offer 8BITMIME"))
	}
	if limit, ok := c.Extension("SIZE"); ok {
		if n, valid := sizeValue(strings.TrimSpace(limit)); valid && n > 0 && uint64(len(msg)) > n {
			return all(localReply(552, "5.3.4", "The message is larger than the server accepts"))
		}
	}

	// The recipients that can be offered to this server.
	var offered []int
	for i, r := range to {
		if !isASCII(r) {
			if !utf8ok {
				results[i].Reply = localReply(550, "5.6.7", "The address needs SMTPUTF8, which the server does not offer")
				continue
			}
			needUTF8 = true
		}
		if len(offered) == maxRcptsPerMail {
			// The rest waits for the next attempt.
			results[i].Reply = localReply(452, "4.5.3", "Too many recipients for one transaction")
			continue
		}
		offered = append(offered, i)
	}
	if len(offered) == 0 {
		return results, nil
	}

	mail := "MAIL FROM:<" + from + ">"
	if _, ok := c.Extension("SIZE"); ok {
		mail += " SIZE=" + strconv.Itoa(len(msg))
	}
	if eight {
		mail += " BODY=8BITMIME"
	}
	if needUTF8 {
		mail += " SMTPUTF8"
	}

	_, pipelining := c.Extension("PIPELINING")
	var mailRep, dataRep Reply
	accepted := 0
	var err error
	c.Sent++
	if pipelining {
		var b bytes.Buffer
		b.WriteString(mail + "\r\n")
		for _, i := range offered {
			b.WriteString("RCPT TO:<" + to[i] + ">\r\n")
		}
		b.WriteString("DATA\r\n")
		if err = c.write(b.Bytes(), c.Timeouts.Mail); err != nil {
			return nil, err
		}
		if mailRep, err = c.read(c.Timeouts.Mail); err != nil {
			return nil, err
		}
		for _, i := range offered {
			if results[i].Reply, err = c.read(c.Timeouts.Rcpt); err != nil {
				return nil, err
			}
			if results[i].Reply.Positive() && mailRep.Positive() {
				accepted++
			}
		}
		if dataRep, err = c.read(c.Timeouts.DataInit); err != nil {
			return nil, err
		}
	} else {
		if mailRep, err = c.cmd(mail, c.Timeouts.Mail); err != nil {
			return nil, err
		}
		if mailRep.Positive() {
			for _, i := range offered {
				if results[i].Reply, err = c.cmd("RCPT TO:<"+to[i]+">", c.Timeouts.Rcpt); err != nil {
					return nil, err
				}
				if results[i].Reply.Positive() {
					accepted++
				}
			}
		}
		if accepted > 0 {
			if dataRep, err = c.cmd("DATA", c.Timeouts.DataInit); err != nil {
				return nil, err
			}
		}
	}

	if !mailRep.Positive() {
		for _, i := range offered {
			results[i].Reply = mailRep
		}
	}
	decide := func(rep Reply) {
		for _, i := range offered {
			if mailRep.Positive() && results[i].Reply.Positive() {
				results[i].Reply = rep
			}
		}
	}
	switch {
	case dataRep.Code == 354 && accepted == 0:
		// A pipelined DATA was accepted although no recipient was: RFC
		// 2920 3.1 has the client send an empty message.
		if err := c.write([]byte(".\r\n"), c.Timeouts.DataBlock); err != nil {
			return nil, err
		}
		if _, err := c.read(c.Timeouts.DataDone); err != nil {
			return nil, err
		}
	case dataRep.Code == 354:
		if err := c.writeData(msg); err != nil {
			return nil, err
		}
		rep, err := c.read(c.Timeouts.DataDone)
		if err != nil {
			return nil, err
		}
		decide(rep)
	case dataRep.Code != 0:
		decide(dataRep)
	case accepted == 0 && !c.broken:
		// Nothing was started that needs ending, but the server is in a
		// transaction after a good MAIL.
		if mailRep.Positive() {
			c.Reset()
		}
	}
	return results, nil
}

// writeData sends the message with dot-stuffing (RFC 5321 4.5.2) and the
// final ".", in pieces that each have the DATA block timeout.
func (c *Client) writeData(msg []byte) error {
	var b bytes.Buffer
	b.Grow(len(msg) + len(msg)/50 + 8)
	for len(msg) > 0 {
		end := bytes.IndexByte(msg, '\n')
		var line []byte
		if end < 0 {
			line, msg = msg, nil
		} else {
			line, msg = msg[:end+1], msg[end+1:]
		}
		if line[0] == '.' {
			b.WriteByte('.')
		}
		b.Write(line)
	}
	if b.Len() > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\r\n")) {
		b.WriteString("\r\n")
	}
	b.WriteString(".\r\n")
	data := b.Bytes()
	const block = 64 << 10
	for len(data) > 0 {
		n := min(len(data), block)
		if err := c.write(data[:n], c.Timeouts.DataBlock); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// Reset ends a transaction that was started, so the connection can carry
// another one.
func (c *Client) Reset() error {
	rep, err := c.cmd("RSET", c.Timeouts.Other)
	if err != nil {
		return err
	}
	if !rep.Positive() {
		c.broken = true
		return rep
	}
	return nil
}

// Quit says QUIT, waits a moment for the 221 and closes the connection.
func (c *Client) Quit() error {
	if !c.broken {
		c.conn.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.conn.Write([]byte("QUIT\r\n")); err == nil {
			c.r.ReadSlice('\n')
		}
	}
	return c.Close()
}

// Close closes the connection without a word.
func (c *Client) Close() error {
	c.broken = true
	return c.conn.Close()
}
