package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/address"
	"github.com/robinmail/robinmail/internal/message"
	"github.com/robinmail/robinmail/internal/rspamd"
	"github.com/robinmail/robinmail/internal/smtp"
	"github.com/robinmail/robinmail/internal/store"
)

// SubmitStore is the part of the store a Submitter needs. *store.Store
// has it.
type SubmitStore interface {
	Account(ctx context.Context, address string) (store.Account, error)
	Submit(ctx context.Context, account string, n store.NewSubmission) (store.Submission, error)
}

// Submitter accepts mail from the users of this server for sending: over
// SMTP submission (Handler) and over JMAP (EmailSubmission/set). It checks
// that the sender is the user, completes the header, has Rspamd scan and
// sign the message, and puts it in the outbound queue.
type Submitter struct {
	Hostname string
	Store    SubmitStore
	// Rspamd scans the message as mail of an authenticated user, which is
	// what makes it add the DKIM signature. nil sends unsigned.
	Rspamd *rspamd.Client
	// FailOpen sends unscanned and unsigned mail when Rspamd cannot be
	// reached. Otherwise the user is told to try again.
	FailOpen        bool
	MaxMessageBytes int64 // [25 MiB]
	MaxRecipients   int   // [100]
	// MaxPerHour is how many recipients an account may send to in an hour;
	// 0 is no limit.
	MaxPerHour int
	// Hold is how long a message submitted over JMAP waits before a node
	// starts on it, so the user can still cancel it (undoStatus).
	Hold time.Duration
	// SendAs, when set, lists the other addresses an account may send as:
	// the aliases that deliver to it (store.SendAddresses).
	SendAs func(ctx context.Context, accountAddress string) ([]string, error)
	Logger *slog.Logger
	Now    func() time.Time
}

// Kinds of refusal. They match the SetErrors of RFC 8621 7.5.
const (
	RefuseMailFrom     = "forbiddenMailFrom"
	RefuseFrom         = "forbiddenFrom"
	RefuseInvalid      = "invalidEmail"
	RefuseTooLarge     = "tooLarge"
	RefuseTooMany      = "tooManyRecipients"
	RefuseNoRecipients = "noRecipients"
	RefuseRecipients   = "invalidRecipients"
	RefuseToSend       = "forbiddenToSend"
	RefuseRateLimit    = "rateLimit"
	// RefuseLater is not a refusal: the server cannot take the message
	// right now.
	RefuseLater = "tryLater"
)

// Refusal is why a message was not accepted for sending.
type Refusal struct {
	Kind string
	Msg  string
	// Recipients are the addresses that are not valid, for
	// RefuseRecipients.
	Recipients []string
}

func (r *Refusal) Error() string { return r.Kind + ": " + r.Msg }

// ProtocolAuto is the Protocol of a message the server wrote for an
// account: an automatic reply.
const ProtocolAuto = "auto"

// Outgoing is a message a user wants sent.
type Outgoing struct {
	Account store.Account
	// From is the envelope sender; empty is the null reverse-path, which a
	// user needs for a message disposition notification.
	From  string
	Rcpts []string
	Raw   []byte
	// EmailID and IdentityID are set for a JMAP submission.
	EmailID    int64
	IdentityID int64
	IfInState  int64
	// How it came in, for the Received line and for Rspamd. The address of
	// the client is given to Rspamd and written nowhere.
	Protocol string // "ESMTPSA", "UTF8SMTPSA" or "JMAP"
	IP       net.IP
	Helo     string
	TLS      *tls.ConnectionState
}

func (s *Submitter) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Submitter) log() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

// Owns reports whether the address is the account's own, with or without
// a "+tag".
func Owns(account, addr string) bool { return address.Owns(account, addr) }

// MaySendAs reports whether an account may send as an address: its own, or
// an alias that delivers to it (aliases as store.SendAddresses lists
// them), each with or without a "+tag".
func MaySendAs(account string, aliases []string, addr string) bool {
	if address.Owns(account, addr) {
		return true
	}
	for _, alias := range aliases {
		if address.Owns(alias, addr) {
			return true
		}
	}
	return false
}

// sendAs is the account's aliases. A lookup that fails counts as none: the
// user is refused an alias for a moment, never given an address.
func (s *Submitter) sendAs(ctx context.Context, account string) []string {
	if s.SendAs == nil {
		return nil
	}
	list, err := s.SendAs(ctx, account)
	if err != nil {
		s.log().Warn("alias lookup failed", "account", account, "err", err)
		return nil
	}
	return list
}

// normalizeLines makes every line of a message end in CRLF. Mail from
// SMTP has that already; a message imported over JMAP may not.
func normalizeLines(raw []byte) []byte {
	if !bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) && !bytes.ContainsRune(bytes.ReplaceAll(raw, []byte("\r\n"), nil), '\r') {
		return raw
	}
	out := make([]byte, 0, len(raw)+len(raw)/40)
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '\r':
			out = append(out, '\r', '\n')
			if i+1 < len(raw) && raw[i+1] == '\n' {
				i++
			}
		case c == '\n':
			out = append(out, '\r', '\n')
		default:
			out = append(out, c)
		}
	}
	return out
}

// submitReceived is the trace line of a submission:
//
//	Received: by <our name> (robinmail) with ESMTPSA id <queue id>
//		(<TLS version and cipher>);
//		<date>
//
// It has no FROM clause. RFC 5321 4.4 has one in its grammar, with the
// name and address of the client, but here the client is the computer of
// one of our users, and every recipient of the message would read where
// that user is. Large providers leave it out for the same reason. The WITH
// clause follows RFC 3848 (ESMTPSA: authenticated, over TLS); a message
// submitted over JMAP has none, since no registered value fits.
func submitReceived(host, qid string, o Outgoing, t time.Time) []byte {
	var b strings.Builder
	b.WriteString("Received: by " + host + " (robinmail)")
	switch o.Protocol {
	case "", "JMAP":
		b.WriteString(" (JMAP)")
	case ProtocolAuto:
		// Written by the server itself: a vacation reply.
		b.WriteString(" (automatic reply)")
	default:
		b.WriteString(" with " + o.Protocol)
	}
	b.WriteString(" id " + qid)
	if o.TLS != nil {
		fmt.Fprintf(&b, "\r\n\t(%s %s)", tls.VersionName(o.TLS.Version), tls.CipherSuiteName(o.TLS.CipherSuite))
	}
	fmt.Fprintf(&b, ";\r\n\t%s\r\n", t.Format(time.RFC1123Z))
	return []byte(b.String())
}

func hasField(fields []field, name string) bool {
	for _, f := range fields {
		if strings.EqualFold(f.name, name) {
			return true
		}
	}
	return false
}

func fieldValue(fields []field, name string) (string, bool) {
	for i := len(fields) - 1; i >= 0; i-- {
		if strings.EqualFold(fields[i].name, name) {
			return string(bytes.TrimRight(fields[i].raw[len(fields[i].name)+1:], "\r\n")), true
		}
	}
	return "", false
}

// Submit checks a message and puts it in the queue. A refusal comes back
// as a *Refusal.
func (s *Submitter) Submit(ctx context.Context, o Outgoing) (store.Submission, error) {
	var none store.Submission
	maxBytes, maxRcpts := s.MaxMessageBytes, s.MaxRecipients
	if maxBytes == 0 {
		maxBytes = 25 << 20
	}
	if maxRcpts == 0 {
		maxRcpts = 100
	}
	aliases := s.sendAs(ctx, o.Account.Address)
	if o.From != "" && !MaySendAs(o.Account.Address, aliases, o.From) {
		return none, &Refusal{Kind: RefuseMailFrom, Msg: "the envelope sender is not an address of this account"}
	}
	var rcpts, invalid []string
	seen := map[string]bool{}
	for _, r := range o.Rcpts {
		if !smtp.ValidMailbox(r) {
			invalid = append(invalid, r)
			continue
		}
		if key := strings.ToLower(r); !seen[key] {
			seen[key] = true
			rcpts = append(rcpts, r)
		}
	}
	switch {
	case len(invalid) > 0:
		return none, &Refusal{Kind: RefuseRecipients, Msg: "not every recipient is a valid address", Recipients: invalid}
	case len(rcpts) == 0:
		return none, &Refusal{Kind: RefuseNoRecipients, Msg: "the message has no recipients"}
	case len(rcpts) > maxRcpts:
		return none, &Refusal{Kind: RefuseTooMany, Msg: fmt.Sprintf("more than %d recipients", maxRcpts)}
	case int64(len(o.Raw)) > maxBytes:
		return none, &Refusal{Kind: RefuseTooLarge, Msg: fmt.Sprintf("the message is larger than %d octets", maxBytes)}
	}

	raw := normalizeLines(o.Raw)
	fields, rest := splitHeader(raw)
	// The addresses a reader sees must be the ones checked below. Two From
	// or Sender fields (RFC 5322 3.6: at most one of each) leave it to the
	// reader's program which one that is, and so does a header that ends,
	// for us, at a line another program still reads as a field, such as
	// the obsolete "From : ..." (RFC 5322 4.5.2).
	if len(rest) > 0 && !bytes.HasPrefix(rest, []byte("\r\n")) {
		return none, &Refusal{Kind: RefuseInvalid, Msg: "the header has a line that is not a header field"}
	}
	if countFields(fields, "From") > 1 || countFields(fields, "Sender") > 1 {
		return none, &Refusal{Kind: RefuseInvalid, Msg: "the message has more than one From or Sender field"}
	}
	// The From field, and Sender if there is one, name the user: RFC 6409
	// 6.1 lets a submission server enforce that, and without it any user
	// could write mail in the name of another.
	from, ok := fieldValue(fields, "From")
	authors := message.Addresses(from)
	if !ok || len(authors) == 0 {
		return none, &Refusal{Kind: RefuseInvalid, Msg: "the message has no From field with an address"}
	}
	check := authors
	if sender, ok := fieldValue(fields, "Sender"); ok {
		check = append(check, message.Addresses(sender)...)
	}
	for _, a := range check {
		if !MaySendAs(o.Account.Address, aliases, a.Email) {
			return none, &Refusal{Kind: RefuseFrom, Msg: "the From or Sender field has an address that is not of this account"}
		}
	}

	now := s.now()
	qid := queueID()
	// Bcc is for the sender's own copy: no recipient gets to see it (RFC
	// 5322 3.6.3).
	fields = removeHeader(fields, "Bcc", 0)
	fields = removeHeader(fields, "Return-Path", 0)
	// RFC 6409 8: a submission server may add what a message lacks.
	if !hasField(fields, "Date") {
		fields = append(fields, field{name: "Date", raw: []byte("Date:" + message.FormatDate(now) + "\r\n")})
	}
	if !hasField(fields, "Message-ID") {
		id := message.NewMessageID(s.Hostname)
		fields = append(fields, field{name: "Message-ID", raw: []byte("Message-ID: <" + id + ">\r\n")})
	}
	if len(rest) == 0 {
		rest = []byte("\r\n")
	}
	out := joinHeader(fields, rest)
	log := s.log().With("qid", qid, "account", o.Account.Address, "from", clean(o.From), "rcpts", len(rcpts))

	var added []field
	if s.Rspamd != nil {
		res, err := s.Rspamd.Check(ctx, rspamd.Request{
			From: o.From, Rcpts: rcpts, IP: o.IP, Helo: o.Helo, QueueID: qid, User: o.Account.Address,
		}, out)
		switch {
		case err != nil && !s.FailOpen:
			log.Error("rspamd failed, not accepting the submission", "err", err)
			return none, &Refusal{Kind: RefuseLater, Msg: "the message could not be scanned, try again later"}
		case err != nil:
			log.Warn("rspamd failed, sending unscanned and unsigned", "err", err)
		default:
			switch v := verdict(res); {
			case v != nil && v.Code >= 500:
				log.Info("submission rejected", "score", res.Score, "symbols", strings.Join(res.Symbols, ","))
				return none, &Refusal{Kind: RefuseToSend, Msg: v.Msg}
			case v != nil:
				return none, &Refusal{Kind: RefuseLater, Msg: v.Msg}
			}
			for name, n := range res.RemoveHeaders {
				fields = removeHeader(fields, name, n)
			}
			signed := false
			for _, h := range res.AddHeaders {
				if f, ok := newField(h.Name, h.Value); ok {
					added = append(added, f)
					signed = signed || strings.EqualFold(h.Name, "DKIM-Signature")
				}
			}
			// The signature goes in as Rspamd folded it. A value that is
			// not a well-formed field is left out: a broken signature is
			// worse than none.
			for _, sig := range res.DKIMSignatures {
				if message.ValidRaw(" " + sig) {
					added = append(added, field{name: "DKIM-Signature", raw: []byte("DKIM-Signature: " + sig + "\r\n")})
					signed = true
				} else {
					log.Warn("rspamd returned a DKIM signature that cannot be a header field; sending unsigned")
				}
			}
			log = log.With("score", res.Score, "dkim", signed)
		}
	}
	top := append([]field{{name: "Received", raw: submitReceived(s.Hostname, qid, o, now)}}, added...)
	out = joinHeader(append(top, fields...), rest)

	n := store.NewSubmission{EmailID: o.EmailID, IdentityID: o.IdentityID, Raw: out, MailFrom: o.From, Rcpts: rcpts,
		MaxPerHour: s.MaxPerHour, IfInState: o.IfInState}
	if o.EmailID != 0 {
		n.SendAt = now.Add(s.Hold)
	}
	sub, err := s.Store.Submit(ctx, o.Account.ID, n)
	switch {
	case errors.Is(err, store.ErrRateLimited):
		log.Warn("submission refused: rate limit")
		return none, &Refusal{Kind: RefuseRateLimit, Msg: fmt.Sprintf("more than %d recipients in an hour; try again later", s.MaxPerHour)}
	case err != nil:
		return none, err
	}
	log.Info("queued", "submission", store.SubmissionID(sub.ID), "size", len(out), "message_id", sub.MessageID)
	return sub, nil
}

// Handler makes a Submitter the handler and authenticator of an SMTP
// submission server (RFC 6409).
type Handler struct {
	*Submitter
	// Login checks an address and password and answers like
	// web.Server.Login: 0, or the HTTP status that says why not.
	Login func(ctx context.Context, ip, username, password string) (store.Account, int, time.Duration)
	// LoginSession, when set, is used instead of Login. For a login it
	// grants it also returns a function that starts watching it: that
	// one's channel is closed when the login is withdrawn (an app password
	// revoked, a password changed), and the SMTP session ends then.
	LoginSession func(ctx context.Context, ip, username, password string) (store.Account, int, time.Duration, func() (<-chan struct{}, func()))
}

// Authenticate implements smtp.Authenticator. The session then acts as the
// account's address.
func (h *Handler) Authenticate(ctx context.Context, ip net.IP, username, password string) (string, error) {
	user, _, stop, err := h.AuthenticateSession(ctx, ip, username, password)
	if stop != nil {
		stop()
	}
	return user, err
}

// AuthenticateSession implements smtp.RevocableAuthenticator: with
// LoginSession set, the session ends when its login is withdrawn.
func (h *Handler) AuthenticateSession(ctx context.Context, ip net.IP, username, password string) (string, <-chan struct{}, func(), error) {
	var (
		acct   store.Account
		status int
		watch  func() (<-chan struct{}, func())
	)
	if h.LoginSession != nil {
		acct, status, _, watch = h.LoginSession(ctx, ip.String(), username, password)
	} else {
		acct, status, _ = h.Login(ctx, ip.String(), username, password)
	}
	switch status {
	case 0:
		if watch != nil {
			revoked, stop := watch()
			return acct.Address, revoked, stop, nil
		}
		return acct.Address, nil, nil, nil
	case http.StatusUnauthorized:
		return "", nil, nil, smtp.ErrAuthFailed
	case http.StatusTooManyRequests:
		return "", nil, nil, &smtp.Error{Code: 454, Enhanced: "4.7.0", Msg: "Too many failed logins, try again later"}
	}
	return "", nil, nil, &smtp.Error{Code: 454, Enhanced: "4.7.0", Msg: "Temporary authentication failure"}
}

// Mail implements smtp.MailChecker: the envelope sender is the user's.
func (h *Handler) Mail(ctx context.Context, env *smtp.Envelope) error {
	if env.From != "" && !Owns(env.AuthUser, env.From) && !MaySendAs(env.AuthUser, h.sendAs(ctx, env.AuthUser), env.From) {
		return &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: "Sender address is not yours"}
	}
	return nil
}

// Rcpt accepts any address: where it goes is the queue's to find out.
func (h *Handler) Rcpt(_ context.Context, _ *smtp.Envelope, to string) error {
	if !smtp.ValidMailbox(to) {
		return &smtp.Error{Code: 501, Enhanced: "5.1.3", Msg: "Bad recipient address"}
	}
	return nil
}

// Deliver queues the message.
func (h *Handler) Deliver(ctx context.Context, env *smtp.Envelope, msg []byte) error {
	acct, err := h.Store.Account(ctx, env.AuthUser)
	if err != nil {
		h.log().Error("submission: account lookup failed", "err", err)
		return errTempFail
	}
	proto := "ESMTPSA"
	if env.UTF8 {
		proto = "UTF8SMTPSA"
	}
	_, err = h.Submit(ctx, Outgoing{Account: acct, From: env.From, Rcpts: env.Rcpts, Raw: msg,
		Protocol: proto, IP: env.RemoteIP, Helo: env.Helo, TLS: env.TLS})
	var r *Refusal
	if !errors.As(err, &r) {
		if err != nil {
			h.log().Error("submission failed", "err", err)
			return errTempFail
		}
		return nil
	}
	switch r.Kind {
	case RefuseMailFrom, RefuseFrom:
		return &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: "Sender address is not yours"}
	case RefuseInvalid:
		return &smtp.Error{Code: 550, Enhanced: "5.6.0", Msg: "The message has no From field with an address"}
	case RefuseTooLarge:
		return &smtp.Error{Code: 552, Enhanced: "5.3.4", Msg: "Message too big"}
	case RefuseTooMany:
		return &smtp.Error{Code: 452, Enhanced: "4.5.3", Msg: "Too many recipients"}
	case RefuseNoRecipients, RefuseRecipients:
		return &smtp.Error{Code: 554, Enhanced: "5.1.3", Msg: "No valid recipients"}
	case RefuseToSend:
		return &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: r.Msg}
	case RefuseRateLimit:
		return &smtp.Error{Code: 451, Enhanced: "4.7.1", Msg: "Too many recipients in the last hour, try again later"}
	}
	return &smtp.Error{Code: 451, Enhanced: "4.7.0", Msg: r.Msg}
}

// LocalDomain reports whether mail for the domain is ours to store.
func (d *Deliverer) LocalDomain(ctx context.Context, domain string) (bool, error) {
	domains, err := d.acceptedDomains(ctx)
	if err != nil {
		return false, err
	}
	return has(domains, strings.ToLower(domain)), nil
}

// Local stores a message from the outbound queue for a recipient at one of
// our own domains, without SMTP. It answers like Rcpt does: an *smtp.Error
// says why the recipient cannot have it. The message was scanned when it
// was submitted and is not scanned again. A second delivery of the same
// message to the same account stores nothing. An alias is followed like
// for mail from outside.
func (d *Deliverer) Local(ctx context.Context, from, to string, msg []byte) error {
	var routed routes
	if err := d.route(ctx, from, to, &routed); err != nil {
		return err
	}
	fields, rest := splitHeader(msg)
	if err := checkLoops(fields, routed.forwards); err != nil {
		return err
	}
	fields = removeHeader(fields, "Return-Path", 0)
	top := []field{{name: "Return-Path", raw: []byte("Return-Path: <" + oneLine(from) + ">\r\n")}}
	out := joinHeader(append(top, fields...), rest)
	key := "local:" + deliveryKey(from, msg)
	// The recipient may be an alias that leads elsewhere, or a rewritten
	// sender address that a bounce of our own queue is going back to; an
	// account's filter runs as for mail from outside.
	return d.finish(ctx, &incoming{key: key, from: from, to: routed, role: store.RoleInbox, final: out,
		relay: fields, rest: rest, log: d.log().With("from", clean(from), "local", true)})
}

// DSNStore records what delivery status notifications say about mail an
// account sent. *store.Store has it.
type DSNStore interface {
	ApplyDSN(ctx context.Context, account, messageID string, reports []store.Recipient) (bool, error)
}

// noteDSN looks whether a message that was just stored is a delivery
// status notification (RFC 3464) about a message one of the accounts
// sent, and if so records what it says in that submission. The message
// itself stays ordinary mail in the Inbox either way.
//
// Only a message with the null reverse-path counts, as RFC 3464 2 requires
// of a DSN, and only for a submission whose Message-ID the report returns.
// That is no proof of origin: someone who knows the Message-ID and a
// recipient of a sent message can make it look failed to its sender. It
// changes what the sender is shown, nothing else.
func (d *Deliverer) noteDSN(ctx context.Context, from string, accounts []store.Account, msg []byte) {
	if d.Submissions == nil || from != "" {
		return
	}
	dsn, ok := message.ParseDSN(message.Parse(msg))
	if !ok || dsn.OriginalMessageID == "" {
		return
	}
	var reports []store.Recipient
	for _, r := range dsn.Recipients {
		rep := store.Recipient{Address: r.Address, Status: r.Status, MTA: r.RemoteMTA, At: d.now().Unix()}
		if rep.MTA == "" {
			rep.MTA = dsn.ReportingMTA
		}
		switch r.Action {
		case "failed":
			rep.State = store.RcptFailed
			if !strings.HasPrefix(rep.Status, "4.") && !strings.HasPrefix(rep.Status, "5.") {
				rep.Status = "5.0.0"
			}
		case "delivered":
			rep.State = store.RcptDelivered
			if !strings.HasPrefix(rep.Status, "2.") {
				rep.Status = "2.0.0"
			}
		default:
			continue // delayed, relayed, expanded: nothing final
		}
		// The reason, as a reply. Reports differ: Postfix quotes the
		// remote server's reply, or gives a text of its own for a
		// failure of its own; Exim and OpenSMTPD give no
		// Diagnostic-Code at all for theirs. The sender should still
		// read who said so.
		text := r.Diagnostic
		if text == "" {
			text = "reported by " + rep.MTA + " without a reason"
		}
		rep.Reply = smtp.StatusReply(rep.Status, text)
		reports = append(reports, rep)
	}
	for _, a := range accounts {
		found, err := d.Submissions.ApplyDSN(ctx, a.ID, dsn.OriginalMessageID, reports)
		if err != nil {
			d.log().Warn("recording a delivery status notification failed", "account", a.Address, "err", err)
		} else if found {
			d.log().Info("delivery status notification recorded", "account", a.Address, "message_id", dsn.OriginalMessageID)
		}
	}
}

// Report queues a message the server itself sends to an address elsewhere
// in the name of one of its domains: a TLS report (RFC 8460). Rspamd signs
// it as it signs a user's mail (RFC 8460 5.3 wants reports DKIM-signed),
// then it goes into the queue under the system account of the domain. key
// makes a second call queue nothing.
func (s *Submitter) Report(ctx context.Context, fw Forwarder, key, from, rcpt string, raw []byte) error {
	at := strings.LastIndexByte(from, '@')
	if at < 0 || !smtp.ValidMailbox(rcpt) {
		return fmt.Errorf("delivery: a report needs a sender and a recipient address")
	}
	fields, rest := splitHeader(normalizeLines(raw))
	out := joinHeader(fields, rest)
	var added []field
	if s.Rspamd != nil {
		res, err := s.Rspamd.Check(ctx, rspamd.Request{From: from, Rcpts: []string{rcpt}, QueueID: queueID(), User: from}, out)
		switch {
		case err != nil && !s.FailOpen:
			return fmt.Errorf("delivery: the report could not be signed: %w", err)
		case err != nil:
			s.log().Warn("rspamd failed, sending a report unsigned", "err", err)
		default:
			for _, sig := range res.DKIMSignatures {
				if message.ValidRaw(" " + sig) {
					added = append(added, field{name: "DKIM-Signature", raw: []byte("DKIM-Signature: " + sig + "\r\n")})
				}
			}
			for _, h := range res.AddHeaders {
				if strings.EqualFold(h.Name, "DKIM-Signature") {
					if f, ok := newField(h.Name, h.Value); ok {
						added = append(added, f)
					}
				}
			}
		}
	}
	err := fw.Forward(ctx, from[at+1:], key, from, rcpt, joinHeader(append(added, fields...), rest))
	if errors.Is(err, store.ErrForwarded) {
		return nil
	}
	return err
}
