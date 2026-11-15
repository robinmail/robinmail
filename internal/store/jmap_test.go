package store

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/db"
)

// The store operations behind the JMAP methods of phase 3.

func ptr[T any](v T) *T { return &v }

// mail builds a message with the given header fields and body.
func mail(fields map[string]string, body string) []byte {
	var b strings.Builder
	for _, name := range []string{"From", "To", "Cc", "Bcc", "Subject", "Date", "Message-ID", "References", "Content-Type"} {
		if v, ok := fields[name]; ok {
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}
	for _, name := range []string{"X-Bar", "X-Foo", "List-Id", "Precedence", "Auto-Submitted"} {
		if v, ok := fields[name]; ok {
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}
	b.WriteString("\r\n" + body)
	return []byte(b.String())
}

func TestDeliverCachesHeaders(t *testing.T) {
	s := open(t)
	a := account(t, s)
	e := deliver(t, s, a, Message{Raw: mail(map[string]string{
		"From": "=?UTF-8?Q?Ren=C3=A9e?= <renee@example.test>", "To": "Bob <bob@example.test>, carol@example.test",
		"Cc": "dave@example.test", "Subject": "Quarterly report", "Date": "Fri, 13 Nov 2026 10:00:00 +0100",
		"Content-Type": "multipart/mixed; boundary=b",
	}, "--b\r\nContent-Type: text/html\r\n\r\n<p>See <b>attached</b>.</p>\r\n"+
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=q.pdf\r\n\r\n%PDF\r\n--b--\r\n")})
	want(t, "sender", e.Sender, "Renée <renee@example.test>")
	want(t, "recipients", e.Recipients, "Bob <bob@example.test>, carol@example.test")
	want(t, "cc", e.Cc, "dave@example.test")
	want(t, "sentAt", e.SentAt, time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	want(t, "preview", e.Preview, "See attached.")
	want(t, "hasAttachment", e.HasAttachment, true)

	// Reindex writes the same columns from the stored message.
	if _, err := s.db.Execute(ctx, db.Stmt(`UPDATE email SET sender = '', recipients = '', cc = '', subject = '',
		sent_at = 0, preview = '', has_attachment = 0 WHERE account_id = ? AND id = ?`, a.ID, e.ID)); err != nil {
		t.Fatal(err)
	}
	blank, _ := s.Email(ctx, a.ID, e.ID)
	want(t, "blanked", []any{blank.Sender, blank.Preview, blank.HasAttachment}, []any{"", "", false})
	if err := s.Reindex(ctx, a.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Email(ctx, a.ID, e.ID)
	want(t, "reindexed", again, e)
}

func TestQueryEmails(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box := mailboxes(t, s, a)
	inbox, junk := box["Inbox"].ID, box["Junk"].ID
	day := func(d int) time.Time { return time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC) }

	// e1 and e3 are one thread. e2 is in Junk. e4 has an attachment.
	e1 := deliver(t, s, a, Message{ReceivedAt: day(1), Raw: mail(map[string]string{
		"From": "Anna <anna@x.test>", "To": "me@example.test", "Subject": "Plan", "Message-ID": "<q1@x>"}, "one")})
	e2 := deliver(t, s, a, Message{ReceivedAt: day(2), MailboxID: junk, Raw: mail(map[string]string{
		"From": "Spammer <win@lottery.test>", "To": "me@example.test", "Subject": "You WON", "Message-ID": "<q2@x>"}, "two two")})
	e3 := deliver(t, s, a, Message{ReceivedAt: day(3), Keywords: []string{"$seen"}, Raw: mail(map[string]string{
		"From": "Bert <bert@x.test>", "To": "me@example.test", "Cc": "anna@x.test", "Subject": "Re: Plan",
		"Message-ID": "<q3@x>", "References": "<q1@x>"}, "three three three")})
	e4 := deliver(t, s, a, Message{ReceivedAt: day(4), Keywords: []string{"$seen", "$flagged"}, Raw: mail(map[string]string{
		"From": "Carl <carl@y.test>", "To": "list@example.test", "Subject": "Invoice", "Message-ID": "<q4@x>",
		"Date": "Sun, 1 Nov 2026 00:00:00 +0000", "Content-Type": "multipart/mixed; boundary=b"},
		"--b\r\n\r\nsee file\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment\r\n\r\nPDF\r\n--b--\r\n")})
	want(t, "thread", e3.ThreadID, e1.ThreadID)

	ids := func(q EmailQuery) []int64 {
		t.Helper()
		if q.Limit == 0 {
			q.Limit = -1
		}
		res, err := s.QueryEmails(ctx, a.ID, q)
		if err != nil {
			t.Fatal(err)
		}
		return res.IDs
	}
	filter := func(f Filter) []int64 { t.Helper(); return ids(EmailQuery{Filter: &f}) }
	list := func(e ...Email) []int64 {
		out := []int64{}
		for _, x := range e {
			out = append(out, x.ID)
		}
		return out
	}

	// Without a sort: newest first.
	want(t, "all", ids(EmailQuery{}), list(e4, e3, e2, e1))
	want(t, "inMailbox", filter(Filter{InMailbox: &inbox}), list(e4, e3, e1))
	want(t, "inMailbox junk", filter(Filter{InMailbox: &junk}), list(e2))
	want(t, "inMailboxOtherThan", filter(Filter{InMailboxOtherThan: []int64{junk}}), list(e4, e3, e1))
	want(t, "inMailboxOtherThan nothing", filter(Filter{InMailboxOtherThan: []int64{}}), list(e4, e3, e2, e1))
	// RFC 8621 4.4.1: before is exclusive, after is inclusive.
	want(t, "before", filter(Filter{Before: ptr(day(3))}), list(e2, e1))
	want(t, "after", filter(Filter{After: ptr(day(3))}), list(e4, e3))
	// minSize is inclusive, maxSize exclusive.
	want(t, "minSize", filter(Filter{MinSize: &e3.Size}), list(e4, e3))
	want(t, "maxSize", filter(Filter{MaxSize: &e3.Size}), list(e2, e1))
	want(t, "hasKeyword", filter(Filter{HasKeyword: ptr("$seen")}), list(e4, e3))
	want(t, "hasKeyword case", filter(Filter{HasKeyword: ptr("$FLAGGED")}), list(e4))
	want(t, "notKeyword", filter(Filter{NotKeyword: ptr("$seen")}), list(e2, e1))
	want(t, "allInThreadHaveKeyword", filter(Filter{AllInThreadHaveKeyword: ptr("$seen")}), list(e4))
	want(t, "someInThreadHaveKeyword", filter(Filter{SomeInThreadHaveKeyword: ptr("$seen")}), list(e4, e3, e1))
	want(t, "noneInThreadHaveKeyword", filter(Filter{NoneInThreadHaveKeyword: ptr("$seen")}), list(e2))
	want(t, "hasAttachment", filter(Filter{HasAttachment: ptr(true)}), list(e4))
	want(t, "no attachment", filter(Filter{HasAttachment: ptr(false)}), list(e3, e2, e1))
	want(t, "from", filter(Filter{From: ptr("ANNA")}), list(e1))
	want(t, "from address", filter(Filter{From: ptr("@x.test")}), list(e3, e1))
	want(t, "to", filter(Filter{To: ptr("list@")}), list(e4))
	want(t, "cc", filter(Filter{Cc: ptr("anna")}), list(e3))
	want(t, "bcc", filter(Filter{Bcc: ptr("anyone")}), list())
	want(t, "subject", filter(Filter{Subject: ptr("plan")}), list(e3, e1))
	// A condition with several properties: all must hold.
	want(t, "two properties", filter(Filter{InMailbox: &inbox, HasKeyword: ptr("$seen"), Subject: ptr("plan")}), list(e3))
	// A LIKE pattern in the text is text.
	want(t, "percent", filter(Filter{Subject: ptr("%")}), list())

	// RFC 8620 5.5: AND, OR, NOT ("none of the conditions must match").
	seen, plan := Filter{HasKeyword: ptr("$seen")}, Filter{Subject: ptr("plan")}
	want(t, "AND", filter(Filter{Operator: "AND", Conditions: []Filter{seen, plan}}), list(e3))
	want(t, "OR", filter(Filter{Operator: "OR", Conditions: []Filter{seen, plan}}), list(e4, e3, e1))
	want(t, "NOT", filter(Filter{Operator: "NOT", Conditions: []Filter{seen, plan}}), list(e2))
	want(t, "nested", filter(Filter{Operator: "AND", Conditions: []Filter{
		{InMailbox: &inbox}, {Operator: "NOT", Conditions: []Filter{{Operator: "OR", Conditions: []Filter{seen}}}}}}), list(e1))
	want(t, "empty AND", filter(Filter{Operator: "AND"}), list(e4, e3, e2, e1))
	want(t, "empty OR", filter(Filter{Operator: "OR"}), list())
	if _, err := s.QueryEmails(ctx, a.ID, EmailQuery{Filter: &Filter{Operator: "XOR"}}); err == nil {
		t.Error("unknown operator accepted")
	}

	sorted := func(sorts ...Sort) []int64 { t.Helper(); return ids(EmailQuery{Sort: sorts}) }
	want(t, "receivedAt asc", sorted(Sort{Property: "receivedAt", Ascending: true}), list(e1, e2, e3, e4))
	want(t, "receivedAt desc", sorted(Sort{Property: "receivedAt"}), list(e4, e3, e2, e1))
	want(t, "size asc", sorted(Sort{Property: "size", Ascending: true})[:3], list(e1, e2, e3))
	want(t, "from asc", sorted(Sort{Property: "from", Ascending: true}), list(e1, e3, e4, e2))
	want(t, "subject desc", sorted(Sort{Property: "subject"}), list(e2, e3, e1, e4))
	// e4 has a Date field long before the others, which have none (0).
	want(t, "sentAt desc", sorted(Sort{Property: "sentAt"}, Sort{Property: "receivedAt", Ascending: true}), list(e4, e1, e2, e3))
	// Flagged first, then newest first: two comparators.
	want(t, "hasKeyword then receivedAt", sorted(Sort{Property: "hasKeyword", Keyword: "$seen"}, Sort{Property: "receivedAt", Ascending: true}),
		list(e3, e4, e1, e2))
	if _, err := s.QueryEmails(ctx, a.ID, EmailQuery{Sort: []Sort{{Property: "colour"}}}); err == nil {
		t.Error("unknown sort accepted")
	}

	// Emails that tie on every comparator come in id order, in the direction
	// of the first comparator: three emails of one second, newest first.
	tie := account(t, s)
	var tied []int64
	for i := 0; i < 3; i++ {
		tied = append(tied, deliver(t, s, tie, Message{ReceivedAt: day(1), Raw: msg(fmt.Sprintf("tie%d@x", i), "same")}).ID)
	}
	res, err := s.QueryEmails(ctx, tie.ID, EmailQuery{Limit: -1, Sort: []Sort{{Property: "receivedAt"}}})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "ties, descending", res.IDs, []int64{tied[2], tied[1], tied[0]})
	res, _ = s.QueryEmails(ctx, tie.ID, EmailQuery{Limit: -1, Sort: []Sort{{Property: "subject", Ascending: true}}})
	want(t, "ties, ascending", res.IDs, tied)

	// collapseThreads: of e1 and e3 only the one that sorts first stays.
	want(t, "collapsed, newest first", ids(EmailQuery{CollapseThreads: true}), list(e4, e3, e2))
	want(t, "collapsed, oldest first", ids(EmailQuery{CollapseThreads: true, Sort: []Sort{{Property: "receivedAt", Ascending: true}}}),
		list(e1, e2, e4))
	want(t, "collapsed after filter", ids(EmailQuery{CollapseThreads: true, Filter: &Filter{NotKeyword: ptr("$seen")}}), list(e2, e1))

	// Window and total.
	res, err = s.QueryEmails(ctx, a.ID, EmailQuery{Offset: 1, Limit: 2, Total: true})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "window", res.IDs, list(e3, e2))
	want(t, "total", res.Total, int64(4))
	want(t, "state", res.State, state(t, s, a))
	res, _ = s.QueryEmails(ctx, a.ID, EmailQuery{CollapseThreads: true, Limit: 1, Total: true})
	want(t, "collapsed window", []any{res.IDs, res.Total}, []any{list(e4), int64(3)})
	res, _ = s.QueryEmails(ctx, a.ID, EmailQuery{Offset: 10, Limit: 5})
	want(t, "past the end", len(res.IDs), 0)
	res, _ = s.QueryEmails(ctx, a.ID, EmailQuery{Limit: 0, Total: true})
	want(t, "limit 0", []any{len(res.IDs), res.Total}, []any{0, int64(4)})
	if _, err := s.QueryEmails(ctx, "nobody", EmailQuery{}); err != ErrNoAccount {
		t.Errorf("unknown account: %v", err)
	}

	got, err := s.EmailsByID(ctx, a.ID, []int64{e3.ID, 999, e1.ID, e3.ID})
	if err != nil || len(got) != 2 || got[0].ID != e3.ID || got[1].ID != e1.ID {
		t.Errorf("EmailsByID: %v %v", got, err)
	}
	threads, err := s.Threads(ctx, a.ID, []int64{e1.ThreadID, e2.ThreadID, 999})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "threads", threads, map[int64][]int64{e1.ThreadID: {e1.ID, e3.ID}, e2.ThreadID: {e2.ID}})
}

func TestIfInState(t *testing.T) {
	s := open(t)
	a := account(t, s)
	e := deliver(t, s, a, Message{Raw: msg("s1@x", "one")})
	now := state(t, s, a)

	_, err := s.UpdateEmail(ctx, a.ID, e.ID, Update{AddKeywords: []string{"$seen"}, IfInState: now - 1})
	wantErr(t, "update in an old state", err, ErrStateMismatch)
	want(t, "nothing written", state(t, s, a), now)
	next, err := s.UpdateEmail(ctx, a.ID, e.ID, Update{AddKeywords: []string{"$seen"}, IfInState: now})
	if err != nil || next != now+1 {
		t.Fatalf("update in the current state: %d %v", next, err)
	}
	_, err = s.NewMailbox(ctx, a.ID, NewMailbox{Name: "X", IfInState: now})
	wantErr(t, "create mailbox", err, ErrStateMismatch)
	_, err = s.UpdateMailbox(ctx, a.ID, 1, MailboxUpdate{Name: ptr("In"), IfInState: now})
	wantErr(t, "update mailbox", err, ErrStateMismatch)
	_, err = s.DestroyEmailIf(ctx, a.ID, e.ID, now)
	wantErr(t, "destroy", err, ErrStateMismatch)
	if _, err = s.DestroyEmailIf(ctx, a.ID, e.ID, next); err != nil {
		t.Fatal(err)
	}

	// Two writers that both expect the same state: exactly one wins.
	e2 := deliver(t, s, a, Message{Raw: msg("s2@x", "two")})
	at := state(t, s, a)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.UpdateEmail(ctx, a.ID, e2.ID, Update{AddKeywords: []string{fmt.Sprintf("k%d", i)}, IfInState: at})
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		} else if err != ErrStateMismatch {
			t.Errorf("unexpected error %v", err)
		}
	}
	want(t, "writers that won", won, 1)
	want(t, "state after the race", state(t, s, a), at+1)
}

func TestReplaceMailboxes(t *testing.T) {
	s := open(t)
	a := account(t, s)
	e := deliver(t, s, a, Message{Raw: msg("r1@x", "one")})
	before := state(t, s, a)
	// Inbox (1) -> exactly Junk (4) and Trash (5).
	if _, err := s.UpdateEmail(ctx, a.ID, e.ID, Update{ReplaceMailboxes: true, AddMailboxes: []int64{4, 5}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Email(ctx, a.ID, e.ID)
	want(t, "mailboxes", got.Mailboxes, map[int64]uint32{4: 1, 5: 1})
	c := changes(t, s, a, before)
	want(t, "mailbox changes", c.Mailbox.Updated, []int64{1, 4, 5})
	want(t, "email changes", c.Email.Updated, []int64{e.ID})
	// Replacing with a set that holds a mailbox it is already in keeps the UID.
	if _, err := s.UpdateEmail(ctx, a.ID, e.ID, Update{ReplaceMailboxes: true, AddMailboxes: []int64{5}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Email(ctx, a.ID, e.ID)
	want(t, "mailboxes after", got.Mailboxes, map[int64]uint32{5: 1})
	_, err := s.UpdateEmail(ctx, a.ID, e.ID, Update{ReplaceMailboxes: true})
	wantErr(t, "empty set", err, ErrNoMailboxes)
	_, err = s.UpdateEmail(ctx, a.ID, e.ID, Update{ReplaceMailboxes: true, AddMailboxes: []int64{99}})
	wantErr(t, "unknown mailbox", err, ErrNoMailbox)
	got, _ = s.Email(ctx, a.ID, e.ID)
	want(t, "unchanged after errors", got.Mailboxes, map[int64]uint32{5: 1})
}

func TestMailboxUpdateDestroy(t *testing.T) {
	s := open(t)
	a := account(t, s)
	work, err := s.NewMailbox(ctx, a.ID, NewMailbox{Name: "Work", SortOrder: 3, Subscribed: false})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "created", []any{work.SortOrder, work.Subscribed, work.ParentID}, []any{int64(3), false, int64(0)})
	sub, _ := s.CreateMailbox(ctx, a.ID, work.ID, "Invoices")
	deep, _ := s.CreateMailbox(ctx, a.ID, sub.ID, "2026")
	want(t, "CreateMailbox subscribes", sub.Subscribed, true)

	// Update: any subset of the properties, the rest stays.
	if _, err := s.UpdateMailbox(ctx, a.ID, work.ID, MailboxUpdate{Subscribed: ptr(true), SortOrder: ptr(int64(7))}); err != nil {
		t.Fatal(err)
	}
	got := mailboxes(t, s, a)["Work"]
	want(t, "updated", []any{got.Name, got.SortOrder, got.Subscribed}, []any{"Work", int64(7), true})

	// A mailbox cannot become its own ancestor.
	_, err = s.UpdateMailbox(ctx, a.ID, work.ID, MailboxUpdate{ParentID: &work.ID})
	wantErr(t, "own parent", err, ErrMailboxLoop)
	_, err = s.UpdateMailbox(ctx, a.ID, work.ID, MailboxUpdate{ParentID: &deep.ID})
	wantErr(t, "under a descendant", err, ErrMailboxLoop)
	_, err = s.UpdateMailbox(ctx, a.ID, work.ID, MailboxUpdate{ParentID: ptr(int64(999))})
	wantErr(t, "unknown parent", err, ErrNoMailbox)
	// Move "2026" to the top level and back under Work.
	if _, err := s.UpdateMailbox(ctx, a.ID, deep.ID, MailboxUpdate{ParentID: ptr(int64(0))}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateMailbox(ctx, a.ID, deep.ID, MailboxUpdate{ParentID: &work.ID, Name: ptr("Archive")}); err != nil {
		t.Fatal(err)
	}
	want(t, "moved", mailboxes(t, s, a)["Archive"].ParentID, work.ID)
	_, err = s.UpdateMailbox(ctx, a.ID, deep.ID, MailboxUpdate{Name: ptr("Invoices")})
	wantErr(t, "sibling name", err, ErrMailboxExists)
	_, err = s.UpdateMailbox(ctx, a.ID, 999, MailboxUpdate{Name: ptr("x")})
	wantErr(t, "unknown mailbox", err, ErrNoMailbox)

	// Destroy.
	_, err = s.DestroyMailbox(ctx, a.ID, 1, true, 0)
	wantErr(t, "inbox", err, ErrMailboxRole)
	_, err = s.DestroyMailbox(ctx, a.ID, work.ID, true, 0)
	wantErr(t, "has children", err, ErrMailboxHasChild)
	_, err = s.DestroyMailbox(ctx, a.ID, 999, false, 0)
	wantErr(t, "unknown", err, ErrNoMailbox)

	// only is in Invoices alone; both is in Invoices and the Inbox; reply
	// is in the Inbox and in only's thread.
	only := deliver(t, s, a, Message{MailboxID: sub.ID, Raw: msg("d1@x", "only")})
	both := deliver(t, s, a, Message{MailboxID: sub.ID, Raw: msg("d2@x", "both")})
	reply := deliver(t, s, a, Message{Raw: msg("d3@x", "reply", "d1@x")})
	alone := deliver(t, s, a, Message{MailboxID: sub.ID, Raw: msg("d4@x", "alone")})
	if _, err := s.UpdateEmail(ctx, a.ID, both.ID, Update{AddMailboxes: []int64{1}}); err != nil {
		t.Fatal(err)
	}
	_, err = s.DestroyMailbox(ctx, a.ID, sub.ID, false, 0)
	wantErr(t, "has emails", err, ErrMailboxHasEmail)
	before := state(t, s, a)
	after, err := s.DestroyMailbox(ctx, a.ID, sub.ID, true, before)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "one state", after, before+1)
	if _, err := s.Email(ctx, a.ID, only.ID); err != ErrNoEmail {
		t.Errorf("email only in the mailbox: %v", err)
	}
	kept, err := s.Email(ctx, a.ID, both.ID)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "email also elsewhere", kept.Mailboxes, map[int64]uint32{1: kept.Mailboxes[1]})
	c := changes(t, s, a, before)
	want(t, "email changes", c.Email, ChangeSet{Updated: []int64{both.ID}, Destroyed: []int64{only.ID, alone.ID}})
	// only's thread still has the reply; alone's thread is gone.
	want(t, "thread changes", c.Thread, ChangeSet{Updated: []int64{only.ThreadID}, Destroyed: []int64{alone.ThreadID}})
	want(t, "mailbox changes", c.Mailbox, ChangeSet{Updated: []int64{1}, Destroyed: []int64{sub.ID}})
	if _, ok := mailboxes(t, s, a)["Invoices"]; ok {
		t.Error("the mailbox is still there")
	}
	// A late reply to the destroyed thread starts a new one.
	late := deliver(t, s, a, Message{Raw: msg("d5@x", "late", "d4@x")})
	if late.ThreadID == alone.ThreadID {
		t.Error("a destroyed thread came back")
	}
	_ = reply

	// Thread counts of a mailbox (RFC 8621 2).
	in := mailboxes(t, s, a)["Inbox"]
	want(t, "inbox counts", []int64{in.Total, in.Unread, in.TotalThreads, in.UnreadThreads}, []int64{3, 3, 3, 3})
	s.UpdateEmail(ctx, a.ID, both.ID, Update{AddKeywords: []string{"$seen"}})
	in = mailboxes(t, s, a)["Inbox"]
	want(t, "after reading one", []int64{in.Total, in.Unread, in.TotalThreads, in.UnreadThreads}, []int64{3, 2, 3, 2})
}

func TestChangesOf(t *testing.T) {
	s := open(t)
	a := account(t, s)
	start := state(t, s, a)
	var es []Email
	for i := 0; i < 5; i++ {
		es = append(es, deliver(t, s, a, Message{Raw: msg(fmt.Sprintf("c%d@x", i), "m")}))
	}
	s.UpdateEmail(ctx, a.ID, es[0].ID, Update{AddKeywords: []string{"$seen"}})
	s.DestroyEmail(ctx, a.ID, es[1].ID)
	end := state(t, s, a)

	set, newState, more, err := s.ChangesOf(ctx, a.ID, "Email", start, 0)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "all", []any{set, newState, more},
		[]any{ChangeSet{Created: []int64{es[0].ID, es[2].ID, es[3].ID, es[4].ID}}, end, false})

	// RFC 8620 5.2: with maxChanges the server returns an intermediate
	// state, and the client asks again from there until hasMoreChanges is
	// false. Walking in steps of 2 must end at the same place.
	seen := map[int64]string{}
	since, steps := start, 0
	for {
		set, next, more, err := s.ChangesOf(ctx, a.ID, "Email", since, 2)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(set.Created) + len(set.Updated) + len(set.Destroyed); n > 2 {
			t.Errorf("%d changes with maxChanges 2", n)
		}
		if more && next <= since {
			t.Fatalf("no progress from state %d", since)
		}
		for _, id := range set.Created {
			seen[id] = "created"
		}
		for _, id := range set.Updated {
			if seen[id] == "" {
				seen[id] = "updated"
			}
		}
		for _, id := range set.Destroyed {
			seen[id] = "destroyed"
		}
		since = next
		steps++
		if !more {
			break
		}
	}
	want(t, "end state", since, end)
	if steps < 3 {
		t.Errorf("%d steps", steps)
	}
	want(t, "seen in steps", seen, map[int64]string{es[0].ID: "created", es[1].ID: "destroyed", es[2].ID: "created",
		es[3].ID: "created", es[4].ID: "created"})

	set, _, _, _ = s.ChangesOf(ctx, a.ID, "Thread", end, 10)
	want(t, "nothing new", set, ChangeSet{})
	_, _, _, err = s.ChangesOf(ctx, a.ID, "Email", end+1, 0)
	wantErr(t, "future state", err, ErrCannotCalculateChanges)
	_, _, _, err = s.ChangesOf(ctx, "nobody", "Email", 0, 0)
	wantErr(t, "unknown account", err, ErrNoAccount)

	// After trimming, a state the log no longer covers cannot be answered.
	// Only this account is trimmed: the database is shared with other tests.
	n, err := s.TrimAccountChanges(ctx, a.ID, time.Now().Add(time.Hour))
	if err != nil || n == 0 {
		t.Fatalf("trim: %d %v", n, err)
	}
	_, _, _, err = s.ChangesOf(ctx, a.ID, "Email", start, 0)
	wantErr(t, "trimmed", err, ErrCannotCalculateChanges)
	_, _, _, err = s.ChangesOf(ctx, a.ID, "Email", end-1, 0)
	wantErr(t, "just before the trim point", err, ErrCannotCalculateChanges)
	if _, _, _, err = s.ChangesOf(ctx, a.ID, "Email", end, 0); err != nil {
		t.Errorf("from the trim point: %v", err)
	}
	// Another account keeps its log.
	other := account(t, s)
	deliver(t, s, other, Message{Raw: msg("keep@x", "keep")})
	if n, err := s.TrimAccountChanges(ctx, a.ID, time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("second trim: %d %v", n, err)
	}
	if _, _, _, err = s.ChangesOf(ctx, other.ID, "Email", 1, 0); err != nil {
		t.Errorf("the other account was trimmed: %v", err)
	}
}

func TestOpenBlob(t *testing.T) {
	s := open(t)
	a, other := account(t, s), account(t, s)
	raw := msg("b1@x", "blob")
	e := deliver(t, s, a, Message{Raw: raw})
	r, size, err := s.OpenBlob(ctx, a.ID, e.Blob)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != string(raw) || size != int64(len(raw)) {
		t.Errorf("blob: %q, size %d", got, size)
	}
	// A blob belongs to its account.
	_, _, err = s.OpenBlob(ctx, other.ID, e.Blob)
	wantErr(t, "another account", err, ErrNoBlob)
	_, _, err = s.OpenBlob(ctx, a.ID, strings.Repeat("0", 64))
	wantErr(t, "unknown", err, ErrNoBlob)
	_, _, err = s.OpenBlob(ctx, a.ID, "../../etc/passwd")
	wantErr(t, "not a hash", err, ErrNoBlob)
	// Destroyed: the file may still be on disk, but it is no longer served.
	s.DestroyEmail(ctx, a.ID, e.ID)
	_, _, err = s.OpenBlob(ctx, a.ID, e.Blob)
	wantErr(t, "destroyed", err, ErrNoBlob)
}
