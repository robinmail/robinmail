package jmap

import (
	"strings"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/store"
)

// RFC 8621 4.4.1: the conditions text, body and header of Email/query, and
// 4.5: the same filter in Email/queryChanges.
func TestRFC8621_4_4_1_TextFilters(t *testing.T) {
	e := newEnv(t)
	day := func(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }
	e1 := e.deliver(store.Message{ReceivedAt: day(1), Raw: []byte("From: Anna <anna@x.test>\r\nTo: me@example.test\r\n" +
		"Subject: =?utf-8?q?Caf=C3=A9_plans?=\r\nX-Tag: one two\r\n\r\nShall we meet at the harbour on Friday?\r\n")})
	e2 := e.deliver(store.Message{ReceivedAt: day(2), Raw: []byte("From: Bert <bert@y.test>\r\nTo: me@example.test\r\n" +
		"Subject: Invoice\r\nX-Tag: three\r\nContent-Type: text/html\r\n\r\n<p>The <i>har</i>bour fee is due.</p><script>var friday</script>\r\n")})
	ids := func(filter string) any {
		t.Helper()
		return e.one("Email/query", `{"accountId": "$A", "filter": `+filter+`, "sort": [{"property": "receivedAt"}]}`)["ids"]
	}
	list := func(es ...store.Email) string {
		parts := make([]string, len(es))
		for i, x := range es {
			parts[i] = `"` + eid(x) + `"`
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	for _, tc := range []struct{ filter, want string }{
		// "text: Looks for the text in Emails. The server MUST look up text in
		// the From, To, Cc, Bcc, and Subject header fields of the message and
		// SHOULD look inside any text/* or other body parts".
		{`{"text": "harbour"}`, list(e1, e2)},
		{`{"text": "anna"}`, list(e1)},
		{`{"text": "y.test"}`, list(e2)},
		{`{"text": "invoice"}`, list(e2)},
		// "Any syntactically correct encoded sections of header fields with a
		// known encoding SHOULD be decoded before attempting to match text."
		// "Text SHOULD be matched in a manner that is ... case-insensitive".
		{`{"text": "CAFE"}`, list(e1)},
		// "When searching inside a text/html body part, any text considered
		// markup rather than content SHOULD be ignored".
		{`{"text": "script"}`, list()},
		{`{"text": "friday"}`, list(e1)},
		// "a phrase in double quotes ... exact match"; other words are all
		// required, in any place.
		{`{"text": "\"fee harbour\""}`, list()},
		{`{"text": "\"harbour fee\""}`, list(e2)},
		{`{"text": "fee harbour"}`, list(e2)},
		// "body: Looks for the text in one of the body parts of the Email."
		{`{"body": "harbour"}`, list(e1, e2)},
		{`{"body": "invoice"}`, list()},
		// "header: ... The first element is the name of the header field to
		// match against. The second (optional) element is the text to look
		// for in the header field value. If not supplied, the message matches
		// simply if it has a header field of the given name."
		{`{"header": ["X-Tag"]}`, list(e1, e2)},
		{`{"header": ["x-tag", "two"]}`, list(e1)},
		{`{"header": ["X-Tag", "three"]}`, list(e2)},
		{`{"header": ["X-Nope"]}`, list()},
		{`{"header": ["Subject", "three"]}`, list()},
		{`{"operator": "AND", "conditions": [{"text": "harbour"}, {"operator": "NOT", "conditions": [{"from": "bert"}]}]}`, list(e1)},
		{`{"operator": "OR", "conditions": [{"header": ["X-Tag", "three"]}, {"body": "friday"}]}`, list(e1, e2)},
	} {
		same(t, tc.filter, ids(tc.filter), tc.want)
	}

	// 4.5: a message that arrives and matches is added, one that does not is not.
	args := `"accountId": "$A", "filter": {"text": "harbour"}, "sort": [{"property": "receivedAt"}]`
	since := e.one("Email/query", `{`+args+`}`)["queryState"].(string)
	e3 := e.deliver(store.Message{ReceivedAt: day(3), Raw: simple("s3@x", "carl@x.test", "Harbour tour")})
	e.deliver(store.Message{ReceivedAt: day(4), Raw: simple("s4@x", "carl@x.test", "Something else")})
	qc := e.one("Email/queryChanges", `{`+args+`, "sinceQueryState": "`+since+`"}`)
	same(t, "queryChanges added", qc["added"], `[{"id": "`+eid(e3)+`", "index": 2}]`)
}

// RFC 8621 5 and 5.1: SearchSnippet/get.
func TestRFC8621_5_SearchSnippet(t *testing.T) {
	e := newEnv(t)
	// The two emails of the example in 5.1.1. The RFC's server marks "Foo"
	// inside "Foosball" for the search "foo": how text matches is the
	// server's to define (4.4.1), and here it is whole words, so the search
	// is for "foosball".
	none := e.deliver(store.Message{Raw: simple("n@x", "a@x.test", "Nothing of the kind")})
	hit := e.deliver(store.Message{Raw: []byte("From: a@x.test\r\nSubject: The Foosball competition\r\n\r\n" +
		"This year the foosball competition will be held in the Stadium de Football <Paris> & more.\r\n")})
	got := e.one("SearchSnippet/get", `{"accountId": "$A", "filter": {"text": "foosball"},
		"emailIds": ["`+eid(none)+`", "`+eid(hit)+`", "e999999", "nonsense"]}`)
	// "subject: If text from the filter matches the subject, this is the
	// subject of the Email with the matching words/phrases wrapped in
	// <mark></mark> tags. If it does not match, this is null."
	// "preview: ... a relevant section of the body (converted to plain
	// text), with matching words/phrases wrapped in <mark></mark> tags.
	// ... any other HTML tags ... MUST be escaped"
	same(t, "list", got["list"], `[
		{"emailId": "`+eid(none)+`", "subject": null, "preview": null},
		{"emailId": "`+eid(hit)+`", "subject": "The <mark>Foosball</mark> competition",
		 "preview": "This year the <mark>foosball</mark> competition will be held in the Stadium de Football &lt;Paris&gt; &amp; more."}]`)
	// "notFound: An array of Email ids requested that could not be found, or
	// null if all ids were found."
	same(t, "notFound", got["notFound"], `["e999999", "nonsense"]`)
	got = e.one("SearchSnippet/get", `{"accountId": "$A", "filter": {"text": "foosball"}, "emailIds": ["`+eid(hit)+`"]}`)
	same(t, "all found", got["notFound"], `null`)

	// A phrase is marked as one; a long body is cut around the match.
	long := e.deliver(store.Message{Raw: []byte("From: a@x.test\r\nSubject: Long\r\n\r\n" +
		strings.Repeat("Lorem ipsum dolor sit amet. ", 40) + "The harbour fee is due. " + strings.Repeat("Consectetur adipiscing. ", 40))})
	got = e.one("SearchSnippet/get", `{"accountId": "$A", "filter": {"body": "\"harbour fee\""}, "emailIds": ["`+eid(long)+`"]}`)
	preview := at(t, got, "/list/0/preview").(string)
	if !strings.Contains(preview, "The <mark>harbour fee</mark> is due.") || !strings.HasPrefix(preview, "…") ||
		!strings.HasSuffix(preview, "…") || len([]rune(preview)) > snippetLen+20 {
		t.Errorf("preview of a long body: %q", preview)
	}
	same(t, "body does not mark the subject", at(t, got, "/list/0/subject"), `null`)

	// The subject condition matches substrings, and is marked as it matched.
	got = e.one("SearchSnippet/get", `{"accountId": "$A", "filter": {"subject": "oosb"}, "emailIds": ["`+eid(hit)+`"]}`)
	same(t, "substring", at(t, got, "/list/0"), `{"emailId": "`+eid(hit)+`", "subject": "The F<mark>oosb</mark>all competition", "preview": null}`)

	// What stands under NOT is not marked; conditions without text mark nothing.
	got = e.one("SearchSnippet/get", `{"accountId": "$A", "filter": {"operator": "NOT", "conditions": [{"text": "foosball"}]}, "emailIds": ["`+eid(hit)+`"]}`)
	same(t, "NOT", at(t, got, "/list/0"), `{"emailId": "`+eid(hit)+`", "subject": null, "preview": null}`)
	got = e.one("SearchSnippet/get", `{"accountId": "$A", "filter": null, "emailIds": ["`+eid(hit)+`", "e999999"]}`)
	same(t, "no filter", []any{got["list"], got["notFound"]}, `[[{"emailId": "`+eid(hit)+`", "subject": null, "preview": null}], ["e999999"]]`)

	// 5.1: "requestTooLarge: The number of actions ... exceeds the maximum".
	many := make([]string, DefaultLimits.MaxObjectsInGet+1)
	for i := range many {
		many[i] = `"e1"`
	}
	for args, want := range map[string]string{
		`{"accountId": "$A", "filter": null, "emailIds": [` + strings.Join(many, ",") + `]}`: "requestTooLarge",
		`{"accountId": "$A", "filter": {"colour": "red"}, "emailIds": []}`:                   "invalidArguments",
		`{"accountId": "$A", "filter": null}`:                                                "invalidArguments",
		`{"accountId": "nope", "filter": null, "emailIds": []}`:                              "accountNotFound",
	} {
		if got := e.fail("SearchSnippet/get", args); got != want {
			t.Errorf("SearchSnippet/get %.80s: %s, want %s", args, got, want)
		}
	}
}
