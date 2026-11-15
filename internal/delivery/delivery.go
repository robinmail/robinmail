// Package delivery takes mail accepted by the SMTP server, has Rspamd scan
// it, and stores it in the recipients' mailboxes.
package delivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"github.com/robinmail/robinmail/internal/metrics"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/address"
	"github.com/robinmail/robinmail/internal/rspamd"
	"github.com/robinmail/robinmail/internal/smtp"
	"github.com/robinmail/robinmail/internal/srs"
	"github.com/robinmail/robinmail/internal/store"
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
	// Aliases finds aliases; nil has none. Forwards queues what an alias
	// sends to addresses elsewhere, and SRS rewrites the envelope sender
	// of such mail; without them an alias can only name accounts here.
	Aliases  AliasStore
	Forwards Forwarder
	SRS      *srs.Rewriter
	// Filters runs the Sieve script of an account on its mail; nil runs
	// none. AutoReply queues an automatic reply of an account (a vacation
	// reply) to one address, with the null reverse-path; nil sends none.
	// SendAs lists the alias addresses of an account, which count as the
	// recipient's own when deciding whether a message names them.
	// Reports, when set, keeps the TLS reports (RFC 8460) that arrive as
	// mail for one of our addresses. *store.Store has it.
	Reports   ReportStore
	Filters   FilterStore
	AutoReply func(ctx context.Context, account store.Account, rcpt string, raw []byte) error
	// Calendar, when set, is given every message stored for an account that
	// was not taken for spam: an invitation or a reply in it (iMIP) is
	// applied to the account's calendar.
	Calendar func(ctx context.Context, account store.Account, raw []byte)
	SendAs   func(ctx context.Context, accountAddress string) ([]string, error)
	scripts  scriptCache
	Resolver Resolver         // nil uses net.DefaultResolver
	Now      func() time.Time // for tests
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// acceptedDomains reads the domain table. Nothing is cached: every node
// asks the database each time, so a domain added or removed on any node counts
// for the next RCPT everywhere. It is one small read next to the account
// lookup every RCPT does anyway. When the database does not answer, the mail is
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

// Rcpt accepts only addresses of the accepted domains that lead somewhere:
// to an account, or through an alias to accounts or to addresses
// elsewhere. For anything else robinmail does not relay.
//
// A recipient whose mailbox is full is refused here already (452 4.2.2):
// the sender's server then keeps the message for that recipient and still
// delivers to the others, which a refusal after DATA could not say.
func (d *Deliverer) Rcpt(ctx context.Context, env *smtp.Envelope, to string) error {
	var r routes
	if err := d.route(ctx, env.From, to, &r); err != nil {
		return err
	}
	if len(r.forwards) > 0 || len(r.accounts) == 0 {
		return nil
	}
	// An alias for several accounts is full when all of them are.
	for _, a := range r.accounts {
		if fits, _ := a.Quota.Room(1); fits {
			return nil
		}
	}
	return errMailboxFull
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

// Deliver scans the message, runs the filter of each account it is for and
// does what those decide: by default one copy per account.
func (d *Deliverer) Deliver(ctx context.Context, env *smtp.Envelope, msg []byte) error {
	now := d.now
	qid := queueID()
	log := d.log().With("qid", qid, "ip", env.RemoteIP.String(), "from", clean(env.From))

	var to routes
	for _, r := range env.Rcpts {
		if err := d.route(ctx, env.From, r, &to); err != nil {
			return err // the user or alias was removed after RCPT
		}
	}
	accounts := to.accounts
	key := deliveryKey(env.From, msg)

	fields, rest := splitHeader(msg)
	if err := checkLoops(fields, to.forwards); err != nil {
		log.Info("refused", "reason", err.Error())
		return err
	}
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
				metrics.Rejected.Inc("scan")
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
	// The trace line names the recipient only when the message goes to one
	// mailbox and nowhere else: a copy that is sent on must not tell where
	// else the alias leads.
	named := accounts
	if len(to.forwards) > 0 {
		named = nil
	}
	trace := field{name: "Received", raw: received(d.Hostname, qid, env, rdns, named, now())}
	top := []field{{name: "Return-Path", raw: []byte("Return-Path: <" + oneLine(env.From) + ">\r\n")}, trace}
	out := joinHeader(append(append(top, added...), fields...), rest)

	return d.finish(ctx, &incoming{key: key, from: env.From, to: to, role: role, final: out,
		relay: append(append([]field{trace}, added...), fields...), rest: rest, log: log})
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
