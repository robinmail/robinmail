package imap

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"sort"
	"strconv"
	"strings"

	"github.com/robinmail/robinmail/internal/message"
)

// The structure of a message as IMAP wants it: a tree of body parts, each
// with the octets of its header and of its body in the message as it is
// stored (RFC 3501 6.4.5 and 7.4.2). JMAP's view of the same message
// (internal/message) has the content decoded and no offsets, so the tree is
// built here; the header fields, their parameters and the addresses are
// read with internal/message.

// maxMIMEDepth bounds the nesting that is followed. Deeper parts are
// leaves.
const maxMIMEDepth = 20

// entity is a message or a body part: raw[start:bodyStart] is its header,
// with the empty line that ends it, raw[bodyStart:end] its body.
type entity struct {
	raw                   []byte
	start, bodyStart, end int
	hdr                   []message.Header
	typ, sub              string // lowercase
	params                map[string]string
	parts                 []*entity // of a multipart
	inner                 *entity   // the message in a message/rfc822
}

func (e *entity) header() []byte { return e.raw[e.start:e.bodyStart] }
func (e *entity) body() []byte   { return e.raw[e.bodyStart:e.end] }
func (e *entity) whole() []byte  { return e.raw[e.start:e.end] }

func (e *entity) get(name string) (string, bool) {
	v, ok := message.Get(e.hdr, name)
	if !ok {
		return "", false
	}
	return unfold(v), true
}

// unfold removes the line breaks of folding and the space around the value.
func unfold(v string) string {
	if strings.ContainsAny(v, "\r\n") {
		v = strings.NewReplacer("\r\n", "", "\n", "", "\r", "").Replace(v)
	}
	return strings.TrimSpace(v)
}

// headerEnd finds where the header of raw[start:end] ends: after the
// first empty line, or at end without one.
func headerEnd(raw []byte, start, end int) int {
	for i := start; i < end; {
		if raw[i] == '\n' {
			return i + 1
		}
		if raw[i] == '\r' && i+1 < end && raw[i+1] == '\n' {
			return i + 2
		}
		nl := bytes.IndexByte(raw[i:end], '\n')
		if nl < 0 {
			return end
		}
		i += nl + 1
	}
	return end
}

// parseEntity reads raw[start:end] as a message or body part.
func parseEntity(raw []byte, start, end, depth int, defaultType string) *entity {
	e := &entity{raw: raw, start: start, end: end}
	e.bodyStart = headerEnd(raw, start, end)
	e.hdr = message.ParseHeader(raw[start:e.bodyStart])
	ct, has := message.Get(e.hdr, "Content-Type")
	value := defaultType
	e.params = map[string]string{}
	if has {
		if v, params := message.Params(ct); strings.Count(v, "/") == 1 && !strings.HasPrefix(v, "/") && !strings.HasSuffix(v, "/") {
			value, e.params = v, params
		}
	}
	e.typ, e.sub, _ = strings.Cut(value, "/")
	if depth >= maxMIMEDepth {
		return e
	}
	switch {
	case e.typ == "multipart":
		boundary := e.params["boundary"]
		if boundary == "" {
			break
		}
		inner := "text/plain"
		if e.sub == "digest" {
			inner = "message/rfc822" // RFC 2046 5.1.5
		}
		for _, span := range splitMultipart(raw, e.bodyStart, end, boundary) {
			e.parts = append(e.parts, parseEntity(raw, span[0], span[1], depth+1, inner))
			if len(e.parts) >= 1000 {
				break
			}
		}
	case e.typ == "message" && (e.sub == "rfc822" || e.sub == "global"):
		// Only as it is: a message inside base64 is an attachment.
		switch e.encoding() {
		case "7BIT", "8BIT", "BINARY":
			e.inner = parseEntity(raw, e.bodyStart, end, depth+1, "text/plain")
		}
	}
	return e
}

// splitMultipart finds the parts of a multipart body (RFC 2046 5.1.1):
// what lies between delimiter lines "--boundary". The line break before a
// delimiter belongs to the delimiter. Without a closing delimiter the last
// part runs to the end.
func splitMultipart(raw []byte, start, end int, boundary string) [][2]int {
	delim := []byte("--" + boundary)
	var spans [][2]int
	partStart := -1
	for i := start; i < end; {
		lineEnd := end
		next := end
		if nl := bytes.IndexByte(raw[i:end], '\n'); nl >= 0 {
			lineEnd, next = i+nl, i+nl+1
		}
		line := raw[i:lineEnd]
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		if bytes.HasPrefix(line, delim) {
			rest := bytes.TrimRight(line[len(delim):], " \t")
			closing := bytes.Equal(rest, []byte("--"))
			if len(rest) == 0 || closing {
				if partStart >= 0 {
					// Back over the line break that precedes the delimiter.
					stop := i
					if stop > partStart && raw[stop-1] == '\n' {
						stop--
						if stop > partStart && raw[stop-1] == '\r' {
							stop--
						}
					}
					spans = append(spans, [2]int{partStart, stop})
				}
				if closing {
					return spans
				}
				partStart = next
			}
		}
		i = next
	}
	if partStart >= 0 && partStart <= end {
		spans = append(spans, [2]int{partStart, end})
	}
	return spans
}

// encoding is the Content-Transfer-Encoding in upper case, "7BIT" without
// one (RFC 2045 6.1).
func (e *entity) encoding() string {
	if v, ok := e.get("Content-Transfer-Encoding"); ok && v != "" {
		return strings.ToUpper(v)
	}
	return "7BIT"
}

// part finds a body part by its number (RFC 3501 6.4.5): the parts of a
// multipart are 1, 2, ...; a message that is not a multipart has one part,
// 1, which is its body; a number after a message/rfc822 part counts
// inside that message.
func (e *entity) part(path []int) *entity {
	cur := e
	for k, n := range path {
		ctx := cur
		if k > 0 {
			switch {
			case cur.inner != nil:
				ctx = cur.inner
			case len(cur.parts) == 0:
				return nil
			}
		}
		switch {
		case len(ctx.parts) > 0:
			if n < 1 || n > len(ctx.parts) {
				return nil
			}
			cur = ctx.parts[n-1]
		case n == 1:
			cur = ctx
		default:
			return nil
		}
	}
	return cur
}

// rawField is one header field as it is in the message, line breaks and
// continuation lines included.
type rawField struct {
	name string
	data []byte
}

// rawFields cuts a header into its fields. The empty line at the end is
// not a field.
func rawFields(header []byte) []rawField {
	var out []rawField
	for i := 0; i < len(header); {
		next := len(header)
		if nl := bytes.IndexByte(header[i:], '\n'); nl >= 0 {
			next = i + nl + 1
		}
		line := header[i:next]
		switch {
		case len(bytes.TrimRight(line, "\r\n")) == 0:
			return out
		case (line[0] == ' ' || line[0] == '\t') && len(out) > 0:
			out[len(out)-1].data = header[i-len(out[len(out)-1].data) : next]
		default:
			name := ""
			if colon := bytes.IndexByte(line, ':'); colon > 0 {
				name = string(bytes.TrimRight(line[:colon], " \t"))
			}
			out = append(out, rawField{name: name, data: line})
		}
		i = next
	}
	return out
}

// headerSubset is HEADER.FIELDS and HEADER.FIELDS.NOT (RFC 3501 6.4.5):
// the fields with (or without) the given names, and the empty line.
func headerSubset(header []byte, names []string, not bool) []byte {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	var b bytes.Buffer
	for _, f := range rawFields(header) {
		if want[strings.ToLower(f.name)] != not {
			b.Write(f.data)
			if !bytes.HasSuffix(f.data, []byte("\n")) {
				b.WriteString("\r\n")
			}
		}
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// lines counts the lines of a body (RFC 3501 7.4.2, "size in text lines").
func lines(body []byte) int {
	n := bytes.Count(body, []byte("\n"))
	if len(body) > 0 && body[len(body)-1] != '\n' {
		n++
	}
	return n
}

func nstr(v string, ok bool, utf8ok bool) string {
	if !ok {
		return "NIL"
	}
	return quote(v, utf8ok)
}

// paramList writes parameters as ("NAME" "value" ...), or NIL.
func paramList(params map[string]string, utf8ok bool) string {
	if len(params) == 0 {
		return "NIL"
	}
	names := make([]string, 0, len(params))
	for n := range params {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteByte('(')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quote(strings.ToUpper(n), utf8ok))
		b.WriteByte(' ')
		b.WriteString(quote(params[n], utf8ok))
	}
	b.WriteByte(')')
	return b.String()
}

// bodyStructure writes BODY (ext false) or BODYSTRUCTURE (RFC 3501 7.4.2).
func (e *entity) bodyStructure(ext, utf8ok bool) string {
	var b strings.Builder
	e.writeStructure(&b, ext, utf8ok)
	return b.String()
}

func (e *entity) writeStructure(b *strings.Builder, ext, utf8ok bool) {
	b.WriteByte('(')
	if len(e.parts) > 0 {
		for _, p := range e.parts {
			p.writeStructure(b, ext, utf8ok)
		}
		b.WriteByte(' ')
		b.WriteString(quote(strings.ToUpper(e.sub), utf8ok))
		if ext {
			b.WriteByte(' ')
			b.WriteString(paramList(e.params, utf8ok))
			e.writeExtension(b, utf8ok)
		}
		b.WriteByte(')')
		return
	}
	typ, sub, params := e.typ, e.sub, e.params
	if typ == "multipart" {
		// A multipart without parts cannot be written as one (the grammar
		// wants at least one body): it is shown as the text it is.
		typ, sub, params = "text", "plain", nil
	}
	if typ == "text" && params["charset"] == "" && !haveHeader(e, "Content-Type") {
		params = map[string]string{"charset": "US-ASCII"} // RFC 2045 5.2
	}
	b.WriteString(quote(strings.ToUpper(typ), utf8ok))
	b.WriteByte(' ')
	b.WriteString(quote(strings.ToUpper(sub), utf8ok))
	b.WriteByte(' ')
	b.WriteString(paramList(params, utf8ok))
	for _, name := range []string{"Content-ID", "Content-Description"} {
		v, ok := e.get(name)
		b.WriteByte(' ')
		b.WriteString(nstr(v, ok, utf8ok))
	}
	b.WriteByte(' ')
	b.WriteString(quote(e.encoding(), utf8ok))
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(len(e.body())))
	switch {
	case e.inner != nil:
		b.WriteByte(' ')
		b.WriteString(e.inner.envelope(utf8ok))
		b.WriteByte(' ')
		e.inner.writeStructure(b, ext, utf8ok)
		b.WriteByte(' ')
		b.WriteString(strconv.Itoa(lines(e.body())))
	case typ == "text":
		b.WriteByte(' ')
		b.WriteString(strconv.Itoa(lines(e.body())))
	}
	if ext {
		v, ok := e.get("Content-MD5")
		b.WriteByte(' ')
		b.WriteString(nstr(v, ok, utf8ok))
		e.writeExtension(b, utf8ok)
	}
	b.WriteByte(')')
}

func haveHeader(e *entity, name string) bool {
	_, ok := message.Get(e.hdr, name)
	return ok
}

// writeExtension writes disposition, language and location.
func (e *entity) writeExtension(b *strings.Builder, utf8ok bool) {
	b.WriteByte(' ')
	if v, ok := message.Get(e.hdr, "Content-Disposition"); ok {
		if disp, params := message.Params(v); disp != "" {
			b.WriteString("(" + quote(disp, utf8ok) + " " + paramList(params, utf8ok) + ")")
		} else {
			b.WriteString("NIL")
		}
	} else {
		b.WriteString("NIL")
	}
	b.WriteByte(' ')
	if v, ok := e.get("Content-Language"); ok && v != "" {
		var tags []string
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, quote(t, utf8ok))
			}
		}
		switch len(tags) {
		case 0:
			b.WriteString("NIL")
		case 1:
			b.WriteString(tags[0])
		default:
			b.WriteString("(" + strings.Join(tags, " ") + ")")
		}
	} else {
		b.WriteString("NIL")
	}
	v, ok := e.get("Content-Location")
	b.WriteByte(' ')
	b.WriteString(nstr(v, ok, utf8ok))
}

// envelope writes ENVELOPE (RFC 3501 7.4.2): date, subject, from, sender,
// reply-to, to, cc, bcc, in-reply-to, message-id. Sender and Reply-To are
// those of From when the message has none.
func (e *entity) envelope(utf8ok bool) string {
	field := func(name string) string {
		v, ok := e.get(name)
		return nstr(v, ok, utf8ok)
	}
	addrs := func(name, fallback string) string {
		v, ok := message.Get(e.hdr, name)
		list := "NIL"
		if ok {
			list = addressList(v, utf8ok)
		}
		// "absent ... or present but empty".
		if list == "NIL" && fallback != "" {
			if v, ok = message.Get(e.hdr, fallback); ok {
				list = addressList(v, utf8ok)
			}
		}
		return list
	}
	return "(" + strings.Join([]string{
		field("Date"), field("Subject"),
		addrs("From", ""), addrs("Sender", "From"), addrs("Reply-To", "From"),
		addrs("To", ""), addrs("Cc", ""), addrs("Bcc", ""),
		field("In-Reply-To"), field("Message-ID"),
	}, " ") + ")"
}

// addressList writes an address list: (name route mailbox host) for each
// mailbox; a group is (NIL NIL name NIL), its members, (NIL NIL NIL NIL).
func addressList(v string, utf8ok bool) string {
	var out []string
	one := func(a message.Address) {
		name := "NIL"
		if a.Name != nil && *a.Name != "" {
			// The display name comes decoded; a name outside ASCII goes
			// back into an encoded word (RFC 2047), as it was sent.
			name = quote(mime.QEncoding.Encode("utf-8", *a.Name), utf8ok)
		}
		local, host := a.Email, ""
		if at := strings.LastIndexByte(a.Email, '@'); at >= 0 {
			local, host = a.Email[:at], a.Email[at+1:]
		}
		out = append(out, "("+name+" NIL "+quote(local, utf8ok)+" "+quote(host, utf8ok)+")")
	}
	for _, g := range message.GroupedAddresses(v) {
		if g.Name != nil {
			out = append(out, "(NIL NIL "+quote(*g.Name, utf8ok)+" NIL)")
		}
		for _, a := range g.Addresses {
			one(a)
		}
		if g.Name != nil {
			out = append(out, "(NIL NIL NIL NIL)")
		}
	}
	if len(out) == 0 {
		return "NIL"
	}
	return "(" + strings.Join(out, "") + ")"
}

// decoded undoes the Content-Transfer-Encoding of a part's body, for
// BINARY (RFC 3516). ok is false for an encoding that is not known.
func (e *entity) decoded() (data []byte, ok bool) {
	body := e.body()
	switch e.encoding() {
	case "7BIT", "8BIT", "BINARY":
		return body, true
	case "BASE64":
		clean := make([]byte, 0, len(body))
		for _, c := range body {
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' {
				clean = append(clean, c)
			}
		}
		out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
		n, _ := base64.RawStdEncoding.Decode(out, clean)
		return out[:n], true
	case "QUOTED-PRINTABLE":
		out, _ := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		return out, true
	}
	return nil, false
}
