package jmap

import (
	"encoding/json"
	"errors"
	"html"
	"strings"

	"github.com/mikepage/robinmail/internal/message"
	"github.com/mikepage/robinmail/internal/search"
	"github.com/mikepage/robinmail/internal/store"
)

// How much of a body a snippet is looked for in, and how long one is (RFC
// 8621 5: "up to 255 octets" is for the client's layout, not a rule; ours
// are at most this many characters of the message).
const (
	snippetScan = 256 << 10
	snippetLen  = 200
)

// snippetTerms are the words of a filter that a snippet marks: in the
// subject, in the body, and the plain strings of subject conditions, which
// Email/query matches as substrings.
type snippetTerms struct {
	subject, body []search.Term
	subjectSubstr []string
}

// collect walks a filter. What stands under a NOT is not marked: a message
// in the results does not have it, or has it by accident.
func (t *snippetTerms) collect(f *store.Filter) {
	if f == nil {
		return
	}
	if f.Operator != "" {
		if f.Operator == "NOT" {
			return
		}
		for i := range f.Conditions {
			t.collect(&f.Conditions[i])
		}
		return
	}
	if f.Text != nil {
		terms := search.ParseQuery(*f.Text)
		t.subject = append(t.subject, terms...)
		t.body = append(t.body, terms...)
	}
	if f.Body != nil {
		t.body = append(t.body, search.ParseQuery(*f.Body)...)
	}
	if f.Subject != nil && *f.Subject != "" {
		t.subject = append(t.subject, search.ParseQuery(*f.Subject)...)
		t.subjectSubstr = append(t.subjectSubstr, *f.Subject)
	}
}

// markSubstring marks the first occurrences of needle in text, ASCII case
// ignored, as HTML.
func markSubstring(text, needle string) (string, bool) {
	lt, ln := strings.ToLower(text), strings.ToLower(needle)
	if len(lt) != len(text) || ln == "" {
		return "", false // lowering changed the length: offsets would be off
	}
	var b strings.Builder
	found := false
	for {
		i := strings.Index(lt, ln)
		if i < 0 {
			break
		}
		found = true
		b.WriteString(html.EscapeString(text[:i]))
		b.WriteString("<mark>" + html.EscapeString(text[i:i+len(ln)]) + "</mark>")
		text, lt = text[i+len(ln):], lt[i+len(ln):]
	}
	b.WriteString(html.EscapeString(text))
	return b.String(), found
}

// searchSnippetGet is SearchSnippet/get (RFC 8621 5.1). The index only
// knows which messages have a word, so the snippet comes from the message
// itself: it is read, decrypted and searched here, for the ids asked for.
func searchSnippetGet(c *call, raw json.RawMessage) (any, *methodError) {
	var args struct {
		AccountID string          `json:"accountId"`
		Filter    json.RawMessage `json:"filter"`
		EmailIDs  []string        `json:"emailIds"`
	}
	if e := c.decodeArgs(raw, &args, &args.AccountID); e != nil {
		return nil, e
	}
	if args.EmailIDs == nil {
		return nil, invalidArgs("emailIds is required")
	}
	if len(args.EmailIDs) > c.h.limits.MaxObjectsInGet {
		return nil, mErr("requestTooLarge", "at most %d emailIds in one call", c.h.limits.MaxObjectsInGet)
	}
	var terms snippetTerms
	if len(args.Filter) > 0 && string(args.Filter) != "null" {
		nodes := 0
		f, e := parseEmailFilter(args.Filter, 0, &nodes)
		if e != nil {
			return nil, e
		}
		terms.collect(f)
	}

	list, notFound := []map[string]any{}, []string{}
	seen := map[string]bool{}
	for _, sid := range args.EmailIDs {
		if seen[sid] {
			continue
		}
		seen[sid] = true
		id, ok := store.ParseEmailID(sid)
		if !ok {
			notFound = append(notFound, sid)
			continue
		}
		item := map[string]any{"emailId": sid, "subject": nil, "preview": nil}
		if len(terms.subject) == 0 && len(terms.body) == 0 {
			// Nothing to mark: no need to open the message, only to know it.
			found, err := c.h.Store.EmailsByID(c.ctx, args.AccountID, []int64{id})
			if err != nil {
				return nil, storeErr(err)
			}
			if len(found) == 0 {
				notFound = append(notFound, sid)
				continue
			}
			list = append(list, item)
			continue
		}
		rawMsg, _, err := c.h.Store.Raw(c.ctx, args.AccountID, id)
		if errors.Is(err, store.ErrNoEmail) {
			notFound = append(notFound, sid)
			continue
		}
		if err != nil {
			return nil, storeErr(err)
		}
		msg := message.Parse(rawMsg)
		if len(terms.subject) > 0 {
			v, _ := message.Get(msg.Headers, "Subject")
			subject := message.OneLine(message.Text(v), 1000)
			if marked, ok := search.Highlight(subject, terms.subject, 0); ok {
				item["subject"] = marked
			} else {
				for _, needle := range terms.subjectSubstr {
					if marked, ok := markSubstring(subject, needle); ok {
						item["subject"] = marked
						break
					}
				}
			}
		}
		if len(terms.body) > 0 {
			body := message.OneLine(msg.BodyText(snippetScan), snippetScan)
			if marked, ok := search.Highlight(body, terms.body, snippetLen); ok {
				item["preview"] = marked
			}
		}
		list = append(list, item)
	}
	out := map[string]any{"accountId": args.AccountID, "list": list, "notFound": nil}
	if len(notFound) > 0 {
		out["notFound"] = notFound
	}
	return out, nil
}
