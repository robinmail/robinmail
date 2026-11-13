package message

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// ViewOptions decide what a sanitised HTML view may load.
type ViewOptions struct {
	// RemoteImages keeps <img> with an http or https source. Off, such an
	// image loses its source and View.BlockedRemote is set.
	RemoteImages bool
	// CID turns the Content-ID of a "cid:" image into a data: URL, or ""
	// if the message has no such part. nil drops all cid: images.
	CID func(cid string) string
}

// View is a sanitised HTML fragment.
type View struct {
	// HTML is safe to put in the body of a document: it holds only the
	// elements and attributes on the lists below.
	HTML string
	// BlockedRemote is true if a remote image was left out.
	BlockedRemote bool
}

// Limits of the sanitiser.
const (
	maxViewInput   = 4 << 20 // bytes of HTML read
	maxViewDepth   = 256     // nesting kept; deeper content is dropped
	maxAttrLen     = 2048
	maxDataURL     = 4 << 20
	maxStyleLen    = 4096
	maxStyleValLen = 256
)

// Elements that are kept. Everything else is unwrapped: its children stay,
// the element goes. That makes this an allowlist: a tag nobody thought of
// has no way in.
var viewElements = set("a", "abbr", "address", "article", "b", "bdi", "bdo", "big", "blockquote", "br",
	"caption", "center", "cite", "code", "col", "colgroup", "dd", "del", "details", "dfn", "div", "dl", "dt",
	"em", "figcaption", "figure", "font", "footer", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hr", "i",
	"img", "ins", "kbd", "li", "main", "mark", "nav", "ol", "p", "pre", "q", "s", "samp", "section", "small",
	"span", "strike", "strong", "sub", "summary", "sup", "table", "tbody", "td", "tfoot", "th", "thead", "tr",
	"tt", "u", "ul", "var", "wbr")

// Elements whose content is not text to show: dropped with all they hold.
var viewDropped = set("script", "style", "head", "title", "iframe", "frame", "frameset", "noframes", "object",
	"embed", "applet", "noscript", "noembed", "template", "svg", "math", "audio", "video", "source", "track",
	"canvas", "link", "meta", "base", "select", "option", "optgroup", "textarea", "input", "button", "datalist",
	"map", "area", "plaintext", "xmp", "listing", "slot", "portal", "dialog")

var viewVoid = set("br", "hr", "img", "col", "wbr")

// Attributes kept on any element. No id, class or name: without scripts
// and style sheets they do nothing, and they are how markup reaches for
// things outside itself.
var viewAttrs = set("align", "alt", "bgcolor", "border", "cellpadding", "cellspacing", "color", "colspan",
	"dir", "face", "height", "lang", "nowrap", "rowspan", "size", "span", "start", "title", "valign", "width")

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// SanitizeHTML turns the HTML of a message into a fragment that cannot run
// script, submit a form, or load anything but what opt allows.
//
// It is built on the parser of golang.org/x/net/html, which reads HTML the
// way a browser does, and writes the result itself from an allowlist of
// elements and attributes, so what the browser later parses is our own
// well-formed output and not the sender's bytes. It is one of two layers:
// the view is also served with a Content-Security-Policy that forbids
// script and remote loads (ViewCSP), into a sandboxed iframe.
func SanitizeHTML(src string, opt ViewOptions) View {
	if len(src) > maxViewInput {
		src = src[:maxViewInput]
	}
	src = strings.ToValidUTF8(src, "\uFFFD")
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		// The parser refuses input nested deeper than its own limit.
		return View{HTML: html.EscapeString(htmlText(src, maxViewInput))}
	}
	s := &sanitizer{opt: opt}
	s.walk(doc, 0)
	return View{HTML: s.out.String(), BlockedRemote: s.blocked}
}

type sanitizer struct {
	opt     ViewOptions
	out     strings.Builder
	blocked bool
}

func (s *sanitizer) walk(n *html.Node, depth int) {
	if depth > maxViewDepth {
		return
	}
	switch n.Type {
	case html.TextNode:
		s.out.WriteString(html.EscapeString(n.Data))
		return
	case html.DocumentNode:
		s.children(n, depth)
		return
	case html.ElementNode:
	default:
		return // comments, doctypes
	}
	// Foreign content (SVG, MathML) has its own parsing rules and its own
	// ways to run script. None of it is kept.
	if n.Namespace != "" || viewDropped[n.Data] {
		return
	}
	if !viewElements[n.Data] {
		s.children(n, depth)
		return
	}
	s.out.WriteByte('<')
	s.out.WriteString(n.Data)
	s.attrs(n)
	s.out.WriteByte('>')
	if viewVoid[n.Data] {
		return
	}
	s.children(n, depth)
	s.out.WriteString("</")
	s.out.WriteString(n.Data)
	s.out.WriteByte('>')
}

func (s *sanitizer) children(n *html.Node, depth int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		s.walk(c, depth+1)
	}
}

func (s *sanitizer) attr(name, value string) {
	s.out.WriteByte(' ')
	s.out.WriteString(name)
	s.out.WriteString(`="`)
	s.out.WriteString(html.EscapeString(value))
	s.out.WriteByte('"')
}

func (s *sanitizer) attrs(n *html.Node) {
	seen := map[string]bool{}
	for _, a := range n.Attr {
		if a.Namespace != "" || seen[a.Key] || len(a.Val) > maxDataURL {
			continue
		}
		seen[a.Key] = true
		switch {
		case a.Key == "style":
			if css := safeStyle(a.Val); css != "" {
				s.attr("style", css)
			}
		case a.Key == "href" && n.Data == "a":
			if u, ok := safeLink(a.Val); ok {
				s.attr("href", u)
				// A link opens outside the view and tells the target nothing.
				s.attr("target", "_blank")
				s.attr("rel", "noopener noreferrer nofollow")
			}
		case a.Key == "src" && n.Data == "img":
			if u := s.imageSource(a.Val); u != "" {
				s.attr("src", u)
				s.attr("referrerpolicy", "no-referrer")
			}
		case viewAttrs[a.Key] && len(a.Val) <= maxAttrLen:
			s.attr(a.Key, a.Val)
		}
	}
}

// cleanURL removes what a browser ignores inside a URL before it looks at
// the scheme: white space around it and tabs and line breaks anywhere, so
// "java\tscript:" is seen as what it is.
func cleanURL(raw string) string {
	raw = strings.TrimFunc(raw, func(r rune) bool { return r <= ' ' })
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, raw)
}

// safeLink accepts absolute http, https and mailto links.
func safeLink(raw string) (string, bool) {
	raw = cleanURL(raw)
	if len(raw) > maxAttrLen {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return "", false
		}
	case "mailto":
	default:
		return "", false
	}
	return u.String(), true
}

// dataImagePrefixes are the inline images a view may hold. No SVG: it is a
// document that can carry script.
var dataImagePrefixes = []string{"data:image/png;base64,", "data:image/gif;base64,", "data:image/jpeg;base64,",
	"data:image/webp;base64,", "data:image/bmp;base64,"}

func isDataImage(u string) bool {
	for _, p := range dataImagePrefixes {
		if len(u) >= len(p) && strings.EqualFold(u[:len(p)], p) {
			for i := len(p); i < len(u); i++ {
				c := u[i]
				if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
					return false
				}
			}
			return true
		}
	}
	return false
}

func (s *sanitizer) imageSource(raw string) string {
	raw = cleanURL(raw)
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "cid:"):
		if s.opt.CID == nil {
			return ""
		}
		cid, err := url.PathUnescape(raw[4:])
		if err != nil {
			cid = raw[4:]
		}
		if u := s.opt.CID(cid); isDataImage(u) {
			return u
		}
		return ""
	case strings.HasPrefix(lower, "data:"):
		if isDataImage(raw) {
			return raw
		}
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || len(raw) > maxAttrLen || u.Host == "" {
		return ""
	}
	if sch := strings.ToLower(u.Scheme); sch != "http" && sch != "https" {
		return ""
	}
	if !s.opt.RemoteImages {
		s.blocked = true
		return ""
	}
	return u.String()
}

// CSS properties kept in a style attribute, by exact name or by prefix.
var (
	styleProps = set("color", "background-color", "background", "font", "font-family", "font-size", "font-style",
		"font-weight", "font-variant", "line-height", "text-align", "text-decoration", "text-indent",
		"text-transform", "vertical-align", "white-space", "word-break", "word-wrap", "overflow-wrap",
		"letter-spacing", "word-spacing", "width", "height", "max-width", "min-width", "max-height", "min-height",
		"display", "float", "clear", "list-style-type", "list-style-position", "table-layout", "direction",
		"border-collapse", "border-spacing", "margin", "padding", "border")
	stylePrefixes = []string{"margin-", "padding-", "border-"}
	// Functions a value may call. Not url(), image-set(), var(), attr(),
	// expression() or anything else that loads or computes.
	styleFuncs = set("rgb", "rgba", "hsl", "hsla")
)

// safeStyle keeps the declarations of a style attribute that only draw:
// known properties with values made of plain characters and colour
// functions. A declaration it does not understand is dropped whole.
func safeStyle(css string) string {
	if len(css) > maxStyleLen {
		return ""
	}
	var out []string
	for _, decl := range strings.Split(css, ";") {
		prop, val, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		val = strings.TrimSpace(val)
		if !styleProps[prop] {
			known := false
			for _, p := range stylePrefixes {
				known = known || strings.HasPrefix(prop, p)
			}
			if !known {
				continue
			}
		}
		for i := 0; i < len(prop); i++ {
			if c := prop[i]; !(c >= 'a' && c <= 'z' || c == '-') {
				prop = ""
				break
			}
		}
		if prop == "" || val == "" || len(val) > maxStyleValLen || !safeStyleValue(val) {
			continue
		}
		out = append(out, prop+": "+val)
	}
	return strings.Join(out, "; ")
}

func safeStyleValue(val string) bool {
	word := 0 // start of the identifier in front of the current position
	for i := 0; i < len(val); i++ {
		c := val[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-':
			continue
		case c == '(':
			if !styleFuncs[strings.ToLower(val[word:i])] {
				return false
			}
		case c >= '0' && c <= '9' || strings.IndexByte(" .,#%)!+'\"", c) >= 0:
		default:
			// Backslashes (CSS escapes), "/" (comments), "@", "<", ":" and
			// everything outside ASCII.
			return false
		}
		word = i + 1
	}
	return true
}

// viewStyle is the only style sheet of a view.
const viewStyle = `body{font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;margin:12px;overflow-wrap:break-word}` +
	`img{max-width:100%;height:auto}pre{white-space:pre-wrap}` +
	`blockquote{margin:0 0 0 .8em;padding-left:.8em;border-left:2px solid #ccc}table{max-width:100%}`

// ViewDocument wraps sanitised fragments in a complete HTML document.
func ViewDocument(body string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="no-referrer">` +
		`<meta name="color-scheme" content="light"><style>` + viewStyle + `</style></head><body>` + body + `</body></html>`
}

// TextView renders a plain text body for the view.
func TextView(text string) string {
	return "<pre>" + html.EscapeString(text) + "</pre>"
}

// ViewCSP is the Content-Security-Policy a view is served with. Nothing
// loads but inline images and our one inline style sheet; with
// remoteImages also images over http and https. No script runs: there is
// no script-src, and "sandbox" without allow-scripts forbids it a second
// time and gives the document an origin of its own, so it has no cookies
// and cannot reach the application that frames it. Links open in a new
// tab, outside the sandbox.
func ViewCSP(remoteImages bool) string {
	img := "img-src data:"
	if remoteImages {
		img += " https: http:"
	}
	return "default-src 'none'; " + img + "; style-src 'unsafe-inline'; form-action 'none'; base-uri 'none'; " +
		"frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox"
}
