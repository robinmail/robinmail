package delivery

import (
	"context"
	"errors"
	"strings"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/smtp"
	"github.com/mikepage/robinmail/internal/srs"
	"github.com/mikepage/robinmail/internal/store"
)

// AliasStore finds the alias for an address of ours. *store.Store has it.
type AliasStore interface {
	LookupAlias(ctx context.Context, addr address.Address) (store.Alias, bool, error)
}

// Forwarder queues a message for a recipient elsewhere. domain is the
// domain of ours it is sent on for. key makes a second call for the same
// message and recipient queue nothing (store.ErrForwarded). *store.Store
// has it.
type Forwarder interface {
	Forward(ctx context.Context, domain, key, mailFrom, rcpt string, raw []byte) error
}

// maxAliasDepth is how many aliases may stand between a recipient and a
// mailbox: an alias whose target is an alias whose target ... A chain that
// is longer, or that comes back to itself, ends there.
const maxAliasDepth = 5

// maxHops is how many Received fields a message may have (RFC 5321 6.3:
// a loop is noticed by counting them; "a large rejection threshold,
// normally at least 100").
const maxHops = 100

var (
	errLoop      = &smtp.Error{Code: 554, Enhanced: "5.4.6", Msg: "Mail forwarding loop"}
	errHops      = &smtp.Error{Code: 554, Enhanced: "5.4.6", Msg: "Too many hops"}
	errOnlyDSN   = &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: "This address only takes delivery status notifications"}
	errSpamFwd   = &smtp.Error{Code: 550, Enhanced: "5.7.1", Msg: "Message looks like spam and is not forwarded"}
	errNoForward = &smtp.Error{Code: 451, Enhanced: "4.3.0", Msg: "Forwarding is not available, try again later"}
)

// forward is a copy of a message that leaves again.
type forward struct {
	to string
	// via is the address of ours that sent it on: an alias, or for a
	// bounce on its way back the rewritten sender address it came for.
	// For an alias it goes into Delivered-To, which is how a loop through
	// the alias is noticed.
	via string
	// bounce marks a message for a rewritten sender address (SRS): it goes
	// to the original sender with the null reverse-path.
	bounce bool
}

// routes is where the recipients of a message lead: mailboxes here and
// addresses elsewhere, each once.
type routes struct {
	accounts []store.Account
	forwards []forward
	seen     map[string]bool
	// rcpt is, per account id, the recipient address that led to it
	// first: the envelope recipient of that account's filter.
	rcpt map[string]string
	// cur is the recipient being routed.
	cur string
}

func (r *routes) account(a store.Account) {
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if !r.seen["a:"+a.ID] {
		r.seen["a:"+a.ID] = true
		r.accounts = append(r.accounts, a)
		if r.rcpt == nil {
			r.rcpt = map[string]string{}
		}
		r.rcpt[a.ID] = r.cur
	}
}

func (r *routes) forward(f forward) {
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if key := "f:" + strings.ToLower(f.to); !r.seen[key] {
		r.seen[key] = true
		r.forwards = append(r.forwards, f)
	}
}

// route finds where mail for one recipient goes. from is the envelope
// sender, which decides whether a rewritten address may be used.
//
// The order: an account with the address, an account with the address
// without its "+tag", an alias (store.LookupAlias: exact, without the
// tag, the best pattern). An alias's targets at our own domains are looked
// up the same way; the others are forwards.
func (d *Deliverer) route(ctx context.Context, from, to string, into *routes) error {
	into.cur = to
	domains, err := d.acceptedDomains(ctx)
	if err != nil {
		d.log().Error("domain lookup failed", "err", err)
		return errTempFail
	}
	if strings.EqualFold(to, "postmaster") {
		// RFC 5321 4.1.1.3: <Postmaster> without a domain.
		for _, dom := range domains {
			err := d.expand(ctx, address.Address{Local: "postmaster", Domain: dom}, domains, 0, map[string]bool{}, into)
			if err == nil || err != errNoUser {
				return err
			}
		}
		return errNoUser
	}
	at := strings.LastIndexByte(to, '@')
	if at < 0 || !has(domains, strings.ToLower(to[at+1:])) {
		if at < 0 {
			return errNoUser
		}
		return errNoRelay
	}
	if srs.IsSRS(to[:at]) {
		return d.routeBounce(from, to, into)
	}
	a, err := address.Parse(to)
	if err != nil {
		return errNoUser
	}
	return d.expand(ctx, a, domains, 0, map[string]bool{}, into)
}

// routeBounce handles mail for an address this server made as the sender
// of forwarded mail. Only a notification may use it (the null
// reverse-path): otherwise anyone who saw such an address could have mail
// passed on through it.
func (d *Deliverer) routeBounce(from, to string, into *routes) error {
	if d.SRS == nil || d.Forwards == nil {
		return errNoUser
	}
	if from != "" {
		return errOnlyDSN
	}
	orig, err := d.SRS.Reverse(to)
	if err != nil {
		return errNoUser
	}
	into.forward(forward{to: orig, via: to, bounce: true})
	return nil
}

func (d *Deliverer) expand(ctx context.Context, a address.Address, domains []string, depth int, visited map[string]bool, into *routes) error {
	candidates := []address.Address{a}
	if base, _, ok := strings.Cut(a.Local, "+"); ok && base != "" {
		candidates = append(candidates, address.Address{Local: base, Domain: a.Domain})
	}
	for _, c := range candidates {
		acct, err := d.Store.Account(ctx, c.String())
		if err == nil {
			into.account(acct)
			return nil
		}
		if !errors.Is(err, store.ErrNoAccount) {
			d.log().Error("account lookup failed", "err", err)
			return errTempFail
		}
	}
	if d.Aliases == nil {
		return errNoUser
	}
	alias, ok, err := d.Aliases.LookupAlias(ctx, a)
	if err != nil {
		d.log().Error("alias lookup failed", "err", err)
		return errTempFail
	}
	if !ok || visited[alias.Address] || depth >= maxAliasDepth {
		return errNoUser
	}
	visited[alias.Address] = true
	// The address that goes into Delivered-To is the one that was written
	// to, so a pattern names the address, not itself.
	via := a.String()
	reached := false
	var temp error
	for _, target := range alias.Targets {
		at := strings.LastIndexByte(target, '@')
		if at > 0 && has(domains, strings.ToLower(target[at+1:])) {
			t, err := address.Parse(target)
			if err != nil {
				continue
			}
			switch err := d.expand(ctx, t, domains, depth+1, visited, into); err {
			case nil:
				reached = true
			case errNoUser:
				d.log().Warn("alias target does not exist", "alias", alias.Address, "target", target)
			default:
				temp = err
			}
			continue
		}
		if !smtp.ValidMailbox(target) {
			d.log().Warn("alias target is not an address", "alias", alias.Address, "target", clean(target))
			continue
		}
		if d.Forwards == nil {
			temp = errNoForward
			continue
		}
		into.forward(forward{to: target, via: via})
		reached = true
	}
	switch {
	case temp != nil:
		return temp // rather late than to some of the targets only
	case !reached:
		return errNoUser
	}
	return nil
}

// forwardSender is the envelope sender a forwarded message leaves with:
// the original sender rewritten under the alias's domain (SRS), so that a
// bounce comes back here and SPF is checked against a domain that really
// sends it. A notification stays one, and a sender that cannot be
// rewritten becomes the null sender: nothing bounces to it, and nothing
// loops.
func (d *Deliverer) forwardSender(from, via string) string {
	if from == "" || d.SRS == nil {
		return ""
	}
	domain := via[strings.LastIndexByte(via, '@')+1:]
	if rewritten, ok := d.SRS.Forward(from, domain); ok {
		return rewritten
	}
	return ""
}

// deliveredTo lists the addresses in the Delivered-To fields of a message.
func deliveredTo(fields []field) map[string]bool {
	out := map[string]bool{}
	for _, f := range fields {
		if strings.EqualFold(f.name, "Delivered-To") {
			out[strings.ToLower(strings.TrimSpace(string(f.raw[len(f.name)+1:])))] = true
		}
	}
	return out
}

func countFields(fields []field, name string) int {
	n := 0
	for _, f := range fields {
		if strings.EqualFold(f.name, name) {
			n++
		}
	}
	return n
}

// checkLoops refuses a message that has been here before on its way
// through one of these aliases, or that has travelled too far.
func checkLoops(fields []field, forwards []forward) error {
	if countFields(fields, "Received") >= maxHops {
		return errHops
	}
	seen := deliveredTo(fields)
	for _, f := range forwards {
		if !f.bounce && seen[f.via] {
			return errLoop
		}
	}
	return nil
}

// sendOn queues the copies of a message that leave again. header and rest
// are the message as it is passed on: with our Received line, without a
// Return-Path, which only final delivery adds. key is the delivery key of
// the message; with the recipient it makes a retry queue nothing twice.
func (d *Deliverer) sendOn(ctx context.Context, key, from string, forwards []forward, header []field, rest []byte) error {
	for _, f := range forwards {
		fields := header
		sender := ""
		if !f.bounce {
			sender = d.forwardSender(from, f.via)
			mark := field{name: "Delivered-To", raw: []byte("Delivered-To: " + f.via + "\r\n")}
			fields = append([]field{mark}, header...)
		}
		domain := f.via[strings.LastIndexByte(f.via, '@')+1:]
		err := d.Forwards.Forward(ctx, domain, key+"\x00"+strings.ToLower(f.to), sender, f.to, joinHeader(fields, rest))
		if errors.Is(err, store.ErrForwarded) {
			d.log().Info("already forwarded", "to", clean(f.to))
			continue
		}
		if err != nil {
			d.log().Error("forwarding failed", "to", clean(f.to), "err", err)
			return errTempFail
		}
		d.log().Info("forwarded", "to", clean(f.to), "via", f.via, "sender", clean(sender), "bounce", f.bounce)
	}
	return nil
}
