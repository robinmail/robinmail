package store

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"unicode"
)

// summary is what a delivery caches from the message: the ids that decide the
// thread and the fields shown in lists. Mail is untrusted, so everything here
// is bounded and nothing fails: a message that does not parse is stored with
// an empty summary.
//
// preview and hasAttachment are rough until internal/message (phase 3) reads
// MIME properly: the preview only exists for a text/plain message, and
// "attachment" means multipart/mixed.
type summary struct {
	messageID     string
	refs          []string // messageID, then References, then In-Reply-To; no doubles
	sender        string
	subject       string
	preview       string
	hasAttachment bool
}

const (
	maxMessageIDLen = 250
	maxRefs         = 64
	maxHeaderRunes  = 250
	maxPreviewRunes = 255
	previewScan     = 4096
)

func summarize(raw []byte) summary {
	var s summary
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return s
	}
	h := msg.Header
	if ids := messageIDs(h.Get("Message-ID")); len(ids) > 0 {
		s.messageID = ids[0]
	}

	// The nearest ancestors are at the end, so those survive the cap.
	rest := append(messageIDs(h.Get("References")), messageIDs(h.Get("In-Reply-To"))...)
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

	dec := new(mime.WordDecoder)
	decode := func(v string) string {
		if out, err := dec.DecodeHeader(v); err == nil {
			v = out
		}
		return oneLine(v, maxHeaderRunes)
	}
	s.sender = decode(h.Get("From"))
	s.subject = decode(h.Get("Subject"))

	mediaType := "text/plain"
	if ct := h.Get("Content-Type"); ct != "" {
		mediaType, _, _ = mime.ParseMediaType(ct)
	}
	s.hasAttachment = mediaType == "multipart/mixed"
	if mediaType == "text/plain" {
		var body io.Reader = msg.Body
		switch strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))) {
		case "quoted-printable":
			body = quotedprintable.NewReader(body)
		case "base64":
			body = base64.NewDecoder(base64.StdEncoding, body)
		}
		buf := make([]byte, previewScan)
		n, _ := io.ReadFull(body, buf)
		s.preview = oneLine(string(buf[:n]), maxPreviewRunes)
	}
	return s
}

// messageIDs returns the ids between angle brackets in a header value,
// without the brackets. Ids with whitespace or of absurd length are skipped.
func messageIDs(v string) []string {
	var out []string
	for {
		start := strings.IndexByte(v, '<')
		if start < 0 {
			return out
		}
		v = v[start+1:]
		end := strings.IndexAny(v, "<>")
		if end < 0 {
			return out
		}
		if v[end] == '<' {
			v = v[end:]
			continue
		}
		id := v[:end]
		v = v[end+1:]
		if id != "" && len(id) <= maxMessageIDLen && !strings.ContainsFunc(id, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) {
			out = append(out, strings.ToValidUTF8(id, ""))
		}
	}
}

// oneLine makes untrusted text fit one line of a list: valid UTF-8, runs of
// whitespace and control characters become one space, at most max runes.
func oneLine(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	n, space := 0, false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = b.Len() > 0
			continue
		}
		if n >= max {
			break
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
			if n >= max {
				break
			}
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
