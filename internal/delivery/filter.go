package delivery

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/sieve"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/store"
	"github.com/mikepage/robinmail/internal/tlsrpt"
)

// FilterStore is what running an account's Sieve script needs.
// *store.Store has it.
type FilterStore interface {
	// ActiveSieveScript returns the hash of the account's active script
	// and a function that reads it; ok is false without one.
	ActiveSieveScript(ctx context.Context, account string) (id string, read func() ([]byte, error), ok bool, err error)
	Mailboxes(ctx context.Context, account string) ([]store.Mailbox, error)
	CreateMailbox(ctx context.Context, account string, parentID int64, name string) (store.Mailbox, error)
	// VacationOnce answers whether an automatic reply with this key may
	// go out now, and remembers a yes.
	VacationOnce(ctx context.Context, account, key string, period time.Duration) (bool, error)
	// VacationResponse is the account's automatic reply as set over JMAP
	// (RFC 8621 8).
	VacationResponse(ctx context.Context, account string) (store.VacationResponse, error)
}

// scriptCache keeps parsed scripts by the hash of their text, so a script
// is parsed once per node and not once per message. A script that changes
// has another hash.
type scriptCache struct {
	mu sync.Mutex
	m  map[string]*sieve.Script
}

const maxCachedScripts = 1000

func (c *scriptCache) get(id string) (*sieve.Script, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[id]
	return s, ok
}

func (c *scriptCache) put(id string, s *sieve.Script) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCachedScripts {
		c.m = map[string]*sieve.Script{}
	}
	c.m[id] = s
}

// plan is what happens to a message for one account.
type plan struct {
	// keep: the message goes into the default mailbox (the Inbox, or Junk
	// for what Rspamd marked as spam). fileinto: and into these.
	keep     bool
	fileinto []sieve.Action
	keywords []string
	// redirects are addresses the message is sent on to.
	redirects []string
	reject    *sieve.Action
	vacation  *sieve.VacationReply
}

// incoming is a message on its way to the mailboxes and addresses its
// recipients lead to.
type incoming struct {
	key  string // the delivery key
	from string // the envelope sender
	to   routes
	// role is the default mailbox: store.RoleInbox, or store.RoleJunk for
	// what Rspamd marked as spam.
	role string
	// final is the message as a mailbox gets it, with Return-Path;
	// relay and rest are it as it is sent on, without.
	final []byte
	relay []field
	rest  []byte
	log   *slog.Logger
}

// script returns the parsed active script of an account, or nil.
func (d *Deliverer) script(ctx context.Context, acct store.Account) (*sieve.Script, error) {
	if d.Filters == nil {
		return nil, nil
	}
	id, read, ok, err := d.Filters.ActiveSieveScript(ctx, acct.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if s, ok := d.scripts.get(id); ok {
		return s, nil
	}
	text, err := read()
	if err != nil {
		return nil, err
	}
	s, err := sieve.Parse(text)
	if err != nil {
		// It was checked when it was stored; a script that no longer
		// parses (another version of this server) must not stop the mail.
		d.log().Warn("the active sieve script does not parse; delivering without it", "account", acct.Address, "err", err)
		return nil, nil
	}
	d.scripts.put(id, s)
	return s, nil
}

// filter runs the account's script on the message and returns what it
// wants. Without a script, and when the script fails, the message is kept.
func (d *Deliverer) filter(ctx context.Context, acct store.Account, in *incoming) (plan, error) {
	script, err := d.script(ctx, acct)
	if err != nil {
		// Rather late than unfiltered: a filter may be what keeps a
		// message from being forwarded or answered.
		in.log.Error("reading the sieve script failed", "account", acct.Address, "err", err)
		return plan{}, errTempFail
	}
	if script == nil {
		p := plan{keep: true}
		d.vacationResponse(ctx, acct, in, &p)
		return p, nil
	}
	var boxes []store.Mailbox
	loaded := false
	res := script.Run(sieve.Env{
		Raw: in.final, From: in.from, To: in.to.rcpt[acct.ID], Now: d.now(),
		MailboxExists: func(name string) bool {
			if !loaded {
				boxes, _ = d.Filters.Mailboxes(ctx, acct.ID)
				loaded = true
			}
			return store.FindMailbox(boxes, name) != 0
		},
	})
	if res.Err != nil {
		in.log.Warn("the sieve script failed; keeping the message", "account", acct.Address, "err", res.Err)
	}
	var p plan
	seen := map[string]bool{}
	flags := func(list []string) {
		for _, f := range list {
			if k, ok := store.FlagKeyword(f); ok && !seen[k] {
				seen[k] = true
				p.keywords = append(p.keywords, k)
			}
		}
	}
	for i, a := range res.Actions {
		switch a.Kind {
		case sieve.Keep:
			p.keep = true
			flags(a.Flags)
		case sieve.FileInto:
			p.fileinto = append(p.fileinto, a)
			flags(a.Flags)
		case sieve.Redirect:
			p.redirects = append(p.redirects, a.Address)
		case sieve.Reject:
			p.reject = &res.Actions[i]
		case sieve.Vacation:
			p.vacation = a.Vacation
		}
	}
	d.vacationResponse(ctx, acct, in, &p)
	return p, nil
}

// mailboxes turns the mailboxes a plan names into ids. A mailbox that is
// not there is made when the script said :create; otherwise the message
// is kept in the default mailbox instead (RFC 5228 4.1 leaves the choice).
func (d *Deliverer) mailboxes(ctx context.Context, acct store.Account, p *plan, role string, log *slog.Logger) []int64 {
	if len(p.fileinto) == 0 {
		return nil
	}
	list, err := d.Filters.Mailboxes(ctx, acct.ID)
	if err != nil {
		log.Warn("listing mailboxes for fileinto failed; keeping the message", "account", acct.Address, "err", err)
		return nil
	}
	var ids []int64
	have := map[int64]bool{}
	add := func(id int64) {
		if id != 0 && !have[id] {
			have[id] = true
			ids = append(ids, id)
		}
	}
	missed := false
	for _, a := range p.fileinto {
		id := store.FindMailbox(list, a.Mailbox)
		if id == 0 && a.Create {
			id, list = d.createMailbox(ctx, acct, list, a.Mailbox, log)
		}
		if id == 0 {
			log.Info("fileinto: no such mailbox; keeping the message", "account", acct.Address, "mailbox", clean(a.Mailbox))
			missed = true
			continue
		}
		add(id)
	}
	if p.keep || missed || len(ids) == 0 {
		// The default mailbox, by its id: the message is in it and in
		// the others at once, as labels.
		def := int64(0)
		for _, m := range list {
			if m.Role == role || (def == 0 && m.Role == store.RoleInbox) {
				def = m.ID
				if m.Role == role {
					break
				}
			}
		}
		add(def)
	}
	return ids
}

// createMailbox makes a mailbox by its full name, with the levels above
// it that are missing. It returns 0 when that does not work.
func (d *Deliverer) createMailbox(ctx context.Context, acct store.Account, list []store.Mailbox, path string, log *slog.Logger) (int64, []store.Mailbox) {
	parts := strings.Split(path, "/")
	if len(parts) > 10 {
		return 0, list
	}
	parent := int64(0)
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		id := store.FindMailbox(list, prefix)
		if id == 0 {
			m, err := d.Filters.CreateMailbox(ctx, acct.ID, parent, parts[i])
			if err != nil {
				// Another delivery may have made it a moment ago.
				if again, lerr := d.Filters.Mailboxes(ctx, acct.ID); lerr == nil {
					list = again
					id = store.FindMailbox(list, prefix)
				}
				if id == 0 {
					log.Warn("fileinto :create failed", "account", acct.Address, "mailbox", clean(path), "err", err)
					return 0, list
				}
			} else {
				id = m.ID
				list = append(list, m)
				log.Info("fileinto :create made a mailbox", "account", acct.Address, "mailbox", clean(prefix))
			}
		}
		parent = id
	}
	return parent, list
}

// finish does what the routes of a message and the filters of the accounts
// they lead to decide: store, send on, turn away, answer.
func (d *Deliverer) finish(ctx context.Context, in *incoming) error {
	accounts := in.to.accounts
	plans := make([]plan, len(accounts))
	for i, a := range accounts {
		p, err := d.filter(ctx, a, in)
		if err != nil {
			return err
		}
		plans[i] = p
	}

	// A script that refuses the message. When this account is all the
	// message was for, the refusal is the answer to the sender's server,
	// which is what RFC 5429 2.1 prefers. Otherwise the others get their
	// copies and the sender a notification.
	alone := len(accounts) == 1 && len(in.to.forwards) == 0
	if alone && plans[0].reject != nil {
		reason := rejectReason(plans[0].reject.Reason)
		in.log.Info("rejected by the recipient's filter", "to", accounts[0].Address, "reason", reason)
		return &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: reason}
	}

	spam := in.role == store.RoleJunk
	forwards := in.to.forwards
	if spam && len(forwards) > 0 {
		// Mail that looks like spam is filed under Junk here and not
		// sent on: the next server would hold it against this one. When
		// forwarding is all the message was for, the sender is told.
		var keep []forward
		for _, f := range forwards {
			if f.bounce {
				keep = append(keep, f)
			}
		}
		if len(keep) == 0 && len(accounts) == 0 {
			in.log.Info("rejected: spam for an alias that only forwards")
			return errSpamFwd
		}
		in.log.Info("not forwarding spam", "forwards", len(forwards)-len(keep))
		forwards = keep
	}

	been := deliveredTo(in.relay)
	for i, a := range accounts {
		p := &plans[i]
		if len(p.redirects) > 0 {
			switch {
			case spam, d.Forwards == nil, been[a.Address]:
				// Not sent on: spam, no queue to send with, or a message
				// that this account already sent on once and that came
				// back. It is kept instead of lost.
				in.log.Info("not redirecting", "account", a.Address, "spam", spam, "loop", been[a.Address])
				p.keep = true
			default:
				for _, addr := range p.redirects {
					forwards = append(forwards, forward{to: addr, via: a.Address})
				}
			}
		}
		if p.reject != nil || !(p.keep || len(p.fileinto) > 0) {
			in.log.Info("not stored", "to", a.Address, "rejected", p.reject != nil, "redirects", len(p.redirects))
			continue
		}
		m := store.Message{Raw: in.final, Key: in.key, Role: in.role, ReceivedAt: d.now(), Keywords: p.keywords}
		m.MailboxIDs = d.mailboxes(ctx, a, p, in.role, in.log)
		e, err := d.Store.Deliver(ctx, a.ID, m)
		if errors.Is(err, store.ErrNoMailbox) && len(m.MailboxIDs) > 0 {
			// A mailbox went between the look and the write.
			m.MailboxIDs = nil
			e, err = d.Store.Deliver(ctx, a.ID, m)
		}
		if errors.Is(err, store.ErrDuplicate) {
			// A retry of a delivery that failed for another recipient.
			in.log.Info("already delivered", "to", a.Address)
			continue
		}
		if errors.Is(err, store.ErrOverQuota) {
			// The account filled up after RCPT, or the message is what
			// does not fit. The reply is for the whole message: accounts
			// that have it already recognise a retry by the delivery key.
			in.log.Info("over quota", "to", a.Address, "size", len(in.final), "used", a.Quota.UsedBytes, "limit", a.Quota.Bytes)
			if _, never := a.Quota.Room(int64(len(in.final))); never && alone {
				return errTooBigForBox
			}
			return errMailboxFull
		}
		if err != nil {
			// The sender retries the whole message. Accounts that have it
			// already recognise the delivery key and store nothing.
			in.log.Error("store failed", "to", a.Address, "err", err)
			return errTempFail
		}
		in.log.Info("delivered", "to", a.Address, "email", store.EmailID(e.ID), "role", in.role, "mailboxes", len(m.MailboxIDs),
			"keywords", strings.Join(p.keywords, " "), "size", len(in.final))
	}
	// What leaves again does so with our trace line and without a
	// Return-Path: that is for whoever delivers it finally.
	if err := d.sendOn(ctx, in.key, in.from, forwards, in.relay, in.rest); err != nil {
		return err
	}
	for i, a := range accounts {
		if p := &plans[i]; p.reject != nil {
			d.rejectNotice(ctx, a, in, p.reject.Reason)
		} else if p.vacation != nil && !spam {
			d.vacation(ctx, a, in, p.vacation)
		}
	}
	d.noteDSN(ctx, in.from, accounts, in.final)
	if len(accounts) > 0 {
		d.noteTLSReport(ctx, in)
	}
	return nil
}

// rejectReason makes the reason of a reject fit an SMTP reply: one line of
// printable text.
func rejectReason(reason string) string {
	reason = strings.Join(strings.Fields(clean(reason)), " ")
	if reason == "" {
		reason = "Message refused by the recipient"
	}
	return reason
}

// rejectNotice tells the sender that one recipient's filter refused the
// message, when the refusal could not be the SMTP reply because the
// message also went to others (RFC 5429 2.1.2: a DSN). Nothing is sent in
// answer to a notification, and a failure to queue it is logged, not
// returned: the other recipients have their copies.
func (d *Deliverer) rejectNotice(ctx context.Context, acct store.Account, in *incoming, reason string) {
	if in.from == "" || d.Forwards == nil {
		return
	}
	now := d.now()
	name := "Mail Delivery System"
	dsn := message.BuildDSN(message.DSNMessage{
		From:      message.Address{Name: &name, Email: "MAILER-DAEMON@" + d.Hostname},
		To:        in.from,
		MessageID: message.NewMessageID(d.Hostname),
		Date:      now,
		Original:  in.final,
	}, message.DSN{ReportingMTA: d.Hostname, ArrivalDate: now, Recipients: []message.DSNRecipient{{
		Address: in.to.rcpt[acct.ID], Action: "failed", Status: "5.7.1",
		Diagnostic: smtp.StatusReply("5.7.1", rejectReason(reason)), LastAttempt: now}}})
	domain := acct.Address[strings.LastIndexByte(acct.Address, '@')+1:]
	err := d.Forwards.Forward(ctx, domain, in.key+"\x00reject\x00"+acct.ID, "", in.from, dsn)
	if err != nil && !errors.Is(err, store.ErrForwarded) {
		in.log.Warn("queueing the rejection notice failed", "account", acct.Address, "err", err)
		return
	}
	in.log.Info("rejection notice queued", "account", acct.Address, "to", clean(in.from))
}

// ReportStore keeps TLS reports sent to us.
type ReportStore interface {
	SaveTLSReport(ctx context.Context, r store.TLSReportIn) (bool, error)
}

// noteTLSReport looks whether a message that was just stored is a TLS
// report (RFC 8460 5.3) and if so keeps the report where the
// administrator finds it ("robinmail tlsrpt received"). The message
// stays ordinary mail in the mailbox it was for, which is how the report
// reached us: the rua address a domain publishes is a mailbox or an alias
// like any other.
//
// Anybody can send one: a report is somebody's claim about how mail to us
// went, nothing is acted on, and it is kept with the name its sender gave
// itself.
func (d *Deliverer) noteTLSReport(ctx context.Context, in *incoming) {
	if d.Reports == nil || !bytes.Contains(in.final[:min(len(in.final), 64<<10)], []byte("tlsrpt")) {
		return
	}
	r, err := tlsrpt.FromMessage(message.Parse(in.final))
	if err != nil {
		return
	}
	ok, failed := r.Totals()
	saved, err := d.Reports.SaveTLSReport(ctx, store.TLSReportIn{
		Organization: clean(r.OrganizationName), ReportID: clean(r.ReportID), Domain: clean(r.Policies[0].Policy.Domain),
		Start: r.DateRange.Start, End: r.DateRange.End, Successful: ok, Failed: failed, JSON: string(r.JSON())})
	if err != nil {
		in.log.Warn("keeping a TLS report failed", "err", err)
		return
	}
	in.log.Info("TLS report received", "from", clean(r.OrganizationName), "domain", clean(r.Policies[0].Policy.Domain),
		"successful", ok, "failed", failed, "new", saved)
}
