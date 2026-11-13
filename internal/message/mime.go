package message

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Part is one body part: the EmailBodyPart of RFC 8621 4.1.4, without its
// blobId, which is the caller's to make. A pointer field is nil where JMAP
// has null.
type Part struct {
	// PartID identifies a leaf within its message: "1", "2", ... in the
	// order the parts appear. It is empty for a multipart.
	PartID  string
	Headers []Header
	// Type is the media type in lowercase, "text/plain" when the part has
	// none (RFC 2045 5.2), "message/rfc822" inside a multipart/digest
	// (RFC 2046 5.1.5).
	Type string
	// Charset is the charset parameter. A text part without one has the
	// implicit "us-ascii"; other parts without one have nil.
	Charset     *string
	Name        *string  // file name, from Content-Disposition or Content-Type
	Disposition *string  // "inline", "attachment", ... in lowercase
	CID         *string  // Content-ID without the angle brackets
	Language    []string // Content-Language tags; nil without the field
	Location    *string  // Content-Location
	// Size is the number of octets after the transfer encoding is undone:
	// what a download of the part yields. 0 for a multipart.
	Size     int64
	SubParts []*Part // of a multipart; never nil for one

	content []byte // the body with the transfer encoding undone
}

// IsMultipart reports whether the part is a multipart/*.
func (p *Part) IsMultipart() bool { return strings.HasPrefix(p.Type, "multipart/") }

// Content is the body of a leaf with its Content-Transfer-Encoding undone.
// Do not change it: it may be a slice of the raw message.
func (p *Part) Content() []byte { return p.content }

// Text is the body of a text part as UTF-8 with LF line ends (RFC 8621
// 4.1.4, EmailBodyValue). problem is true if the charset is unknown or the
// bytes are not valid in it; the text is then a best effort.
func (p *Part) Text() (text string, problem bool) {
	label := "us-ascii"
	if p.Charset != nil {
		label = *p.Charset
	}
	text, known := decodeCharset(p.content, label)
	problem = !known
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "\uFFFD")
		problem = true
	} else if known && strings.ContainsRune(text, utf8.RuneError) && !bytes.Contains(p.content, []byte("\uFFFD")) {
		problem = true // the decoder met bytes that are nothing in this charset
	}
	if strings.Contains(text, "\r") {
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	return text, problem
}

// Message is a parsed message.
type Message struct {
	Headers []Header
	// Root is the body structure: one leaf, or a multipart with its parts.
	Root *Part
	// Leaves are all parts that are not multiparts, in order. The PartID
	// of Leaves[i] is i+1.
	Leaves []*Part
}

// Part finds a leaf by its id.
func (m *Message) Part(id string) *Part {
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 || n > len(m.Leaves) || m.Leaves[n-1].PartID != id {
		return nil
	}
	return m.Leaves[n-1]
}

// ParseHeader reads only the header section of a message.
func ParseHeader(raw []byte) []Header {
	fields, _ := parseHeaders(raw)
	return fields
}

// Parse reads a message. It never fails; see the package comment.
func Parse(raw []byte) *Message {
	m := &Message{}
	count := 0
	m.Root = m.parsePart(raw, 0, "text/plain", &count)
	m.Headers = m.Root.Headers
	return m
}

func (m *Message) parsePart(raw []byte, depth int, defaultType string, count *int) *Part {
	*count++
	fields, off := parseHeaders(raw)
	body := raw[off:]
	p := &Part{Headers: fields, Type: defaultType}

	var params map[string]string
	ct, hasType := Get(fields, "Content-Type")
	if hasType {
		typ, ps := parseParams(ct)
		// "type/subtype", both tokens. Anything else is treated as absent.
		if slash := strings.IndexByte(typ, '/'); slash > 0 && slash < len(typ)-1 && isToken(typ[:slash]) && isToken(typ[slash+1:]) {
			p.Type, params = typ, ps
		} else {
			hasType = false
		}
	}
	if cs, ok := params["charset"]; ok && cs != "" {
		cs = strings.ToLower(cs)
		p.Charset = &cs
	} else if !hasType || strings.HasPrefix(p.Type, "text/") {
		cs := "us-ascii"
		p.Charset = &cs
	}

	if v, ok := Get(fields, "Content-Disposition"); ok {
		disp, dparams := parseParams(v)
		if disp != "" {
			p.Disposition = &disp
		}
		if name, ok := dparams["filename"]; ok && name != "" {
			p.Name = &name
		}
	}
	if name, ok := params["name"]; ok && name != "" && p.Name == nil {
		p.Name = &name
	}
	if v, ok := Get(fields, "Content-ID"); ok {
		if ids := MessageIDs(v); len(ids) > 0 {
			p.CID = &ids[0]
		} else if id := strings.TrimSpace(stripComments(unfold(v))); id != "" && len(id) <= MaxMessageIDLen {
			id = strings.ToValidUTF8(id, "\uFFFD")
			p.CID = &id
		}
	}
	if v, ok := Get(fields, "Content-Language"); ok {
		p.Language = []string{}
		for _, tag := range strings.Split(stripComments(unfold(v)), ",") {
			if tag = strings.TrimSpace(tag); tag != "" && len(p.Language) < MaxParams {
				p.Language = append(p.Language, strings.ToValidUTF8(tag, "\uFFFD"))
			}
		}
	}
	if v, ok := Get(fields, "Content-Location"); ok {
		loc := strings.TrimSpace(Text(v))
		p.Location = &loc
	}

	if p.IsMultipart() {
		p.SubParts = []*Part{}
		boundary := params["boundary"]
		if depth >= MaxDepth || boundary == "" {
			return p
		}
		childType := "text/plain"
		if p.Type == "multipart/digest" {
			childType = "message/rfc822"
		}
		for _, piece := range splitMultipart(body, boundary) {
			if *count >= MaxParts {
				break
			}
			p.SubParts = append(p.SubParts, m.parsePart(piece, depth+1, childType, count))
		}
		return p
	}

	enc, _ := Get(fields, "Content-Transfer-Encoding")
	switch strings.ToLower(strings.TrimSpace(stripComments(unfold(enc)))) {
	case "base64":
		p.content, _ = decodeBase64(body)
	case "quoted-printable":
		p.content = decodeQuotedPrintable(body)
	default:
		// 7bit, 8bit, binary, and what we do not know: as it is.
		p.content = body
	}
	p.Size = int64(len(p.content))
	m.Leaves = append(m.Leaves, p)
	p.PartID = strconv.Itoa(len(m.Leaves))
	return p
}

// isToken reports whether s is a token of RFC 2045 5.1.
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?=`, c) >= 0 {
			return false
		}
	}
	return s != ""
}

// splitMultipart cuts a multipart body into its parts (RFC 2046 5.1.1). A
// delimiter is a line that starts with "--" and the boundary and has only
// white space after it; "--" after the boundary closes the multipart. The
// line break in front of a delimiter belongs to the delimiter. The preamble
// and the epilogue are dropped. A multipart without its closing delimiter
// ends where the input does.
func splitMultipart(body []byte, boundary string) [][]byte {
	delim := []byte("--" + boundary)
	var parts [][]byte
	start := -1
	for pos := 0; pos <= len(body); {
		lineEnd := bytes.IndexByte(body[pos:], '\n')
		if lineEnd < 0 {
			lineEnd = len(body)
		} else {
			lineEnd += pos
		}
		line := body[pos:lineEnd]
		if bytes.HasPrefix(line, delim) {
			rest := line[len(delim):]
			closing := bytes.HasPrefix(rest, []byte("--"))
			if closing {
				rest = rest[2:]
			}
			if len(bytes.TrimRight(rest, " \t\r")) == 0 {
				if start >= 0 {
					end := pos
					if end > start && body[end-1] == '\n' {
						end--
						if end > start && body[end-1] == '\r' {
							end--
						}
					}
					parts = append(parts, body[start:end])
				}
				if closing {
					return parts
				}
				start = min(lineEnd+1, len(body))
			}
		}
		if lineEnd >= len(body) {
			break
		}
		pos = lineEnd + 1
	}
	if start >= 0 {
		parts = append(parts, body[start:])
	}
	return parts
}
