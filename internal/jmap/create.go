package jmap

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/message"
	"github.com/robinmail/robinmail/internal/store"
)

// emailRef reads an email id that may be a "#creationId" reference.
func (c *call) emailRef(s string) (int64, bool) {
	if strings.HasPrefix(s, "#") {
		id, ok := c.created[s[1:]]
		if !ok {
			return 0, false
		}
		s = id
	}
	return store.ParseEmailID(s)
}

// convenience maps the header properties of an Email to the field and form
// they stand for (RFC 8621 4.1.2 and 4.1.3).
var convenience = map[string]headerSpec{
	"messageId":  {"Message-ID", message.FormMessageIDs, false},
	"inReplyTo":  {"In-Reply-To", message.FormMessageIDs, false},
	"references": {"References", message.FormMessageIDs, false},
	"sender":     {"Sender", message.FormAddresses, false},
	"from":       {"From", message.FormAddresses, false},
	"to":         {"To", message.FormAddresses, false},
	"cc":         {"Cc", message.FormAddresses, false},
	"bcc":        {"Bcc", message.FormAddresses, false},
	"replyTo":    {"Reply-To", message.FormAddresses, false},
	"subject":    {"Subject", message.FormText, false},
	"sentAt":     {"Date", message.FormDate, false},
}

// headerOrder is where the common fields go in a message we write; the
// others follow by name.
var headerOrder = []string{"date", "from", "sender", "reply-to", "to", "cc", "bcc", "subject", "message-id", "in-reply-to", "references"}

func validAddr(a message.Address) bool {
	if a.Email == "" || len(a.Email) > 320 || !strings.Contains(a.Email, "@") {
		return false
	}
	for _, r := range a.Email {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("<>,;", r) {
			return false
		}
	}
	return true
}

func cleanToken(s string) bool {
	if s == "" || len(s) > 900 {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f || r == '<' || r == '>' {
			return false
		}
	}
	return true
}

// formValue writes one header value given in a parsed form (RFC 8621
// 4.1.2) as the Raw value of the field. ok is false if the JSON is not of
// the form, or cannot be written.
func formValue(name, form string, raw json.RawMessage) (string, bool) {
	switch form {
	case message.FormRaw:
		var v string
		if json.Unmarshal(raw, &v) != nil || !message.ValidRaw(v) {
			return "", false
		}
		// A Raw value is what follows the colon, and by convention that
		// starts with a space. A client that gives "cat" means the field
		// "Foo: cat", not "Foo:cat", which is legal and which some
		// readers do not expect (RFC 8621 4.6 lets the server adjust a
		// message it writes; the JMAP-TestSuite reads " cat" back).
		if v != "" && v[0] != ' ' && v[0] != '\t' {
			v = " " + v
		}
		return v, true
	case message.FormText:
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return "", false
		}
		return message.FormatText(name, v), true
	case message.FormAddresses:
		var v []message.Address
		if json.Unmarshal(raw, &v) != nil || v == nil || len(v) > message.MaxAddresses {
			return "", false
		}
		for _, a := range v {
			if !validAddr(a) {
				return "", false
			}
		}
		return message.FormatAddresses(name, v), true
	case message.FormGroupedAddresses:
		var v []message.Group
		if json.Unmarshal(raw, &v) != nil || v == nil || len(v) > message.MaxAddresses {
			return "", false
		}
		for _, g := range v {
			for _, a := range g.Addresses {
				if !validAddr(a) {
					return "", false
				}
			}
		}
		return message.FormatGroups(name, v), true
	case message.FormMessageIDs, message.FormURLs:
		var v []string
		if json.Unmarshal(raw, &v) != nil || v == nil || len(v) > 1000 {
			return "", false
		}
		for _, id := range v {
			if !cleanToken(id) {
				return "", false
			}
		}
		if form == message.FormURLs {
			return message.FormatURLs(name, v), true
		}
		return message.FormatMessageIDs(name, v), true
	case message.FormDate:
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return "", false
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return "", false
		}
		return message.FormatDate(t), true
	}
	return "", false
}

// headerFields reads the header properties of an object that is being
// created (an Email or an EmailBodyPart) into fields. A property set to
// null is as if it was not given. bad lists the properties that are not
// valid; a field named by two properties is one of them.
func headerFields(obj map[string]json.RawMessage) (fields []message.Header, bad []string) {
	seen := map[string]string{} // field name in lowercase -> the property that set it
	props := make([]string, 0, len(obj))
	for p := range obj {
		props = append(props, p)
	}
	sort.Strings(props)
	for _, p := range props {
		spec, ok := convenience[p]
		if !ok {
			if !strings.HasPrefix(p, "header:") {
				continue
			}
			if spec, ok = parseHeaderSpec(p); !ok {
				bad = append(bad, p)
				continue
			}
		}
		raw := obj[p]
		if string(raw) == "null" {
			continue
		}
		key := strings.ToLower(spec.name)
		if _, dup := seen[key]; dup {
			// RFC 8621 4.6: "header fields MUST NOT be specified more
			// than once", for instance as from and header:From.
			bad = append(bad, p)
			continue
		}
		seen[key] = p
		values := []json.RawMessage{raw}
		if spec.all {
			if json.Unmarshal(raw, &values) != nil {
				bad = append(bad, p)
				continue
			}
		}
		for _, v := range values {
			text, ok := formValue(spec.name, spec.form, v)
			if !ok {
				bad = append(bad, p)
				break
			}
			fields = append(fields, message.Header{Name: spec.name, Value: text})
		}
	}
	return fields, bad
}

// createBody carries what building the body of a new email needs and
// collects what is wrong with it.
type createBody struct {
	c       *call
	account string
	values  map[string]string // bodyValues: partId -> text
	missing []string          // blob ids that do not exist
	bad     []string          // properties that are not valid
	parts   int
	// top has the names, in lower case, of the header fields the Email
	// itself defines.
	top map[string]bool
}

// partProperties are the properties an EmailBodyPart may have when an
// email is created, besides "header:" ones.
var partProperties = map[string]bool{"partId": true, "blobId": true, "size": true, "name": true, "type": true, "charset": true,
	"disposition": true, "cid": true, "language": true, "location": true, "subParts": true}

// part reads one EmailBodyPart of a creation (RFC 8621 4.6) and returns
// what to write. path names the part in error reports.
func (b *createBody) part(raw json.RawMessage, path string, depth int) *message.BuildPart {
	var obj map[string]json.RawMessage
	if b.parts++; json.Unmarshal(raw, &obj) != nil || obj == nil || depth > message.MaxDepth || b.parts > message.MaxParts {
		b.bad = append(b.bad, path)
		return nil
	}
	str := func(name string) (string, bool) {
		v, ok := obj[name]
		if !ok || string(v) == "null" {
			return "", false
		}
		var s string
		if json.Unmarshal(v, &s) != nil || strings.ContainsAny(s, "\r\n\x00") {
			b.bad = append(b.bad, path+"/"+name)
			return "", false
		}
		return s, true
	}
	p := &message.BuildPart{}
	for name := range obj {
		if partProperties[name] {
			continue
		}
		spec, ok := parseHeaderSpec(name)
		// The fields the other properties stand for are written from
		// those, and "headers" may not be given at all (RFC 8621 4.6).
		// Content-Transfer-Encoding is ours to choose.
		if !ok || strings.HasPrefix(strings.ToLower(spec.name), "content-") && !strings.EqualFold(spec.name, "Content-Description") {
			b.bad = append(b.bad, path+"/"+name)
		}
	}
	fields, bad := headerFields(obj)
	for _, name := range bad {
		b.bad = append(b.bad, path+"/"+name)
	}
	if depth == 0 {
		// "The bodyStructure EmailBodyPart MUST NOT contain a property
		// representing a header field that is already defined on the
		// top-level Email object": the two are one header section, and
		// the field would be there twice.
		for name := range obj {
			if spec, ok := parseHeaderSpec(name); ok && b.top[strings.ToLower(spec.name)] {
				b.bad = append(b.bad, path+"/"+name)
			}
		}
	}
	p.Headers = fields
	p.Type, _ = str("type")
	p.Type = strings.ToLower(p.Type)
	if p.Type == "" {
		p.Type = "text/plain"
	}
	if slash := strings.IndexByte(p.Type, '/'); slash <= 0 || slash == len(p.Type)-1 || strings.ContainsAny(p.Type, " ;()<>@,:\\\"[]?=") {
		b.bad = append(b.bad, path+"/type")
		return nil
	}
	p.Name, _ = str("name")
	p.Disposition, _ = str("disposition")
	p.CID, _ = str("cid")
	p.Location, _ = str("location")
	if v, ok := obj["language"]; ok && string(v) != "null" {
		if json.Unmarshal(v, &p.Language) != nil {
			b.bad = append(b.bad, path+"/language")
		}
	}
	partID, hasPart := str("partId")
	blobID, hasBlob := str("blobId")

	if strings.HasPrefix(p.Type, "multipart/") {
		var subs []json.RawMessage
		if json.Unmarshal(obj["subParts"], &subs) != nil || hasPart || hasBlob {
			b.bad = append(b.bad, path+"/subParts")
			return nil
		}
		for i, sub := range subs {
			if sp := b.part(sub, path+"/subParts/"+itoa(i), depth+1); sp != nil {
				p.SubParts = append(p.SubParts, sp)
			}
		}
		return p
	}
	if v, ok := obj["subParts"]; ok && string(v) != "null" {
		b.bad = append(b.bad, path+"/subParts")
	}
	switch {
	case hasPart == hasBlob:
		// One of the two, not both and not neither.
		b.bad = append(b.bad, path+"/partId")
	case hasPart:
		text, ok := b.values[partID]
		if !ok {
			b.bad = append(b.bad, path+"/partId")
			break
		}
		// "If [partId] is given, [...] the charset property MUST be
		// omitted": the value is Unicode and we write UTF-8. A size
		// cannot be known by the client either.
		for _, name := range []string{"charset", "size"} {
			if v, given := obj[name]; given && string(v) != "null" {
				b.bad = append(b.bad, path+"/"+name)
			}
		}
		// Any type: the RFC does not keep a value to text parts, and a
		// client may well put JSON or a calendar in one. The content is
		// the value in UTF-8.
		p.Body = []byte(text)
	default:
		data, err := b.c.h.blobData(b.c.ctx, b.account, blobID)
		if err != nil {
			b.missing = append(b.missing, blobID)
			break
		}
		// "If a blobId is given, [size] may be included but is ignored
		// by the server": also one that is wrong.
		p.Charset, _ = str("charset")
		p.Body = data
		// A file: a text attachment with LF line ends must download as
		// it was uploaded.
		p.Exact = true
	}
	return p
}

func itoa(n int) string { return strconv.Itoa(n) }

// serverSet are the properties of an Email only the server sets.
var serverSet = []string{"id", "blobId", "threadId", "size", "hasAttachment", "preview", "headers"}

// placement reads mailboxIds, keywords and receivedAt of a creation.
func (c *call) placement(obj map[string]json.RawMessage, needMailboxes bool) (mailboxes []int64, keywords []string, received time.Time, se *setError) {
	if v, ok := obj["mailboxIds"]; ok && string(v) != "null" {
		var m map[string]bool
		if json.Unmarshal(v, &m) != nil || len(m) == 0 {
			return nil, nil, received, invalidProps("mailboxIds must name at least one mailbox, as id: true", "mailboxIds")
		}
		for sid, on := range m {
			id, found := c.mailboxRef(sid)
			if !on || !found {
				return nil, nil, received, invalidProps("there is no mailbox "+sid, "mailboxIds")
			}
			mailboxes = append(mailboxes, id)
		}
		sort.Slice(mailboxes, func(i, j int) bool { return mailboxes[i] < mailboxes[j] })
	} else if needMailboxes {
		return nil, nil, received, invalidProps("an email must be in at least one mailbox", "mailboxIds")
	}
	if v, ok := obj["keywords"]; ok && string(v) != "null" {
		var m map[string]bool
		if json.Unmarshal(v, &m) != nil {
			return nil, nil, received, invalidProps("keywords must be an object of keyword: true", "keywords")
		}
		keywords = []string{}
		for k, on := range m {
			if !on {
				return nil, nil, received, invalidProps("a keyword is either true or not there", "keywords")
			}
			keywords = append(keywords, k)
		}
		sort.Strings(keywords)
	}
	if v, ok := obj["receivedAt"]; ok && string(v) != "null" {
		var s string
		json.Unmarshal(v, &s)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, nil, received, invalidProps("receivedAt must be a UTCDate", "receivedAt")
		}
		received = t
	}
	return mailboxes, keywords, received, nil
}

// storeEmail stores a message as a new Email and returns what a client is
// told about a creation (RFC 8621 4.6): the properties the server set.
func (c *call) storeEmail(run *setRun, raw []byte, mailboxes []int64, keywords []string, received time.Time) (map[string]any, *setError, *methodError) {
	if int64(len(raw)) > c.h.limits.MaxSizeUpload {
		return nil, &setError{Type: "tooLarge", Description: "the email is larger than the server stores", MaxSize: c.h.limits.MaxSizeUpload}, nil
	}
	e, err := c.h.Store.Deliver(c.ctx, run.account, store.Message{Raw: raw, MailboxIDs: mailboxes, Keywords: keywords,
		ReceivedAt: received, IfInState: run.ifState})
	switch {
	case errors.Is(err, store.ErrStateMismatch):
		return nil, nil, mErr("stateMismatch", "the state is not the ifInState given")
	case errors.Is(err, store.ErrNoMailbox):
		return nil, invalidProps("one of the mailboxes does not exist", "mailboxIds"), nil
	case errors.Is(err, store.ErrKeyword):
		return nil, invalidProps("a keyword is 1 to 255 printable ASCII characters without ( ) { ] % * \" \\", "keywords"), nil
	case errors.Is(err, store.ErrOverQuota):
		// RFC 8620 5.3: "overQuota: The create would exceed a
		// server-defined limit on the number or total size of objects".
		return nil, &setError{Type: "overQuota", Description: "the account is full"}, nil
	case err != nil:
		return nil, &setError{Type: "serverFail"}, nil
	}
	run.wroteState(e.Modseq)
	return map[string]any{"id": store.EmailID(e.ID), "blobId": e.Blob, "threadId": store.ThreadID(e.ThreadID), "size": e.Size}, nil, nil
}

// createEmail is the create of Email/set (RFC 8621 4.6): a message is
// written from the properties given and stored.
//
// The body is given either as bodyStructure, any tree of parts, or as
// textBody, htmlBody and attachments, from which the usual tree is made:
// multipart/alternative for a text and an HTML version, multipart/related
// around the HTML and the attachments it shows inline (disposition
// "inline" with a cid), multipart/mixed around all that and the other
// attachments. A part is either text from bodyValues (partId) or a blob
// (blobId): an upload, a message, or a part of a message.
//
// Date and Message-ID are added when missing, as the RFC requires.
func (c *call) createEmail(run *setRun, raw json.RawMessage) (map[string]any, *setError, *methodError) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, invalidProps("an email to create must be an object"), nil
	}
	var bad []string
	for _, p := range serverSet {
		if _, ok := obj[p]; ok {
			bad = append(bad, p)
		}
	}
	known := map[string]bool{"mailboxIds": true, "keywords": true, "receivedAt": true, "bodyStructure": true, "bodyValues": true,
		"textBody": true, "htmlBody": true, "attachments": true}
	for p := range obj {
		_, conv := convenience[p]
		switch {
		case known[p], conv, contains(serverSet, p):
		case strings.HasPrefix(p, "header:"):
			// Content-* fields describe the body and are part of it.
			if spec, ok := parseHeaderSpec(p); ok && strings.HasPrefix(strings.ToLower(spec.name), "content-") {
				bad = append(bad, p)
			}
		default:
			bad = append(bad, p)
		}
	}
	mailboxes, keywords, received, se := c.placement(obj, true)
	if se != nil {
		return nil, se, nil
	}
	fields, badHeaders := headerFields(obj)
	bad = append(bad, badHeaders...)

	b := &createBody{c: c, account: run.account, values: map[string]string{}, top: map[string]bool{}}
	for _, f := range fields {
		b.top[strings.ToLower(f.Name)] = true
	}
	if v, ok := obj["bodyValues"]; ok && string(v) != "null" {
		var values map[string]struct {
			Value             *string `json:"value"`
			IsEncodingProblem bool    `json:"isEncodingProblem"`
			IsTruncated       bool    `json:"isTruncated"`
		}
		if json.Unmarshal(v, &values) != nil {
			bad = append(bad, "bodyValues")
		}
		for id, bv := range values {
			// A value that is damaged or cut off is not something to
			// write a message from (RFC 8621 4.6): "isEncodingProblem
			// and isTruncated MUST be either false or omitted".
			if bv.Value == nil {
				bad = append(bad, "bodyValues/"+id+"/value")
				continue
			}
			if bv.IsEncodingProblem {
				bad = append(bad, "bodyValues/"+id+"/isEncodingProblem")
			}
			if bv.IsTruncated {
				bad = append(bad, "bodyValues/"+id+"/isTruncated")
			}
			// Known all the same, so the part that uses it is not
			// reported as well.
			b.values[id] = *bv.Value
		}
	}

	var root *message.BuildPart
	_, hasStructure := obj["bodyStructure"]
	list := func(name string) []*message.BuildPart {
		v, ok := obj[name]
		if !ok || string(v) == "null" {
			return nil
		}
		var items []json.RawMessage
		if json.Unmarshal(v, &items) != nil {
			b.bad = append(b.bad, name)
			return nil
		}
		var out []*message.BuildPart
		for i, item := range items {
			if p := b.part(item, name+"/"+itoa(i), 1); p != nil {
				out = append(out, p)
			}
		}
		return out
	}
	if hasStructure {
		for _, p := range []string{"textBody", "htmlBody", "attachments"} {
			if _, both := obj[p]; both {
				bad = append(bad, p)
			}
		}
		root = b.part(obj["bodyStructure"], "bodyStructure", 0)
	} else {
		text, html, attachments := list("textBody"), list("htmlBody"), list("attachments")
		// "textBody: if given, MUST contain exactly one body part and it
		// MUST be of type text/plain", and the same for htmlBody.
		if len(text) > 1 || len(text) == 1 && text[0].Type != "text/plain" {
			bad = append(bad, "textBody")
		}
		if len(html) > 1 || len(html) == 1 && html[0].Type != "text/html" {
			bad = append(bad, "htmlBody")
		}
		if len(bad) == 0 && len(b.bad) == 0 {
			root = assemble(text, html, attachments)
		}
	}
	bad = append(bad, b.bad...)
	if len(b.missing) > 0 {
		sort.Strings(b.missing)
		return nil, &setError{Type: "blobNotFound", NotFound: b.missing}, nil
	}
	if len(bad) > 0 || root == nil {
		sortProperties(bad)
		return nil, invalidProps("these properties cannot be used to create an email as given", bad...), nil
	}

	have := func(name string) bool { _, ok := message.Get(fields, name); return ok }
	if !have("Date") {
		fields = append(fields, message.Header{Name: "Date", Value: message.FormatDate(c.h.now())})
	}
	if !have("Message-ID") {
		host := c.h.Hostname
		if host == "" {
			host = c.user.Username[strings.LastIndexByte(c.user.Username, '@')+1:]
		}
		fields = append(fields, message.Header{Name: "Message-ID", Value: " <" + message.NewMessageID(host) + ">"})
	}
	rank := func(name string) int {
		for i, n := range headerOrder {
			if strings.EqualFold(n, name) {
				return i
			}
		}
		return len(headerOrder)
	}
	sort.SliceStable(fields, func(i, j int) bool {
		ri, rj := rank(fields[i].Name), rank(fields[j].Name)
		if ri != rj {
			return ri < rj
		}
		return ri == len(headerOrder) && strings.ToLower(fields[i].Name) < strings.ToLower(fields[j].Name)
	})
	return c.storeEmail(run, message.Build(fields, root), mailboxes, keywords, received)
}

// assemble makes the body tree for a text version, an HTML version and
// attachments.
func assemble(text, html, attachments []*message.BuildPart) *message.BuildPart {
	var inline, other []*message.BuildPart
	for _, a := range attachments {
		if len(html) == 1 && strings.EqualFold(a.Disposition, "inline") && a.CID != "" {
			inline = append(inline, a)
		} else {
			if a.Disposition == "" {
				a.Disposition = "attachment"
			}
			other = append(other, a)
		}
	}
	var body *message.BuildPart
	if len(html) == 1 {
		body = html[0]
		if len(inline) > 0 {
			body = &message.BuildPart{Type: "multipart/related", Params: map[string]string{"type": "text/html"},
				SubParts: append([]*message.BuildPart{html[0]}, inline...)}
		}
		if len(text) == 1 {
			body = &message.BuildPart{Type: "multipart/alternative", SubParts: []*message.BuildPart{text[0], body}}
		}
	} else if len(text) == 1 {
		body = text[0]
	}
	switch {
	case body == nil && len(other) == 0:
		return &message.BuildPart{Type: "text/plain"}
	case len(other) == 0:
		return body
	case body == nil:
		return &message.BuildPart{Type: "multipart/mixed", SubParts: other}
	}
	return &message.BuildPart{Type: "multipart/mixed", SubParts: append([]*message.BuildPart{body}, other...)}
}

// emailImport is Email/import (RFC 8621 4.8): a blob that holds a whole
// message becomes an Email.
func emailImport(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID string                     `json:"accountId"`
		IfInState *string                    `json:"ifInState"`
		Emails    map[string]json.RawMessage `json:"emails"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.Emails == nil {
		return nil, invalidArgs("emails is required")
	}
	run, e := c.begin(&setArgs{AccountID: args.AccountID, IfInState: args.IfInState, Create: args.Emails})
	if e != nil {
		return nil, e
	}
	cids := make([]string, 0, len(args.Emails))
	for cid := range args.Emails {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	for _, cid := range cids {
		var obj map[string]json.RawMessage
		if json.Unmarshal(args.Emails[cid], &obj) != nil || obj == nil {
			run.notCreated[cid] = invalidProps("an email to import must be an object")
			continue
		}
		var bad []string
		for p := range obj {
			if p != "blobId" && p != "mailboxIds" && p != "keywords" && p != "receivedAt" {
				bad = append(bad, p)
			}
		}
		var blobID string
		if json.Unmarshal(obj["blobId"], &blobID) != nil || blobID == "" {
			bad = append(bad, "blobId")
		}
		// Both are required: a client that left out both hears so at
		// once.
		if v, ok := obj["mailboxIds"]; !ok || string(v) == "null" {
			bad = append(bad, "mailboxIds")
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			run.notCreated[cid] = invalidProps("an import has blobId, mailboxIds, keywords and receivedAt", bad...)
			continue
		}
		mailboxes, keywords, received, se := c.placement(obj, true)
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		data, err := c.h.blobData(c.ctx, args.AccountID, blobID)
		if errors.Is(err, store.ErrNoBlob) {
			// RFC 8621 4.8: "If the blobId, mailboxIds, or keywords
			// properties are invalid (e.g., missing, wrong type, id not
			// found), the server MUST reject the import with an
			// invalidProperties SetError." blobNotFound is for
			// Email/set (4.6).
			run.notCreated[cid] = invalidProps("there is no blob "+blobID, "blobId")
			continue
		}
		if err != nil {
			run.notCreated[cid] = &setError{Type: "serverFail"}
			continue
		}
		// A message is a header section: without a single field the blob
		// is something else (RFC 8621 4.8, invalidEmail).
		if len(message.ParseHeader(data)) == 0 {
			run.notCreated[cid] = &setError{Type: "invalidEmail", Description: "the blob is not a message"}
			continue
		}
		created, se, me := c.storeEmail(run, data, mailboxes, keywords, received)
		switch {
		case me != nil:
			return nil, me
		case se != nil:
			run.notCreated[cid] = se
		default:
			if run.created == nil {
				run.created = map[string]any{}
			}
			run.created[cid] = created
			c.created[cid] = created["id"].(string)
		}
	}
	res := run.result()
	return map[string]any{"accountId": res["accountId"], "oldState": res["oldState"], "newState": res["newState"],
		"created": res["created"], "notCreated": res["notCreated"]}, nil
}

// emailCopy is Email/copy (RFC 8621 4.7): a new Email with the message of
// an existing one.
//
// RFC 8620 5.4 defines /copy between two accounts and says the two "MUST"
// differ. A user here has one account, so that could never be called. It
// is offered within the account instead, where it does what a client
// would otherwise need a download and an Email/import for: a second Email
// of the same message, with mailboxes and keywords of its own.
func emailCopy(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		FromAccountID            string                     `json:"fromAccountId"`
		IfFromInState            *string                    `json:"ifFromInState"`
		AccountID                string                     `json:"accountId"`
		IfInState                *string                    `json:"ifInState"`
		Create                   map[string]json.RawMessage `json:"create"`
		OnSuccessDestroyOriginal bool                       `json:"onSuccessDestroyOriginal"`
		DestroyFromIfInState     *string                    `json:"destroyFromIfInState"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.FromAccountID != c.user.AccountID {
		return nil, mErr("fromAccountNotFound", "no such account to copy from")
	}
	if args.Create == nil {
		return nil, invalidArgs("create is required")
	}
	run, e := c.begin(&setArgs{AccountID: args.AccountID, IfInState: args.IfInState, Create: args.Create})
	if e != nil {
		return nil, e
	}
	if args.IfFromInState != nil {
		if want, ok := parseState(*args.IfFromInState); !ok || want != run.oldState {
			return nil, mErr("stateMismatch", "the state is not the ifFromInState given")
		}
	}
	cids := make([]string, 0, len(args.Create))
	for cid := range args.Create {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	var originals []string
	for _, cid := range cids {
		var obj map[string]json.RawMessage
		if json.Unmarshal(args.Create[cid], &obj) != nil || obj == nil {
			run.notCreated[cid] = invalidProps("a copy must be an object")
			continue
		}
		var bad []string
		for p := range obj {
			if p != "id" && p != "mailboxIds" && p != "keywords" && p != "receivedAt" {
				bad = append(bad, p)
			}
		}
		var sid string
		if json.Unmarshal(obj["id"], &sid) != nil || sid == "" {
			bad = append(bad, "id")
		}
		if len(bad) > 0 {
			sort.Strings(bad)
			run.notCreated[cid] = invalidProps("a copy has id, mailboxIds, keywords and receivedAt", bad...)
			continue
		}
		id, ok := c.emailRef(sid)
		if !ok {
			run.notCreated[cid] = &setError{Type: "notFound"}
			continue
		}
		data, orig, err := c.h.Store.Raw(c.ctx, args.FromAccountID, id)
		if errors.Is(err, store.ErrNoEmail) {
			run.notCreated[cid] = &setError{Type: "notFound"}
			continue
		}
		if err != nil {
			run.notCreated[cid] = &setError{Type: "serverFail"}
			continue
		}
		// What is not given is as on the original (RFC 8620 5.4).
		mailboxes, keywords, received, se := c.placement(obj, false)
		if se != nil {
			run.notCreated[cid] = se
			continue
		}
		if mailboxes == nil {
			for m := range orig.Mailboxes {
				mailboxes = append(mailboxes, m)
			}
		}
		if keywords == nil {
			keywords = orig.Keywords
		}
		if received.IsZero() {
			received = orig.ReceivedAt
		}
		created, se, me := c.storeEmail(run, data, mailboxes, keywords, received)
		switch {
		case me != nil:
			return nil, me
		case se != nil:
			run.notCreated[cid] = se
		default:
			if run.created == nil {
				run.created = map[string]any{}
			}
			run.created[cid] = created
			c.created[cid] = created["id"].(string)
			originals = append(originals, store.EmailID(id))
		}
	}
	res := run.result()
	out := map[string]any{"fromAccountId": args.FromAccountID, "accountId": res["accountId"], "oldState": res["oldState"],
		"newState": res["newState"], "created": res["created"], "notCreated": res["notCreated"]}
	if args.OnSuccessDestroyOriginal && len(originals) > 0 {
		// RFC 8620 5.4: an Email/set that destroys the originals, whose
		// response follows this one.
		set := map[string]any{"accountId": args.FromAccountID, "destroy": originals}
		if args.DestroyFromIfInState != nil {
			set["ifInState"] = *args.DestroyFromIfInState
		}
		c.imply("Email/set", set, emailSet)
	}
	return out, nil
}

// imply runs a method on behalf of the one that is running and queues its
// response behind that one's.
func (c *call) imply(name string, args any, run func(*call, json.RawMessage) (any, *methodError)) {
	raw, _ := json.Marshal(args)
	result, e := run(c, raw)
	if e != nil {
		body := map[string]any{"type": e.Type}
		if e.Description != "" {
			body["description"] = e.Description
		}
		out, _ := json.Marshal(body)
		c.implied = append(c.implied, invocation{name: "error", args: out})
		return
	}
	out, _ := json.Marshal(result)
	c.implied = append(c.implied, invocation{name: name, args: out})
}

// The default properties of Email/parse (RFC 8621 4.9).
var parseDefaultProperties = []string{"messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo",
	"subject", "sentAt", "hasAttachment", "preview", "bodyValues", "textBody", "htmlBody", "attachments"}

// emailParse is Email/parse (RFC 8621 4.9): the Email a blob would be,
// without storing it. It is how a client reads a message that is attached
// to another one.
func emailParse(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID           string    `json:"accountId"`
		BlobIDs             []string  `json:"blobIds"`
		Properties          *[]string `json:"properties"`
		BodyProperties      *[]string `json:"bodyProperties"`
		FetchTextBodyValues bool      `json:"fetchTextBodyValues"`
		FetchHTMLBodyValues bool      `json:"fetchHTMLBodyValues"`
		FetchAllBodyValues  bool      `json:"fetchAllBodyValues"`
		MaxBodyValueBytes   int64     `json:"maxBodyValueBytes"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.BlobIDs == nil {
		return nil, invalidArgs("blobIds is required")
	}
	if len(args.BlobIDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "more blobs than maxObjectsInGet")
	}
	if args.MaxBodyValueBytes < 0 {
		return nil, invalidArgs("maxBodyValueBytes is negative")
	}
	props := parseDefaultProperties
	if args.Properties != nil {
		props = *args.Properties
	}
	view := messageView{specs: map[string]headerSpec{}, bodyProps: bodyDefaultProperties, bodySpecs: map[string]headerSpec{},
		text: args.FetchTextBodyValues, html: args.FetchHTMLBodyValues, all: args.FetchAllBodyValues, maxBytes: args.MaxBodyValueBytes}
	for _, p := range props {
		if contains(EmailProperties, p) {
			continue
		}
		spec, ok := parseHeaderSpec(p)
		if !ok {
			return nil, invalidArgs("unknown property %q", p)
		}
		view.specs[p] = spec
	}
	if args.BodyProperties != nil {
		view.bodyProps = *args.BodyProperties
	}
	for _, p := range view.bodyProps {
		if contains(BodyProperties, p) {
			continue
		}
		spec, ok := parseHeaderSpec(p)
		if !ok {
			return nil, invalidArgs("unknown body property %q", p)
		}
		view.bodySpecs[p] = spec
	}

	parsed, notFound, notParsable := map[string]any{}, []string{}, []string{}
	for _, blobID := range args.BlobIDs {
		if _, done := parsed[blobID]; done {
			continue
		}
		data, err := c.h.blobData(c.ctx, args.AccountID, blobID)
		if errors.Is(err, store.ErrNoBlob) {
			notFound = append(notFound, blobID)
			continue
		}
		if err != nil {
			return nil, storeErr(err)
		}
		msg := message.Parse(data)
		if len(msg.Headers) == 0 {
			notParsable = append(notParsable, blobID)
			continue
		}
		obj := map[string]any{}
		for _, p := range props {
			switch p {
			case "id", "mailboxIds", "keywords", "receivedAt", "threadId":
				// "will be null if requested": the blob is no Email.
				obj[p] = nil
			case "blobId":
				obj[p] = blobID
			case "size":
				obj[p] = len(data)
			case "hasAttachment":
				obj[p] = msg.HasAttachment()
			case "preview":
				obj[p] = message.OneLine(msg.Preview(), message.PreviewLen)
			default:
				obj[p] = view.prop(p, blobID, msg.Headers, msg)
			}
		}
		parsed[blobID] = obj
	}
	out := map[string]any{"accountId": args.AccountID, "parsed": nil, "notParsable": nil, "notFound": nil}
	if len(parsed) > 0 {
		out["parsed"] = parsed
	}
	if len(notParsable) > 0 {
		out["notParsable"] = notParsable
	}
	if len(notFound) > 0 {
		out["notFound"] = notFound
	}
	return out, nil
}

// sortProperties orders the names of invalidProperties as RFC 8621 4.1.4
// lists the body properties (textBody, htmlBody, attachments,
// bodyStructure, bodyValues), the rest by name after them, each name once.
func sortProperties(names []string) {
	rank := func(name string) int {
		top, _, _ := strings.Cut(name, "/")
		for i, p := range []string{"textBody", "htmlBody", "attachments", "bodyStructure", "bodyValues"} {
			if top == p {
				return i
			}
		}
		return 5
	}
	sort.SliceStable(names, func(i, j int) bool {
		if ri, rj := rank(names[i]), rank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
}
