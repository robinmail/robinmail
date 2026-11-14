package imap

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// fetchItem is one data item of a FETCH (RFC 3501 6.4.5).
type fetchItem struct {
	// kind is the item name; "SECTION" for BODY[...], "BINARY" and
	// "BINARY.SIZE" for RFC 3516.
	kind string
	peek bool
	// A section: the part number, then what of the part.
	path   []int
	text   string // "", HEADER, HEADER.FIELDS, HEADER.FIELDS.NOT, TEXT, MIME
	fields []string
	// partial is <offset.count>.
	partial       bool
	offset, count int64
	// label is the item's name in the response.
	label string
}

// needsMessage reports whether the item is answered from the message
// itself and not from what the session holds.
func (it fetchItem) needsMessage() bool {
	switch it.kind {
	case "FLAGS", "INTERNALDATE", "RFC822.SIZE", "UID", "MODSEQ":
		return false
	}
	return true
}

// marksSeen: RFC 3501 6.4.5, the items that set \Seen.
func (it fetchItem) marksSeen() bool {
	switch it.kind {
	case "RFC822", "RFC822.TEXT":
		return true
	case "SECTION", "BINARY":
		return !it.peek
	}
	return false
}

func itemNameChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.'
}

// fetchItem reads one item.
func (p *parser) fetchItem() (fetchItem, error) {
	name := strings.ToUpper(p.takeWhile(itemNameChar))
	it := fetchItem{kind: name, label: name}
	switch name {
	case "ENVELOPE", "FLAGS", "INTERNALDATE", "RFC822", "RFC822.HEADER", "RFC822.SIZE", "RFC822.TEXT", "BODYSTRUCTURE", "UID", "MODSEQ":
		return it, nil
	case "BODY", "BODY.PEEK", "BINARY", "BINARY.PEEK", "BINARY.SIZE":
	default:
		return it, bad("unknown FETCH item %q", clip(name, 30))
	}
	if p.peek() != '[' {
		if name == "BODY" {
			return it, nil // the structure without extension data
		}
		return it, bad("%s needs a section", name)
	}
	p.pos++
	binary := strings.HasPrefix(name, "BINARY")
	it.peek = strings.HasSuffix(name, ".PEEK")
	switch {
	case name == "BINARY.SIZE":
		it.kind = "BINARY.SIZE"
	case binary:
		it.kind = "BINARY"
	default:
		it.kind = "SECTION"
	}
	// Part numbers.
	for p.peek() >= '0' && p.peek() <= '9' {
		n, err := p.number()
		if err != nil {
			return it, err
		}
		if n == 0 || len(it.path) >= maxMIMEDepth*2 {
			return it, bad("not a part number")
		}
		it.path = append(it.path, int(n))
		if !p.skip('.') {
			break
		}
	}
	if p.peek() != ']' {
		if binary {
			return it, bad("a BINARY section is a part number")
		}
		it.text = strings.ToUpper(p.takeWhile(itemNameChar))
		if it.text == "" {
			return it, bad(`"]" is missing`)
		}
		switch it.text {
		case "HEADER", "TEXT":
		case "MIME":
			if len(it.path) == 0 {
				return it, bad("MIME needs a part number")
			}
		case "HEADER.FIELDS", "HEADER.FIELDS.NOT":
			if err := p.space(); err != nil {
				return it, err
			}
			if err := p.expect('('); err != nil {
				return it, err
			}
			for !p.skip(')') {
				if len(it.fields) > 0 {
					if err := p.space(); err != nil {
						return it, err
					}
				}
				f, err := p.astring()
				if err != nil {
					return it, err
				}
				if it.fields = append(it.fields, f); len(it.fields) > 200 {
					return it, bad("too many header fields")
				}
			}
			if len(it.fields) == 0 {
				return it, bad("the list of header fields is empty")
			}
		default:
			return it, bad("unknown section %q", clip(it.text, 30))
		}
	}
	if err := p.expect(']'); err != nil {
		return it, err
	}
	if p.skip('<') {
		var err error
		if it.offset, err = p.number64(); err != nil {
			return it, err
		}
		if err := p.expect('.'); err != nil {
			return it, err
		}
		if it.count, err = p.number64(); err != nil {
			return it, err
		}
		if it.count == 0 {
			return it, bad("a partial fetch of 0 octets")
		}
		if err := p.expect('>'); err != nil {
			return it, err
		}
		if name == "BINARY.SIZE" {
			return it, bad("BINARY.SIZE takes no partial")
		}
		it.partial = true
	}

	// The name in the response: the section as asked, without .PEEK.
	var b strings.Builder
	switch it.kind {
	case "BINARY.SIZE":
		b.WriteString("BINARY.SIZE[")
	case "BINARY":
		b.WriteString("BINARY[")
	default:
		b.WriteString("BODY[")
	}
	for i, n := range it.path {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.Itoa(n))
	}
	if it.text != "" {
		if len(it.path) > 0 {
			b.WriteByte('.')
		}
		b.WriteString(it.text)
	}
	if len(it.fields) > 0 {
		b.WriteString(" (")
		for i, f := range it.fields {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(quoteName(f, false))
		}
		b.WriteByte(')')
	}
	b.WriteByte(']')
	if it.partial {
		b.WriteString("<" + strconv.FormatInt(it.offset, 10) + ">")
	}
	it.label = b.String()
	return it, nil
}

// fetchItems reads the items of a FETCH: a macro, one item, or a list.
func (p *parser) fetchItems() ([]fetchItem, error) {
	mk := func(names ...string) []fetchItem {
		out := make([]fetchItem, len(names))
		for i, n := range names {
			out[i] = fetchItem{kind: n, label: n}
		}
		return out
	}
	if !p.skip('(') {
		// RFC 3501 6.4.5: the macros stand alone.
		rest := strings.ToUpper(p.cur())
		for _, m := range []struct {
			name  string
			items []fetchItem
		}{
			{"ALL", mk("FLAGS", "INTERNALDATE", "RFC822.SIZE", "ENVELOPE")},
			{"FAST", mk("FLAGS", "INTERNALDATE", "RFC822.SIZE")},
			{"FULL", mk("FLAGS", "INTERNALDATE", "RFC822.SIZE", "ENVELOPE", "BODY")},
		} {
			if rest == m.name || strings.HasPrefix(rest, m.name+" ") {
				p.pos += len(m.name)
				return m.items, nil
			}
		}
		it, err := p.fetchItem()
		if err != nil {
			return nil, err
		}
		return []fetchItem{it}, nil
	}
	var items []fetchItem
	for !p.skip(')') {
		if len(items) > 0 {
			if err := p.space(); err != nil {
				return nil, err
			}
		}
		it, err := p.fetchItem()
		if err != nil {
			return nil, err
		}
		if items = append(items, it); len(items) > 100 {
			return nil, bad("too many FETCH items")
		}
	}
	if len(items) == 0 {
		return nil, bad("FETCH needs an item")
	}
	return items, nil
}

// section returns the octets a BODY[...] item names, or ok false for a
// part the message does not have.
func (it fetchItem) section(root *entity) (data []byte, ok bool) {
	e := root
	if len(it.path) > 0 {
		if e = root.part(it.path); e == nil {
			return nil, false
		}
	}
	switch it.text {
	case "":
		if len(it.path) == 0 {
			return e.whole(), true
		}
		return e.body(), true
	case "MIME":
		return e.header(), true
	}
	// HEADER and TEXT of a part are those of the message it holds.
	if len(it.path) > 0 {
		if e.inner == nil {
			return nil, false
		}
		e = e.inner
	}
	switch it.text {
	case "HEADER":
		return e.header(), true
	case "TEXT":
		return e.body(), true
	case "HEADER.FIELDS":
		return headerSubset(e.header(), it.fields, false), true
	case "HEADER.FIELDS.NOT":
		return headerSubset(e.header(), it.fields, true), true
	}
	return nil, false
}

// cut applies a partial fetch.
func (it fetchItem) cut(data []byte) []byte {
	if !it.partial {
		return data
	}
	if it.offset >= int64(len(data)) {
		return nil
	}
	data = data[it.offset:]
	if it.count < int64(len(data)) {
		data = data[:it.count]
	}
	return data
}

func writeLiteral(b *bytes.Buffer, data []byte, binary bool) {
	if binary && bytes.IndexByte(data, 0) >= 0 {
		b.WriteByte('~') // literal8, RFC 3516 4.2
	}
	b.WriteByte('{')
	b.WriteString(strconv.Itoa(len(data)))
	b.WriteString("}\r\n")
	b.Write(data)
}

// errUnknownCTE is a BINARY fetch of a part whose encoding cannot be
// undone.
var errUnknownCTE = errors.New("imap: unknown content transfer encoding")

// cmdFetch is RFC 3501 6.4.5, with CHANGEDSINCE and MODSEQ of RFC 7162
// 3.1.4 and BINARY of RFC 3516.
func (c *conn) cmdFetch(tag string, p *parser, uid bool) (reply, error) {
	name := "FETCH"
	if uid {
		name = "UID FETCH"
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	set, err := p.seqSet()
	if err != nil {
		return reply{}, err
	}
	if err := p.space(); err != nil {
		return reply{}, err
	}
	items, err := p.fetchItems()
	if err != nil {
		return reply{}, err
	}
	changedSince := int64(-1)
	if p.peek() == ' ' {
		p.pos++
		if err := p.expect('('); err != nil {
			return reply{}, err
		}
		if w, _ := p.word(); w != "CHANGEDSINCE" {
			return reply{}, bad("unknown FETCH modifier")
		}
		if err := p.space(); err != nil {
			return reply{}, err
		}
		if changedSince, err = p.number64(); err != nil {
			return reply{}, err
		}
		if err := p.expect(')'); err != nil {
			return reply{}, err
		}
	}
	if err := p.end(); err != nil {
		return reply{}, err
	}
	has := func(kind string) bool {
		for _, it := range items {
			if it.kind == kind {
				return true
			}
		}
		return false
	}
	// RFC 3501 6.4.8: UID FETCH always returns the UID.
	if uid && !has("UID") {
		items = append([]fetchItem{{kind: "UID", label: "UID"}}, items...)
	}
	// RFC 7162 3.1.4.1: CHANGEDSINCE implies MODSEQ; both enable CONDSTORE.
	if changedSince >= 0 && !has("MODSEQ") {
		items = append(items, fetchItem{kind: "MODSEQ", label: "MODSEQ"})
	}
	if has("MODSEQ") {
		c.condstore = true
	}
	needMessage := false
	for _, it := range items {
		needMessage = needMessage || it.needsMessage()
	}
	idx, err := c.indexes(set, uid)
	if err != nil {
		return reply{}, err
	}

	s := c.sel
	var seen []int
	var out bytes.Buffer
	for _, i := range idx {
		m := s.msgs[i]
		if changedSince >= 0 && m.modseq <= changedSince {
			continue
		}
		var root *entity
		if needMessage {
			raw, err := c.user.Open(c.ctx, m.id)
			if errors.Is(err, ErrNoMessage) {
				// Expunged by another session since this one last looked
				// (RFC 2180 4.1.2): the message is left out, and the next
				// command that may say so says EXPUNGE.
				s.dirty = true
				continue
			}
			if err != nil {
				return reply{}, err
			}
			root = parseEntity(raw, 0, len(raw), 0, "text/plain")
		}
		out.Reset()
		out.WriteString("* " + strconv.Itoa(i+1) + " FETCH (")
		markSeen := false
		for k, it := range items {
			if k > 0 {
				out.WriteByte(' ')
			}
			out.WriteString(it.label)
			out.WriteByte(' ')
			switch it.kind {
			case "UID":
				out.WriteString(strconv.FormatUint(uint64(m.uid), 10))
			case "FLAGS":
				out.WriteString("(" + wireFlags(m.flags) + ")")
			case "INTERNALDATE":
				out.WriteString(`"` + formatDateTime(m.date) + `"`)
			case "RFC822.SIZE":
				out.WriteString(strconv.FormatInt(m.size, 10))
			case "MODSEQ":
				out.WriteString("(" + strconv.FormatInt(m.modseq, 10) + ")")
			case "ENVELOPE":
				out.WriteString(root.envelope(c.utf8))
			case "BODY":
				out.WriteString(root.bodyStructure(false, c.utf8))
			case "BODYSTRUCTURE":
				out.WriteString(root.bodyStructure(true, c.utf8))
			case "RFC822":
				writeLiteral(&out, root.whole(), false)
			case "RFC822.HEADER":
				writeLiteral(&out, root.header(), false)
			case "RFC822.TEXT":
				writeLiteral(&out, root.body(), false)
			case "SECTION":
				if data, ok := it.section(root); ok {
					writeLiteral(&out, it.cut(data), false)
				} else {
					out.WriteString("NIL")
				}
			case "BINARY", "BINARY.SIZE":
				e := root
				if len(it.path) > 0 {
					e = root.part(it.path)
				}
				var data []byte
				ok := e != nil
				if ok && len(it.path) == 0 {
					data = e.whole()
				} else if ok {
					if data, ok = e.decoded(); !ok {
						// RFC 3516 4.3: the whole command fails.
						return noCode("UNKNOWN-CTE", "a part has a content transfer encoding that cannot be decoded"), nil
					}
				}
				switch {
				case !ok && it.kind == "BINARY.SIZE":
					out.WriteString("0")
				case !ok:
					out.WriteString("NIL")
				case it.kind == "BINARY.SIZE":
					out.WriteString(strconv.Itoa(len(data)))
				default:
					writeLiteral(&out, it.cut(data), true)
				}
			}
			markSeen = markSeen || it.marksSeen()
		}
		out.WriteString(")\r\n")
		if _, err := c.bw.Write(out.Bytes()); err != nil {
			c.state = stateLogout
			return reply{}, nil
		}
		if markSeen && !s.readOnly && !hasFlag(m.flags, "$seen") {
			seen = append(seen, i)
		}
	}
	if len(seen) > 0 {
		// RFC 3501 6.4.5: "the \Seen flag is implicitly set; if this causes
		// the flags to change, they SHOULD be included as part of the
		// FETCH responses". They follow as FETCH responses of their own.
		if _, err := c.user.Store(c.ctx, s.mailbox, s.uidRanges(seen), FlagsAdd, []string{"$seen"}, -1); err != nil {
			return reply{}, err
		}
		if _, err := c.sync(uid, nil); err != nil {
			return reply{}, err
		}
	}
	return ok(name + " completed"), nil
}
