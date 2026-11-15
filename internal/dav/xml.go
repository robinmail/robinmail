package dav

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"sort"
	"strings"
)

// Namespaces.
const (
	nsDAV     = "DAV:"
	nsCalDAV  = "urn:ietf:params:xml:ns:caldav"
	nsCardDAV = "urn:ietf:params:xml:ns:carddav"
	nsCS      = "http://calendarserver.org/ns/"
	nsApple   = "http://apple.com/ns/ical/"
)

var prefixes = map[string]string{nsDAV: "d", nsCalDAV: "c", nsCardDAV: "card", nsCS: "cs", nsApple: "ical"}

func name(ns, local string) xml.Name { return xml.Name{Space: ns, Local: local} }

// node is an element of a request body.
type node struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Children []*node
	Text     string
}

// child returns the first child of a name, or nil.
func (n *node) child(ns, local string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name.Space == ns && c.Name.Local == local {
			return c
		}
	}
	return nil
}

func (n *node) all(ns, local string) []*node {
	var out []*node
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name.Space == ns && c.Name.Local == local {
			out = append(out, c)
		}
	}
	return out
}

func (n *node) attr(local string) (string, bool) {
	for _, a := range n.Attrs {
		if a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

// Limits of a request body.
const (
	maxBody  = 2 << 20
	maxDepth = 32
	maxNodes = 20_000
)

var errXML = errors.New("dav: the body is not well-formed XML of the expected kind")

// parseXML reads a request body into a tree. An empty body is nil.
func parseXML(r io.Reader) (*node, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, errXML
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	// No external entities: encoding/xml resolves none, and DTDs are
	// refused below.
	var stack []*node
	var root *node
	count := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errXML
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if count++; count > maxNodes || len(stack) >= maxDepth {
				return nil, errXML
			}
			n := &node{Name: t.Name, Attrs: t.Attr}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.Children = append(p.Children, n)
			} else if root == nil {
				root = n
			} else {
				return nil, errXML
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(t)
			}
		case xml.Directive:
			return nil, errXML
		}
	}
	if root == nil {
		return nil, errXML
	}
	return root, nil
}

// writer builds a multistatus response with fixed prefixes.
type writer struct {
	b     bytes.Buffer
	extra map[string]string // namespace -> prefix, for other namespaces
}

func esc(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// tag writes the qualified name of an element.
func (w *writer) tag(n xml.Name) string {
	if p, ok := prefixes[n.Space]; ok {
		return p + ":" + n.Local
	}
	if n.Space == "" {
		return n.Local
	}
	if w.extra == nil {
		w.extra = map[string]string{}
	}
	p, ok := w.extra[n.Space]
	if !ok {
		p = "x" + string(rune('0'+len(w.extra)%10)) + string(rune('a'+len(w.extra)/10%26))
		w.extra[n.Space] = p
	}
	return p + ":" + n.Local
}

// el is "<name>inner</name>", or empty "<name/>".
func (w *writer) el(n xml.Name, inner string) string {
	t := w.tag(n)
	if inner == "" {
		return "<" + t + "/>"
	}
	return "<" + t + ">" + inner + "</" + t + ">"
}

func (w *writer) text(n xml.Name, s string) string { return w.el(n, esc(s)) }

func (w *writer) href(h string) string { return "<d:href>" + esc(h) + "</d:href>" }

// propstat is one response element of a multistatus.
type response struct {
	href    string
	status  int // for a response without props (404 for a missing href)
	found   map[xml.Name]string
	missing []xml.Name
	denied  []xml.Name
	extra   string // more inside the response (sync-collection, errors)
}

func statusLine(code int) string {
	return "HTTP/1.1 " + itoa(code) + " " + statusText(code)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		d = append([]byte{'-'}, d...)
	}
	return string(d)
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 409:
		return "Conflict"
	case 424:
		return "Failed Dependency"
	case 507:
		return "Insufficient Storage"
	}
	return "Status"
}

func (w *writer) response(r response) {
	w.b.WriteString("<d:response>")
	w.b.WriteString(w.href(r.href))
	if r.status != 0 {
		w.b.WriteString("<d:status>" + statusLine(r.status) + "</d:status>")
	}
	write := func(code int, props []string) {
		if len(props) == 0 {
			return
		}
		w.b.WriteString("<d:propstat><d:prop>")
		for _, p := range props {
			w.b.WriteString(p)
		}
		w.b.WriteString("</d:prop><d:status>" + statusLine(code) + "</d:status></d:propstat>")
	}
	if r.found != nil {
		names := make([]xml.Name, 0, len(r.found))
		for n := range r.found {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool {
			if names[i].Space != names[j].Space {
				return names[i].Space < names[j].Space
			}
			return names[i].Local < names[j].Local
		})
		var props []string
		for _, n := range names {
			props = append(props, w.el(n, r.found[n]))
		}
		write(200, props)
	}
	var miss []string
	for _, n := range r.missing {
		miss = append(miss, w.el(n, ""))
	}
	write(404, miss)
	var den []string
	for _, n := range r.denied {
		den = append(den, w.el(n, ""))
	}
	write(403, den)
	w.b.WriteString(r.extra)
	w.b.WriteString("</d:response>")
}

// multistatus wraps the responses written so far.
func (w *writer) multistatus(tail string) []byte {
	var out bytes.Buffer
	out.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	out.WriteString(`<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:card="urn:ietf:params:xml:ns:carddav"` +
		` xmlns:cs="http://calendarserver.org/ns/" xmlns:ical="http://apple.com/ns/ical/"`)
	var spaces []string
	for ns := range w.extra {
		spaces = append(spaces, ns)
	}
	sort.Strings(spaces)
	for _, ns := range spaces {
		out.WriteString(` xmlns:` + w.extra[ns] + `="` + esc(ns) + `"`)
	}
	out.WriteString(">")
	out.Write(w.b.Bytes())
	out.WriteString(tail)
	out.WriteString("</d:multistatus>")
	return out.Bytes()
}

// xmlError is the body of an error with a precondition (RFC 4918 16).
func xmlError(ns, cond string) []byte {
	p := prefixes[ns]
	if p == "" {
		p = "x"
	}
	nsDecl := ` xmlns:` + p + `="` + ns + `"`
	if ns == nsDAV {
		nsDecl = ""
	}
	return []byte(`<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<d:error xmlns:d="DAV:"` + nsDecl + `><` + p + `:` + cond + `/></d:error>`)
}

// inner renders the children of a node as XML again (for dead
// properties, which are kept as the client wrote them).
func inner(n *node) string {
	var b strings.Builder
	var walk func(n *node)
	walk = func(n *node) {
		for _, c := range n.Children {
			b.WriteString("<" + c.Name.Local + ` xmlns="` + esc(c.Name.Space) + `">`)
			b.WriteString(esc(c.Text))
			walk(c)
			b.WriteString("</" + c.Name.Local + ">")
		}
	}
	if len(n.Children) == 0 {
		return esc(n.Text)
	}
	walk(n)
	return b.String()
}
