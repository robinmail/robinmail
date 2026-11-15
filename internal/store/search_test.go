package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/robinmail/robinmail/internal/rqlite"
)

func queryIDs(t *testing.T, s *Store, a Account, f Filter) []int64 {
	t.Helper()
	res, err := s.QueryEmails(ctx, a.ID, EmailQuery{Filter: &f, Sort: []Sort{{Property: "receivedAt", Ascending: true}}, Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	return res.IDs
}

// RFC 8621 4.4.1: the conditions text, body and header of Email/query.
func TestTextSearch(t *testing.T) {
	s := open(t)
	a, other := account(t, s), account(t, s)

	plain := deliver(t, s, a, Message{Raw: mail(map[string]string{
		"From": "Anna Müller <anna@sender.test>", "To": "bob@example.test", "Subject": "Meeting about bananas",
		"X-Foo": "bar"}, "We should discuss the banana supply chain in detail.\r\nRésumé attached.")})
	html := deliver(t, s, a, Message{Raw: []byte("From: carl@sender.test\r\nSubject: Newsletter\r\n" +
		"Content-Type: text/html; charset=utf-8\r\nX-Foo: baz\r\nX-Bar: bar\r\n\r\n" +
		"<html><head><style>.hidden{color:red}</style></head><body><p>Fresh <b>pine</b>apples and <a href=\"https://mango.test\">a link</a></p></body></html>")})
	attached := deliver(t, s, a, Message{Raw: []byte("From: dora@sender.test\r\nSubject: Files\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nSee the files.\r\n" +
		"--b\r\nContent-Type: text/csv\r\nContent-Disposition: attachment; filename=\"prices.csv\"\r\n\r\nkiwi;3\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"invoice-2026.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nc2VjcmV0cGFwYXlh\r\n--b--\r\n")})
	// The same words in another account must never be found.
	deliver(t, s, other, Message{Raw: mail(map[string]string{"Subject": "bananas", "X-Foo": "bar"}, "banana kiwi pineapples")})

	str := func(v string) *string { return &v }
	for _, c := range []struct {
		name string
		f    Filter
		want []int64
	}{
		{"text finds a body word", Filter{Text: str("banana")}, []int64{plain.ID}},
		{"text is whole words", Filter{Text: str("banan")}, nil},
		{"text finds the subject", Filter{Text: str("meeting")}, []int64{plain.ID}},
		{"text finds From", Filter{Text: str("anna")}, []int64{plain.ID}},
		{"text finds an address", Filter{Text: str("bob@example.test")}, []int64{plain.ID}},
		{"text ignores case and diacritics", Filter{Text: str("MULLER resume")}, []int64{plain.ID}},
		{"all words are needed", Filter{Text: str("banana pineapples")}, nil},
		{"words in any order", Filter{Text: str("chain banana")}, []int64{plain.ID}},
		{"a quoted phrase is in order", Filter{Text: str(`"chain banana"`)}, nil},
		{"a quoted phrase", Filter{Text: str(`"banana supply chain"`)}, []int64{plain.ID}},
		{"HTML text, words joined across inline tags", Filter{Text: str("pineapples")}, []int64{html.ID}},
		{"HTML markup is not text", Filter{Text: str("hidden")}, nil},
		{"HTML attributes are not text", Filter{Text: str("mango")}, nil},
		{"a text attachment", Filter{Text: str("kiwi")}, []int64{attached.ID}},
		{"an attachment's name", Filter{Text: str("invoice")}, []int64{attached.ID}},
		{"not what is inside a PDF", Filter{Text: str("secretpapaya")}, nil},
		{"body does not look in the subject", Filter{Body: str("meeting")}, nil},
		{"body does not look in attachments", Filter{Body: str("kiwi")}, nil},
		{"body", Filter{Body: str("supply chain")}, []int64{plain.ID}},
		{"nothing to look for finds nothing", Filter{Text: str("!!! ...")}, nil},
		{"FTS5 syntax is text", Filter{Text: str(`banana) OR (subject: newsletter`)}, nil},
		{"FTS5 operators are words", Filter{Text: str(`banana NOT pineapples`)}, nil},
		{"header exists", Filter{Header: []string{"x-foo"}}, []int64{plain.ID, html.ID}},
		{"header exists, any case", Filter{Header: []string{"X-BAR"}}, []int64{html.ID}},
		{"header with a value", Filter{Header: []string{"x-foo", "bar"}}, []int64{plain.ID}},
		{"the value must be in that field", Filter{Header: []string{"x-bar", "baz"}}, nil},
		{"no such header", Filter{Header: []string{"x-nope"}}, nil},
		{"a standard header", Filter{Header: []string{"Subject", "about bananas"}}, []int64{plain.ID}},
		{"NOT", Filter{Operator: "NOT", Conditions: []Filter{{Text: str("banana")}}}, []int64{html.ID, attached.ID}},
		{"OR", Filter{Operator: "OR", Conditions: []Filter{{Text: str("banana")}, {Text: str("kiwi")}}}, []int64{plain.ID, attached.ID}},
		{"with a mailbox", Filter{Text: str("banana"), InMailbox: ptr(int64(1))}, []int64{plain.ID}},
		{"with another mailbox", Filter{Text: str("banana"), InMailbox: ptr(int64(2))}, nil},
		{"IMAP BODY is a phrase", Filter{BodyPhrase: str("the banana supply")}, []int64{plain.ID}},
		{"IMAP BODY out of order", Filter{BodyPhrase: str("supply banana")}, nil},
		{"IMAP TEXT finds a header", Filter{TextPhrase: str("bar")}, []int64{plain.ID, html.ID}},
	} {
		got := queryIDs(t, s, a, c.f)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// Destroying an email takes it out of the index, by every way an email goes.
	if _, err := s.DestroyEmail(ctx, a.ID, plain.ID); err != nil {
		t.Fatal(err)
	}
	if got := queryIDs(t, s, a, Filter{Text: str("banana")}); len(got) != 0 {
		t.Errorf("after destroy: %v", got)
	}
	mb, err := s.CreateMailbox(ctx, a.ID, 0, "Fruit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateEmail(ctx, a.ID, html.ID, Update{AddMailboxes: []int64{mb.ID}, ReplaceMailboxes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DestroyMailbox(ctx, a.ID, mb.ID, true, 0); err != nil {
		t.Fatal(err)
	}
	if got := queryIDs(t, s, a, Filter{Text: str("pineapples")}); len(got) != 0 {
		t.Errorf("after the mailbox was destroyed with its mail: %v", got)
	}
	res, err := s.query(ctx, a.ID, q(`SELECT count(*) FROM email_fts WHERE rowid BETWEEN `+sqlFTSBase+` AND `+sqlFTSBase+` + 4294967295`))
	if err != nil {
		t.Fatal(err)
	}
	if n := asInt(res[0].Values[0][0]); n != 1 {
		t.Errorf("%d rows of the account left in the index, want 1", n)
	}

	// Reindex puts an entry back, once.
	if _, err := s.db.Execute(ctx, rqlite.Statement{SQL: `DELETE FROM email_fts WHERE rowid = ` + sqlFTSRow(":e"),
		Named: map[string]any{"a": a.ID, "e": attached.ID}}); err != nil {
		t.Fatal(err)
	}
	if got := queryIDs(t, s, a, Filter{Text: str("kiwi")}); len(got) != 0 {
		t.Fatalf("index entry not removed: %v", got)
	}
	for i := 0; i < 2; i++ {
		if err := s.Reindex(ctx, a.ID, attached.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := queryIDs(t, s, a, Filter{Text: str("kiwi")}); !reflect.DeepEqual(got, []int64{attached.ID}) {
		t.Errorf("after reindex: %v", got)
	}
}

// A delivery that fails on a guard leaves nothing in the index, and a long
// body is indexed up to the limit.
func TestTextSearchBatchAndLimit(t *testing.T) {
	s := open(t)
	a := account(t, s)
	str := func(v string) *string { return &v }
	if _, err := s.Deliver(ctx, a.ID, Message{Raw: mail(nil, "orphanword"), MailboxID: 999}); err != ErrNoMailbox {
		t.Fatalf("got %v, want ErrNoMailbox", err)
	}
	long := strings.Repeat("filler ", maxIndexText/7+10) + "tailword"
	e := deliver(t, s, a, Message{Raw: mail(map[string]string{"Subject": "long"}, "headword "+long)})
	// The refused delivery took no email id, so nothing may sit at this one's place.
	if got := queryIDs(t, s, a, Filter{Text: str("orphanword")}); len(got) != 0 {
		t.Errorf("a refused delivery is in the index: %v", got)
	}
	if got := queryIDs(t, s, a, Filter{Text: str("headword")}); !reflect.DeepEqual(got, []int64{e.ID}) {
		t.Errorf("start of a long body: %v", got)
	}
	if got := queryIDs(t, s, a, Filter{Text: str("tailword")}); len(got) != 0 {
		t.Errorf("text past the limit was indexed: %v", got)
	}
}
