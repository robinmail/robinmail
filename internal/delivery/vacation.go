package delivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/address"
	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/sieve"
	"github.com/mikepage/robinmail/internal/store"
)

// Automatic replies (the vacation action of RFC 5230, and JMAP's
// VacationResponse, which is one). The rules that keep a reply from being
// a nuisance are RFC 5230 4.5 and 4.6 and RFC 3834 2: one per sender per
// period; never to a notification, a list, bulk mail or another automatic
// message; only to mail that names the recipient.

// Periods of "once per sender", in days: RFC 5230 4.1 has 7 as the
// default, 1 as the least, and lets a site cap the most.
const (
	vacationDefaultDays = 7
	vacationMaxDays     = 30
)

// autoSenders are local parts of senders that are programs (RFC 3834 2:
// "MAILER-DAEMON", "owner-", "-request"; RFC 5230 4.6 adds LISTSERV and
// majordomo). The no-reply names are not in an RFC: they say themselves
// that nobody reads an answer.
func autoSender(local string) bool {
	local = strings.ToLower(local)
	switch local {
	case "mailer-daemon", "listserv", "majordomo", "noreply", "no-reply", "donotreply", "do-not-reply":
		return true
	}
	return strings.HasPrefix(local, "owner-") || strings.HasSuffix(local, "-request")
}

// whyNoReply returns why a message gets no automatic reply, or "" when it
// may have one. mine are the recipient's addresses: every address the
// account is known by, the envelope recipient, and the :addresses of the
// script.
func whyNoReply(from string, hdr []message.Header, mine []string) string {
	if from == "" {
		return "the null reverse-path: a notification"
	}
	at := strings.LastIndexByte(from, '@')
	if at <= 0 {
		return "a sender without a domain"
	}
	if autoSender(from[:at]) {
		return "the sender is a program"
	}
	own := func(addr string) bool {
		for _, m := range mine {
			if strings.EqualFold(m, addr) || address.Owns(strings.ToLower(m), addr) {
				return true
			}
		}
		return false
	}
	if own(from) {
		return "from the recipient's own address"
	}
	// RFC 3834 2: no response to a message with Auto-Submitted other
	// than "no".
	if v, ok := message.Get(hdr, "Auto-Submitted"); ok {
		value, _, _ := strings.Cut(strings.TrimSpace(message.Text(v)), ";")
		if !strings.EqualFold(strings.TrimSpace(value), "no") {
			return "an automatic message (Auto-Submitted)"
		}
	}
	// RFC 5230 4.6: no response to mail of a list; RFC 2369 and RFC 2919
	// name the fields.
	for _, name := range []string{"List-Id", "List-Help", "List-Subscribe", "List-Unsubscribe", "List-Post", "List-Owner", "List-Archive"} {
		if _, ok := message.Get(hdr, name); ok {
			return "mail of a list (" + name + ")"
		}
	}
	// RFC 3834 2: "Precedence: bulk", "junk" and "list" are widely used.
	if v, ok := message.Get(hdr, "Precedence"); ok {
		switch strings.ToLower(strings.TrimSpace(message.Text(v))) {
		case "bulk", "junk", "list":
			return "bulk mail (Precedence)"
		}
	}
	// Not in an RFC: Microsoft's way to ask for no automatic answer.
	if v, ok := message.Get(hdr, "X-Auto-Response-Suppress"); ok && !strings.EqualFold(strings.TrimSpace(message.Text(v)), "none") {
		return "the sender asked for no automatic reply"
	}
	// RFC 5230 4.5: only when one of the recipient's addresses is in To,
	// Cc, Bcc or their Resent- forms: not for a Bcc that hides who it is
	// for, and not for mail that reached the user through a list.
	for _, name := range []string{"To", "Cc", "Bcc", "Resent-To", "Resent-Cc", "Resent-Bcc"} {
		for _, v := range message.All(hdr, name) {
			for _, a := range message.Addresses(v) {
				if own(a.Email) {
					return ""
				}
			}
		}
	}
	return "the message does not name the recipient"
}

// vacationKey names a reply and whom it is for (RFC 5230 4.2): the
// :handle, or without one everything that makes the reply what it is, and
// the sender, without regard to case.
func vacationKey(v *sieve.VacationReply, from string) string {
	h := sha256.New()
	if v.Handle != "" {
		h.Write([]byte("handle\x00" + v.Handle))
	} else {
		h.Write([]byte("reply\x00" + v.Subject + "\x00" + v.From + "\x00" + v.Reason))
		if v.MIME {
			h.Write([]byte("\x00mime"))
		}
	}
	h.Write([]byte("\x00" + strings.ToLower(from)))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// buildVacation writes the reply (RFC 5230 5).
func buildVacation(host string, acct store.Account, aliases []string, from string, hdr []message.Header, v *sieve.VacationReply, now time.Time) []byte {
	// 5.3: From is the :from of the script if that is an address of the
	// user's, else the user's address.
	author := message.Address{Email: acct.Address}
	if list := message.Addresses(v.From); len(list) == 1 && MaySendAs(acct.Address, aliases, list[0].Email) {
		author = list[0]
	}
	// 5.2: the subject of the script, or "Auto: " and the original one.
	subject := v.Subject
	if subject == "" {
		orig, _ := message.Get(hdr, "Subject")
		subject = strings.TrimSpace("Auto: " + message.OneLine(message.Text(orig), 200))
	}
	headers := []message.Header{
		{Name: "From", Value: message.FormatAddresses("From", []message.Address{author})},
		{Name: "To", Value: message.FormatAddresses("To", []message.Address{{Email: from}})},
		{Name: "Subject", Value: message.FormatText("Subject", subject)},
		{Name: "Date", Value: message.FormatDate(now)},
		{Name: "Message-ID", Value: message.FormatMessageIDs("Message-ID", []string{message.NewMessageID(host)})},
	}
	// 5.8: In-Reply-To and References as for any reply (RFC 5322 3.6.4).
	origID, _ := message.Get(hdr, "Message-ID")
	if ids := message.MessageIDs(origID); len(ids) > 0 {
		refs, _ := message.Get(hdr, "References")
		chain := append(message.MessageIDs(refs), ids[0])
		if len(chain) > 20 {
			chain = chain[len(chain)-20:]
		}
		headers = append(headers,
			message.Header{Name: "In-Reply-To", Value: message.FormatMessageIDs("In-Reply-To", ids[:1])},
			message.Header{Name: "References", Value: message.FormatMessageIDs("References", chain)})
	}
	// RFC 3834 5: so that nothing answers the answer.
	headers = append(headers, message.Field("Auto-Submitted", "auto-replied"))
	if !v.MIME {
		return message.Build(headers, &message.BuildPart{Type: "text/plain", Body: []byte(v.Reason)})
	}
	// 4.5: with :mime the reason is a MIME entity, header fields first.
	var b bytes.Buffer
	b.Write(message.Build(headers, &message.BuildPart{Type: "text/plain"}))
	out := b.Bytes()
	// Keep our header fields, drop the Content-Type and the empty body
	// Build wrote, and let the entity bring its own.
	fields, _ := splitHeader(out)
	fields = removeHeader(removeHeader(fields, "Content-Type", 0), "Content-Transfer-Encoding", 0)
	return joinHeader(fields, normalizeLines([]byte(v.Reason)))
}

// vacation sends an automatic reply for an account, if the rules allow one
// for this message. Nothing it does can fail the delivery: the message
// itself is stored by now.
func (d *Deliverer) vacation(ctx context.Context, acct store.Account, in *incoming, v *sieve.VacationReply) {
	if d.AutoReply == nil || d.Filters == nil {
		return
	}
	hdr := message.ParseHeader(in.final)
	var aliases []string
	if d.SendAs != nil {
		aliases, _ = d.SendAs(ctx, acct.Address)
	}
	mine := append([]string{acct.Address}, aliases...)
	if rcpt := in.to.rcpt[acct.ID]; rcpt != "" {
		mine = append(mine, rcpt)
	}
	mine = append(mine, v.Addresses...)
	if why := whyNoReply(in.from, hdr, mine); why != "" {
		in.log.Info("no automatic reply", "account", acct.Address, "why", why)
		return
	}
	days := v.Days
	if days == 0 {
		days = vacationDefaultDays
	}
	days = min(max(days, 1), vacationMaxDays)
	ok, err := d.Filters.VacationOnce(ctx, acct.ID, vacationKey(v, in.from), time.Duration(days)*24*time.Hour)
	if err != nil {
		in.log.Warn("automatic reply: the store did not answer", "account", acct.Address, "err", err)
		return
	}
	if !ok {
		in.log.Info("no automatic reply", "account", acct.Address, "why", "this sender had one within the period")
		return
	}
	raw := buildVacation(d.Hostname, acct, aliases, in.from, hdr, v, d.now())
	if err := d.AutoReply(ctx, acct, in.from, raw); err != nil {
		in.log.Warn("queueing the automatic reply failed", "account", acct.Address, "err", err)
		return
	}
	in.log.Info("automatic reply queued", "account", acct.Address, "to", clean(in.from), "days", days)
}
