package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// queueAccount makes an account in a domain of its own, so that
// DueSubmissions can be asked for this test's rows only: the database is
// shared with every other test.
func queueAccount(t *testing.T, s *Store) (Account, []string) {
	t.Helper()
	var r [6]byte
	rand.Read(r[:])
	domain := "queue-" + hex.EncodeToString(r[:]) + ".test"
	if err := s.AddDomain(ctx, domain); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAccount(ctx, "sender@"+domain, "$argon2id$test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.PurgeSubmissions(ctx, a.ID) })
	return a, []string{domain}
}

func submit(t *testing.T, s *Store, a Account, n NewSubmission) Submission {
	t.Helper()
	if n.Raw == nil {
		n.Raw = msg("out-"+hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))+"@example.test", "outgoing")
	}
	if n.Rcpts == nil {
		n.Rcpts = []string{"anna@elsewhere.test", "bob@elsewhere.test"}
	}
	sub, err := s.Submit(ctx, a.ID, n)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestSubmitAndRead(t *testing.T) {
	s := open(t)
	a, _ := queueAccount(t, s)
	e := deliver(t, s, a, Message{Raw: msg("draft-1@example.test", "a draft"), Role: RoleDrafts})
	before := state(t, s, a)
	now := s.Now().Truncate(time.Second)

	sub := submit(t, s, a, NewSubmission{EmailID: e.ID, IdentityID: 1, Raw: msg("sent-1@example.test", "as sent"),
		MailFrom: a.Address, SendAt: now.Add(10 * time.Second)})
	if sub.ID != 1 || sub.EmailID != e.ID || sub.ThreadID != e.ThreadID || sub.IdentityID != 1 || sub.MessageID != "sent-1@example.test" ||
		sub.Undo != UndoPending || sub.Queue != QueueActive || sub.Tries != 0 || sub.MailFrom != a.Address ||
		!sub.SendAt.Equal(now.Add(10*time.Second)) || !sub.NextTry.Equal(sub.SendAt) || len(sub.Recipients) != 2 ||
		sub.Recipients[0] != (Recipient{Address: "anna@elsewhere.test", State: RcptQueued}) || sub.Done() {
		t.Errorf("submission %+v", sub)
	}
	want(t, "state", state(t, s, a), before+1)
	c := changes(t, s, a, before)
	want(t, "changes", c.Submission, ChangeSet{Created: []int64{1}})

	// The message as it goes out is a blob of its own, and is kept by a
	// sweep although no email refers to it.
	raw, err := s.SubmissionMessage(ctx, sub)
	if err != nil || string(raw) != string(msg("sent-1@example.test", "as sent")) {
		t.Errorf("message %q, %v", raw, err)
	}
	if sub.Blob == e.Blob {
		t.Error("the queued blob is the draft's")
	}
	if res, err := s.SweepBlobs(ctx, s.Now().Add(time.Hour)); err != nil || res.Removed != 0 {
		t.Errorf("sweep removed %d blobs, %v", res.Removed, err)
	}
	if _, err := s.SubmissionMessage(ctx, sub); err != nil {
		t.Errorf("after a sweep: %v", err)
	}

	list, err := s.Submissions(ctx, a.ID, nil)
	if err != nil || len(list) != 1 || list[0].ID != 1 {
		t.Errorf("list %+v %v", list, err)
	}
	// A submission over SMTP has no Email; JMAP does not show it and the
	// change log does not mention it.
	smtp := submit(t, s, a, NewSubmission{MailFrom: a.Address})
	if smtp.ID != 2 || smtp.EmailID != 0 || !smtp.SendAt.Equal(now) && !smtp.SendAt.Equal(now.Add(time.Second)) {
		t.Errorf("SMTP submission %+v", smtp)
	}
	if list, _ := s.Submissions(ctx, a.ID, []int64{1, 2, 99}); len(list) != 1 {
		t.Errorf("JMAP sees %d submissions, want 1", len(list))
	}
	want(t, "changes after an SMTP submission", changes(t, s, a, before).Submission, ChangeSet{Created: []int64{1}})

	// What Submit refuses.
	_, err = s.Submit(ctx, a.ID, NewSubmission{EmailID: 999, Raw: msg("x@example.test", "x"), Rcpts: []string{"a@b.test"}})
	wantErr(t, "no such email", err, ErrNoEmail)
	_, err = s.Submit(ctx, a.ID, NewSubmission{IdentityID: 99, Raw: msg("x@example.test", "x"), Rcpts: []string{"a@b.test"}})
	wantErr(t, "no such identity", err, ErrNoIdentity)
	_, err = s.Submit(ctx, a.ID, NewSubmission{Raw: msg("x@example.test", "x"), Rcpts: []string{"a@b.test"}, IfInState: 1})
	wantErr(t, "ifInState", err, ErrStateMismatch)
	_, err = s.Submit(ctx, "nosuchaccount", NewSubmission{Raw: msg("x@example.test", "x"), Rcpts: []string{"a@b.test"}})
	if err == nil {
		t.Error("submitted for an unknown account")
	}
	if _, err := s.Submit(ctx, a.ID, NewSubmission{Raw: msg("x@example.test", "x")}); err == nil {
		t.Error("submitted without recipients")
	}
}

// The rate limit counts recipients of the last hour in the table, so it
// holds for all nodes together and inside the insert.
func TestSubmitRateLimit(t *testing.T) {
	s := open(t)
	a, _ := queueAccount(t, s)
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	three := []string{"a@x.test", "b@x.test", "c@x.test"}
	submit(t, s, a, NewSubmission{Rcpts: three, MaxPerHour: 5})
	_, err := s.Submit(ctx, a.ID, NewSubmission{Raw: msg("2@example.test", "x"), Rcpts: three, MaxPerHour: 5})
	wantErr(t, "over the limit", err, ErrRateLimited)
	submit(t, s, a, NewSubmission{Rcpts: three[:2], MaxPerHour: 5})
	_, err = s.Submit(ctx, a.ID, NewSubmission{Raw: msg("3@example.test", "x"), Rcpts: three[:1], MaxPerHour: 5})
	wantErr(t, "at the limit", err, ErrRateLimited)
	clock = clock.Add(time.Hour + time.Second)
	submit(t, s, a, NewSubmission{Rcpts: three, MaxPerHour: 5})

	// Many at once: never more than the limit gets in.
	b, _ := queueAccount(t, s)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Submit(ctx, b.ID, NewSubmission{Raw: msg(hex.EncodeToString([]byte{byte(i)})+"@example.test", "x"), Rcpts: three[:1], MaxPerHour: 4})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if !errors.Is(err, ErrRateLimited) {
				t.Errorf("concurrent submit: %v", err)
			}
		}()
	}
	wg.Wait()
	want(t, "accepted of 12 concurrent with a limit of 4", ok, 4)
}

// A claim is one guarded update: one of several nodes gets a row, a claim
// that ran out frees it, and only the holder writes results.
func TestClaimSubmission(t *testing.T) {
	s := open(t)
	a, domains := queueAccount(t, s)
	clock := time.Now().Truncate(time.Second)
	s.Now = func() time.Time { return clock }
	e := deliver(t, s, a, Message{Raw: msg("d@example.test", "draft"), Role: RoleDrafts})
	sub := submit(t, s, a, NewSubmission{EmailID: e.ID, SendAt: clock.Add(10 * time.Second)})
	ref := SubmissionRef{a.ID, sub.ID}

	due := func() []SubmissionRef {
		t.Helper()
		refs, err := s.DueSubmissions(ctx, clock, 10, domains)
		if err != nil {
			t.Fatal(err)
		}
		return refs
	}
	// Not before sendAt: the time in which it can be cancelled.
	want(t, "due before sendAt", len(due()), 0)
	_, err := s.ClaimSubmission(ctx, ref, "n1:a", clock, clock.Add(2*time.Minute))
	wantErr(t, "claim before sendAt", err, ErrNotClaimed)
	clock = clock.Add(10 * time.Second)
	want(t, "due at sendAt", due(), []SubmissionRef{ref})
	// Other domains' rows are not listed with the filter.
	if refs, _ := s.DueSubmissions(ctx, clock, 10, []string{"nobody-" + domains[0]}); len(refs) != 0 {
		t.Errorf("rows of another domain: %v", refs)
	}

	// Eight nodes at once: one gets it.
	before := state(t, s, a)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []string
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := "n" + string(rune('1'+i)) + ":x"
			got, err := s.ClaimSubmission(ctx, ref, token, clock, clock.Add(2*time.Minute))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners = append(winners, token)
				if got.ClaimedBy != token || got.Tries != 1 || got.Undo != UndoFinal {
					t.Errorf("claimed row %+v", got)
				}
			} else if !errors.Is(err, ErrNotClaimed) {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("%d nodes got the claim: %v", len(winners), winners)
	}
	token := winners[0]
	// The first claim ends undo: one state, one change.
	want(t, "state after the first claim", state(t, s, a), before+1)
	want(t, "change", changes(t, s, a, before).Submission, ChangeSet{Updated: []int64{sub.ID}})
	want(t, "due while claimed", len(due()), 0)
	_, err = s.CancelSubmission(ctx, a.ID, sub.ID, 0)
	wantErr(t, "cancel after a claim", err, ErrCannotUnsend)

	// The claim is renewed, and lost when it ran out and another took it.
	if err := s.RenewSubmission(ctx, ref, token, clock.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(3 * time.Minute)
	want(t, "due while the renewed claim lasts", len(due()), 0)
	clock = clock.Add(2 * time.Minute)
	want(t, "due after the claim ran out", due(), []SubmissionRef{ref})
	second, err := s.ClaimSubmission(ctx, ref, "other:y", clock, clock.Add(2*time.Minute))
	if err != nil || second.Tries != 2 {
		t.Fatalf("second claim: %+v %v", second, err)
	}
	wantErr(t, "renewing a lost claim", s.RenewSubmission(ctx, ref, token, clock.Add(time.Hour)), ErrNotClaimed)
	second.Recipients[0].State = RcptDelivered
	wantErr(t, "saving with a lost claim", s.SaveSubmission(ctx, second, token, true, clock), ErrNotClaimed)

	// The holder writes: first what it knows, keeping the claim; then the
	// end of the attempt, with the time of the next.
	second.Recipients[0] = Recipient{Address: "anna@elsewhere.test", State: RcptDelivered, Reply: "250 2.0.0 OK", Status: "2.0.0", MTA: "mx.elsewhere.test", At: clock.Unix()}
	if err := s.SaveSubmission(ctx, second, "other:y", false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Submission(ctx, a.ID, sub.ID)
	if got.ClaimedBy != "other:y" || got.Recipients[0].State != RcptDelivered || got.Queue != QueueActive {
		t.Errorf("after a partial save: %+v", got)
	}
	second.Recipients[1].Reply = "451 4.7.1 try later"
	next := clock.Add(10 * time.Minute)
	if err := s.SaveSubmission(ctx, second, "other:y", true, next); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Submission(ctx, a.ID, sub.ID)
	if got.ClaimedBy != "" || !got.NextTry.Equal(next) || got.Queue != QueueActive || got.Blob == "" || got.Recipients[1].Reply != "451 4.7.1 try later" {
		t.Errorf("after the attempt: %+v", got)
	}
	want(t, "due before the next try", len(due()), 0)
	clock = next
	third, err := s.ClaimSubmission(ctx, ref, "n1:z", clock, clock.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	third.Recipients[1].State = RcptFailed
	if err := s.SaveSubmission(ctx, third, "n1:z", true, clock); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Submission(ctx, a.ID, sub.ID)
	if got.Queue != QueueDone || got.Blob != "" || !got.Done() || got.Undo != UndoFinal {
		t.Errorf("when done: %+v", got)
	}
	want(t, "due when done", len(due()), 0)
	// The outbound blob is released for the sweeper.
	if _, err := s.SubmissionMessage(ctx, got); !errors.Is(err, ErrNoBlob) {
		t.Errorf("message of a done submission: %v", err)
	}
}

func TestCancelAndDestroySubmission(t *testing.T) {
	s := open(t)
	a, domains := queueAccount(t, s)
	clock := time.Now().Truncate(time.Second)
	s.Now = func() time.Time { return clock }
	e := deliver(t, s, a, Message{Raw: msg("d@example.test", "draft"), Role: RoleDrafts})
	sub := submit(t, s, a, NewSubmission{EmailID: e.ID, SendAt: clock.Add(time.Minute)})
	before := state(t, s, a)

	_, err := s.CancelSubmission(ctx, a.ID, sub.ID, before+5)
	wantErr(t, "ifInState", err, ErrStateMismatch)
	_, err = s.CancelSubmission(ctx, a.ID, 99, 0)
	wantErr(t, "unknown", err, ErrNoSubmission)
	st, err := s.CancelSubmission(ctx, a.ID, sub.ID, before)
	if err != nil || st != before+1 {
		t.Fatalf("cancel: %d %v", st, err)
	}
	got, _ := s.Submission(ctx, a.ID, sub.ID)
	if got.Undo != UndoCanceled || got.Queue != QueueDone || got.Blob != "" {
		t.Errorf("cancelled: %+v", got)
	}
	clock = clock.Add(time.Hour)
	if refs, _ := s.DueSubmissions(ctx, clock, 10, domains); len(refs) != 0 {
		t.Errorf("a cancelled submission is due: %v", refs)
	}
	_, err = s.CancelSubmission(ctx, a.ID, sub.ID, 0)
	wantErr(t, "cancel twice", err, ErrCannotUnsend)
	// Destroying a finished one removes the row.
	if _, err := s.DestroySubmission(ctx, a.ID, sub.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submission(ctx, a.ID, sub.ID); !errors.Is(err, ErrNoSubmission) {
		t.Errorf("after destroy: %v", err)
	}
	want(t, "changes", changes(t, s, a, before).Submission, ChangeSet{Destroyed: []int64{sub.ID}})
	_, err = s.DestroySubmission(ctx, a.ID, sub.ID, 0)
	wantErr(t, "destroy twice", err, ErrNoSubmission)

	// Destroying one with work left hides it from JMAP and leaves it to
	// the queue (RFC 8621 7.5: destroying does not stop delivery).
	active := submit(t, s, a, NewSubmission{EmailID: e.ID})
	ref := SubmissionRef{a.ID, active.ID}
	if _, err := s.DestroySubmission(ctx, a.ID, active.ID, 0); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Submissions(ctx, a.ID, nil); len(list) != 0 {
		t.Errorf("JMAP still sees %v", list)
	}
	want(t, "still due", func() []SubmissionRef { r, _ := s.DueSubmissions(ctx, clock, 10, domains); return r }(), []SubmissionRef{ref})
	mid := state(t, s, a)
	claimed, err := s.ClaimSubmission(ctx, ref, "n:1", clock, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for i := range claimed.Recipients {
		claimed.Recipients[i].State = RcptDelivered
	}
	if err := s.SaveSubmission(ctx, claimed, "n:1", true, clock); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submission(ctx, a.ID, active.ID); !errors.Is(err, ErrNoSubmission) {
		t.Errorf("a destroyed row is kept after its work: %v", err)
	}
	// Nothing about it reaches the client any more.
	want(t, "state", state(t, s, a), mid)
}

func TestApplyDSN(t *testing.T) {
	s := open(t)
	a, _ := queueAccount(t, s)
	clock := time.Now().Truncate(time.Second)
	s.Now = func() time.Time { return clock }
	e := deliver(t, s, a, Message{Raw: msg("d@example.test", "draft"), Role: RoleDrafts})
	sub := submit(t, s, a, NewSubmission{EmailID: e.ID, Raw: msg("sent-9@example.test", "sent")})
	report := []Recipient{{Address: "ANNA@elsewhere.test", State: RcptFailed, Reply: "550 5.1.1 no such user", Status: "5.1.1", MTA: "relay.elsewhere.test"}}

	// Not while we still have the message ourselves.
	if found, err := s.ApplyDSN(ctx, a.ID, "sent-9@example.test", report); err != nil || found {
		t.Errorf("applied to an active submission: %v %v", found, err)
	}
	claimed, err := s.ClaimSubmission(ctx, SubmissionRef{a.ID, sub.ID}, "n:1", clock, clock.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed.Recipients[0].State, claimed.Recipients[0].Reply = RcptDelivered, "250 OK"
	claimed.Recipients[1].State, claimed.Recipients[1].Mailbox = RcptDelivered, true
	if err := s.SaveSubmission(ctx, claimed, "n:1", true, clock); err != nil {
		t.Fatal(err)
	}
	before := state(t, s, a)
	for _, id := range []string{"", "other@example.test"} {
		if found, _ := s.ApplyDSN(ctx, a.ID, id, report); found {
			t.Errorf("applied for message id %q", id)
		}
	}
	found, err := s.ApplyDSN(ctx, a.ID, "sent-9@example.test", report)
	if err != nil || !found {
		t.Fatalf("not applied: %v", err)
	}
	got, _ := s.Submission(ctx, a.ID, sub.ID)
	if r := got.Recipients[0]; r.State != RcptFailed || !r.Remote || r.Reply != "550 5.1.1 no such user" || r.MTA != "relay.elsewhere.test" {
		t.Errorf("recipient %+v", r)
	}
	want(t, "change", changes(t, s, a, before).Submission, ChangeSet{Updated: []int64{sub.ID}})
	// A second report for the same recipient, or one for a recipient that
	// is in a mailbox here, changes nothing.
	for _, rep := range [][]Recipient{report, {{Address: "bob@elsewhere.test", State: RcptFailed}}, {{Address: "stranger@x.test", State: RcptFailed}}} {
		if found, _ := s.ApplyDSN(ctx, a.ID, "sent-9@example.test", rep); found {
			t.Errorf("applied again: %+v", rep)
		}
	}
}

func TestIdentities(t *testing.T) {
	s := open(t)
	a := account(t, s)
	list, err := s.Identities(ctx, a.ID)
	if err != nil || len(list) != 1 || list[0] != (Identity{ID: 1, Email: a.Address}) || list[0].MayDelete() {
		t.Fatalf("a new account has %+v, %v", list, err)
	}
	before := state(t, s, a)
	made, st, err := s.CreateIdentity(ctx, a.ID, Identity{Name: "Anna", Email: a.Address, ReplyTo: `[{"name":null,"email":"r@x.test"}]`, TextSignature: "-- \nAnna"}, before)
	if err != nil || made.ID != 2 || st != before+1 || !made.MayDelete() {
		t.Fatalf("create: %+v %d %v", made, st, err)
	}
	_, _, err = s.CreateIdentity(ctx, a.ID, Identity{Email: a.Address}, before)
	wantErr(t, "ifInState", err, ErrStateMismatch)
	name, sig := "Anna B.", ""
	if _, err := s.UpdateIdentity(ctx, a.ID, 2, IdentityUpdate{Name: &name, TextSignature: &sig}, 0); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Identities(ctx, a.ID)
	if len(list) != 2 || list[1].Name != "Anna B." || list[1].TextSignature != "" || list[1].ReplyTo == "" {
		t.Errorf("after update: %+v", list)
	}
	_, err = s.UpdateIdentity(ctx, a.ID, 9, IdentityUpdate{Name: &name}, 0)
	wantErr(t, "update unknown", err, ErrNoIdentity)
	_, err = s.DestroyIdentity(ctx, a.ID, 1, 0)
	wantErr(t, "destroy the first", err, ErrIdentityDefault)
	if _, err := s.DestroyIdentity(ctx, a.ID, 2, 0); err != nil {
		t.Fatal(err)
	}
	_, err = s.DestroyIdentity(ctx, a.ID, 2, 0)
	wantErr(t, "destroy twice", err, ErrNoIdentity)
	// Created and destroyed since: not in the changes at all.
	want(t, "changes", changes(t, s, a, before).Identity, ChangeSet{})
	// Ids are not used again.
	again, _, _ := s.CreateIdentity(ctx, a.ID, Identity{Email: a.Address}, 0)
	want(t, "next id", again.ID, int64(3))
}

func TestUploads(t *testing.T) {
	s := open(t)
	a, b := account(t, s), account(t, s)
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	data := []byte("%PDF-1.7 an upload")
	id, err := s.Upload(ctx, a.ID, data, "application/pdf")
	if err != nil || len(id) != 64 {
		t.Fatalf("upload: %q %v", id, err)
	}
	if again, _ := s.Upload(ctx, a.ID, data, "application/pdf"); again != id {
		t.Errorf("the same content has another id: %s", again)
	}
	r, size, err := s.OpenBlob(ctx, a.ID, id)
	if err != nil || size != int64(len(data)) {
		t.Fatalf("open: %d %v", size, err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != string(data) {
		t.Errorf("content %q", got)
	}
	// Not for another account.
	if _, _, err := s.OpenBlob(ctx, b.ID, id); !errors.Is(err, ErrNoBlob) {
		t.Errorf("another account opens the upload: %v", err)
	}
	// A sweep keeps it while the upload row is there, and takes it after.
	if res, _ := s.SweepBlobs(ctx, clock.Add(time.Hour)); res.Removed != 0 {
		t.Errorf("swept an upload: %+v", res)
	}
	clock = clock.Add(UploadRetention + time.Minute)
	if _, err := s.TrimUploads(ctx, clock.Add(-UploadRetention)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenBlob(ctx, a.ID, id); !errors.Is(err, ErrNoBlob) {
		t.Errorf("a trimmed upload opens: %v", err)
	}
	if res, _ := s.SweepBlobs(ctx, time.Now().Add(time.Hour)); res.Removed < 1 {
		t.Errorf("the file of a trimmed upload stays: %+v", res)
	}
}

func TestDeliverToSeveralMailboxes(t *testing.T) {
	s := open(t)
	a := account(t, s)
	before := state(t, s, a)
	e, err := s.Deliver(ctx, a.ID, Message{Raw: msg("multi@example.test", "two labels"), MailboxIDs: []int64{drafts, 3, drafts},
		Keywords: []string{"$draft", "$seen"}, IfInState: before})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Mailboxes) != 2 || e.Mailboxes[drafts] != 1 || e.Mailboxes[3] != 1 || !e.HasKeyword("$draft") || e.Modseq != before+1 {
		t.Errorf("email %+v", e)
	}
	want(t, "changes", changes(t, s, a, before).Mailbox, ChangeSet{Updated: []int64{drafts, 3}})
	_, err = s.Deliver(ctx, a.ID, Message{Raw: msg("x@example.test", "x"), MailboxIDs: []int64{drafts, 99}})
	wantErr(t, "an unknown mailbox", err, ErrNoMailbox)
	_, err = s.Deliver(ctx, a.ID, Message{Raw: msg("x@example.test", "x"), MailboxIDs: []int64{drafts}, IfInState: before})
	wantErr(t, "ifInState", err, ErrStateMismatch)
	want(t, "state after refusals", state(t, s, a), before+1)
}

func TestSTSPolicyCache(t *testing.T) {
	s := open(t)
	var r [6]byte
	rand.Read(r[:])
	domain := "sts-" + hex.EncodeToString(r[:]) + ".test"
	if _, ok, err := s.STSPolicy(ctx, domain); ok || err != nil {
		t.Fatalf("a policy from nowhere: %v", err)
	}
	now := time.Now().Truncate(time.Second)
	p := STSPolicy{Domain: domain, ID: "20261114", Mode: "enforce", MX: []string{"mx1." + domain, "*.backup." + domain}, FetchedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.SaveSTSPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.STSPolicy(ctx, domain)
	if err != nil || !ok || got.ID != p.ID || got.Mode != "enforce" || len(got.MX) != 2 || got.MX[1] != p.MX[1] || !got.ExpiresAt.Equal(p.ExpiresAt) {
		t.Errorf("read back %+v %v", got, err)
	}
	p.ID, p.Mode, p.MX = "2", "none", nil
	s.SaveSTSPolicy(ctx, p)
	if got, _, _ := s.STSPolicy(ctx, domain); got.ID != "2" || got.Mode != "none" || len(got.MX) != 0 {
		t.Errorf("replaced: %+v", got)
	}
}
