package pim

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robinmail/robinmail/internal/ical"
	"github.com/robinmail/robinmail/internal/jscal"
	"github.com/robinmail/robinmail/internal/store"
)

// iMIP (RFC 6047): an iTIP message in an email. robinmail writes it as
// most programs do: the text for people and the text/calendar part with
// its method side by side (multipart/alternative), and the same calendar
// as an attachment "invite.ics", inside a multipart/mixed.

// BuildIMIP writes the email for one iTIP message.
func BuildIMIP(from, to string, msg *ical.Component, now time.Time) []byte {
	method := strings.ToUpper(msg.Value("METHOD"))
	comps := jscal.Components(msg)
	var summary, when, who string
	if len(comps) > 0 {
		c := comps[0]
		summary = c.Text("SUMMARY")
		if p := c.Get("DTSTART"); p != nil {
			if d, err := ical.ZonesOf(msg).ParseDateTime(p); err == nil {
				if d.Date {
					when = d.Time.Format("Monday 2 January 2006")
				} else {
					when = d.Time.Format("Monday 2 January 2006 15:04 MST")
				}
			}
		}
		for _, a := range c.All("ATTENDEE") {
			if strings.EqualFold(mailtoOf(a.Value), from) {
				who = strings.ToLower(a.Param("PARTSTAT"))
			}
		}
	}
	if summary == "" {
		summary = "(no title)"
	}
	var subject, text string
	switch method {
	case "REQUEST", "ADD":
		subject = "Invitation: " + summary
		text = "You are invited to " + summary + "."
	case "CANCEL":
		subject = "Cancelled: " + summary
		text = summary + " has been cancelled."
	case "REPLY":
		label := map[string]string{"accepted": "Accepted", "declined": "Declined", "tentative": "Tentatively accepted", "delegated": "Delegated"}[who]
		if label == "" {
			label = "Reply"
		}
		subject = label + ": " + summary
		text = from + " " + strings.ToLower(label) + " " + summary + "."
	default:
		subject = summary
		text = summary
	}
	if when != "" {
		subject += " (" + when + ")"
		text += "\r\n\r\nWhen: " + when
	}
	if len(comps) > 0 {
		if loc := comps[0].Text("LOCATION"); loc != "" {
			text += "\r\nWhere: " + loc
		}
		if d := comps[0].Text("DESCRIPTION"); d != "" && method != "REPLY" {
			text += "\r\n\r\n" + strings.ReplaceAll(d, "\n", "\r\n")
		}
	}
	text += "\r\n"
	cal := ical.Encode(msg)

	boundary := func() string {
		b := make([]byte, 12)
		rand.Read(b)
		return "=_" + hex.EncodeToString(b)
	}
	outer, inner := boundary(), boundary()
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n", outer)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", outer, inner)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", inner)
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(text))
	qp.Close()
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/calendar; charset=utf-8; method=%s\r\nContent-Transfer-Encoding: base64\r\n\r\n", inner, method)
	writeBase64(&b, cal)
	fmt.Fprintf(&b, "--%s--\r\n", inner)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: application/ics; name=\"invite.ics\"\r\nContent-Disposition: attachment; filename=\"invite.ics\"\r\nContent-Transfer-Encoding: base64\r\n\r\n", outer)
	writeBase64(&b, cal)
	fmt.Fprintf(&b, "--%s--\r\n", outer)
	return b.Bytes()
}

func writeBase64(b *bytes.Buffer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
}

// Limits of FindITIP.
const (
	maxParts     = 50
	maxPartDepth = 5
	maxCalendar  = 1 << 20
)

// FindITIP looks in an email for an iTIP message: a text/calendar part
// with a method, or a calendar with METHOD in an application/ics part. It
// returns the address of the From field too.
func FindITIP(raw []byte) (*ical.Component, string, bool) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, "", false
	}
	from := ""
	if list, err := mail.ParseAddressList(m.Header.Get("From")); err == nil && len(list) == 1 {
		from = list[0].Address
	}
	parts := 0
	var found *ical.Component
	var walk func(ct, cte string, body io.Reader, depth int)
	walk = func(ct, cte string, body io.Reader, depth int) {
		if found != nil || depth > maxPartDepth || parts > maxParts {
			return
		}
		parts++
		mt, params, err := mime.ParseMediaType(ct)
		if err != nil {
			return
		}
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if err != nil {
					return
				}
				walk(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1)
				if found != nil {
					return
				}
			}
		}
		if mt != "text/calendar" && mt != "application/ics" {
			return
		}
		var r io.Reader = io.LimitReader(body, maxCalendar*2)
		switch strings.ToLower(strings.TrimSpace(cte)) {
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, &noSpace{r: bufio.NewReader(r)})
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		}
		data, err := io.ReadAll(io.LimitReader(r, maxCalendar))
		if err != nil || !utf8.Valid(data) {
			return
		}
		cal, err := ical.ParseOne(data, "VCALENDAR")
		if err != nil || cal.Value("METHOD") == "" {
			return
		}
		found = cal
	}
	ct := m.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	walk(ct, m.Header.Get("Content-Transfer-Encoding"), m.Body, 0)
	return found, from, found != nil
}

// noSpace drops the line breaks of base64 text.
type noSpace struct{ r *bufio.Reader }

func (n *noSpace) Read(p []byte) (int, error) {
	i := 0
	for i < len(p) {
		c, err := n.r.ReadByte()
		if err != nil {
			if i > 0 {
				return i, nil
			}
			return 0, err
		}
		if c == '\r' || c == '\n' || c == ' ' || c == '\t' {
			continue
		}
		p[i] = c
		i++
	}
	return i, nil
}

// IncomingMail is called for every message delivered to an account that
// was not taken for spam. An iTIP message in it from the right sender is
// applied to the account's calendar (RFC 6047 2, RFC 5546 3.2).
//
// The From field of mail from outside is whatever the sender wrote, so
// that it names the organizer or the attendee proves nothing by itself
// (RFC 6047 3). authenticated says the delivery checked it: DMARC passed
// for its domain, or the message came from an account of this server. A
// message that is not is applied only when it is a new invitation; what
// would change an event the account has, and every reply, is kept in the
// scheduling inbox for the user to look at, and the email stays as it is.
func (s *Service) IncomingMail(ctx context.Context, acct store.Account, raw []byte, authenticated bool) {
	msg, from, ok := FindITIP(raw)
	if !ok || from == "" {
		return
	}
	if err := s.apply(ctx, acct, msg, from, authenticated || s.IMIPUnverified); err != nil {
		s.log().Info("imip: not applied", "to", acct.Address, "from", from, "method", msg.Value("METHOD"),
			"authenticated", authenticated, "err", err)
		return
	}
	s.log().Info("imip: applied", "to", acct.Address, "from", from, "method", msg.Value("METHOD"))
}
