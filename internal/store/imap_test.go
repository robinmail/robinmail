package store

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// imapBox is a mailbox with n messages, UIDs 1..n, and their email ids.
func imapBox(t *testing.T, s *Store, account, name string, n int) (Mailbox, []int64) {
	t.Helper()
	m, err := s.CreateMailbox(ctx, account, 0, name)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, n)
	for i := range ids {
		e, err := s.Deliver(ctx, account, Message{Raw: msg(fmt.Sprintf("<%s-%d@example.test>", name, i), fmt.Sprintf("%s %d", name, i)), MailboxID: m.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Mailboxes[m.ID]; got != uint32(i+1) {
			t.Fatalf("message %d got UID %d", i, got)
		}
		ids[i] = e.ID
	}
	return m, ids
}

func view(t *testing.T, s *Store, account string, mailbox, since int64) MailboxView {
	t.Helper()
	v, err := s.ViewMailbox(ctx, account, mailbox, since)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func flagsOf(v MailboxView) map[uint32][]string {
	out := map[uint32][]string{}
	for _, m := range v.Changed {
		kw := append([]string{}, m.Keywords...)
		sort.Strings(kw)
		out[m.UID] = kw
	}
	return out
}

func changesSince(t *testing.T, s *Store, account string, since int64) Changes {
	t.Helper()
	c, err := s.Changes(ctx, account, since)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A mailbox as IMAP sees it: every UID, and what changed since a modseq.
func TestViewMailbox(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, ids := imapBox(t, s, a.ID, "View", 4)

	v := view(t, s, a.ID, box.ID, 0)
	want(t, "uids", v.UIDs, []uint32{1, 2, 3, 4})
	want(t, "uidnext", v.UIDNext, uint32(5))
	want(t, "changed", len(v.Changed), 4)
	want(t, "modseq is the account's", v.Modseq, state(t, s, a))
	for i, m := range v.Changed {
		if m.UID != uint32(i+1) || m.EmailID != ids[i] || m.Size == 0 || m.ReceivedAt.IsZero() || m.Modseq > v.Modseq {
			t.Errorf("message %d: %+v", i, m)
		}
	}

	// Nothing happened: nothing changed.
	v2 := view(t, s, a.ID, box.ID, v.Modseq)
	want(t, "changed since now", len(v2.Changed), 0)
	want(t, "uids", v2.UIDs, []uint32{1, 2, 3, 4})

	// A flag over JMAP, a delivery, a destroy: the next view has the
	// message with the flag and the new one, and lacks the UID that went.
	if _, err := s.UpdateEmail(ctx, a.ID, ids[1], Update{AddKeywords: []string{"$flagged"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(ctx, a.ID, Message{Raw: msg("<view-new@example.test>", "new"), MailboxID: box.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DestroyEmail(ctx, a.ID, ids[2]); err != nil {
		t.Fatal(err)
	}
	v3 := view(t, s, a.ID, box.ID, v.Modseq)
	want(t, "uids", v3.UIDs, []uint32{1, 2, 4, 5})
	want(t, "flags of what changed", flagsOf(v3), map[uint32][]string{2: {"$flagged"}, 5: {}})
	want(t, "uidnext", v3.UIDNext, uint32(6))

	if _, err := s.ViewMailbox(ctx, a.ID, 999, 0); !errors.Is(err, ErrNoMailbox) {
		t.Errorf("a mailbox that is not there: %v", err)
	}
}

// STORE: add, remove and replace for a set of UIDs in one write, which
// JMAP sees as updated Emails. A message that has the flags already is
// left alone and keeps its modseq.
func TestSetFlags(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, ids := imapBox(t, s, a.ID, "Flags", 5)
	before := state(t, s, a)

	st, mod, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 2}, {4, 4}},
		Mode: FlagsAdd, Keywords: []string{"$Seen", "custom"}, UnchangedSince: -1})
	if err != nil || len(mod) != 0 {
		t.Fatalf("add: %v %v", mod, err)
	}
	want(t, "one write, one state", st, before+1)
	v := view(t, s, a.ID, box.ID, before)
	want(t, "after add", flagsOf(v), map[uint32][]string{1: {"$seen", "custom"}, 2: {"$seen", "custom"}, 4: {"$seen", "custom"}})
	for _, m := range v.Changed {
		want(t, "modseq of a changed message", m.Modseq, st)
	}
	c := changesSince(t, s, a.ID, before)
	want(t, "JMAP: emails updated", c.Email.Updated, []int64{ids[0], ids[1], ids[3]})
	want(t, "JMAP: the mailbox's unread count changed", c.Mailbox.Updated, []int64{box.ID})

	// Again: nothing to do, no new state.
	st2, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 2}, {4, 4}},
		Mode: FlagsAdd, Keywords: []string{"$seen"}, UnchangedSince: -1})
	if err != nil || st2 != 0 || state(t, s, a) != st {
		t.Fatalf("a change that changes nothing: state %d (account %d, was %d), %v", st2, state(t, s, a), st, err)
	}
	// Adding to 1:5 touches only the two that lack it.
	st3, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 5}},
		Mode: FlagsAdd, Keywords: []string{"$seen"}, UnchangedSince: -1})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "only the messages that needed it", flagsOf(view(t, s, a.ID, box.ID, st)), map[uint32][]string{3: {"$seen"}, 5: {"$seen"}})

	// Remove.
	st4, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 1}, {3, 3}},
		Mode: FlagsRemove, Keywords: []string{"$seen", "nothere"}, UnchangedSince: -1})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "after remove", flagsOf(view(t, s, a.ID, box.ID, st3)), map[uint32][]string{1: {"custom"}, 3: {}})

	// Replace: exactly these, whatever was there.
	if _, _, err = s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 5}},
		Mode: FlagsReplace, Keywords: []string{"$flagged", "$seen"}, UnchangedSince: -1}); err != nil {
		t.Fatal(err)
	}
	all := flagsOf(view(t, s, a.ID, box.ID, 0))
	for uid := uint32(1); uid <= 5; uid++ {
		want(t, fmt.Sprintf("after replace, uid %d", uid), all[uid], []string{"$flagged", "$seen"})
	}
	// Replace by nothing.
	st5 := state(t, s, a)
	if _, _, err = s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{2, 3}}, Mode: FlagsReplace, UnchangedSince: -1}); err != nil {
		t.Fatal(err)
	}
	want(t, "after replace by nothing", flagsOf(view(t, s, a.ID, box.ID, st5)), map[uint32][]string{2: {}, 3: {}})
	_ = st4

	// UIDs that are not there, a mailbox that is not there, a bad keyword.
	if st, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{50, 60}}, Mode: FlagsAdd,
		Keywords: []string{"x"}, UnchangedSince: -1}); err != nil || st != 0 {
		t.Errorf("no such UIDs: %d %v", st, err)
	}
	if _, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: 999, UIDs: []UIDRange{{1, 1}}, Mode: FlagsAdd,
		Keywords: []string{"x"}, UnchangedSince: -1}); !errors.Is(err, ErrNoMailbox) {
		t.Errorf("no such mailbox: %v", err)
	}
	if _, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, Mode: FlagsAdd, Keywords: []string{"a b"}, UnchangedSince: -1}); !errors.Is(err, ErrKeyword) {
		t.Errorf("bad keyword: %v", err)
	}
}

// A flag belongs to the Email: set in one mailbox, it shows in the others.
func TestSetFlagsShowInEveryMailbox(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, ids := imapBox(t, s, a.ID, "Here", 1)
	other, _ := imapBox(t, s, a.ID, "There", 0)
	if _, err := s.UpdateEmail(ctx, a.ID, ids[0], Update{AddMailboxes: []int64{other.ID}}); err != nil {
		t.Fatal(err)
	}
	before := state(t, s, a)
	if _, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 1}}, Mode: FlagsAdd,
		Keywords: []string{"$answered"}, UnchangedSince: -1}); err != nil {
		t.Fatal(err)
	}
	want(t, "in the other mailbox", flagsOf(view(t, s, a.ID, other.ID, before)), map[uint32][]string{1: {"$answered"}})
	want(t, "both mailboxes changed", changesSince(t, s, a.ID, before).Mailbox.Updated, []int64{box.ID, other.ID})
}

// RFC 7162 3.1.3: with UNCHANGEDSINCE a message whose modseq is higher is
// not changed and is reported; the others are.
func TestSetFlagsUnchangedSince(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, ids := imapBox(t, s, a.ID, "Cond", 3)
	known := state(t, s, a)
	// Someone else changes message 2 after that.
	if _, err := s.UpdateEmail(ctx, a.ID, ids[1], Update{AddKeywords: []string{"$flagged"}}); err != nil {
		t.Fatal(err)
	}
	st, mod, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 3}}, Mode: FlagsAdd,
		Keywords: []string{"$seen"}, UnchangedSince: known})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "left out", mod, []uint32{2})
	want(t, "changed", flagsOf(view(t, s, a.ID, box.ID, st-1)), map[uint32][]string{1: {"$seen"}, 3: {"$seen"}})

	// All of them too new: nothing is written, all are reported.
	now := state(t, s, a)
	st, mod, err = s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 3}}, Mode: FlagsAdd,
		Keywords: []string{"other"}, UnchangedSince: 0})
	if err != nil || st != 0 || state(t, s, a) != now {
		t.Fatalf("unchangedsince 0: state %d, %v", st, err)
	}
	want(t, "all left out", mod, []uint32{1, 2, 3})
}

// COPY adds a label: the same Email, a new UID in the target, in the order
// of the source. JMAP sees the Emails updated, not created.
func TestCopyMessages(t *testing.T) {
	s := open(t)
	a := account(t, s)
	src, ids := imapBox(t, s, a.ID, "Src", 5)
	dst, _ := imapBox(t, s, a.ID, "Dst", 2)
	before := state(t, s, a)

	r, err := s.CopyMessages(ctx, a.ID, src.ID, []UIDRange{{2, 3}, {5, 5}}, dst.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "state", r.State, before+1)
	want(t, "uidvalidity of the target", r.UIDValidity, dst.UIDValidity)
	want(t, "pairs", r.Pairs, []UIDPair{{2, 3}, {3, 4}, {5, 5}})
	want(t, "source keeps its messages", view(t, s, a.ID, src.ID, 0).UIDs, []uint32{1, 2, 3, 4, 5})
	dv := view(t, s, a.ID, dst.ID, 0)
	want(t, "target", dv.UIDs, []uint32{1, 2, 3, 4, 5})
	want(t, "target uidnext", dv.UIDNext, uint32(6))
	want(t, "the same emails", []int64{dv.Changed[2].EmailID, dv.Changed[3].EmailID, dv.Changed[4].EmailID}, []int64{ids[1], ids[2], ids[4]})
	c := changesSince(t, s, a.ID, before)
	want(t, "JMAP: updated", c.Email.Updated, []int64{ids[1], ids[2], ids[4]})
	want(t, "JMAP: none created", len(c.Email.Created), 0)
	want(t, "JMAP: the target changed", c.Mailbox.Updated, []int64{dst.ID})
	e, _ := s.Email(ctx, a.ID, ids[1])
	want(t, "the email is in both", e.Mailboxes, map[int64]uint32{src.ID: 2, dst.ID: 3})

	// Copy again, with one that is new to the target: the ones already
	// there keep their UID, the new one gets the next.
	r, err = s.CopyMessages(ctx, a.ID, src.ID, []UIDRange{{1, 2}}, dst.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "pairs of a second copy", r.Pairs, []UIDPair{{1, 6}, {2, 3}})
	want(t, "target after it", view(t, s, a.ID, dst.ID, 0).UIDs, []uint32{1, 2, 3, 4, 5, 6})

	// Into the mailbox they are in: every message is its own copy.
	st := state(t, s, a)
	r, err = s.CopyMessages(ctx, a.ID, src.ID, []UIDRange{{1, 2}}, src.ID, false)
	if err != nil || r.State != 0 || state(t, s, a) != st {
		t.Fatalf("copy into itself: %+v %v", r, err)
	}
	want(t, "pairs of a copy into itself", r.Pairs, []UIDPair{{1, 1}, {2, 2}})

	// No such message, no such mailbox.
	r, err = s.CopyMessages(ctx, a.ID, src.ID, []UIDRange{{70, 80}}, dst.ID, false)
	if err != nil || r.State != 0 || len(r.Pairs) != 0 || r.UIDValidity != dst.UIDValidity {
		t.Errorf("no such UIDs: %+v %v", r, err)
	}
	if _, err = s.CopyMessages(ctx, a.ID, src.ID, []UIDRange{{1, 1}}, 999, false); !errors.Is(err, ErrNoMailbox) {
		t.Errorf("no such target: %v", err)
	}
}

// MOVE: the label changes, the Email stays the Email it was.
func TestMoveMessages(t *testing.T) {
	s := open(t)
	a := account(t, s)
	src, ids := imapBox(t, s, a.ID, "From", 4)
	dst, _ := imapBox(t, s, a.ID, "To", 1)
	if _, err := s.UpdateEmail(ctx, a.ID, ids[0], Update{AddKeywords: []string{"$seen"}}); err != nil {
		t.Fatal(err)
	}
	before := state(t, s, a)

	r, err := s.CopyMessages(ctx, a.ID, src.ID, []UIDRange{{1, 1}, {3, 4}}, dst.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "pairs", r.Pairs, []UIDPair{{1, 2}, {3, 3}, {4, 4}})
	want(t, "source", view(t, s, a.ID, src.ID, 0).UIDs, []uint32{2})
	dv := view(t, s, a.ID, dst.ID, 0)
	want(t, "target", dv.UIDs, []uint32{1, 2, 3, 4})
	want(t, "flags travel", flagsOf(dv)[2], []string{"$seen"})
	c := changesSince(t, s, a.ID, before)
	want(t, "JMAP: updated", c.Email.Updated, []int64{ids[0], ids[2], ids[3]})
	want(t, "JMAP: nothing created or destroyed", len(c.Email.Created)+len(c.Email.Destroyed), 0)
	want(t, "JMAP: both mailboxes", c.Mailbox.Updated, []int64{src.ID, dst.ID})
	e, _ := s.Email(ctx, a.ID, ids[0])
	want(t, "the email is only in the target", e.Mailboxes, map[int64]uint32{dst.ID: 2})

	// A message that is in the target already: it leaves the source and
	// keeps its place there.
	if _, err := s.UpdateEmail(ctx, a.ID, ids[1], Update{AddMailboxes: []int64{dst.ID}}); err != nil {
		t.Fatal(err)
	}
	r, err = s.CopyMessages(ctx, a.ID, src.ID, nil, dst.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "pairs", r.Pairs, []UIDPair{{2, 5}})
	want(t, "source is empty", len(view(t, s, a.ID, src.ID, 0).UIDs), 0)
	want(t, "target", view(t, s, a.ID, dst.ID, 0).UIDs, []uint32{1, 2, 3, 4, 5})
}

// EXPUNGE takes the messages with $deleted out of the mailbox. An Email
// with another label stays and loses the keyword; one without is destroyed.
func TestExpunge(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, ids := imapBox(t, s, a.ID, "Bin", 5)
	other, _ := imapBox(t, s, a.ID, "Keep", 0)
	// Message 2 is also in Keep. Messages 2, 3 and 5 are marked.
	if _, err := s.UpdateEmail(ctx, a.ID, ids[1], Update{AddMailboxes: []int64{other.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{2, 3}, {5, 5}}, Mode: FlagsAdd,
		Keywords: []string{KeywordDeleted}, UnchangedSince: -1}); err != nil {
		t.Fatal(err)
	}
	before := state(t, s, a)

	// UID EXPUNGE 1:3 (RFC 4315): only the marked ones among those.
	st, err := s.Expunge(ctx, a.ID, box.ID, []UIDRange{{1, 3}})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "state", st, before+1)
	want(t, "the mailbox", view(t, s, a.ID, box.ID, 0).UIDs, []uint32{1, 4, 5})
	c := changesSince(t, s, a.ID, before)
	want(t, "JMAP: the one with another label is updated", c.Email.Updated, []int64{ids[1]})
	want(t, "JMAP: the one without is destroyed", c.Email.Destroyed, []int64{ids[2]})
	want(t, "JMAP: its thread is gone", len(c.Thread.Destroyed), 1)
	want(t, "JMAP: both mailboxes changed", c.Mailbox.Updated, []int64{box.ID, other.ID})
	if _, err := s.Email(ctx, a.ID, ids[2]); !errors.Is(err, ErrNoEmail) {
		t.Errorf("the destroyed email: %v", err)
	}
	e, err := s.Email(ctx, a.ID, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	want(t, "the survivor is only in the other mailbox", e.Mailboxes, map[int64]uint32{other.ID: 1})
	want(t, "and is no longer marked", e.Keywords, []string{})

	// EXPUNGE: the rest of the marked ones.
	if _, err := s.Expunge(ctx, a.ID, box.ID, nil); err != nil {
		t.Fatal(err)
	}
	want(t, "the mailbox", view(t, s, a.ID, box.ID, 0).UIDs, []uint32{1, 4})
	// Nothing marked: nothing happens.
	now := state(t, s, a)
	if st, err := s.Expunge(ctx, a.ID, box.ID, nil); err != nil || st != 0 || state(t, s, a) != now {
		t.Errorf("expunge of nothing: %d %v", st, err)
	}
	// No row is left behind.
	res, err := s.query(ctx, a.ID,
		q(`SELECT count(*) FROM keyword WHERE account_id = :a AND email_id NOT IN (SELECT id FROM email WHERE account_id = :a)`),
		q(`SELECT count(*) FROM email_mailbox WHERE account_id = :a AND email_id NOT IN (SELECT id FROM email WHERE account_id = :a)`),
		q(`SELECT count(*) FROM email WHERE account_id = :a AND id NOT IN (SELECT email_id FROM email_mailbox WHERE account_id = :a)`))
	if err != nil {
		t.Fatal(err)
	}
	for i, what := range []string{"keywords of no email", "labels of no email", "emails in no mailbox"} {
		want(t, what, asInt(res[i].Values[0][0]), int64(0))
	}
	// The blob of a destroyed email is the sweeper's, as with JMAP.
}

// A thread that keeps another email is updated, not destroyed, and a late
// reply still finds it.
func TestExpungeKeepsThreadWithOtherEmails(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, _ := imapBox(t, s, a.ID, "Thread", 0)
	first, err := s.Deliver(ctx, a.ID, Message{Raw: msg("<root@example.test>", "root"), MailboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.Deliver(ctx, a.ID, Message{Raw: msg("<reply@example.test>", "Re: root", "<root@example.test>"), MailboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "one thread", reply.ThreadID, first.ThreadID)
	if _, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: []UIDRange{{1, 1}}, Mode: FlagsAdd,
		Keywords: []string{KeywordDeleted}, UnchangedSince: -1}); err != nil {
		t.Fatal(err)
	}
	before := state(t, s, a)
	if _, err := s.Expunge(ctx, a.ID, box.ID, nil); err != nil {
		t.Fatal(err)
	}
	c := changesSince(t, s, a.ID, before)
	want(t, "thread updated", c.Thread.Updated, []int64{first.ThreadID})
	want(t, "thread not destroyed", len(c.Thread.Destroyed), 0)
	late, err := s.Deliver(ctx, a.ID, Message{Raw: msg("<late@example.test>", "Re: root", "<root@example.test>"), MailboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "a late reply joins the thread", late.ThreadID, first.ThreadID)
}

// More ranges than one batch carries are split, and all are applied.
func TestManyUIDRanges(t *testing.T) {
	var ranges []UIDRange
	for i := uint32(1); i <= 2*maxUIDRanges+7; i++ {
		ranges = append(ranges, UIDRange{2 * i, 2 * i})
	}
	pieces := splitRanges(ranges)
	total := 0
	for _, p := range pieces {
		if len(p) > maxUIDRanges {
			t.Fatalf("a piece of %d ranges", len(p))
		}
		total += len(p)
	}
	want(t, "pieces", len(pieces), 3)
	want(t, "ranges", total, len(ranges))

	s := open(t)
	a := account(t, s)
	box, _ := imapBox(t, s, a.ID, "Many", 3)
	// Ranges for UIDs that mostly do not exist, 2 among them.
	if _, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: box.ID, UIDs: ranges, Mode: FlagsAdd,
		Keywords: []string{"$seen"}, UnchangedSince: -1}); err != nil {
		t.Fatal(err)
	}
	got := flagsOf(view(t, s, a.ID, box.ID, 0))
	if !reflect.DeepEqual(got, map[uint32][]string{1: {}, 2: {"$seen"}, 3: {}}) {
		t.Errorf("flags: %v", got)
	}
}

// IMAP's list of mailboxes with counts.
func TestMailboxStatuses(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, ids := imapBox(t, s, a.ID, "Counts", 3)
	if _, err := s.UpdateEmail(ctx, a.ID, ids[0], Update{AddKeywords: []string{"$seen", KeywordDeleted}}); err != nil {
		t.Fatal(err)
	}
	list, err := s.MailboxStatuses(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "the five default mailboxes and ours", len(list), 6)
	var size int64
	for _, m := range view(t, s, a.ID, box.ID, 0).Changed {
		size += m.Size
	}
	for _, m := range list {
		want(t, "highest modseq", m.HighestModseq, state(t, s, a))
		if m.ID != box.ID {
			continue
		}
		if m.Total != 3 || m.Unread != 2 || m.Deleted != 1 || m.Size != size || m.UIDNext != 4 || m.Name != "Counts" {
			t.Errorf("counts: %+v, size want %d", m, size)
		}
	}
	if _, err := s.MailboxStatuses(ctx, "nobody"); !errors.Is(err, ErrNoAccount) {
		t.Errorf("no such account: %v", err)
	}
}

// A move of messages into the mailbox they are in gives them new UIDs, in
// the order they had, above every UID the mailbox gave out before.
func TestMoveWithinMailbox(t *testing.T) {
	s := open(t)
	a := account(t, s)
	box, ids := imapBox(t, s, a.ID, "Self", 4)
	before := state(t, s, a)
	r, err := s.CopyMessages(ctx, a.ID, box.ID, []UIDRange{{1, 1}, {3, 3}}, box.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "state", r.State, before+1)
	want(t, "pairs", r.Pairs, []UIDPair{{1, 5}, {3, 6}})
	v := view(t, s, a.ID, box.ID, 0)
	want(t, "uids", v.UIDs, []uint32{2, 4, 5, 6})
	want(t, "uidnext", v.UIDNext, uint32(7))
	want(t, "the same emails", []int64{v.Changed[2].EmailID, v.Changed[3].EmailID}, []int64{ids[0], ids[2]})
	c := changesSince(t, s, a.ID, before)
	want(t, "JMAP: updated", c.Email.Updated, []int64{ids[0], ids[2]})
	// The next delivery comes after them.
	e, err := s.Deliver(ctx, a.ID, Message{Raw: msg("<self-new@example.test>", "new"), MailboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "uid of the next delivery", e.Mailboxes[box.ID], uint32(7))
}

// RFC 3501 2.3.1.1: "the combination of mailbox name, UIDVALIDITY, and UID
// must refer to a single immutable message on that server forever". A
// mailbox that is deleted and made again under its name, within the same
// second, gets another UIDVALIDITY; so do all mailboxes made in a second.
func TestUIDValidityIsNeverReused(t *testing.T) {
	s := open(t)
	a := account(t, s)
	seen := map[uint32]bool{}
	for i := 0; i < 5; i++ {
		m, err := s.CreateMailbox(ctx, a.ID, 0, "Again")
		if err != nil {
			t.Fatal(err)
		}
		if seen[m.UIDValidity] {
			t.Fatalf("UIDVALIDITY %d was given out twice", m.UIDValidity)
		}
		seen[m.UIDValidity] = true
		if _, err := s.DestroyMailbox(ctx, a.ID, m.ID, true, 0); err != nil {
			t.Fatal(err)
		}
	}
	// It is still near the clock, and above the mailboxes the account
	// was made with.
	inbox := mailboxByRole(t, s, a.ID, RoleInbox)
	for v := range seen {
		if v <= inbox.UIDValidity || int64(v) > s.Now().Unix()+10 {
			t.Errorf("UIDVALIDITY %d, the inbox has %d, now is %d", v, inbox.UIDValidity, s.Now().Unix())
		}
	}
}
