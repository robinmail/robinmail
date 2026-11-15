package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/robinmail/robinmail/internal/db"
)

func quotaOf(t *testing.T, s *Store, a Account) Quota {
	t.Helper()
	q, err := s.Quota(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// The counters follow every way an email comes and goes.
func TestQuotaCounting(t *testing.T) {
	s := open(t)
	a := account(t, s)
	want(t, "a new account", quotaOf(t, s, a), Quota{})

	one := deliver(t, s, a, Message{Raw: msg("q1@x", "one")})
	two := deliver(t, s, a, Message{Raw: msg("q2@x", "two, a little longer")})
	three := deliver(t, s, a, Message{Raw: msg("q3@x", "three")})
	size := one.Size + two.Size + three.Size
	want(t, "after three deliveries", []int64{quotaOf(t, s, a).UsedBytes, quotaOf(t, s, a).UsedMessages}, []int64{size, 3})

	// A label is no second message.
	box, err := s.CreateMailbox(ctx, a.ID, 0, "Label")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateEmail(ctx, a.ID, one.ID, Update{AddMailboxes: []int64{box.ID}}); err != nil {
		t.Fatal(err)
	}
	want(t, "after labelling", quotaOf(t, s, a).UsedMessages, int64(3))

	// Destroy, expunge, and a mailbox destroyed with its mail.
	if _, err := s.DestroyEmail(ctx, a.ID, one.ID); err != nil {
		t.Fatal(err)
	}
	want(t, "after a destroy", []int64{quotaOf(t, s, a).UsedBytes, quotaOf(t, s, a).UsedMessages}, []int64{two.Size + three.Size, 2})
	if _, _, err := s.SetFlags(ctx, a.ID, FlagUpdate{Mailbox: 1, UIDs: []UIDRange{{Lo: two.Mailboxes[1], Hi: two.Mailboxes[1]}}, Mode: FlagsAdd, Keywords: []string{"$deleted"}, UnchangedSince: -1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Expunge(ctx, a.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	want(t, "after an expunge", []int64{quotaOf(t, s, a).UsedBytes, quotaOf(t, s, a).UsedMessages}, []int64{three.Size, 1})
	if _, err := s.UpdateEmail(ctx, a.ID, three.ID, Update{AddMailboxes: []int64{box.ID}, ReplaceMailboxes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DestroyMailbox(ctx, a.ID, box.ID, true, 0); err != nil {
		t.Fatal(err)
	}
	want(t, "after a mailbox went with its mail", quotaOf(t, s, a), Quota{})

	// A delivery that is refused (no such mailbox) counts nothing, nor
	// does a repeated one.
	if _, err := s.Deliver(ctx, a.ID, Message{Raw: msg("q4@x", "four"), MailboxID: 999}); !errors.Is(err, ErrNoMailbox) {
		t.Fatal(err)
	}
	four := deliver(t, s, a, Message{Raw: msg("q4@x", "four"), Key: "k4"})
	if _, err := s.Deliver(ctx, a.ID, Message{Raw: msg("q4@x", "four"), Key: "k4"}); !errors.Is(err, ErrDuplicate) {
		t.Fatal(err)
	}
	want(t, "after a refused and a repeated delivery", []int64{quotaOf(t, s, a).UsedBytes, quotaOf(t, s, a).UsedMessages}, []int64{four.Size, 1})

	// Counters that were changed behind the store's back are put right.
	if _, err := s.db.Execute(ctx, db.Stmt(`UPDATE account SET used_bytes = 5, used_count = 9 WHERE id = ?`, a.ID)); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.RecountQuota(ctx, a.ID); err != nil || !changed {
		t.Fatalf("recount: %v, %v", changed, err)
	}
	if changed, err := s.RecountQuota(ctx, a.ID); err != nil || changed {
		t.Fatalf("a second recount: %v, %v", changed, err)
	}
	want(t, "after a recount", []int64{quotaOf(t, s, a).UsedBytes, quotaOf(t, s, a).UsedMessages}, []int64{four.Size, 1})
}

// Limits: the account's own, the default, none; by octets and by count.
func TestQuotaLimits(t *testing.T) {
	s := open(t)
	a := account(t, s)
	n := func(v int64) *int64 { return &v }
	raw := msg("l1@x", "limit")
	size := int64(len(raw))

	// By count.
	if err := s.SetQuota(ctx, a.ID, nil, n(2)); err != nil {
		t.Fatal(err)
	}
	deliver(t, s, a, Message{Raw: raw})
	first := deliver(t, s, a, Message{Raw: raw})
	if _, err := s.Deliver(ctx, a.ID, Message{Raw: raw}); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("a third message with a limit of two: %v", err)
	}
	want(t, "the refused one counted nothing", quotaOf(t, s, a).UsedMessages, int64(2))
	// What the server owes the account still goes in.
	bounce := deliver(t, s, a, Message{Raw: raw, IgnoreQuota: true})
	// Room is made by destroying.
	for _, id := range []int64{first.ID, bounce.ID} {
		if _, err := s.DestroyEmail(ctx, a.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	deliver(t, s, a, Message{Raw: raw})

	// By octets: exactly full is allowed, one octet more is not.
	if err := s.SetQuota(ctx, a.ID, n(3*size), n(0)); err != nil {
		t.Fatal(err)
	}
	deliver(t, s, a, Message{Raw: raw})
	if _, err := s.Deliver(ctx, a.ID, Message{Raw: append(append([]byte{}, raw...), 'x')}); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("one octet over: %v", err)
	}
	q := quotaOf(t, s, a)
	want(t, "full", []int64{q.Bytes, q.Messages, q.UsedBytes, q.UsedMessages}, []int64{3 * size, 0, 3 * size, 3})
	for _, c := range []struct {
		size        int64
		fits, never bool
	}{{0, true, false}, {1, false, false}, {3 * size, false, false}, {3*size + 1, false, true}} {
		if fits, never := q.Room(c.size); fits != c.fits || never != c.never {
			t.Errorf("Room(%d) = %v, %v", c.size, fits, never)
		}
	}

	// The default applies to an account without limits of its own, and
	// an account's own limit of 0 means none.
	b := account(t, s)
	s.DefaultQuota = Limits{Messages: 1}
	deliver(t, s, b, Message{Raw: raw})
	if _, err := s.Deliver(ctx, b.ID, Message{Raw: raw}); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("the default limit: %v", err)
	}
	want(t, "default in effect", quotaOf(t, s, b).Messages, int64(1))
	if err := s.SetQuota(ctx, b.ID, nil, n(0)); err != nil {
		t.Fatal(err)
	}
	deliver(t, s, b, Message{Raw: raw})
	// The account lookup of delivery carries the quota along.
	got, err := s.Account(ctx, b.Address)
	if err != nil || got.Quota.UsedMessages != 2 || got.Quota.Messages != 0 || *got.Quota.OwnMessages != 0 || got.Quota.OwnBytes != nil {
		t.Errorf("Account: %+v, %v", got.Quota, err)
	}
	if err := s.SetQuota(ctx, b.ID, n(-1), nil); err == nil {
		t.Error("a negative quota was taken")
	}
	// A changed limit is a change of the account's Quota.
	c, err := s.Changes(ctx, b.ID, 1)
	if err != nil || c.Other["Quota"] == nil {
		t.Errorf("changes: %+v, %v", c.Other, err)
	}
}

// Concurrent deliveries through two stores (two nodes) to one account with
// room for ten messages: ten are stored, the rest refused, and the
// counters say ten.
func TestQuotaUnderConcurrency(t *testing.T) {
	s1, s2 := twoNodes(t)
	a := account(t, s1)
	const limit, senders, each = 10, 8, 5
	raw := msg("c@x", "concurrent")
	lim := int64(limit) * int64(len(raw))
	if err := s1.SetQuota(ctx, a.ID, &lim, nil); err != nil {
		t.Fatal(err)
	}
	var stored, refused, failed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := []*Store{s1, s2}[i%2]
			for j := 0; j < each; j++ {
				_, err := st.Deliver(ctx, a.ID, Message{Raw: raw, Key: fmt.Sprintf("k-%d-%d", i, j)})
				switch {
				case err == nil:
					stored.Add(1)
				case errors.Is(err, ErrOverQuota):
					refused.Add(1)
				default:
					failed.Add(1)
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	q := quotaOf(t, s1, a)
	if stored.Load() != limit || refused.Load() != senders*each-limit || failed.Load() != 0 {
		t.Errorf("stored %d, refused %d, failed %d; want %d stored", stored.Load(), refused.Load(), failed.Load(), limit)
	}
	if q.UsedMessages != limit || q.UsedBytes != lim {
		t.Errorf("the counters say %d messages, %d octets; want %d and %d", q.UsedMessages, q.UsedBytes, limit, lim)
	}
	ids, err := s1.EmailIDs(ctx, a.ID)
	if err != nil || len(ids) != limit {
		t.Errorf("%d emails in the account, %v", len(ids), err)
	}
}
