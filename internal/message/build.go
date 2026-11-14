package message

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"mime/quotedprintable"
	"strings"
	"time"
	"unicode/utf8"
)

// This file writes messages: the other direction of the rest of the
// package. Header values are produced in the Raw form the parser returns
// (the text after the colon, folding included), so what Build writes reads
// back through Parse as the same values.

// Line lengths (RFC 5322 2.1.1): 78 is where lines should end, 998 where
// they must. A line with an encoded word is limited to 76 (RFC 2047 2).
const (
	foldAt      = 78
	wordLineMax = 76
	maxLineLen  = 998
)

// BuildPart is a body part to write. A multipart has SubParts and no Body.
type BuildPart struct {
	// Type is the media type, such as "text/plain" or "multipart/mixed".
	Type string
	// Params are further Content-Type parameters. charset, name and
	// boundary are set from the fields below and by Build.
	Params map[string]string
	// Charset is written for a text part; empty means utf-8.
	Charset     string
	Name        string // file name; goes into Content-Disposition and Content-Type
	Disposition string // "attachment", "inline" or empty
	CID         string // Content-ID, without the angle brackets
	Language    []string
	Location    string
	// Headers are further fields of the part, in Raw form.
	Headers []Header
	// Body is the content before any transfer encoding. Text has its line
	// ends made CRLF.
	Body []byte
	// Exact says Body is the content of a file: what a reader decodes
	// must be these bytes. Text whose line ends are not all CRLF is then
	// written as base64 instead of being changed to fit.
	Exact    bool
	SubParts []*BuildPart
}

// Build writes a message from its header fields (in Raw form, see Field)
// and its body. MIME-Version is added. A leaf gets the transfer encoding
// its content needs: 7bit for short ASCII lines, quoted-printable for
// other text, base64 for the rest. Nothing 8-bit comes out, except inside
// an attached message that is itself 8-bit.
func Build(headers []Header, root *BuildPart) []byte {
	var b bytes.Buffer
	for _, h := range headers {
		writeField(&b, h)
	}
	if _, ok := Get(headers, "MIME-Version"); !ok {
		b.WriteString("MIME-Version: 1.0\r\n")
	}
	writePart(&b, root, 0)
	return b.Bytes()
}

func writeField(b *bytes.Buffer, h Header) {
	b.WriteString(h.Name)
	b.WriteByte(':')
	b.WriteString(h.Value)
	b.WriteString("\r\n")
}

// Field makes a header field from a value without the leading space.
func Field(name, value string) Header { return Header{Name: name, Value: " " + value} }

func writePart(b *bytes.Buffer, p *BuildPart, depth int) {
	typ := strings.ToLower(p.Type)
	if typ == "" {
		typ = "text/plain"
	}
	multipart := strings.HasPrefix(typ, "multipart/")
	var body []byte
	var boundary, encoding string
	var children [][]byte
	if multipart {
		if depth < MaxDepth {
			for _, sp := range p.SubParts {
				var cb bytes.Buffer
				writePart(&cb, sp, depth+1)
				children = append(children, cb.Bytes())
			}
		}
		boundary = newBoundary(children)
	} else {
		body = p.Body
		switch crlf := toCRLF(body); {
		case !strings.HasPrefix(typ, "text/"):
			encoding = transferEncoding(typ, body)
		case p.Exact && (len(crlf) != len(body) || depth == 0 && len(body) > 0 && !bytes.HasSuffix(body, []byte("\r\n"))):
			// Canonical text has CRLF line ends (RFC 2045 2.8), and the
			// message as a whole ends in one. A file that is not like
			// that stays as it is inside base64.
			encoding = "base64"
		default:
			body = crlf
			encoding = transferEncoding(typ, body)
		}
	}

	ct := []string{typ}
	if strings.HasPrefix(typ, "text/") {
		cs := p.Charset
		if cs == "" {
			cs = "utf-8"
		}
		ct = append(ct, param("charset", cs)...)
	}
	for _, k := range sortedKeys(p.Params) {
		switch k {
		case "charset", "boundary", "name":
		default:
			ct = append(ct, param(k, p.Params[k])...)
		}
	}
	if p.Name != "" {
		ct = append(ct, param("name", p.Name)...)
	}
	if multipart {
		ct = append(ct, param("boundary", boundary)...)
	}
	writeField(b, Header{"Content-Type", foldTokens("Content-Type", joinParams(ct), foldAt)})
	if encoding != "" && encoding != "7bit" {
		b.WriteString("Content-Transfer-Encoding: " + encoding + "\r\n")
	}
	if p.Disposition != "" || p.Name != "" {
		disp := strings.ToLower(p.Disposition)
		if disp == "" || !isToken(disp) {
			disp = "attachment"
		}
		cd := []string{disp}
		if p.Name != "" {
			cd = append(cd, param("filename", p.Name)...)
		}
		writeField(b, Header{"Content-Disposition", foldTokens("Content-Disposition", joinParams(cd), foldAt)})
	}
	if p.CID != "" {
		writeField(b, Field("Content-ID", "<"+oneLineASCII(p.CID)+">"))
	}
	if len(p.Language) > 0 {
		writeField(b, Field("Content-Language", oneLineASCII(strings.Join(p.Language, ", "))))
	}
	if p.Location != "" {
		writeField(b, Header{"Content-Location", FormatText("Content-Location", p.Location)})
	}
	for _, h := range p.Headers {
		writeField(b, h)
	}
	b.WriteString("\r\n")

	if multipart {
		for _, c := range children {
			b.WriteString("--" + boundary + "\r\n")
			b.Write(c)
			b.WriteString("\r\n")
		}
		b.WriteString("--" + boundary + "--\r\n")
		return
	}
	switch encoding {
	case "base64":
		enc := base64.StdEncoding.EncodeToString(body)
		for len(enc) > 76 {
			b.WriteString(enc[:76])
			b.WriteString("\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc)
	case "quoted-printable":
		w := quotedprintable.NewWriter(b)
		w.Write(body)
		w.Close()
	default:
		b.Write(body)
	}
	// A part inside a multipart ends where the line break of the next
	// delimiter begins (RFC 2046 5.1.1): adding one here would add it to
	// the content. Only the message as a whole is made to end in CRLF.
	if depth == 0 && len(body) > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\r\n")) {
		b.WriteString("\r\n")
	}
}

// toCRLF makes every line end CRLF: a bare LF or CR becomes one.
func toCRLF(body []byte) []byte {
	clean := true
	for i, c := range body {
		if c == '\n' && (i == 0 || body[i-1] != '\r') || c == '\r' && (i+1 >= len(body) || body[i+1] != '\n') {
			clean = false
			break
		}
	}
	if clean {
		return body
	}
	out := make([]byte, 0, len(body)+len(body)/40)
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\r':
			out = append(out, '\r', '\n')
			if i+1 < len(body) && body[i+1] == '\n' {
				i++
			}
		case c == '\n':
			out = append(out, '\r', '\n')
		default:
			out = append(out, c)
		}
	}
	return out
}

// transferEncoding picks the Content-Transfer-Encoding of a leaf (RFC 2045
// 6). An attached message may only be 7bit, 8bit or binary (RFC 2046
// 5.2.1), so it is never re-encoded.
func transferEncoding(typ string, body []byte) string {
	ascii, longest, line, escapes := true, 0, 0, 0
	for _, c := range body {
		switch {
		case c == '\n':
			line = 0
			continue
		case c == '\r':
			continue
		case c >= 0x80:
			ascii = false
			escapes++
		case c < 0x20 && c != '\t' || c == 0x7f || c == '=':
			escapes++
			if c == 0 {
				ascii = false
			}
		}
		if line++; line > longest {
			longest = line
		}
	}
	if strings.HasPrefix(typ, "message/") {
		if ascii {
			return "7bit"
		}
		return "8bit"
	}
	if !strings.HasPrefix(typ, "text/") {
		return "base64"
	}
	if ascii && longest <= foldAt && !hasBareControl(body) {
		return "7bit"
	}
	if escapes*3 > len(body) {
		return "base64"
	}
	return "quoted-printable"
}

func hasBareControl(body []byte) bool {
	for _, c := range body {
		if c < 0x20 && c != '\t' && c != '\r' && c != '\n' || c == 0x7f {
			return true
		}
	}
	return false
}

func newBoundary(children [][]byte) string {
	for {
		var r [12]byte
		rand.Read(r[:])
		boundary := "=_" + hex.EncodeToString(r[:])
		used := false
		for _, c := range children {
			used = used || bytes.Contains(c, []byte(boundary))
		}
		if !used {
			return boundary
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// joinParams puts "; " between a value and its parameters: every token but
// the last gets a semicolon.
func joinParams(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		if i < len(tokens)-1 {
			t += ";"
		}
		out[i] = t
	}
	return out
}

// param writes one parameter of a Content-Type or Content-Disposition
// field. A value that is a token goes as it is, other short ASCII as a
// quoted string. Everything else uses RFC 2231: "name*=utf-8”%XX..", cut
// into numbered sections ("name*0*", "name*1*") when it is long.
func param(name, value string) []string {
	name = strings.ToLower(name)
	if !isToken(name) {
		return nil
	}
	plain := len(value) <= 60
	for i := 0; i < len(value) && plain; i++ {
		if c := value[i]; c < 0x20 || c >= 0x7f {
			plain = false
		}
	}
	if plain {
		if isToken(value) {
			return []string{name + "=" + value}
		}
		return []string{name + `="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`}
	}
	value = strings.ToValidUTF8(value, "�")
	const hexDigits = "0123456789ABCDEF"
	var chunks []string
	var cur strings.Builder
	for _, r := range value {
		var enc string
		// attribute-char of RFC 2231 7: a token character other than * ' %.
		if r < 0x80 && isToken(string(r)) && !strings.ContainsRune("*'%", r) {
			enc = string(r)
		} else {
			var buf [4]byte
			for _, c := range buf[:utf8.EncodeRune(buf[:], r)] {
				enc += "%" + string(hexDigits[c>>4]) + string(hexDigits[c&15])
			}
		}
		if cur.Len()+len(enc) > 50 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		cur.WriteString(enc)
	}
	chunks = append(chunks, cur.String())
	if len(chunks) == 1 {
		return []string{name + "*=utf-8''" + chunks[0]}
	}
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = name + "*" + itoa(i) + "*="
		if i == 0 {
			out[i] += "utf-8''"
		}
		out[i] += c
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

// foldTokens joins tokens with single spaces into the Raw value of the
// field name, starting a new line (CRLF and a space) where the next token
// would pass limit. A token is never cut, so one longer than a line stays
// long.
func foldTokens(name string, tokens []string, limit int) string {
	var b strings.Builder
	line := len(name) + 1
	for i, t := range tokens {
		if t == "" {
			continue
		}
		if i > 0 && line+1+len(t) > limit {
			b.WriteString("\r\n")
			line = 0
		}
		b.WriteByte(' ')
		b.WriteString(t)
		line += 1 + len(t)
	}
	if b.Len() == 0 {
		return " "
	}
	return b.String()
}

func oneLineASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f {
			return -1
		}
		return r
	}, s)
}

func needsEncoding(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x7f || c < 0x20 && c != '\t' {
			return true
		}
	}
	return strings.Contains(s, "=?")
}

// encodedWords writes text as RFC 2047 encoded words in UTF-8, each at
// most 75 characters and none splitting a character (5 (3)). first is the
// room on the first line. Q is used for mostly-ASCII text, B otherwise.
func encodedWords(text string, first int) []string {
	text = strings.ToValidUTF8(text, "�")
	other := 0
	for i := 0; i < len(text); i++ {
		if !qSafe(text[i]) && text[i] != ' ' {
			other++
		}
	}
	useB := other*3 > len(text)
	const overhead = len("=?utf-8?q??=")
	room := func(n int) int {
		n -= overhead
		if n > 75-overhead {
			n = 75 - overhead
		}
		if useB {
			n = n / 4 * 3 // octets that fit
		}
		return n
	}
	var words []string
	var cur []byte
	budget := room(first)
	if budget < 12 {
		budget = room(75)
	}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if useB {
			words = append(words, "=?utf-8?b?"+base64.StdEncoding.EncodeToString(cur)+"?=")
		} else {
			words = append(words, "=?utf-8?q?"+string(cur)+"?=")
		}
		cur = nil
		budget = room(75)
	}
	const hexDigits = "0123456789ABCDEF"
	for _, r := range text {
		var enc []byte
		var buf [4]byte
		raw := buf[:utf8.EncodeRune(buf[:], r)]
		switch {
		case useB:
			enc = raw
		case r == ' ':
			enc = []byte{'_'}
		case r < 0x80 && qSafe(byte(r)):
			enc = raw
		default:
			for _, c := range raw {
				enc = append(enc, '=', hexDigits[c>>4], hexDigits[c&15])
			}
		}
		if len(cur)+len(enc) > budget {
			flush()
		}
		cur = append(cur, enc...)
	}
	flush()
	return words
}

// qSafe is what a Q-encoded word may hold as itself wherever it appears,
// also in a phrase (RFC 2047 5 (3)).
func qSafe(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!*+-/", c) >= 0
}

// FormatText is the Raw value of an unstructured field such as Subject
// (RFC 5322 2.2.1): folded at spaces, and as encoded words (RFC 2047) when
// the text is not printable ASCII or a word is too long for a line.
func FormatText(name, text string) string {
	text = strings.Join(strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' }), " ")
	if !needsEncoding(text) {
		words := strings.Split(text, " ")
		ok := true
		for _, w := range words {
			ok = ok && len(w) <= maxLineLen-len(name)-10
		}
		if ok {
			// Split keeps runs of spaces as empty words; they are put back.
			return foldSpaced(name, words)
		}
	}
	return foldTokens(name, encodedWords(text, wordLineMax-len(name)-2), wordLineMax)
}

// foldSpaced folds text at its own spaces, keeping every one of them: a
// fold adds CRLF in front of a space that was there.
func foldSpaced(name string, words []string) string {
	var b strings.Builder
	line := len(name) + 1
	for i, w := range words {
		if i > 0 && w != "" && line+1+len(w) > foldAt && line > len(name)+2 {
			b.WriteString("\r\n")
			line = 0
		}
		b.WriteByte(' ')
		b.WriteString(w)
		line += 1 + len(w)
	}
	return b.String()
}

// phrase writes a display name: atoms as they are, other ASCII as a quoted
// string, the rest as encoded words (RFC 5322 3.2.5, RFC 2047 5 (3)).
func phrase(name string, first int) []string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return nil
	}
	if needsEncoding(name) || len(name) > 60 {
		return encodedWords(name, first)
	}
	atoms := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == ' ' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0) {
			atoms = false
		}
	}
	if atoms {
		return strings.Fields(name)
	}
	return []string{`"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`}
}

// addrSpec writes an address for a header. A local part that is not a
// dot-atom is quoted. Characters that would break the header are dropped;
// whether the address is one at all is for the caller to check.
func addrSpec(email string) string {
	email = strings.Map(func(r rune) rune {
		if r <= 0x20 || r == 0x7f || r == '<' || r == '>' || r == ',' || r == ';' {
			return -1
		}
		return r
	}, email)
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return email
	}
	local, domain := email[:at], email[at+1:]
	dotAtom := !strings.HasPrefix(local, ".") && !strings.HasSuffix(local, ".") && !strings.Contains(local, "..")
	for i := 0; i < len(local) && dotAtom; i++ {
		c := local[i]
		dotAtom = c >= 0x80 || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("!#$%&'*+-/=?^_`{|}~", c) >= 0
	}
	if dotAtom || strings.HasPrefix(local, `"`) {
		return email
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(local) + `"@` + domain
}

func addressTokens(a Address, last bool) []string {
	var tokens []string
	if a.Name != nil {
		tokens = phrase(*a.Name, 60)
	}
	spec := addrSpec(a.Email)
	if len(tokens) > 0 {
		spec = "<" + spec + ">"
	}
	if !last {
		spec += ","
	}
	return append(tokens, spec)
}

// FormatAddresses is the Raw value of an address field such as To (RFC
// 5322 3.4), one mailbox after the other, folded between words.
func FormatAddresses(name string, list []Address) string {
	var tokens []string
	for i, a := range list {
		tokens = append(tokens, addressTokens(a, i == len(list)-1)...)
	}
	return foldTokens(name, tokens, wordLineMax)
}

// FormatGroups is FormatAddresses for groups: "name: a, b;". A group
// without a name is a run of mailboxes outside any group.
func FormatGroups(name string, groups []Group) string {
	var tokens []string
	for gi, g := range groups {
		last := gi == len(groups)-1
		if g.Name == nil {
			for i, a := range g.Addresses {
				tokens = append(tokens, addressTokens(a, last && i == len(g.Addresses)-1)...)
			}
			continue
		}
		head := phrase(*g.Name, 60)
		if len(head) == 0 {
			head = []string{`""`}
		}
		head[len(head)-1] += ":"
		tokens = append(tokens, head...)
		for i, a := range g.Addresses {
			tokens = append(tokens, addressTokens(a, i == len(g.Addresses)-1)...)
		}
		end := ";"
		if !last {
			end = ";,"
		}
		if len(g.Addresses) == 0 {
			tokens = append(tokens, end)
		} else {
			tokens[len(tokens)-1] += end
		}
	}
	return foldTokens(name, tokens, wordLineMax)
}

// FormatMessageIDs is the Raw value of Message-ID, In-Reply-To or
// References: every id in angle brackets (RFC 5322 3.6.4).
func FormatMessageIDs(name string, ids []string) string {
	tokens := make([]string, len(ids))
	for i, id := range ids {
		tokens[i] = "<" + oneLineASCII(strings.NewReplacer("<", "", ">", "", " ", "").Replace(id)) + ">"
	}
	return foldTokens(name, tokens, foldAt)
}

// FormatDate is the Raw value of a date field (RFC 5322 3.3).
func FormatDate(t time.Time) string {
	return " " + t.Format("Mon, 2 Jan 2006 15:04:05 -0700")
}

// FormatURLs is the Raw value of a List-* field (RFC 2369): URLs in angle
// brackets, separated by commas.
func FormatURLs(name string, urls []string) string {
	tokens := make([]string, len(urls))
	for i, u := range urls {
		tokens[i] = "<" + oneLineASCII(strings.NewReplacer("<", "", ">", "", " ", "").Replace(u)) + ">"
		if i < len(urls)-1 {
			tokens[i] += ","
		}
	}
	return foldTokens(name, tokens, foldAt)
}

// ValidRaw reports whether v can be written as the Raw value of a field: no
// line break except as folding, no NUL, and no line over 998 octets.
func ValidRaw(v string) bool {
	line := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == 0:
			return false
		case c == '\r':
			if i+2 >= len(v) || v[i+1] != '\n' || (v[i+2] != ' ' && v[i+2] != '\t') {
				return false
			}
			i++
			line = 0
		case c == '\n':
			return false
		default:
			if line++; line > maxLineLen-2 {
				return false
			}
		}
	}
	return true
}

// NewMessageID makes a message id for a message that leaves through host.
func NewMessageID(host string) string {
	var r [16]byte
	rand.Read(r[:])
	return hex.EncodeToString(r[:]) + "@" + host
}
