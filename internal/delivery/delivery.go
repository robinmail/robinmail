// Package delivery takes mail accepted by the SMTP server, has Rspamd scan
// it, and stores it in the recipients' mailboxes.
package delivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/rspamd"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
)

// Store is the part of the mail store delivery needs. *store.Store has it.
type Store interface {
	// Account returns store.ErrNoAccount for an unknown address.
	Account(ctx context.Context, address string) (store.Account, error)
	Deliver(ctx context.Context, account string, m store.Message) (store.Email, error)
	// Domains lists the domains we accept mail for, sorted.
	Domains(ctx context.Context) ([]string, error)
}

// Deliverer implements smtp.Handler for inbound mail on port 25.
type Deliverer struct {
	Hostname string
	Store    Store
	Rspamd   *rspamd.Client // nil delivers without scanning
	// FailOpen delivers unscanned mail when Rspamd cannot be reached.
	// Otherwise the sender gets a 451 and retries later.
	FailOpen bool
	Logger   *slog.Logger
	// Submissions, when set, is told about delivery status notifications
	// that arrive for mail an account sent (see noteDSN).
	Submissions DSNStore
	Resolver    Resolver         // nil uses net.DefaultResolver
	Now         func() time.Time // for tests
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// acceptedDomains reads the domain table. Nothing is cached: every node
// asks rqlite each time, so a domain added or removed on any node counts
// for the next RCPT everywhere. It is one small read next to the account
// lookup every RCPT does anyway. When rqlite does not answer, the mail is
// deferred, as it would be at the account lookup a line later.
func (d *Deliverer) acceptedDomains(ctx context.Context) ([]string, error) {
	return d.Store.Domains(ctx)
}

// Resolver is the part of *net.Resolver used for reverse DNS.
type Resolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// reverseDNS returns the PTR name of ip, but only when that name resolves
// back to ip (forward-confirmed). Anyone can publish any PTR name for their
// own addresses, so an unconfirmed one is worth nothing. Empty if unknown.
func (d *Deliverer) reverseDNS(ctx context.Context, ip net.IP) string {
	if ip == nil {
		return ""
	}
	r := d.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	names, err := r.LookupAddr(ctx, ip.String())
	if err != nil {
		return ""
	}
	for i, name := range names {
		if i == 3 {
			break
		}
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		if !address.ValidDomain(name) {
			continue
		}
		addrs, err := r.LookupIPAddr(ctx, name)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if a.IP.Equal(ip) {
				return name
			}
		}
	}
	return ""
}

var (
	errNoUser   = &smtp.Error{Code: 550, Enhanced: "5.1.1", Msg: "No such user"}
	errNoRelay  = &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: "Relaying denied"}
	errTempFail = &smtp.Error{Code: 451, Enhanced: "4.3.0", Msg: "Temporary failure, try again later"}
)

func (d *Deliverer) log() *slog.Logger {
	if d.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Logger
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// resolve maps a recipient to an account. "user+tag@domain" reaches "user"
// when no account with the full name exists. <Postmaster> without a domain
// (RFC 5321 4.1.1.3) reaches the postmaster of the first accepted domain, by
// name, that has one.
func (d *Deliverer) resolve(ctx context.Context, to string) (store.Account, error) {
	var none store.Account
	domains, err := d.acceptedDomains(ctx)
	if err != nil {
		d.log().Error("domain lookup failed", "err", err)
		return none, errTempFail
	}
	var candidates []address.Address
	if strings.EqualFold(to, "postmaster") {
		for _, dom := range domains {
			candidates = append(candidates, address.Address{Local: "postmaster", Domain: dom})
		}
	} else {
		a, err := address.Parse(to)
		if err != nil {
			if at := strings.LastIndexByte(to, '@'); at >= 0 && !has(domains, strings.ToLower(to[at+1:])) {
				return none, errNoRelay
			}
			return none, errNoUser
		}
		if !has(domains, a.Domain) {
			return none, errNoRelay
		}
		candidates = []address.Address{a}
		if base, _, ok := strings.Cut(a.Local, "+"); ok && base != "" {
			candidates = append(candidates, address.Address{Local: base, Domain: a.Domain})
		}
	}
	for _, c := range candidates {
		acct, err := d.Store.Account(ctx, c.String())
		if err == nil {
			return acct, nil
		}
		if !errors.Is(err, store.ErrNoAccount) {
			d.log().Error("account lookup failed", "err", err)
			return none, errTempFail
		}
	}
	return none, errNoUser
}

// Rcpt accepts only existing users in the accepted domains, so robinmail
// never relays for unauthenticated senders.
func (d *Deliverer) Rcpt(ctx context.Context, _ *smtp.Envelope, to string) error {
	_, err := d.resolve(ctx, to)
	return err
}

// deliveryKey identifies a message for idempotent delivery: the envelope
// sender and the bytes as the sender gave them, before our own headers. A
// sender that retries sends the same bytes, so an account that already has
// the message does not get it twice. Our queue id cannot do this: a retry
// gets a new one.
func deliveryKey(from string, msg []byte) string {
	h := sha256.New()
	h.Write([]byte(from))
	h.Write([]byte{0})
	h.Write(msg)
	return hex.EncodeToString(h.Sum(nil))
}

// Deliver scans the message and stores one copy per account.
func (d *Deliverer) Deliver(ctx context.Context, env *smtp.Envelope, msg []byte) error {
	now := d.now
	qid := queueID()
	log := d.log().With("qid", qid, "ip", env.RemoteIP.String(), "from", clean(env.From))

	var accounts []store.Account
	seen := map[string]bool{}
	for _, r := range env.Rcpts {
		a, err := d.resolve(ctx, r)
		if err != nil {
			return err // the user was removed after RCPT
		}
		if !seen[a.ID] {
			seen[a.ID] = true
			accounts = append(accounts, a)
		}
	}
	key := deliveryKey(env.From, msg)

	fields, rest := splitHeader(msg)
	var added []field
	role := store.RoleInbox
	rdns := d.reverseDNS(ctx, env.RemoteIP)

	if d.Rspamd != nil {
		res, err := d.Rspamd.Check(ctx, rspamd.Request{
			From: env.From, Rcpts: env.Rcpts, IP: env.RemoteIP, Helo: env.Helo, Hostname: rdns, QueueID: qid,
		}, msg)
		switch {
		case err != nil && !d.FailOpen:
			log.Error("rspamd failed, deferring", "err", err)
			return &smtp.Error{Code: 451, Enhanced: "4.7.0", Msg: "Message could not be scanned, try again later"}
		case err != nil:
			log.Warn("rspamd failed, delivering unscanned", "err", err)
		default:
			log = log.With("action", res.Action, "score", res.Score)
			if e := verdict(res); e != nil {
				log.Info("rejected", "code", e.Code, "symbols", strings.Join(res.Symbols, ","))
				return e
			}
			for name, n := range res.RemoveHeaders {
				fields = removeHeader(fields, name, n)
			}
			if res.Action == rspamd.RewriteSubject && res.Subject != "" {
				if f, ok := newField("Subject", res.Subject); ok {
					fields = setHeader(fields, f)
				}
			}
			for _, h := range res.AddHeaders {
				if f, ok := newField(h.Name, h.Value); ok {
					added = append(added, f)
				}
			}
			if res.Action == rspamd.AddHeader || res.Action == rspamd.RewriteSubject {
				role = store.RoleJunk
				if f, ok := newField(res.SpamHeader, "Yes"); ok {
					added = append(added, f)
				}
			}
		}
	}

	// RFC 5321 4.4: final delivery puts a Return-Path line with the
	// reverse-path at the very top, then our Received line. A Return-Path
	// the message came with is removed, so exactly one is left. Everything
	// else, the Received lines of earlier hops first of all, stays as it
	// is and in its order.
	fields = removeHeader(fields, "Return-Path", 0)
	top := []field{
		{name: "Return-Path", raw: []byte("Return-Path: <" + oneLine(env.From) + ">\r\n")},
		{name: "Received", raw: received(d.Hostname, qid, env, rdns, accounts, now())},
	}
	out := joinHeader(append(append(top, added...), fields...), rest)

	for _, a := range accounts {
		// The store falls back to the inbox when the account has no mailbox
		// with the role.
		e, err := d.Store.Deliver(ctx, a.ID, store.Message{Raw: out, Key: key, Role: role, ReceivedAt: now()})
		if errors.Is(err, store.ErrDuplicate) {
			// A retry of a delivery that failed for another recipient.
			log.Info("already delivered", "to", a.Address)
			continue
		}
		if err != nil {
			// The sender retries the whole message. Accounts that have it
			// already recognise the delivery key and store nothing.
			log.Error("store failed", "to", a.Address, "err", err)
			return errTempFail
		}
		log.Info("delivered", "to", a.Address, "email", store.EmailID(e.ID), "role", role, "size", len(out))
	}
	d.noteDSN(ctx, env.From, accounts, out)
	return nil
}

// verdict turns a reject, soft reject or greylist into an SMTP error.
//
// Rspamd returns the greylist action for any score above the greylist
// threshold, also when its greylist module is off (it needs Redis). Without
// the module nothing ever lets a retry through, so robinmail defers only when the
// module actually greylisted the message, and delivers otherwise.
func verdict(res *rspamd.Result) *smtp.Error {
	msg := clean(res.SMTPMessage)
	switch res.Action {
	case rspamd.Reject:
		if msg == "" {
			msg = "Message rejected as spam"
		}
		return &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: msg}
	case rspamd.Greylist:
		if !res.Has("GREYLIST") {
			return nil
		}
		fallthrough
	case rspamd.SoftReject:
		if msg == "" {
			msg = "Try again later"
		}
		return &smtp.Error{Code: 451, Enhanced: "4.7.1", Msg: msg}
	}
	return nil
}

// received builds the time stamp line of RFC 5321 4.4:
//
//	Received: from <EHLO name> (<PTR name> <address literal>)
//		by <our name> (robinmail) with <protocol> id <queue id>
//		(<TLS version and cipher>)
//		for <recipient>;
//		<date>
//
// The FROM clause has the name the client gave in EHLO and, as TCP-info,
// the address of the connection as an address literal, preceded by its
// PTR name when that was confirmed. A client can say anything in EHLO; a
// name that is not a Domain or address literal would break the line's
// syntax, so the address literal stands in for it. The comments are CFWS,
// which the grammar allows between the clauses. FOR appears only with a
// single recipient: "it MUST contain exactly one <path>", and with more
// than one it would tell each about the others.
func received(host, qid string, env *smtp.Envelope, rdns string, to []store.Account, t time.Time) []byte {
	// RFC 3848 and RFC 6531 4 register the WITH protocol names.
	proto := "ESMTP"
	if env.UTF8 {
		proto = "UTF8SMTP"
	}
	if env.TLS != nil {
		proto += "S"
	}
	literal := addressLiteral(env.RemoteIP)
	from := env.Helo
	if !smtp.ValidHelo(from) {
		from = literal
	}
	if from == "" {
		from = "unknown" // no EHLO name and no address: not over TCP
	}
	var b strings.Builder
	b.WriteString("Received: from " + from)
	switch {
	case literal != "" && rdns != "":
		b.WriteString(" (" + rdns + " " + literal + ")")
	case literal != "":
		b.WriteString(" (" + literal + ")")
	}
	fmt.Fprintf(&b, "\r\n\tby %s (robinmail) with %s id %s", host, proto, qid)
	if env.TLS != nil {
		fmt.Fprintf(&b, "\r\n\t(%s %s)", tls.VersionName(env.TLS.Version), tls.CipherSuiteName(env.TLS.CipherSuite))
	}
	if len(to) == 1 {
		fmt.Fprintf(&b, "\r\n\tfor <%s>", to[0].Address)
	}
	// "SHOULD use explicit offsets in the dates", and local time.
	fmt.Fprintf(&b, ";\r\n\t%s\r\n", t.Format(time.RFC1123Z))
	return []byte(b.String())
}

// addressLiteral formats an IP address as RFC 5321 4.1.3 wants it in a
// Received line: "[192.0.2.1]" or "[IPv6:2001:db8::1]". Empty for no address.
func addressLiteral(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return "[" + v4.String() + "]"
	}
	if len(ip) == net.IPv6len {
		return "[IPv6:" + ip.String() + "]"
	}
	return ""
}

func queueID() string {
	var r [8]byte
	rand.Read(r[:])
	return strings.ToUpper(hex.EncodeToString(r[:]))
}

// oneLine drops control characters. The SMTP server never lets a
// reverse-path with one through; this keeps any other caller from writing
// header lines of its own.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// clean strips control characters from untrusted text for logs and replies.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.ToValidUTF8(s, "")
}
