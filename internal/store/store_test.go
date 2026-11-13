package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/rqlite"
)

// The tests below need a real rqlite:
//
//	docker compose -f contrib/compose/compose.yaml up -d
//	ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 go test ./internal/store
//
// They share the database with each other and with whatever else is in it:
// every test works in accounts of its own with random addresses.

var ctx = context.Background()

func open(t *testing.T) *Store {
	t.Helper()
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	db, err := rqlite.New(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	master, err := blob.NewMasterKey(testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	blobs := blob.NewLocal(t.TempDir())
	s := New(db, blobs, master)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDomain(ctx, "example.test"); err != nil && !errors.Is(err, ErrDomainExists) {
		t.Fatal(err)
	}
	locals[s] = blobs
	t.Cleanup(func() { delete(locals, s) })
	return s
}

// testMasterKey is fixed, so accounts left in the database by an earlier run
// stay readable.
var testMasterKey = bytes.Repeat([]byte{0x42}, 32)

// locals gives a test the blob directory behind its store.
var locals = map[*Store]*blob.Local{}

func account(t *testing.T, s *Store) Account {
	t.Helper()
	var r [8]byte
	rand.Read(r[:])
	a, err := s.CreateAccount(ctx, "test-"+hex.EncodeToString(r[:])+"@example.test", "$argon2id$test")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// msg builds a message. refs become the References header.
func msg(id, subject string, refs ...string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: Anna <anna@sender.test>\r\nSubject: %s\r\n", subject)
	if id != "" {
		fmt.Fprintf(&b, "Message-ID: <%s>\r\n", id)
	}
	if len(refs) > 0 {
		fmt.Fprintf(&b, "References: <%s>\r\nIn-Reply-To: <%s>\r\n", strings.Join(refs, "> <"), refs[len(refs)-1])
	}
	fmt.Fprintf(&b, "\r\nBody of %s.\r\n", subject)
	return []byte(b.String())
}

func deliver(t *testing.T, s *Store, a Account, m Message) Email {
	t.Helper()
	e, err := s.Deliver(ctx, a.ID, m)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func state(t *testing.T, s *Store, a Account) int64 {
	t.Helper()
	n, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func changes(t *testing.T, s *Store, a Account, since int64) Changes {
	t.Helper()
	c, err := s.Changes(ctx, a.ID, since)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mailboxes(t *testing.T, s *Store, a Account) map[string]Mailbox {
	t.Helper()
	list, err := s.Mailboxes(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Mailbox{}
	for _, m := range list {
		out[m.Name] = m
	}
	return out
}

func want(t *testing.T, name string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %+v, want %+v", name, got, want)
	}
}

func wantErr(t *testing.T, name string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", name, err, want)
	}
}

const (
	inbox  = 1
	drafts = 2
	sent   = 3
	junk   = 4
	trash  = 5
)

func TestIDs(t *testing.T) {
	want(t, "EmailID", []string{EmailID(1), EmailID(35), EmailID(36), MailboxID(5), ThreadID(1296)}, []string{"e1", "ez", "e10", "m5", "t100"})
	for _, n := range []int64{1, 9, 10, 35, 36, 1 << 40, 1<<63 - 1} {
		if got, ok := ParseEmailID(EmailID(n)); !ok || got != n {
			t.Errorf("ParseEmailID(%q) = %d, %v", EmailID(n), got, ok)
		}
	}
	for _, bad := range []string{"", "e", "e0", "e01", "eZ", "m1", "e-1", "e+1", "e 1", "e1y2p0ij32e8e8", "1"} {
		if _, ok := ParseEmailID(bad); ok {
			t.Errorf("ParseEmailID(%q) accepted", bad)
		}
	}
}

func TestInitTwice(t *testing.T) {
	s := open(t)
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAccount(t *testing.T) {
	s := open(t)
	a := account(t, s)

	got, err := s.Account(ctx, a.Address)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "Account", got, a)
	_, err = s.CreateAccount(ctx, a.Address, "x")
	wantErr(t, "second CreateAccount", err, ErrAccountExists)
	_, err = s.Account(ctx, "nobody@example.test")
	wantErr(t, "Account(nobody)", err, ErrNoAccount)

	list, err := s.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range list {
		found = found || x == a
	}
	if !found {
		t.Error("Accounts does not list the new account")
	}

	boxes, err := s.Mailboxes(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var names, roles []string
	for i, m := range boxes {
		names, roles = append(names, m.Name), append(roles, m.Role)
		if m.ID != int64(i+1) || m.UIDNext != 1 || m.UIDValidity == 0 || m.Modseq != 1 || m.ParentID != 0 || m.Total != 0 {
			t.Errorf("mailbox %+v", m)
		}
	}
	want(t, "names", names, []string{"Inbox", "Drafts", "Sent", "Junk", "Trash"})
	want(t, "roles", roles, []string{RoleInbox, RoleDrafts, RoleSent, RoleJunk, RoleTrash})

	c := changes(t, s, a, 0)
	want(t, "changes after create", c, Changes{OldState: 0, NewState: 1, Mailbox: ChangeSet{Created: []int64{1, 2, 3, 4, 5}}})

	// Nothing exists for an unknown account, and nothing is created for it.
	_, err = s.Mailboxes(ctx, "nope")
	wantErr(t, "Mailboxes(nope)", err, ErrNoAccount)
	_, err = s.Deliver(ctx, "nope", Message{Raw: msg("x@x", "x"), Key: "k"})
	wantErr(t, "Deliver(nope)", err, ErrNoAccount)
	_, err = s.CreateMailbox(ctx, "nope", 0, "X")
	wantErr(t, "CreateMailbox(nope)", err, ErrNoAccount)
	_, err = s.Changes(ctx, "nope", 0)
	wantErr(t, "Changes(nope)", err, ErrNoAccount)
}

func TestDeliverAndRead(t *testing.T) {
	s := open(t)
	a := account(t, s)
	raw := msg("one@sender.test", "=?utf-8?q?H=C3=A9llo?=")
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	e := deliver(t, s, a, Message{Raw: raw, ReceivedAt: at, Keywords: []string{"$Flagged", "custom"}})
	want(t, "email", e, Email{ID: 1, ThreadID: 1, Blob: e.Blob, Size: int64(len(raw)), ReceivedAt: at,
		MessageID: "one@sender.test", Sender: "Anna <anna@sender.test>", Subject: "Héllo",
		Preview: "Body of =?utf-8?q?H=C3=A9llo?=.", Modseq: 2,
		Mailboxes: map[int64]uint32{inbox: 1}, Keywords: []string{"$flagged", "custom"}})
	if len(e.Blob) != 64 {
		t.Errorf("blob %q", e.Blob)
	}

	got, err := s.Email(ctx, a.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "Email", got, e)
	r, meta, err := s.Open(ctx, a.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r)
	r.Close()
	if string(body) != string(raw) || meta.ID != e.ID {
		t.Errorf("Open returned %q", body)
	}

	// By role, with the inbox as fallback, and by mailbox id.
	spam := deliver(t, s, a, Message{Raw: msg("two@sender.test", "spam"), Role: RoleJunk})
	want(t, "junk delivery", spam.Mailboxes, map[int64]uint32{junk: 1})
	other := deliver(t, s, a, Message{Raw: msg("three@sender.test", "x"), Role: "archive"})
	want(t, "fallback delivery", other.Mailboxes, map[int64]uint32{inbox: 2})
	direct := deliver(t, s, a, Message{Raw: msg("four@sender.test", "x"), MailboxID: sent})
	want(t, "delivery by id", direct.Mailboxes, map[int64]uint32{sent: 1})
	want(t, "ids", []int64{spam.ID, other.ID, direct.ID}, []int64{2, 3, 4})

	list, err := s.Emails(ctx, a.ID, inbox)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "inbox", []int64{list[0].ID, list[1].ID}, []int64{1, 3})
	want(t, "listed email", list[0], e)
	boxes := mailboxes(t, s, a)
	want(t, "inbox counts", []int64{boxes["Inbox"].Total, boxes["Inbox"].Unread, int64(boxes["Inbox"].UIDNext)}, []int64{2, 2, 3})

	before := state(t, s, a)
	_, err = s.Deliver(ctx, a.ID, Message{Raw: raw, MailboxID: 99})
	wantErr(t, "Deliver to a missing mailbox", err, ErrNoMailbox)
	_, err = s.Deliver(ctx, a.ID, Message{Raw: raw, Keywords: []string{"two words"}})
	wantErr(t, "Deliver with a bad keyword", err, ErrKeyword)
	_, err = s.Emails(ctx, a.ID, 99)
	wantErr(t, "Emails(missing)", err, ErrNoMailbox)
	_, err = s.Email(ctx, a.ID, 99)
	wantErr(t, "Email(missing)", err, ErrNoEmail)
	want(t, "state after failures", state(t, s, a), before)

	// A message that is not mail at all is still stored.
	odd := deliver(t, s, a, Message{Raw: []byte("\x00\xff not mail")})
	want(t, "odd message", []any{odd.ThreadID, odd.MessageID, odd.Subject}, []any{int64(5), "", ""})
}

func TestBlobsEncrypted(t *testing.T) {
	s := open(t)
	a, b := account(t, s), account(t, s)
	raw := msg("secret@sender.test", "the launch codes")
	e := deliver(t, s, a, Message{Raw: raw})
	eb := deliver(t, s, b, Message{Raw: raw})
	want(t, "same content, same blob name", eb.Blob, e.Blob)

	// On disk: <account>/<2 hex>/<sha256>, and not a byte of the message.
	path, err := locals[s].Path(blob.Key(a.ID, e.Blob))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/"+a.ID+"/"+e.Blob[:2]+"/"+e.Blob) {
		t.Errorf("blob path %s", path)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(file, []byte("launch")) || bytes.Contains(file, []byte("sender.test")) || len(file) <= len(raw) {
		t.Errorf("blob file is not encrypted: %q", file)
	}
	if err := s.VerifyEmail(ctx, a.ID, e.ID); err != nil {
		t.Errorf("VerifyEmail: %v", err)
	}
	ids, err := s.EmailIDs(ctx, a.ID)
	want(t, "EmailIDs", ids, []int64{e.ID})
	if err != nil {
		t.Fatal(err)
	}

	// A store without the key cache, as another node or a restart would be,
	// reads the key from the account row.
	master, _ := blob.NewMasterKey(testMasterKey)
	fresh := New(s.db, locals[s], master)
	r, _, err := fresh.Open(ctx, a.ID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, raw) {
		t.Errorf("read on a fresh store: %q, %v", got, err)
	}
	// With another master key the account key does not unwrap.
	wrong, _ := blob.NewMasterKey(bytes.Repeat([]byte{1}, 32))
	if _, _, err := New(s.db, locals[s], wrong).Open(ctx, a.ID, e.ID); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("Open under another master key: %v", err)
	}
	if _, err := New(s.db, locals[s], wrong).Deliver(ctx, a.ID, Message{Raw: raw}); err == nil {
		t.Error("Deliver under another master key succeeded")
	}

	// Another account's file under this name: each account has its own key.
	otherPath, _ := locals[s].Path(blob.Key(b.ID, eb.Blob))
	other, _ := os.ReadFile(otherPath)
	if bytes.Equal(other, file) {
		t.Error("two accounts have identical blob files")
	}
	damage := map[string][]byte{
		"another account's file": other,
		"a flipped bit":          append(bytes.Clone(file[:len(file)-1]), file[len(file)-1]^1),
		"a truncated file":       file[:len(file)-5],
		"plaintext":              raw,
	}
	for name, content := range damage {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		wantErr(t, "VerifyEmail with "+name, s.VerifyEmail(ctx, a.ID, e.ID), blob.ErrCorrupt)
		if r, _, err := s.Open(ctx, a.ID, e.ID); err == nil {
			got, err := io.ReadAll(r)
			r.Close()
			if err == nil || len(got) != 0 {
				t.Errorf("Open with %s returned %q, %v", name, got, err)
			}
		}
	}
	os.Remove(path)
	wantErr(t, "VerifyEmail without the file", s.VerifyEmail(ctx, a.ID, e.ID), blob.ErrNotFound)
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyEmail(ctx, a.ID, e.ID); err != nil {
		t.Errorf("VerifyEmail after restoring the file: %v", err)
	}
	wantErr(t, "VerifyEmail(missing)", s.VerifyEmail(ctx, a.ID, 99), ErrNoEmail)
}

func TestIdempotentDelivery(t *testing.T) {
	s := open(t)
	a, b := account(t, s), account(t, s)
	raw := msg("dup@sender.test", "once")

	first := deliver(t, s, a, Message{Raw: raw, Key: "QID1"})
	before := state(t, s, a)
	_, err := s.Deliver(ctx, a.ID, Message{Raw: raw, Key: "QID1"})
	wantErr(t, "second delivery", err, ErrDuplicate)
	_, err = s.Deliver(ctx, a.ID, Message{Raw: msg("else@sender.test", "other"), Key: "QID1", Role: RoleJunk})
	wantErr(t, "same key, other message", err, ErrDuplicate)

	// The refused deliveries left no trace: no email, no id taken, no UID,
	// no modseq, no change rows.
	want(t, "state", state(t, s, a), before)
	list, _ := s.Emails(ctx, a.ID, inbox)
	want(t, "emails", len(list), 1)
	next := deliver(t, s, a, Message{Raw: raw, Key: "QID2"})
	want(t, "next email", []any{next.ID, next.Mailboxes[inbox], next.Modseq}, []any{first.ID + 1, uint32(2), before + 1})

	// Keys are per account, and no key means no check.
	deliver(t, s, b, Message{Raw: raw, Key: "QID1"})
	deliver(t, s, a, Message{Raw: raw})
	deliver(t, s, a, Message{Raw: raw})

	// After the retention the key is forgotten. (2002: TestTrimChanges
	// counts the change rows of 2001.)
	s.Now = func() time.Time { return time.Date(2002, 1, 1, 0, 0, 0, 0, time.UTC) }
	deliver(t, s, a, Message{Raw: raw, Key: "OLD"})
	s.Now = time.Now
	if n, err := s.TrimDelivered(ctx, time.Date(2002, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil || n < 1 {
		t.Fatalf("TrimDelivered = %d, %v", n, err)
	}
	deliver(t, s, a, Message{Raw: raw, Key: "OLD"})
	_, err = s.Deliver(ctx, a.ID, Message{Raw: raw, Key: "QID1"})
	wantErr(t, "recent key after trimming", err, ErrDuplicate)
}

func TestThreads(t *testing.T) {
	s := open(t)
	a, b := account(t, s), account(t, s)
	d := func(acct Account, id string, refs ...string) Email {
		return deliver(t, s, acct, Message{Raw: msg(id, id, refs...)})
	}

	root := d(a, "root@x")
	reply := d(a, "reply@x", "root@x")
	deep := d(a, "deep@x", "root@x", "reply@x")
	unrelated := d(a, "other@x")
	// Only knows a message we never received, as does its sibling.
	orphan := d(a, "orphan@x", "missing@x")
	sibling := d(a, "sibling@x", "missing@x")
	// A reply that arrives before its parent pulls the parent in.
	early := d(a, "early@x", "late@x")
	late := d(a, "late@x")
	// The same Message-ID twice, such as a list copy.
	again := d(a, "root@x")
	noID := d(a, "")
	noID2 := d(a, "")

	want(t, "thread of the root", []int64{root.ThreadID, reply.ThreadID, deep.ThreadID, again.ThreadID}, []int64{1, 1, 1, 1})
	if unrelated.ThreadID == root.ThreadID {
		t.Error("unrelated message joined the thread")
	}
	want(t, "orphan and sibling", sibling.ThreadID, orphan.ThreadID)
	want(t, "early and late", late.ThreadID, early.ThreadID)
	if noID.ThreadID == noID2.ThreadID || noID.ThreadID == root.ThreadID {
		t.Errorf("messages without ids share thread %d", noID.ThreadID)
	}
	ids, err := s.ThreadEmails(ctx, a.ID, root.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "ThreadEmails", ids, []int64{root.ID, reply.ID, deep.ID, again.ID})

	// Threads do not cross accounts.
	foreign := d(b, "reply2@x", "root@x")
	want(t, "thread in another account", foreign.ThreadID, int64(1))
	ids, _ = s.ThreadEmails(ctx, b.ID, 1)
	want(t, "ThreadEmails in another account", ids, []int64{foreign.ID})

	// The change log says which threads are new and which grew.
	c := changes(t, s, a, 1)
	want(t, "thread changes", c.Thread, ChangeSet{Created: []int64{
		root.ThreadID, unrelated.ThreadID, orphan.ThreadID, early.ThreadID, noID.ThreadID, noID2.ThreadID}})
	since := state(t, s, a)
	d(a, "more@x", "deep@x")
	fresh := d(a, "fresh@x")
	c = changes(t, s, a, since)
	want(t, "later thread changes", c.Thread, ChangeSet{Created: []int64{fresh.ThreadID}, Updated: []int64{root.ThreadID}})

	// Destroying part of a thread updates it, destroying all of it ends it:
	// a late reply then starts a new thread.
	since = state(t, s, a)
	if _, err := s.DestroyEmail(ctx, a.ID, orphan.ID); err != nil {
		t.Fatal(err)
	}
	want(t, "thread after one destroy", changes(t, s, a, since).Thread, ChangeSet{Updated: []int64{orphan.ThreadID}})
	since = state(t, s, a)
	if _, err := s.DestroyEmail(ctx, a.ID, sibling.ID); err != nil {
		t.Fatal(err)
	}
	want(t, "thread after the last destroy", changes(t, s, a, since).Thread, ChangeSet{Destroyed: []int64{orphan.ThreadID}})
	revived := d(a, "late-reply@x", "missing@x", "orphan@x")
	if revived.ThreadID == orphan.ThreadID {
		t.Error("a reply revived a destroyed thread")
	}
}

func TestManyReferences(t *testing.T) {
	s := open(t)
	a := account(t, s)
	root := deliver(t, s, a, Message{Raw: msg("near@x", "root")})
	refs := make([]string, 0, 200)
	for i := 0; i < 199; i++ {
		refs = append(refs, fmt.Sprintf("unknown-%d@x", i))
	}
	refs = append(refs, "near@x") // the parent is last and survives the cap
	e := deliver(t, s, a, Message{Raw: msg("long@x", "long", refs...)})
	want(t, "thread", e.ThreadID, root.ThreadID)
}

func TestMoveLabelAndUIDs(t *testing.T) {
	s := open(t)
	a := account(t, s)
	e1 := deliver(t, s, a, Message{Raw: msg("1@x", "one")})
	e2 := deliver(t, s, a, Message{Raw: msg("2@x", "two")})
	archive, err := s.CreateMailbox(ctx, a.ID, 0, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	want(t, "new mailbox", []any{archive.ID, archive.Name, archive.Role, archive.UIDNext, archive.Modseq}, []any{int64(6), "Archive", "", uint32(1), int64(4)})
	get := func(id int64) Email {
		t.Helper()
		e, err := s.Email(ctx, a.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	// Move: same id, same thread, same blob, a new UID in the target.
	since := state(t, s, a)
	st, err := s.MoveEmail(ctx, a.ID, e2.ID, inbox, archive.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved := get(e2.ID)
	want(t, "moved email", []any{moved.ID, moved.ThreadID, moved.Blob, moved.Mailboxes, moved.Modseq},
		[]any{e2.ID, e2.ThreadID, e2.Blob, map[int64]uint32{archive.ID: 1}, st})
	want(t, "state after move", st, since+1)
	want(t, "changes of a move", changes(t, s, a, since), Changes{OldState: since, NewState: st,
		Email: ChangeSet{Updated: []int64{e2.ID}}, Mailbox: ChangeSet{Updated: []int64{inbox, archive.ID}}})

	// Label: the email is in both, with a UID in each.
	since = st
	st, err = s.UpdateEmail(ctx, a.ID, e1.ID, Update{AddMailboxes: []int64{archive.ID, trash}})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "labelled", get(e1.ID).Mailboxes, map[int64]uint32{inbox: 1, archive.ID: 2, trash: 1})
	want(t, "changes of a label", changes(t, s, a, since).Mailbox, ChangeSet{Updated: []int64{trash, archive.ID}})

	// Labelling again changes nothing in the mailbox: no new UID.
	since = st
	if st, err = s.UpdateEmail(ctx, a.ID, e1.ID, Update{AddMailboxes: []int64{archive.ID}}); err != nil {
		t.Fatal(err)
	}
	want(t, "labelled twice", get(e1.ID).Mailboxes, map[int64]uint32{inbox: 1, archive.ID: 2, trash: 1})
	want(t, "changes of a repeated label", changes(t, s, a, since), Changes{OldState: since, NewState: st, Email: ChangeSet{Updated: []int64{e1.ID}}})

	// Back to the inbox: UIDs are never reused, in either mailbox.
	if _, err = s.MoveEmail(ctx, a.ID, e2.ID, archive.ID, inbox); err != nil {
		t.Fatal(err)
	}
	want(t, "moved back", get(e2.ID).Mailboxes, map[int64]uint32{inbox: 3})
	if _, err = s.MoveEmail(ctx, a.ID, e2.ID, inbox, archive.ID); err != nil {
		t.Fatal(err)
	}
	want(t, "moved again", get(e2.ID).Mailboxes, map[int64]uint32{archive.ID: 3})
	e3 := deliver(t, s, a, Message{Raw: msg("3@x", "three")})
	want(t, "next inbox uid", e3.Mailboxes, map[int64]uint32{inbox: 4})
	boxes := mailboxes(t, s, a)
	want(t, "uidnext", []uint32{boxes["Inbox"].UIDNext, boxes["Archive"].UIDNext, boxes["Trash"].UIDNext, boxes["Sent"].UIDNext}, []uint32{5, 4, 2, 1})
	want(t, "totals", []int64{boxes["Inbox"].Total, boxes["Archive"].Total, boxes["Trash"].Total}, []int64{2, 2, 1})
	list, _ := s.Emails(ctx, a.ID, archive.ID)
	want(t, "archive in UID order", []int64{list[0].ID, list[1].ID}, []int64{e1.ID, e2.ID})

	// Unlabel, down to one mailbox but not to none.
	if _, err = s.UpdateEmail(ctx, a.ID, e1.ID, Update{RemoveMailboxes: []int64{inbox, trash, 99}}); err != nil {
		t.Fatal(err)
	}
	want(t, "unlabelled", get(e1.ID).Mailboxes, map[int64]uint32{archive.ID: 2})
	before := state(t, s, a)
	_, err = s.UpdateEmail(ctx, a.ID, e1.ID, Update{RemoveMailboxes: []int64{archive.ID}})
	wantErr(t, "removing the last mailbox", err, ErrNoMailboxes)
	_, err = s.UpdateEmail(ctx, a.ID, e1.ID, Update{AddMailboxes: []int64{inbox, 99}})
	wantErr(t, "adding a missing mailbox", err, ErrNoMailbox)
	_, err = s.MoveEmail(ctx, a.ID, e1.ID, inbox, trash)
	wantErr(t, "moving from a mailbox it is not in", err, ErrNoEmail)
	_, err = s.UpdateEmail(ctx, a.ID, 99, Update{AddMailboxes: []int64{inbox}})
	wantErr(t, "updating a missing email", err, ErrNoEmail)
	_, err = s.UpdateEmail(ctx, "nope", e1.ID, Update{AddMailboxes: []int64{inbox}})
	wantErr(t, "updating in a missing account", err, ErrNoAccount)
	want(t, "state after failures", state(t, s, a), before)
	want(t, "email after failures", get(e1.ID).Mailboxes, map[int64]uint32{archive.ID: 2})
	if st, err := s.UpdateEmail(ctx, a.ID, e1.ID, Update{}); err != nil || st != before {
		t.Errorf("empty update = %d, %v", st, err)
	}
}

func TestMailboxCreateRename(t *testing.T) {
	s := open(t)
	a := account(t, s)
	e := deliver(t, s, a, Message{Raw: msg("1@x", "one")})
	work, err := s.CreateMailbox(ctx, a.ID, 0, "Work")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateMailbox(ctx, a.ID, work.ID, "Invoices")
	if err != nil {
		t.Fatal(err)
	}
	want(t, "child", []int64{child.ID, child.ParentID}, []int64{7, work.ID})
	if _, err := s.MoveEmail(ctx, a.ID, e.ID, inbox, work.ID); err != nil {
		t.Fatal(err)
	}

	// A rename changes the name and nothing else.
	since := state(t, s, a)
	st, err := s.RenameMailbox(ctx, a.ID, work.ID, "Werk")
	if err != nil {
		t.Fatal(err)
	}
	renamed := mailboxes(t, s, a)["Werk"]
	want(t, "renamed", []any{renamed.ID, renamed.UIDValidity, renamed.UIDNext, renamed.Total, renamed.Modseq},
		[]any{work.ID, work.UIDValidity, uint32(2), int64(1), st})
	got, _ := s.Email(ctx, a.ID, e.ID)
	want(t, "email after rename", []any{got.ID, got.Mailboxes, got.Modseq}, []any{e.ID, map[int64]uint32{work.ID: 1}, since})
	want(t, "child after rename", mailboxes(t, s, a)["Invoices"].ParentID, work.ID)
	want(t, "changes of a rename", changes(t, s, a, since), Changes{OldState: since, NewState: st, Mailbox: ChangeSet{Updated: []int64{work.ID}}})

	before := state(t, s, a)
	_, err = s.CreateMailbox(ctx, a.ID, 0, "Werk")
	wantErr(t, "duplicate name", err, ErrMailboxExists)
	_, err = s.CreateMailbox(ctx, a.ID, 0, "Inbox")
	wantErr(t, "duplicate of a default", err, ErrMailboxExists)
	_, err = s.RenameMailbox(ctx, a.ID, child.ID, "Invoices")
	if err != nil {
		t.Errorf("rename to its own name: %v", err)
	}
	_, err = s.RenameMailbox(ctx, a.ID, work.ID, "Trash")
	wantErr(t, "rename onto another", err, ErrMailboxExists)
	_, err = s.RenameMailbox(ctx, a.ID, 99, "X")
	wantErr(t, "rename a missing mailbox", err, ErrNoMailbox)
	_, err = s.CreateMailbox(ctx, a.ID, 99, "X")
	wantErr(t, "missing parent", err, ErrNoMailbox)
	for _, bad := range []string{"", " x", "x ", "a\x00b", "a\nb", strings.Repeat("x", 256), "\xff"} {
		_, err = s.CreateMailbox(ctx, a.ID, 0, bad)
		wantErr(t, fmt.Sprintf("name %q", bad), err, ErrMailboxName)
		_, err = s.RenameMailbox(ctx, a.ID, work.ID, bad)
		wantErr(t, fmt.Sprintf("rename to %q", bad), err, ErrMailboxName)
	}
	want(t, "state after failures", state(t, s, a), before+1) // the one rename that worked

	// The same name under another parent is fine, and ids are not reused.
	sub, err := s.CreateMailbox(ctx, a.ID, work.ID, "Werk")
	if err != nil {
		t.Fatal(err)
	}
	want(t, "next mailbox id", sub.ID, int64(8))
}

func TestKeywords(t *testing.T) {
	s := open(t)
	a := account(t, s)
	e := deliver(t, s, a, Message{Raw: msg("1@x", "one")})
	if _, err := s.UpdateEmail(ctx, a.ID, e.ID, Update{AddMailboxes: []int64{trash}}); err != nil {
		t.Fatal(err)
	}
	keywords := func() []string {
		t.Helper()
		got, err := s.Email(ctx, a.ID, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Keywords
	}

	since := state(t, s, a)
	st, err := s.UpdateEmail(ctx, a.ID, e.ID, Update{AddKeywords: []string{"$Seen", "$seen", "todo"}})
	if err != nil {
		t.Fatal(err)
	}
	want(t, "added", keywords(), []string{"$seen", "todo"})
	want(t, "unread", mailboxes(t, s, a)["Inbox"].Unread, int64(0))
	// The unread count of both mailboxes changed.
	want(t, "changes of a keyword", changes(t, s, a, since), Changes{OldState: since, NewState: st,
		Email: ChangeSet{Updated: []int64{e.ID}}, Mailbox: ChangeSet{Updated: []int64{inbox, trash}}})
	got, _ := s.Email(ctx, a.ID, e.ID)
	want(t, "email modseq", got.Modseq, st)
	if !got.HasKeyword("$seen") || got.HasKeyword("$flagged") {
		t.Errorf("HasKeyword on %v", got.Keywords)
	}

	if _, err = s.UpdateEmail(ctx, a.ID, e.ID, Update{RemoveKeywords: []string{"$SEEN", "absent"}, AddKeywords: []string{"$flagged"}}); err != nil {
		t.Fatal(err)
	}
	want(t, "patched", keywords(), []string{"$flagged", "todo"})
	if _, err = s.UpdateEmail(ctx, a.ID, e.ID, Update{ReplaceKeywords: true, AddKeywords: []string{"$answered"}}); err != nil {
		t.Fatal(err)
	}
	want(t, "replaced", keywords(), []string{"$answered"})
	if _, err = s.UpdateEmail(ctx, a.ID, e.ID, Update{ReplaceKeywords: true}); err != nil {
		t.Fatal(err)
	}
	want(t, "cleared", len(keywords()), 0)

	for _, bad := range []string{"", "two words", "a(b", "tab\t", "é", strings.Repeat("k", 256), `q"`, "a%", "a*"} {
		_, err = s.UpdateEmail(ctx, a.ID, e.ID, Update{AddKeywords: []string{bad}})
		wantErr(t, fmt.Sprintf("keyword %q", bad), err, ErrKeyword)
	}
}

func TestDestroy(t *testing.T) {
	s := open(t)
	a := account(t, s)
	e1 := deliver(t, s, a, Message{Raw: msg("1@x", "one"), Keywords: []string{"$seen"}})
	e2 := deliver(t, s, a, Message{Raw: msg("2@x", "two")})
	if _, err := s.UpdateEmail(ctx, a.ID, e1.ID, Update{AddMailboxes: []int64{trash}}); err != nil {
		t.Fatal(err)
	}

	since := state(t, s, a)
	st, err := s.DestroyEmail(ctx, a.ID, e1.ID)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "changes of a destroy", changes(t, s, a, since), Changes{OldState: since, NewState: st,
		Email: ChangeSet{Destroyed: []int64{e1.ID}}, Mailbox: ChangeSet{Updated: []int64{inbox, trash}},
		Thread: ChangeSet{Destroyed: []int64{e1.ThreadID}}})
	_, err = s.Email(ctx, a.ID, e1.ID)
	wantErr(t, "Email after destroy", err, ErrNoEmail)
	_, err = s.DestroyEmail(ctx, a.ID, e1.ID)
	wantErr(t, "second destroy", err, ErrNoEmail)
	want(t, "state after a failed destroy", state(t, s, a), st)
	boxes := mailboxes(t, s, a)
	want(t, "counts", []int64{boxes["Inbox"].Total, boxes["Trash"].Total, boxes["Inbox"].Modseq, boxes["Trash"].Modseq}, []int64{1, 0, st, st})

	// Ids and UIDs of the destroyed email are not handed out again.
	e3 := deliver(t, s, a, Message{Raw: msg("3@x", "three")})
	want(t, "next email", []any{e3.ID, e3.Mailboxes}, []any{e2.ID + 1, map[int64]uint32{inbox: 3}})

	// Seen from the start, the destroyed email never existed.
	c := changes(t, s, a, 1)
	want(t, "email changes from the start", c.Email, ChangeSet{Created: []int64{e2.ID, e3.ID}})
	want(t, "thread changes from the start", c.Thread, ChangeSet{Created: []int64{e2.ThreadID, e3.ThreadID}})
}

func TestModseqAndChanges(t *testing.T) {
	s := open(t)
	a := account(t, s)
	want(t, "initial state", state(t, s, a), int64(1))

	// Every write is one step, and stamps what it touched.
	e := deliver(t, s, a, Message{Raw: msg("1@x", "one")})
	want(t, "after delivery", []int64{state(t, s, a), e.Modseq, mailboxes(t, s, a)["Inbox"].Modseq}, []int64{2, 2, 2})
	box, _ := s.CreateMailbox(ctx, a.ID, 0, "Box")
	want(t, "after create", []int64{state(t, s, a), box.Modseq}, []int64{3, 3})
	st, _ := s.UpdateEmail(ctx, a.ID, e.ID, Update{AddMailboxes: []int64{box.ID}, AddKeywords: []string{"$seen"}})
	want(t, "after update", []int64{st, state(t, s, a)}, []int64{4, 4})
	st, _ = s.RenameMailbox(ctx, a.ID, box.ID, "Crate")
	want(t, "after rename", st, int64(5))
	boxes := mailboxes(t, s, a)
	want(t, "mailbox modseqs", []int64{boxes["Inbox"].Modseq, boxes["Crate"].Modseq, boxes["Sent"].Modseq}, []int64{4, 5, 1})

	// Created then updated is created; the window decides.
	want(t, "from 1", changes(t, s, a, 1), Changes{OldState: 1, NewState: 5,
		Email:   ChangeSet{Created: []int64{e.ID}},
		Mailbox: ChangeSet{Created: []int64{box.ID}, Updated: []int64{inbox}},
		Thread:  ChangeSet{Created: []int64{e.ThreadID}}})
	want(t, "from 3", changes(t, s, a, 3), Changes{OldState: 3, NewState: 5,
		Email: ChangeSet{Updated: []int64{e.ID}}, Mailbox: ChangeSet{Updated: []int64{inbox, box.ID}}})
	want(t, "from now", changes(t, s, a, 5), Changes{OldState: 5, NewState: 5})
	_, err := s.Changes(ctx, a.ID, 6)
	wantErr(t, "a state from the future", err, ErrCannotCalculateChanges)
	_, err = s.Changes(ctx, a.ID, -1)
	wantErr(t, "a negative state", err, ErrCannotCalculateChanges)

	// Another account is untouched by all of this.
	b := account(t, s)
	want(t, "other account", changes(t, s, b, 1), Changes{OldState: 1, NewState: 1})
}

func TestTrimChanges(t *testing.T) {
	s := open(t)
	a, b := account(t, s), account(t, s)

	// Three old writes in a, then recent ones in both. Nothing else in the
	// database is as old as 2001, so trimming there only hits this test.
	real := s.Now
	s.Now = func() time.Time { return time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) }
	old1 := deliver(t, s, a, Message{Raw: msg("1@x", "one")})
	deliver(t, s, a, Message{Raw: msg("2@x", "two")})
	oldState, err := s.UpdateEmail(ctx, a.ID, old1.ID, Update{AddKeywords: []string{"$seen"}})
	if err != nil {
		t.Fatal(err)
	}
	s.Now = real
	recent := deliver(t, s, a, Message{Raw: msg("3@x", "three")})
	deliver(t, s, b, Message{Raw: msg("1@x", "one")})

	n, err := s.TrimChanges(ctx, time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	// 3 per delivery (Email, Mailbox, Thread) and 2 for the keyword.
	if n != 8 {
		t.Errorf("TrimChanges removed %d rows, want 8", n)
	}

	// A state from before the cut cannot be answered any more; the state at
	// the cut and later ones can.
	for _, since := range []int64{0, 1, oldState - 1} {
		_, err = s.Changes(ctx, a.ID, since)
		wantErr(t, fmt.Sprintf("Changes(%d) after trimming", since), err, ErrCannotCalculateChanges)
	}
	want(t, "changes from the cut", changes(t, s, a, oldState), Changes{OldState: oldState, NewState: oldState + 1,
		Email: ChangeSet{Created: []int64{recent.ID}}, Mailbox: ChangeSet{Updated: []int64{inbox}},
		Thread: ChangeSet{Created: []int64{recent.ThreadID}}})
	// The mail itself is untouched, and so is the other account.
	list, _ := s.Emails(ctx, a.ID, inbox)
	want(t, "emails after trimming", len(list), 3)
	want(t, "other account", len(changes(t, s, b, 0).Mailbox.Created), 5)

	// Trimming again finds nothing and does not move the cut back.
	if n, err = s.TrimChanges(ctx, time.Date(2001, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil || n != 0 {
		t.Errorf("second TrimChanges = %d, %v", n, err)
	}
	if n, err = s.TrimChanges(ctx, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil || n != 0 {
		t.Errorf("earlier TrimChanges = %d, %v", n, err)
	}
	if _, err = s.Changes(ctx, a.ID, oldState); err != nil {
		t.Errorf("Changes from the cut after a second trim: %v", err)
	}
}

// TestConcurrentDeliveries is the phase 0 spike as a test: replies delivered
// twice each from many goroutines, as retrying senders on several nodes
// would. Ids, UIDs and modseq must come out without gaps or doubles.
func TestConcurrentDeliveries(t *testing.T) {
	s := open(t)
	const roots, replies, workers = 8, 10, 16
	accounts := []Account{account(t, s), account(t, s)}
	rootThread := map[string]int64{}
	for _, a := range accounts {
		for r := 0; r < roots; r++ {
			id := fmt.Sprintf("root-%d@x", r)
			rootThread[a.ID+id] = deliver(t, s, a, Message{Raw: msg(id, id), Key: id}).ThreadID
		}
	}

	type job struct {
		a          Account
		id, parent string
	}
	jobs := make(chan job)
	var stored, refused, wrongThread, failed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				e, err := s.Deliver(ctx, j.a.ID, Message{Raw: msg(j.id, j.id, j.parent), Key: j.id})
				switch {
				case err == nil:
					stored.Add(1)
					if e.ThreadID != rootThread[j.a.ID+j.parent] {
						wrongThread.Add(1)
					}
				case errors.Is(err, ErrDuplicate):
					refused.Add(1)
				default:
					failed.Add(1)
					t.Error(err)
				}
			}
		}()
	}
	for round := 0; round < 2; round++ {
		for _, a := range accounts {
			for r := 0; r < roots; r++ {
				for n := 0; n < replies; n++ {
					jobs <- job{a, fmt.Sprintf("reply-%d-%d@x", r, n), fmt.Sprintf("root-%d@x", r)}
				}
			}
		}
	}
	close(jobs)
	wg.Wait()

	total := int64(len(accounts) * roots * replies)
	want(t, "stored, refused, failed, wrong thread",
		[]int64{stored.Load(), refused.Load(), failed.Load(), wrongThread.Load()}, []int64{total, total, 0, 0})
	per := roots + roots*replies
	for _, a := range accounts {
		list, err := s.Emails(ctx, a.ID, inbox)
		if err != nil {
			t.Fatal(err)
		}
		want(t, "emails", len(list), per)
		ids, modseqs, threads := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
		for i, e := range list {
			if e.Mailboxes[inbox] != uint32(i+1) {
				t.Fatalf("email %d has UID %d, want %d", e.ID, e.Mailboxes[inbox], i+1)
			}
			if e.ID < 1 || e.ID > int64(per) || e.Modseq < 2 || e.Modseq > int64(per)+1 {
				t.Fatalf("email %+v out of range", e)
			}
			ids[e.ID], modseqs[e.Modseq], threads[e.ThreadID] = true, true, true
		}
		want(t, "distinct ids, modseqs, threads", []int{len(ids), len(modseqs), len(threads)}, []int{per, per, roots})
		want(t, "state", state(t, s, a), int64(per)+1)
		want(t, "uidnext", mailboxes(t, s, a)["Inbox"].UIDNext, uint32(per)+1)
		c := changes(t, s, a, 1)
		want(t, "changes", []int{len(c.Email.Created), len(c.Thread.Created), len(c.Mailbox.Updated)}, []int{per, roots, 1})
	}
}

func TestSummarize(t *testing.T) {
	raw := "From: =?utf-8?q?J=C3=BCrgen?= <j@x.test>\r\nSubject: Re: plan\r\n\tfor\x07 tomorrow\r\n" +
		"Message-ID: <own@x>\r\nReferences: <a@x> <b@x>\r\n\t<c@x> <b@x>\r\nIn-Reply-To: <c@x>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Hall=C3=B6,\r\n\r\n  see   you =\r\nthere.\r\n"
	got := summarize([]byte(raw))
	want(t, "summary", got, summary{messageID: "own@x", refs: []string{"own@x", "a@x", "b@x", "c@x"},
		sender: "Jürgen <j@x.test>", subject: "Re: plan for tomorrow", preview: "Hallö, see you there."})

	html := summarize([]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nhi\r\n--x--\r\n"))
	want(t, "multipart", html, summary{hasAttachment: true})
	b64 := summarize([]byte("Content-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29y\r\nbGQ=\r\n"))
	want(t, "base64 preview", b64.preview, "hello world")
	want(t, "not mail", summarize([]byte("\x00\x01")), summary{})
	want(t, "empty", summarize(nil), summary{})

	want(t, "messageIDs", messageIDs("<a@x>,<b c@x> junk <<d@x> <> <e@x"), []string{"a@x", "d@x"})
	want(t, "long id skipped", len(messageIDs("<"+strings.Repeat("a", 251)+">")), 0)
	long := summarize([]byte("Subject: " + strings.Repeat("é", 400) + "\r\n\r\n" + strings.Repeat("word ", 400)))
	if n := len([]rune(long.subject)); n != maxHeaderRunes {
		t.Errorf("subject of %d runes", n)
	}
	if n := len([]rune(long.preview)); n > maxPreviewRunes || n < maxPreviewRunes-5 {
		t.Errorf("preview of %d runes", n)
	}
}

func FuzzSummarize(f *testing.F) {
	f.Add([]byte("From: a@x\r\nMessage-ID: <a@x>\r\nReferences: <b@x> <c@x>\r\n\r\nbody"))
	f.Add([]byte("Content-Transfer-Encoding: base64\r\n\r\n!!!"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		s := summarize(raw)
		if len(s.refs) > maxRefs || len([]rune(s.subject)) > maxHeaderRunes || len([]rune(s.sender)) > maxHeaderRunes ||
			len([]rune(s.preview)) > maxPreviewRunes {
			t.Fatalf("summary over its limits: %+v", s)
		}
		for _, id := range s.refs {
			if id == "" || len(id) > maxMessageIDLen || strings.ContainsAny(id, "<> \r\n") {
				t.Fatalf("bad id %q", id)
			}
		}
	})
}

func TestDomains(t *testing.T) {
	s := open(t)
	var r [6]byte
	rand.Read(r[:])
	name := "d" + hex.EncodeToString(r[:]) + ".test"

	_, err := s.CreateAccount(ctx, "anna@"+name, "$argon2id$test")
	wantErr(t, "CreateAccount in an unknown domain", err, ErrNoDomain)
	wantErr(t, "RemoveDomain(unknown)", s.RemoveDomain(ctx, name), ErrNoDomain)

	wantErr(t, "AddDomain", s.AddDomain(ctx, name), nil)
	wantErr(t, "second AddDomain", s.AddDomain(ctx, name), ErrDomainExists)
	list, err := s.Domains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, d := range list {
		found = found || d == name
		if i > 0 && list[i-1] >= d {
			t.Errorf("Domains not sorted: %q before %q", list[i-1], d)
		}
	}
	if !found {
		t.Errorf("Domains lacks %s", name)
	}

	// A domain with an account stays. A domain whose name merely ends the
	// same way does not count as in use.
	if _, err := s.CreateAccount(ctx, "anna@"+name, "$argon2id$test"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "RemoveDomain with an account", s.RemoveDomain(ctx, name), ErrDomainInUse)
	sub := "sub." + name
	wantErr(t, "AddDomain(sub)", s.AddDomain(ctx, sub), nil)
	if _, err := s.CreateAccount(ctx, "anna@"+sub, "$argon2id$test"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "RemoveDomain while only the subdomain's twin has accounts", s.RemoveDomain(ctx, name), ErrDomainInUse)

	empty := "empty." + name
	wantErr(t, "AddDomain(empty)", s.AddDomain(ctx, empty), nil)
	wantErr(t, "RemoveDomain(empty)", s.RemoveDomain(ctx, empty), nil)
	wantErr(t, "RemoveDomain again", s.RemoveDomain(ctx, empty), ErrNoDomain)
	_, err = s.CreateAccount(ctx, "anna@"+empty, "$argon2id$test")
	wantErr(t, "CreateAccount after RemoveDomain", err, ErrNoDomain)
}

// TestConcurrentTrim runs the daily trim the way several nodes would: all at
// once. Each trim is one atomic batch, so together they remove every old row
// exactly once and agree on where the change log now starts.
func TestConcurrentTrim(t *testing.T) {
	s := open(t)
	a := account(t, s)

	// Nothing else in the database is as old as 1999, see TestTrimChanges.
	real := s.Now
	s.Now = func() time.Time { return time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC) }
	var oldState int64
	for i := 0; i < 5; i++ {
		oldState = deliver(t, s, a, Message{Raw: msg(fmt.Sprintf("%d@trim", i), "old"), Key: fmt.Sprintf("old-%d", i)}).Modseq
	}
	s.Now = real
	recent := deliver(t, s, a, Message{Raw: msg("new@trim", "new"), Key: "new"})

	before := time.Date(1999, 1, 2, 0, 0, 0, 0, time.UTC)
	const nodes = 8
	var wg sync.WaitGroup
	var rows, keys atomic.Int64
	for i := 0; i < nodes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.TrimChanges(ctx, before)
			if err != nil {
				t.Error(err)
			}
			rows.Add(n)
			if n, err = s.TrimDelivered(ctx, before); err != nil {
				t.Error(err)
			}
			keys.Add(n)
		}()
	}
	wg.Wait()
	want(t, "change rows removed by all nodes together", rows.Load(), int64(15))
	want(t, "delivery keys removed by all nodes together", keys.Load(), int64(5))

	_, err := s.Changes(ctx, a.ID, oldState-1)
	wantErr(t, "Changes from before the cut", err, ErrCannotCalculateChanges)
	want(t, "changes from the cut", changes(t, s, a, oldState), Changes{OldState: oldState, NewState: oldState + 1,
		Email: ChangeSet{Created: []int64{recent.ID}}, Mailbox: ChangeSet{Updated: []int64{inbox}},
		Thread: ChangeSet{Created: []int64{recent.ThreadID}}})
	// An old key is free again; the recent one still blocks a retry.
	_, err = s.Deliver(ctx, a.ID, Message{Raw: msg("again@trim", "again"), Key: "old-0"})
	wantErr(t, "old key after trimming", err, nil)
	_, err = s.Deliver(ctx, a.ID, Message{Raw: msg("new@trim", "new"), Key: "new"})
	wantErr(t, "recent key after trimming", err, ErrDuplicate)
}
