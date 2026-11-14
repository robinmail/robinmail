package message

import (
	"bytes"
	"strings"
	"time"
)

// DSN is a delivery status notification (RFC 3464): the fields a sender of
// mail cares about.
type DSN struct {
	// ReportingMTA is the name of the system that wrote the report.
	ReportingMTA string
	ArrivalDate  time.Time
	Recipients   []DSNRecipient
	// OriginalMessageID is the Message-ID of the message the report is
	// about, from the returned message or headers; empty if not there.
	OriginalMessageID string
}

// DSNRecipient is one per-recipient group of a DSN (RFC 3464 2.3).
type DSNRecipient struct {
	// Address is the Final-Recipient, without its address type.
	Address string
	// Action is failed, delayed, delivered, relayed or expanded.
	Action string
	// Status is the enhanced status code (RFC 3463), such as "5.1.1".
	Status    string
	RemoteMTA string
	// Diagnostic is the Diagnostic-Code without its type: for "smtp" the
	// reply of the remote server.
	Diagnostic  string
	LastAttempt time.Time
}

// DSNMessage is what BuildDSN needs besides the report itself.
type DSNMessage struct {
	From      Address // who the report is from: the mailer daemon
	To        string  // the sender of the original message
	MessageID string
	Date      time.Time
	// Original is the message the report is about. Only its header is
	// returned to the sender.
	Original []byte
}

// BuildDSN writes a delivery status notification as RFC 3464 defines it: a
// multipart/report (RFC 6522) with a text for people, a
// message/delivery-status for programs and the header of the original
// message.
func BuildDSN(m DSNMessage, d DSN) []byte {
	failed, delayed := 0, 0
	for _, r := range d.Recipients {
		switch r.Action {
		case "failed":
			failed++
		case "delayed":
			delayed++
		}
	}
	subject := "Delivery Status Notification"
	switch {
	case failed > 0:
		subject = "Undelivered Mail Returned to Sender"
	case delayed > 0:
		subject = "Delivery delayed"
	}
	origHeader := ParseHeader(m.Original)
	origID, _ := Get(origHeader, "Message-ID")
	ids := MessageIDs(origID)

	headers := []Header{
		Field("Date", strings.TrimPrefix(FormatDate(m.Date), " ")),
		{"From", FormatAddresses("From", []Address{m.From})},
		{"To", FormatAddresses("To", []Address{{Email: m.To}})},
		{"Subject", FormatText("Subject", subject)},
		{"Message-ID", FormatMessageIDs("Message-ID", []string{m.MessageID})},
	}
	if len(ids) > 0 {
		headers = append(headers,
			Header{"In-Reply-To", FormatMessageIDs("In-Reply-To", ids[:1])},
			Header{"References", FormatMessageIDs("References", ids[:1])})
	}
	// RFC 3834 5: an automatic message says so, and nothing replies to it.
	headers = append(headers, Field("Auto-Submitted", "auto-replied"))

	var text strings.Builder
	text.WriteString("This is the mail system at " + d.ReportingMTA + ".\r\n\r\n")
	if failed > 0 {
		text.WriteString("Your message could not be delivered to one or more recipients.\r\nIt will not be tried again.\r\n")
	} else if delayed > 0 {
		text.WriteString("Your message has not been delivered yet. It will be tried again.\r\n")
	}
	if subj, ok := Get(origHeader, "Subject"); ok {
		text.WriteString("\r\nSubject: " + OneLine(Text(subj), 200) + "\r\n")
	}
	for _, r := range d.Recipients {
		text.WriteString("\r\n<" + r.Address + ">: " + r.Action)
		if r.Status != "" {
			text.WriteString(" (" + r.Status + ")")
		}
		text.WriteString("\r\n")
		if r.Diagnostic != "" {
			text.WriteString("    " + OneLine(r.Diagnostic, 400) + "\r\n")
		}
		if r.RemoteMTA != "" {
			text.WriteString("    said by " + r.RemoteMTA + "\r\n")
		}
	}

	// RFC 3464 2.2 and 2.3: one group of fields for the message, then one
	// per recipient, with an empty line between groups.
	var status bytes.Buffer
	status.WriteString("Reporting-MTA: dns; " + oneLineASCII(d.ReportingMTA) + "\r\n")
	if !d.ArrivalDate.IsZero() {
		status.WriteString("Arrival-Date:" + FormatDate(d.ArrivalDate) + "\r\n")
	}
	for _, r := range d.Recipients {
		status.WriteString("\r\n")
		addrType := "rfc822"
		if !isASCIIString(r.Address) {
			addrType = "utf-8" // RFC 6533 3
		}
		status.WriteString("Final-Recipient: " + addrType + "; " + dsnText(r.Address) + "\r\n")
		status.WriteString("Action: " + r.Action + "\r\n")
		status.WriteString("Status: " + r.Status + "\r\n")
		if r.RemoteMTA != "" {
			status.WriteString("Remote-MTA: dns; " + dsnText(r.RemoteMTA) + "\r\n")
		}
		if r.Diagnostic != "" {
			status.WriteString("Diagnostic-Code: smtp; " + dsnText(r.Diagnostic) + "\r\n")
		}
		if !r.LastAttempt.IsZero() {
			status.WriteString("Last-Attempt-Date:" + FormatDate(r.LastAttempt) + "\r\n")
		}
	}

	var returned bytes.Buffer
	for _, h := range origHeader {
		if strings.EqualFold(h.Name, "Bcc") {
			continue
		}
		writeField(&returned, h)
	}

	root := &BuildPart{
		Type:   "multipart/report",
		Params: map[string]string{"report-type": "delivery-status"},
		SubParts: []*BuildPart{
			{Type: "text/plain", Body: []byte(text.String())},
			{Type: "message/delivery-status", Body: status.Bytes()},
			{Type: "text/rfc822-headers", Body: returned.Bytes()},
		},
	}
	return Build(headers, root)
}

func isASCIIString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// dsnText makes text fit a field of a delivery status: one line, without
// parentheses, which would start a comment (RFC 3464 2.1.1).
func dsnText(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		case r == '(':
			return '['
		case r == ')':
			return ']'
		}
		return r
	}, s)
	if len(s) > 500 {
		s = strings.ToValidUTF8(s[:500], "")
	}
	return strings.TrimSpace(s)
}

// reportType returns the report-type parameter of a multipart/report.
func reportType(p *Part) string {
	if p.Type != "multipart/report" {
		return ""
	}
	v, _ := Get(p.Headers, "Content-Type")
	_, params := parseParams(v)
	return strings.ToLower(params["report-type"])
}

// ParseDSN reads a delivery status notification. ok is false when the
// message is not one: no multipart/report with report-type
// delivery-status, or no message/delivery-status part that names a
// recipient.
//
// One thing that is not RFC 3464 is read as well: a multipart of another
// kind that has a message/delivery-status part directly in it. OpenSMTPD
// (7.6) sends its notifications as multipart/mixed, with the same three
// parts a report has. Its status part is as good as any, and without this
// a message that bounced there would stay "sent" for its sender. A report
// someone forwards sits a level deeper, inside a message/rfc822 part, and
// is not taken for one.
func ParseDSN(m *Message) (d *DSN, ok bool) {
	parts := m.Leaves
	switch kind := reportType(m.Root); {
	case kind == "delivery-status":
	case kind == "" && m.Root.Type != "multipart/report" && strings.HasPrefix(m.Root.Type, "multipart/"):
		parts = m.Root.SubParts
	default:
		return nil, false
	}
	d = &DSN{}
	var status *Part
	for _, p := range parts {
		switch p.Type {
		case "message/delivery-status", "message/global-delivery-status":
			if status == nil {
				status = p
			}
		case "text/rfc822-headers", "message/rfc822", "message/global", "message/global-headers":
			if d.OriginalMessageID == "" {
				if v, ok := Get(ParseHeader(p.Content()), "Message-ID"); ok {
					if ids := MessageIDs(v); len(ids) > 0 {
						d.OriginalMessageID = ids[0]
					}
				}
			}
		}
	}
	if status == nil {
		return nil, false
	}
	// typed reads "type; value" and returns the value.
	typed := func(v string) string {
		v = strings.TrimSpace(stripComments(unfold(v)))
		if _, rest, found := strings.Cut(v, ";"); found {
			return strings.TrimSpace(rest)
		}
		return v
	}
	rest := status.Content()
	for group := 0; len(rest) > 0 && group <= MaxAddresses; group++ {
		fields, off := parseHeaders(rest)
		if off == 0 {
			break // not a field: nothing more to read
		}
		rest = rest[off:]
		if len(fields) == 0 {
			group--
			continue
		}
		get := func(name string) string {
			v, _ := Get(fields, name)
			return v
		}
		if group == 0 {
			d.ReportingMTA = typed(get("Reporting-MTA"))
			d.ArrivalDate, _ = Date(get("Arrival-Date"))
			if get("Final-Recipient") == "" {
				continue
			}
		}
		addr := typed(get("Final-Recipient"))
		if addr == "" {
			continue
		}
		r := DSNRecipient{
			Address:    strings.Trim(addr, "<>"),
			Action:     strings.ToLower(strings.TrimSpace(stripComments(unfold(get("Action"))))),
			RemoteMTA:  typed(get("Remote-MTA")),
			Diagnostic: OneLine(strings.TrimSpace(typedRaw(get("Diagnostic-Code"))), 500),
		}
		if f := strings.Fields(stripComments(unfold(get("Status")))); len(f) > 0 && validStatus(f[0]) {
			r.Status = f[0]
		}
		r.LastAttempt, _ = Date(get("Last-Attempt-Date"))
		d.Recipients = append(d.Recipients, r)
	}
	if len(d.Recipients) == 0 {
		return nil, false
	}
	return d, true
}

// typedRaw is "type; value" without removing comments: an SMTP reply in a
// Diagnostic-Code often has parentheses that are not meant as one.
func typedRaw(v string) string {
	v = unfold(v)
	if _, rest, found := strings.Cut(v, ";"); found {
		return rest
	}
	return v
}

// validStatus checks the status-code of RFC 3463 2: class "." subject "."
// detail, class 2, 4 or 5, the others one to three digits.
func validStatus(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 || (parts[0] != "2" && parts[0] != "4" && parts[0] != "5") {
		return false
	}
	for _, p := range parts[1:] {
		if len(p) < 1 || len(p) > 3 {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// ValidStatus reports whether s is an enhanced status code (RFC 3463 2).
func ValidStatus(s string) bool { return validStatus(s) }
