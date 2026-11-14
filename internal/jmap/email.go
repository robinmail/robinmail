package jmap

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/store"
)

// threadGet is Thread/get (RFC 8621 3.1).
func threadGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID  string    `json:"accountId"`
		IDs        *[]string `json:"ids"`
		Properties *[]string `json:"properties"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if _, e := checkProperties(args.Properties, []string{"id", "emailIds"}); e != nil {
		return nil, e
	}
	if args.IDs == nil {
		return nil, mErr("requestTooLarge", "the server does not return all threads at once; give ids")
	}
	if len(*args.IDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
	}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	var ids []int64
	for _, s := range *args.IDs {
		if id, ok := store.ParseThreadID(s); ok {
			ids = append(ids, id)
		}
	}
	threads, err := c.h.Store.Threads(c.ctx, args.AccountID, ids)
	if err != nil {
		return nil, storeErr(err)
	}
	list, notFound := []map[string]any{}, []string{}
	seen := map[string]bool{}
	for _, s := range *args.IDs {
		if seen[s] {
			continue
		}
		seen[s] = true
		id, _ := store.ParseThreadID(s)
		emails, ok := threads[id]
		if !ok {
			notFound = append(notFound, s)
			continue
		}
		emailIDs := make([]string, len(emails))
		for i, e := range emails {
			emailIDs[i] = store.EmailID(e)
		}
		obj := map[string]any{"id": s, "emailIds": emailIDs}
		if args.Properties != nil {
			obj = pick(obj, *args.Properties)
		}
		list = append(list, obj)
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// threadChanges is Thread/changes (RFC 8621 3.2).
func threadChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.changes(raw, "Thread", store.ThreadID)
}

// emailChanges is Email/changes (RFC 8621 4.3).
func emailChanges(c *call, raw json.RawMessage) (any, *methodError) {
	return c.changes(raw, "Email", store.EmailID)
}

// Properties of an Email (RFC 8621 4.1). The first group comes from the
// email row; the others need the message itself.
var (
	emailMetaProperties   = []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt", "hasAttachment", "preview"}
	emailHeaderProperties = []string{"messageId", "inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo",
		"subject", "sentAt", "headers"}
	emailBodyProperties = []string{"bodyStructure", "bodyValues", "textBody", "htmlBody", "attachments"}
	// EmailProperties is every plain property of an Email the server has.
	// "header:Name:asForm:all" properties come on top.
	EmailProperties = append(append(append([]string{}, emailMetaProperties...), emailHeaderProperties...), emailBodyProperties...)
	// The default of Email/get (RFC 8621 4.2).
	emailDefaultProperties = []string{"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt", "messageId",
		"inReplyTo", "references", "sender", "from", "to", "cc", "bcc", "replyTo", "subject", "sentAt", "hasAttachment",
		"preview", "bodyValues", "textBody", "htmlBody", "attachments"}
	// BodyProperties is every property of an EmailBodyPart.
	BodyProperties = []string{"partId", "blobId", "size", "headers", "name", "type", "charset", "disposition", "cid",
		"language", "location", "subParts"}
	// The default bodyProperties (RFC 8621 4.2).
	bodyDefaultProperties = []string{"partId", "blobId", "size", "name", "type", "charset", "disposition", "cid", "language", "location"}
)

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// headerSpec is a parsed "header:Name:asForm:all" property (RFC 8621
// 4.1.3).
type headerSpec struct {
	name string
	form string
	all  bool
}

func parseHeaderSpec(prop string) (headerSpec, bool) {
	parts := strings.Split(prop, ":")
	if len(parts) < 2 || len(parts) > 4 || parts[0] != "header" || parts[1] == "" {
		return headerSpec{}, false
	}
	for i := 0; i < len(parts[1]); i++ {
		if c := parts[1][i]; c < 33 || c > 126 {
			return headerSpec{}, false
		}
	}
	spec := headerSpec{name: parts[1], form: message.FormRaw}
	rest := parts[2:]
	if len(rest) > 0 && strings.HasPrefix(rest[0], "as") {
		spec.form = rest[0]
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == "all" {
		spec.all = true
		rest = rest[1:]
	}
	if len(rest) > 0 || !message.FormAllowed(spec.name, spec.form) {
		return headerSpec{}, false
	}
	return spec, true
}

// headerForm converts one header value to a form.
func headerForm(v, form string) any {
	switch form {
	case message.FormText:
		return message.Text(v)
	case message.FormAddresses:
		return message.Addresses(v)
	case message.FormGroupedAddresses:
		return message.GroupedAddresses(v)
	case message.FormMessageIDs:
		if ids := message.MessageIDs(v); len(ids) > 0 {
			return ids
		}
		return nil
	case message.FormDate:
		if t, ok := message.Date(v); ok {
			return t.Format(time.RFC3339)
		}
		return nil
	case message.FormURLs:
		if urls := message.URLs(v); len(urls) > 0 {
			return urls
		}
		return nil
	}
	return v
}

// headerValue is the value of a header property: of the last field with
// the name, or of all of them (RFC 8621 4.1.3).
func (s headerSpec) value(fields []message.Header) any {
	values := message.All(fields, s.name)
	if s.all {
		out := make([]any, len(values))
		for i, v := range values {
			out[i] = headerForm(v, s.form)
		}
		return out
	}
	if len(values) == 0 {
		return nil
	}
	return headerForm(values[len(values)-1], s.form)
}

func headersList(fields []message.Header) []map[string]string {
	out := make([]map[string]string, len(fields))
	for i, f := range fields {
		out[i] = map[string]string{"name": f.Name, "value": f.Value}
	}
	return out
}

// headerProperty is the value of one of the convenience properties, each
// of which RFC 8621 4.1.2 defines as a header in a form.
func headerProperty(fields []message.Header, prop string) any {
	spec := map[string]headerSpec{
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
	}[prop]
	return spec.value(fields)
}

// PartBlobID is the blobId of a body part: the blob of the message, a dash
// and the part id. The part is found again by parsing the message.
func PartBlobID(messageBlob, partID string) string { return messageBlob + "-" + partID }

// partObject is an EmailBodyPart (RFC 8621 4.1.4) with the asked
// properties.
func partObject(p *message.Part, blob string, props []string, specs map[string]headerSpec, withSubParts bool) map[string]any {
	out := map[string]any{}
	for _, prop := range props {
		switch prop {
		case "partId":
			if p.IsMultipart() {
				out[prop] = nil
			} else {
				out[prop] = p.PartID
			}
		case "blobId":
			if p.IsMultipart() {
				out[prop] = nil
			} else {
				out[prop] = PartBlobID(blob, p.PartID)
			}
		case "size":
			out[prop] = p.Size
		case "headers":
			out[prop] = headersList(p.Headers)
		case "name":
			out[prop] = p.Name
		case "type":
			out[prop] = p.Type
		case "charset":
			out[prop] = p.Charset
		case "disposition":
			out[prop] = p.Disposition
		case "cid":
			out[prop] = p.CID
		case "language":
			if p.Language == nil {
				out[prop] = nil
			} else {
				out[prop] = p.Language
			}
		case "location":
			out[prop] = p.Location
		case "subParts":
			// Only in bodyStructure, below.
		default:
			out[prop] = specs[prop].value(p.Headers)
		}
	}
	if withSubParts {
		if p.IsMultipart() {
			subs := make([]map[string]any, len(p.SubParts))
			for i, sp := range p.SubParts {
				subs[i] = partObject(sp, blob, props, specs, true)
			}
			out["subParts"] = subs
		} else {
			out["subParts"] = nil
		}
	}
	return out
}

func utcDate(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// emailGet is Email/get (RFC 8621 4.2).
func emailGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID           string    `json:"accountId"`
		IDs                 *[]string `json:"ids"`
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
	if args.IDs == nil {
		return nil, mErr("requestTooLarge", "the server does not return all emails at once; give ids")
	}
	if len(*args.IDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "more ids than maxObjectsInGet")
	}
	if args.MaxBodyValueBytes < 0 {
		return nil, invalidArgs("maxBodyValueBytes is negative")
	}

	props := emailDefaultProperties
	if args.Properties != nil {
		props = *args.Properties
	}
	specs := map[string]headerSpec{}
	needMessage, needBody := false, false
	for _, p := range props {
		switch {
		case contains(emailMetaProperties, p):
		case contains(emailHeaderProperties, p):
			needMessage = true
		case contains(emailBodyProperties, p):
			needMessage, needBody = true, true
		default:
			spec, ok := parseHeaderSpec(p)
			if !ok {
				return nil, invalidArgs("unknown property %q", p)
			}
			specs[p] = spec
			needMessage = true
		}
	}
	bodyProps := bodyDefaultProperties
	if args.BodyProperties != nil {
		bodyProps = *args.BodyProperties
	}
	bodySpecs := map[string]headerSpec{}
	for _, p := range bodyProps {
		if contains(BodyProperties, p) {
			continue
		}
		spec, ok := parseHeaderSpec(p)
		if !ok {
			return nil, invalidArgs("unknown body property %q", p)
		}
		bodySpecs[p] = spec
	}

	view := messageView{specs: specs, bodyProps: bodyProps, bodySpecs: bodySpecs, text: args.FetchTextBodyValues,
		html: args.FetchHTMLBodyValues, all: args.FetchAllBodyValues, maxBytes: args.MaxBodyValueBytes}
	state, err := c.h.Store.State(c.ctx, args.AccountID)
	if err != nil {
		return nil, storeErr(err)
	}
	var ids []int64
	for _, s := range *args.IDs {
		if id, ok := store.ParseEmailID(s); ok {
			ids = append(ids, id)
		}
	}
	emails, err := c.h.Store.EmailsByID(c.ctx, args.AccountID, ids)
	if err != nil {
		return nil, storeErr(err)
	}
	byID := map[string]store.Email{}
	for _, e := range emails {
		byID[store.EmailID(e.ID)] = e
	}

	list, notFound := []map[string]any{}, []string{}
	seen := map[string]bool{}
	for _, sid := range *args.IDs {
		if seen[sid] {
			continue
		}
		seen[sid] = true
		e, ok := byID[sid]
		if !ok {
			notFound = append(notFound, sid)
			continue
		}
		var fields []message.Header
		var msg *message.Message
		if needMessage {
			data, _, err := c.h.Store.Raw(c.ctx, args.AccountID, e.ID)
			if errors.Is(err, store.ErrNoEmail) {
				notFound = append(notFound, sid) // destroyed since the lookup
				continue
			}
			if err != nil {
				return nil, storeErr(err)
			}
			if needBody {
				msg = message.Parse(data)
				fields = msg.Headers
			} else {
				fields = message.ParseHeader(data)
			}
		}
		obj := map[string]any{"id": sid}
		for _, p := range props {
			switch p {
			case "id":
			case "blobId":
				obj[p] = e.Blob
			case "threadId":
				obj[p] = store.ThreadID(e.ThreadID)
			case "mailboxIds":
				m := map[string]bool{}
				for id := range e.Mailboxes {
					m[store.MailboxID(id)] = true
				}
				obj[p] = m
			case "keywords":
				m := map[string]bool{}
				for _, k := range e.Keywords {
					m[k] = true
				}
				obj[p] = m
			case "size":
				obj[p] = e.Size
			case "receivedAt":
				obj[p] = utcDate(e.ReceivedAt)
			case "hasAttachment":
				obj[p] = e.HasAttachment
			case "preview":
				obj[p] = e.Preview
			default:
				obj[p] = view.prop(p, e.Blob, fields, msg)
			}
		}
		list = append(list, obj)
	}
	return map[string]any{"accountId": args.AccountID, "state": stateString(state), "list": list, "notFound": notFound}, nil
}

// messageView is how the properties of an Email that come from its message
// are to be shown: Email/get and Email/parse share it.
type messageView struct {
	specs           map[string]headerSpec
	bodyProps       []string
	bodySpecs       map[string]headerSpec
	text, html, all bool // which body values to fetch
	maxBytes        int64
}

// prop is the value of one header or body property. msg is nil when no
// body property was asked for.
func (v messageView) prop(p, blob string, fields []message.Header, msg *message.Message) any {
	parts := func(list []*message.Part) []map[string]any {
		out := make([]map[string]any, len(list))
		for i, part := range list {
			out[i] = partObject(part, blob, v.bodyProps, v.bodySpecs, false)
		}
		return out
	}
	switch p {
	case "headers":
		return headersList(fields)
	case "bodyStructure":
		return partObject(msg.Root, blob, v.bodyProps, v.bodySpecs, true)
	case "textBody", "htmlBody", "attachments":
		text, html, attachments := msg.Bodies()
		return parts(map[string][]*message.Part{"textBody": text, "htmlBody": html, "attachments": attachments}[p])
	case "bodyValues":
		text, html, _ := msg.Bodies()
		values := map[string]any{}
		add := func(list []*message.Part) {
			for _, part := range list {
				if _, done := values[part.PartID]; done || !strings.HasPrefix(part.Type, "text/") {
					continue
				}
				val, problem, truncated := part.BodyValue(int(min(v.maxBytes, 1<<30)))
				values[part.PartID] = map[string]any{"value": val, "isEncodingProblem": problem, "isTruncated": truncated}
			}
		}
		if v.all {
			add(msg.Leaves)
		}
		if v.text {
			add(text)
		}
		if v.html {
			add(html)
		}
		return values
	}
	if spec, ok := v.specs[p]; ok {
		return spec.value(fields)
	}
	return headerProperty(fields, p)
}

// Filter conditions of Email/query (RFC 8621 4.4.1) the server has, and
// those of the RFC it does not: they need the text of messages, which is
// not indexed before phase 8.
var (
	EmailFilters = []string{"inMailbox", "inMailboxOtherThan", "before", "after", "minSize", "maxSize",
		"allInThreadHaveKeyword", "someInThreadHaveKeyword", "noneInThreadHaveKeyword", "hasKeyword", "notKeyword",
		"hasAttachment", "from", "to", "cc", "bcc", "subject"}
	unsupportedEmailFilters = []string{"text", "body", "header"}
)

func parseEmailFilter(raw json.RawMessage, depth int, nodes *int) (*store.Filter, *methodError) {
	if *nodes++; *nodes > maxFilterNodes || depth > maxFilterDepth {
		return nil, invalidArgs("the filter is too large")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, invalidArgs("a filter must be an object")
	}
	f := &store.Filter{}
	if op, ok := obj["operator"]; ok {
		var conds []json.RawMessage
		if json.Unmarshal(op, &f.Operator) != nil || (f.Operator != "AND" && f.Operator != "OR" && f.Operator != "NOT") {
			return nil, invalidArgs("a filter operator must be AND, OR or NOT")
		}
		if len(obj) != 2 || json.Unmarshal(obj["conditions"], &conds) != nil || conds == nil {
			return nil, invalidArgs("a filter operator needs conditions and nothing else")
		}
		for _, cr := range conds {
			sub, e := parseEmailFilter(cr, depth+1, nodes)
			if e != nil {
				return nil, e
			}
			f.Conditions = append(f.Conditions, *sub)
		}
		return f, nil
	}
	// An id that cannot exist matches nothing: no mailbox has id -1.
	mailbox := func(s string) int64 {
		if id, ok := store.ParseMailboxID(s); ok {
			return id
		}
		return -1
	}
	for key, v := range obj {
		bad := invalidArgs("the filter property %q has the wrong type", key)
		str := func(dst **string) *methodError {
			if json.Unmarshal(v, dst) != nil || *dst == nil {
				return bad
			}
			return nil
		}
		keyword := func(dst **string) *methodError {
			if e := str(dst); e != nil {
				return e
			}
			if len(**dst) == 0 || len(**dst) > 255 {
				return bad
			}
			return nil
		}
		date := func(dst **time.Time) *methodError {
			var s string
			if json.Unmarshal(v, &s) != nil {
				return bad
			}
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return bad
			}
			*dst = &t
			return nil
		}
		size := func(dst **int64) *methodError {
			if json.Unmarshal(v, dst) != nil || *dst == nil || **dst < 0 {
				return bad
			}
			return nil
		}
		var e *methodError
		switch key {
		case "inMailbox":
			var s *string
			if e = str(&s); e == nil {
				id := mailbox(*s)
				f.InMailbox = &id
			}
		case "inMailboxOtherThan":
			var list []string
			if json.Unmarshal(v, &list) != nil || list == nil || len(list) > maxFilterNodes {
				return nil, bad
			}
			f.InMailboxOtherThan = []int64{}
			for _, s := range list {
				f.InMailboxOtherThan = append(f.InMailboxOtherThan, mailbox(s))
			}
		case "before":
			e = date(&f.Before)
		case "after":
			e = date(&f.After)
		case "minSize":
			e = size(&f.MinSize)
		case "maxSize":
			e = size(&f.MaxSize)
		case "allInThreadHaveKeyword":
			e = keyword(&f.AllInThreadHaveKeyword)
		case "someInThreadHaveKeyword":
			e = keyword(&f.SomeInThreadHaveKeyword)
		case "noneInThreadHaveKeyword":
			e = keyword(&f.NoneInThreadHaveKeyword)
		case "hasKeyword":
			e = keyword(&f.HasKeyword)
		case "notKeyword":
			e = keyword(&f.NotKeyword)
		case "hasAttachment":
			if json.Unmarshal(v, &f.HasAttachment) != nil || f.HasAttachment == nil {
				return nil, bad
			}
		case "from":
			e = str(&f.From)
		case "to":
			e = str(&f.To)
		case "cc":
			e = str(&f.Cc)
		case "bcc":
			e = str(&f.Bcc)
		case "subject":
			e = str(&f.Subject)
		default:
			if contains(unsupportedEmailFilters, key) {
				return nil, mErr("unsupportedFilter", "the server cannot search the text of messages yet; %q is not supported", key)
			}
			return nil, invalidArgs("unknown filter property %q", key)
		}
		if e != nil {
			return nil, e
		}
	}
	return f, nil
}

type emailQueryArgs struct {
	AccountID string          `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	Sort      *[]struct {
		Property    string  `json:"property"`
		IsAscending *bool   `json:"isAscending"`
		Collation   *string `json:"collation"`
		Keyword     *string `json:"keyword"`
	} `json:"sort"`
	queryArgs
	CollapseThreads bool `json:"collapseThreads"`
}

// storeQuery turns the filter and sort of an Email/query into the store's.
func (args *emailQueryArgs) storeQuery() (store.EmailQuery, *methodError) {
	q := store.EmailQuery{CollapseThreads: args.CollapseThreads, Limit: -1}
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		nodes := 0
		f, e := parseEmailFilter(args.Filter, 0, &nodes)
		if e != nil {
			return q, e
		}
		q.Filter = f
	}
	if args.Sort != nil {
		for _, s := range *args.Sort {
			if !contains(store.SortProperties, s.Property) {
				return q, mErr("unsupportedSort", "the server cannot sort emails by %q", s.Property)
			}
			if s.Collation != nil && *s.Collation != "i;ascii-casemap" {
				return q, mErr("unsupportedSort", "the server does not have the collation %q", *s.Collation)
			}
			srt := store.Sort{Property: s.Property, Ascending: s.IsAscending == nil || *s.IsAscending}
			if s.Property == "hasKeyword" {
				if s.Keyword == nil || *s.Keyword == "" {
					return q, invalidArgs("sorting by hasKeyword needs a keyword")
				}
				srt.Keyword = *s.Keyword
			}
			q.Sort = append(q.Sort, srt)
		}
	}
	return q, nil
}

func emailIDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = store.EmailID(id)
	}
	return out
}

// emailQuery is Email/query (RFC 8621 4.4).
func emailQuery(c *call, raw json.RawMessage) (any, *methodError) {
	var args emailQueryArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	limit, clamped, e := args.limit()
	if e != nil {
		return nil, e
	}
	q, e := args.storeQuery()
	if e != nil {
		return nil, e
	}
	q.Total = args.CalculateTotal
	out := map[string]any{"accountId": args.AccountID, "canCalculateChanges": true}
	if args.Anchor == nil && args.Position >= 0 {
		// The common case: the window is cut in SQL.
		q.Offset, q.Limit = args.Position, limit
		res, err := c.h.Store.QueryEmails(c.ctx, args.AccountID, q)
		if err != nil {
			return nil, storeErr(err)
		}
		out["queryState"], out["position"], out["ids"] = stateString(res.State), args.Position, emailIDs(res.IDs)
		if args.CalculateTotal {
			out["total"] = res.Total
		}
	} else {
		// An anchor or a position from the end needs the whole list.
		res, err := c.h.Store.QueryEmails(c.ctx, args.AccountID, q)
		if err != nil {
			return nil, storeErr(err)
		}
		position, ids, e := window(emailIDs(res.IDs), args.Position, args.Anchor, args.AnchorOffset, limit)
		if e != nil {
			return nil, e
		}
		out["queryState"], out["position"], out["ids"] = stateString(res.State), position, ids
		if args.CalculateTotal {
			out["total"] = len(res.IDs)
		}
	}
	if clamped {
		out["limit"] = limit
	}
	return out, nil
}

// emailQueryChanges is Email/queryChanges (RFC 8621 4.5). The candidates
// are the emails that changed and all emails of the threads that changed:
// with collapseThreads, or a filter on the keywords of a thread, a change
// to one email moves others of its thread in or out of the results.
func emailQueryChanges(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		emailQueryArgs
		queryChangesArgs
	}
	var probe map[string]json.RawMessage
	json.Unmarshal(raw, &probe)
	for _, k := range []string{"position", "anchor", "anchorOffset", "limit"} {
		if _, ok := probe[k]; ok {
			return nil, invalidArgs("unknown argument %q", k)
		}
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.SinceQueryState == nil {
		return nil, invalidArgs("sinceQueryState is required")
	}
	since, ok := parseState(*args.SinceQueryState)
	if !ok {
		return nil, mErr("cannotCalculateChanges", "the server cannot calculate the changes from that state")
	}
	q, e := args.storeQuery()
	if e != nil {
		return nil, e
	}
	st := c.h.Store
	emails, _, _, err := st.ChangesOf(c.ctx, args.AccountID, "Email", since, 0)
	if err != nil {
		return nil, storeErr(err)
	}
	threads, _, _, err := st.ChangesOf(c.ctx, args.AccountID, "Thread", since, 0)
	if err != nil {
		return nil, storeErr(err)
	}
	if len(emails.Created)+len(emails.Updated)+len(emails.Destroyed) > maxChangesLimit {
		return nil, mErr("cannotCalculateChanges", "too much has changed; run the query again")
	}
	var candidates []string
	for _, list := range [][]int64{emails.Created, emails.Updated, emails.Destroyed} {
		candidates = append(candidates, emailIDs(list)...)
	}
	touched := append(append([]int64{}, threads.Created...), threads.Updated...)
	if len(touched) > 0 {
		members, err := st.Threads(c.ctx, args.AccountID, touched)
		if err != nil {
			return nil, storeErr(err)
		}
		keys := make([]int64, 0, len(members))
		for t := range members {
			keys = append(keys, t)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, t := range keys {
			candidates = append(candidates, emailIDs(members[t])...)
		}
	}
	res, err := st.QueryEmails(c.ctx, args.AccountID, q)
	if err != nil {
		return nil, storeErr(err)
	}
	return queryChanges(args.AccountID, args.queryChangesArgs, since, res.State, candidates, emailIDs(res.IDs))
}

// unescapePointer undoes the escaping of one JSON Pointer token.
func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// emailPatch reads the PatchObject of an Email/set update (RFC 8620 5.3,
// RFC 8621 4.6) into a store update. Only keywords and mailboxIds can
// change: everything else of an Email is immutable.
func (c *call) emailPatch(patch map[string]json.RawMessage) (store.Update, *setError) {
	var u store.Update
	var immutable []string
	wholeKeywords, wholeMailboxes := false, false
	flag := func(v json.RawMessage) (set, ok bool) {
		switch string(v) {
		case "true":
			return true, true
		case "null":
			return false, true
		}
		return false, false
	}
	for key, v := range patch {
		switch {
		case key == "keywords":
			var m map[string]bool
			if json.Unmarshal(v, &m) != nil || m == nil {
				return u, invalidProps("keywords must be an object of keyword: true", "keywords")
			}
			wholeKeywords, u.ReplaceKeywords = true, true
			for k, on := range m {
				if !on {
					return u, invalidProps("a keyword is either true or not there", "keywords")
				}
				u.AddKeywords = append(u.AddKeywords, k)
			}
		case key == "mailboxIds":
			var m map[string]bool
			if json.Unmarshal(v, &m) != nil || m == nil {
				return u, invalidProps("mailboxIds must be an object of id: true", "mailboxIds")
			}
			if len(m) == 0 {
				return u, invalidProps("an email must be in at least one mailbox", "mailboxIds")
			}
			wholeMailboxes, u.ReplaceMailboxes = true, true
			for sid, on := range m {
				id, ok := c.mailboxRef(sid)
				if !on || !ok {
					return u, invalidProps("there is no mailbox "+sid, "mailboxIds")
				}
				u.AddMailboxes = append(u.AddMailboxes, id)
			}
		case strings.HasPrefix(key, "keywords/"):
			k := unescapePointer(strings.TrimPrefix(key, "keywords/"))
			set, ok := flag(v)
			if !ok {
				return u, invalidProps("a keyword is set with true and removed with null", "keywords")
			}
			if set {
				u.AddKeywords = append(u.AddKeywords, k)
			} else {
				u.RemoveKeywords = append(u.RemoveKeywords, k)
			}
		case strings.HasPrefix(key, "mailboxIds/"):
			id, found := c.mailboxRef(unescapePointer(strings.TrimPrefix(key, "mailboxIds/")))
			set, ok := flag(v)
			if !ok {
				return u, invalidProps("a mailbox is added with true and removed with null", "mailboxIds")
			}
			switch {
			case set && !found:
				return u, invalidProps("there is no mailbox "+strings.TrimPrefix(key, "mailboxIds/"), "mailboxIds")
			case set:
				u.AddMailboxes = append(u.AddMailboxes, id)
			case found:
				u.RemoveMailboxes = append(u.RemoveMailboxes, id)
			}
		default:
			immutable = append(immutable, key)
		}
	}
	if len(immutable) > 0 {
		sort.Strings(immutable)
		return u, invalidProps("only keywords and mailboxIds of an email can change", immutable...)
	}
	// RFC 8620 5.3: a patch may not set a property and something inside it.
	if wholeKeywords {
		for key := range patch {
			if strings.HasPrefix(key, "keywords/") {
				return u, &setError{Type: "invalidPatch", Description: "keywords is set as a whole and in part"}
			}
		}
	}
	if wholeMailboxes {
		for key := range patch {
			if strings.HasPrefix(key, "mailboxIds/") {
				return u, &setError{Type: "invalidPatch", Description: "mailboxIds is set as a whole and in part"}
			}
		}
	}
	sort.Strings(u.AddKeywords)
	sort.Strings(u.RemoveKeywords)
	sort.Slice(u.AddMailboxes, func(i, j int) bool { return u.AddMailboxes[i] < u.AddMailboxes[j] })
	return u, nil
}

// emailSet is Email/set (RFC 8621 4.6): create (create.go), update of
// keywords and mailboxIds, and destroy.
func emailSet(c *call, raw json.RawMessage) (any, *methodError) {
	var args setArgs
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	run, e := c.begin(&args)
	if e != nil {
		return nil, e
	}
	st := c.h.Store
	mismatch := mErr("stateMismatch", "the state is not the ifInState given")

	creates := make([]string, 0, len(args.Create))
	for cid := range args.Create {
		creates = append(creates, cid)
	}
	sort.Strings(creates)
	for _, cid := range creates {
		created, se, me := c.createEmail(run, args.Create[cid])
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

	updates := make([]string, 0, len(args.Update))
	for id := range args.Update {
		updates = append(updates, id)
	}
	sort.Strings(updates)
	for _, sid := range updates {
		id, ok := c.emailRef(sid)
		if !ok {
			run.notUpdated[sid] = &setError{Type: "notFound"}
			continue
		}
		u, se := c.emailPatch(args.Update[sid])
		if se != nil {
			run.notUpdated[sid] = se
			continue
		}
		u.IfInState = run.ifState
		empty := len(args.Update[sid]) == 0
		state, err := st.UpdateEmail(c.ctx, args.AccountID, id, u)
		switch {
		case errors.Is(err, store.ErrStateMismatch):
			return nil, mismatch
		case errors.Is(err, store.ErrNoEmail):
			run.notUpdated[sid] = &setError{Type: "notFound"}
		case errors.Is(err, store.ErrNoMailbox):
			run.notUpdated[sid] = invalidProps("one of the mailboxes does not exist", "mailboxIds")
		case errors.Is(err, store.ErrNoMailboxes):
			run.notUpdated[sid] = invalidProps("an email must be in at least one mailbox", "mailboxIds")
		case errors.Is(err, store.ErrKeyword):
			run.notUpdated[sid] = invalidProps("a keyword is 1 to 255 printable ASCII characters without ( ) { ] % * \" \\", "keywords")
		case err != nil:
			run.notUpdated[sid] = &setError{Type: "serverFail"}
		default:
			if empty {
				// Nothing to change; but the email has to exist.
				if found, err := st.EmailsByID(c.ctx, args.AccountID, []int64{id}); err != nil || len(found) == 0 {
					run.notUpdated[sid] = &setError{Type: "notFound"}
					continue
				}
			} else {
				run.wroteState(state)
			}
			if run.updated == nil {
				run.updated = map[string]any{}
			}
			run.updated[sid] = nil
		}
	}

	if args.Destroy != nil {
		for _, sid := range *args.Destroy {
			id, ok := store.ParseEmailID(sid)
			if !ok {
				run.notDestroyed[sid] = &setError{Type: "notFound"}
				continue
			}
			state, err := st.DestroyEmailIf(c.ctx, args.AccountID, id, run.ifState)
			switch {
			case errors.Is(err, store.ErrStateMismatch):
				return nil, mismatch
			case errors.Is(err, store.ErrNoEmail):
				run.notDestroyed[sid] = &setError{Type: "notFound"}
			case err != nil:
				run.notDestroyed[sid] = &setError{Type: "serverFail"}
			default:
				run.wroteState(state)
				run.destroyed = append(run.destroyed, sid)
			}
		}
	}
	return run.result(), nil
}
