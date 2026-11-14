package message

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Bodies flattens the body structure into the three lists of RFC 8621
// 4.1.4: the parts to show as the plain text body, the parts to show as the
// HTML body, and the attachments. It is the algorithm the RFC gives there,
// statement for statement.
func (m *Message) Bodies() (text, htmlBody, attachments []*Part) {
	text, htmlBody, attachments = []*Part{}, []*Part{}, []*Part{}
	parseStructure([]*Part{m.Root}, "mixed", false, &htmlBody, &text, &attachments)
	return text, htmlBody, attachments
}

func isInlineMediaType(t string) bool {
	return strings.HasPrefix(t, "image/") || strings.HasPrefix(t, "audio/") || strings.HasPrefix(t, "video/")
}

// parseStructure is the function of the same name in RFC 8621 4.1.4. A nil
// htmlBody or textBody is the RFC's null: that list is not being collected
// on this branch.
func parseStructure(parts []*Part, multipartType string, inAlternative bool, htmlBody, textBody, attachments *[]*Part) {
	textLength, htmlLength := -1, -1
	if textBody != nil {
		textLength = len(*textBody)
	}
	if htmlBody != nil {
		htmlLength = len(*htmlBody)
	}

	for i, part := range parts {
		isInline := (part.Disposition == nil || *part.Disposition != "attachment") &&
			(part.Type == "text/plain" || part.Type == "text/html" || isInlineMediaType(part.Type)) &&
			// Must be either not the first part or have no file name.
			(i == 0 || (multipartType != "related" && (isInlineMediaType(part.Type) || part.Name == nil)))

		switch {
		case part.IsMultipart():
			subType := part.Type[len("multipart/"):]
			parseStructure(part.SubParts, subType, inAlternative || subType == "alternative", htmlBody, textBody, attachments)
		case isInline:
			if multipartType == "alternative" {
				switch part.Type {
				case "text/plain":
					if textBody != nil {
						*textBody = append(*textBody, part)
					}
				case "text/html":
					if htmlBody != nil {
						*htmlBody = append(*htmlBody, part)
					}
				default:
					*attachments = append(*attachments, part)
				}
				continue
			} else if inAlternative {
				if part.Type == "text/plain" {
					htmlBody = nil
				}
				if part.Type == "text/html" {
					textBody = nil
				}
			}
			if textBody != nil {
				*textBody = append(*textBody, part)
			}
			if htmlBody != nil {
				*htmlBody = append(*htmlBody, part)
			}
			if (htmlBody == nil || textBody == nil) && isInlineMediaType(part.Type) {
				*attachments = append(*attachments, part)
			}
		default:
			*attachments = append(*attachments, part)
		}
	}

	if multipartType == "alternative" && textBody != nil && htmlBody != nil {
		// Found HTML part only.
		if textLength == len(*textBody) && htmlLength != len(*htmlBody) {
			*textBody = append(*textBody, (*htmlBody)[htmlLength:]...)
		}
		// Found plain text part only.
		if htmlLength == len(*htmlBody) && textLength != len(*textBody) {
			*htmlBody = append(*htmlBody, (*textBody)[textLength:]...)
		}
	}
}

// HasAttachment is the hasAttachment property (RFC 8621 4.1.4): true if
// the attachments hold a part that is not marked inline. Detached
// signatures are left out, as the RFC allows for parts "processed
// automatically".
func (m *Message) HasAttachment() bool {
	_, _, attachments := m.Bodies()
	for _, p := range attachments {
		if p.Disposition != nil && *p.Disposition == "inline" {
			continue
		}
		switch p.Type {
		case "application/pgp-signature", "application/pkcs7-signature", "application/x-pkcs7-signature":
			continue
		}
		return true
	}
	return false
}

// PreviewLen is the most characters a preview has (RFC 8621 4.1.4: "up to
// 256 characters").
const PreviewLen = 256

// previewScan is how much of a body is read to make a preview.
const previewScan = 16 << 10

// Preview is the preview property: the start of the text body as one line
// of plain text. For a message with only HTML it is the text of the HTML.
func (m *Message) Preview() string {
	text, _, _ := m.Bodies()
	for _, p := range text {
		var s string
		switch p.Type {
		case "text/plain":
			s, _ = p.Text()
			if len(s) > previewScan {
				s = s[:previewScan]
			}
		case "text/html":
			s, _ = p.Text()
			s = htmlText(s, previewScan)
		default:
			continue
		}
		if s = OneLine(s, PreviewLen); s != "" {
			return s
		}
	}
	return ""
}

// inlineElements do not break a word: "<b>at</b>tached" reads "attached".
var inlineElements = set("a", "abbr", "b", "bdi", "bdo", "big", "cite", "code", "del", "dfn", "em", "font", "i", "ins",
	"kbd", "mark", "q", "s", "samp", "small", "span", "strike", "strong", "sub", "sup", "tt", "u", "var", "wbr")

// htmlText returns the text of an HTML document, without what a browser
// does not show, until it has about max bytes.
func htmlText(src string, max int) string {
	if len(src) > 64*max {
		src = src[:64*max]
	}
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	skip := ""
	for b.Len() < max {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.StartTagToken:
			name, _ := z.TagName()
			switch n := string(name); {
			case n == "script" || n == "style" || n == "head" || n == "title" || n == "template" || n == "noscript":
				if skip == "" {
					skip = n
				}
			case !inlineElements[n]:
				b.WriteByte(' ')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			if n := string(name); n == skip {
				skip = ""
			} else if !inlineElements[n] {
				b.WriteByte(' ')
			}
		case html.SelfClosingTagToken:
			b.WriteByte(' ')
		case html.TextToken:
			if skip == "" {
				b.Write(z.Text())
			}
		}
	}
	return b.String()
}

// OneLine makes untrusted text fit one line of a list: valid UTF-8, runs of
// white space and control characters become one space, at most max
// characters.
func OneLine(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	n, space := 0, false
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == 0x200B || r == 0xFEFF {
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

// BodyValue is the EmailBodyValue of a text part (RFC 8621 4.1.4). max is
// the most bytes of text to return, 0 for all of it. A cut never lands
// inside a UTF-8 character.
func (p *Part) BodyValue(max int) (value string, encodingProblem, truncated bool) {
	value, encodingProblem = p.Text()
	if max > 0 && len(value) > max {
		for max > 0 && !utf8.RuneStart(value[max]) {
			max--
		}
		value, truncated = value[:max], true
	}
	return value, encodingProblem, truncated
}

// IndexText is what full-text search indexes of a message besides its
// header fields: the text a reader sees as the body, and the text of
// attachments that are text.
type IndexText struct {
	Body        string
	Attachments string
}

// partText is the text of a text part as a reader sees it, up to about max
// bytes: an HTML part without its markup.
func partText(p *Part, max int) string {
	s, _ := p.Text()
	if p.Type == "text/html" {
		return htmlText(s, max)
	}
	if len(s) > max {
		for max > 0 && !utf8.RuneStart(s[max]) {
			max--
		}
		s = s[:max]
	}
	return s
}

// HTMLText is the text of an HTML document as a reader sees it, without
// markup, scripts and styles, up to about max bytes.
func HTMLText(src string, max int) string { return htmlText(src, max) }

// PartText is the text of a text part as a reader sees it, up to about max
// bytes: an HTML part without its markup.
func PartText(p *Part, max int) string { return partText(p, max) }

// BodyText is the text body (RFC 8621 4.1.4, textBody) as plain text, the
// parts under each other, up to about max bytes. A message that only has
// HTML gives the text of that.
func (m *Message) BodyText(max int) string {
	text, _, _ := m.Bodies()
	var b strings.Builder
	for _, p := range text {
		if !strings.HasPrefix(p.Type, "text/") || b.Len() >= max {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(partText(p, max-b.Len()))
	}
	return b.String()
}

// IndexText returns the text to index, each of the two up to about max
// bytes. Of the attachments only those of a text type are read (text/plain,
// text/html, text/csv, ...), and every attachment's file name is included:
// a PDF or an image is found by its name, not by what is in it.
func (m *Message) IndexText(max int) IndexText {
	out := IndexText{Body: m.BodyText(max)}
	_, _, attachments := m.Bodies()
	var b strings.Builder
	for _, p := range attachments {
		if b.Len() >= max {
			break
		}
		if p.Name != nil {
			b.WriteString(*p.Name)
			b.WriteByte('\n')
		}
		if strings.HasPrefix(p.Type, "text/") {
			b.WriteString(partText(p, max-b.Len()))
			b.WriteByte('\n')
		}
	}
	out.Attachments = b.String()
	return out
}
