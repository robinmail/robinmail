package store

import (
	"github.com/mikepage/robinmail/internal/message"
)

// summary is what a delivery caches from the message: the ids that decide the
// thread and the fields that lists, filters and sorting use. Mail is
// untrusted, so everything here is bounded and nothing fails: a message that
// does not parse is stored with an empty summary.
type summary struct {
	messageID     string
	refs          []string // messageID, then References, then In-Reply-To; no doubles
	sender        string   // From, as text
	recipients    string   // To
	cc            string
	bcc           string
	subject       string
	sentAt        int64 // the Date field in Unix seconds, 0 without one
	preview       string
	hasAttachment bool
}

const (
	maxMessageIDLen = message.MaxMessageIDLen
	maxRefs         = 64
	maxHeaderRunes  = 250
	maxPreviewRunes = 255
)

// messageIDs reads a Message-ID, In-Reply-To or References field.
var messageIDs = message.MessageIDs

func summarize(raw []byte) summary {
	var s summary
	m := message.Parse(raw)
	h := m.Headers
	get := func(name string) string {
		v, _ := message.Get(h, name)
		return v
	}
	if ids := messageIDs(get("Message-ID")); len(ids) > 0 {
		s.messageID = ids[0]
	}

	// The nearest ancestors are at the end, so those survive the cap.
	rest := append(messageIDs(get("References")), messageIDs(get("In-Reply-To"))...)
	if len(rest) > maxRefs-1 {
		rest = rest[len(rest)-(maxRefs-1):]
	}
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			s.refs = append(s.refs, id)
		}
	}
	add(s.messageID)
	for _, id := range rest {
		add(id)
	}

	text := func(name string) string { return message.OneLine(message.Text(get(name)), maxHeaderRunes) }
	s.sender = text("From")
	s.recipients = text("To")
	s.cc = text("Cc")
	s.bcc = text("Bcc")
	s.subject = text("Subject")
	if t, ok := message.Date(get("Date")); ok && t.Unix() > 0 {
		s.sentAt = t.Unix()
	}
	s.preview = message.OneLine(m.Preview(), maxPreviewRunes)
	s.hasAttachment = m.HasAttachment()
	return s
}
