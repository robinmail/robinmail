package store

import (
	"context"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/notify"
	"github.com/robinmail/robinmail/internal/raftdb/raftdbtest"
)

// twoNodes is two stores on the two nodes of one cluster, as two robinmail
// nodes have them: a blob directory each, the same master key.
func twoNodes(t *testing.T) (*Store, *Store) {
	t.Helper()
	dbs := raftdbtest.Cluster(t, 2)
	master, err := blob.NewMasterKey(testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	s1 := New(dbs[0], blob.NewLocal(t.TempDir()), master)
	if err := s1.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s1.AddDomain(ctx, "example.test"); err != nil {
		t.Fatal(err)
	}
	return s1, New(dbs[1], blob.NewLocal(t.TempDir()), master)
}

func waitWake(t *testing.T, c <-chan struct{}, within time.Duration, what string) time.Duration {
	t.Helper()
	start := time.Now()
	select {
	case <-c:
		return time.Since(start)
	case <-time.After(within):
		t.Fatalf("%s: no wake-up within %v", what, within)
		return 0
	}
}

func drain(c <-chan struct{}) {
	for {
		select {
		case <-c:
		case <-time.After(150 * time.Millisecond):
			return
		}
	}
}

// Two nodes of one cluster. A write through node 1 wakes node 1's listeners
// through the store's hook, at once, and node 2's through node 2's poll of
// the account's modseq. Every kind of write that moves the modseq does.
func TestChangeReachesListenersOnBothNodes(t *testing.T) {
	s1, s2 := twoNodes(t)
	h1 := &notify.Hub{States: s1.ModSeqs}
	h2 := &notify.Hub{States: s2.ModSeqs, Interval: 50 * time.Millisecond}
	s1.OnChange, s2.OnChange = h1.Changed, h2.Changed
	// Node 1 does not poll at all here: what its listener hears, it hears
	// from the hook.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go h2.Run(runCtx)

	acct := account(t, s1)
	other := account(t, s1)
	local, stopLocal := h1.Subscribe(acct.ID)
	defer stopLocal()
	remote, stopRemote := h2.Subscribe(acct.ID)
	defer stopRemote()
	bystander, stopBystander := h2.Subscribe(other.ID)
	defer stopBystander()
	drain(remote) // the first poll of an account always wakes
	drain(bystander)

	inbox := mailboxByRole(t, s1, acct.ID, RoleInbox)
	var emailID int64
	writes := []struct {
		name string
		do   func() error
	}{
		{"delivery", func() error {
			e, err := s1.Deliver(ctx, acct.ID, Message{Raw: msg("<n1@example.test>", "push")})
			emailID = e.ID
			return err
		}},
		{"keyword", func() error {
			_, err := s1.UpdateEmail(ctx, acct.ID, emailID, Update{AddKeywords: []string{"$seen"}})
			return err
		}},
		{"mailbox created", func() error {
			_, err := s1.CreateMailbox(ctx, acct.ID, 0, "Push")
			return err
		}},
		{"mailbox renamed", func() error {
			_, err := s1.RenameMailbox(ctx, acct.ID, inbox.ID, "In")
			return err
		}},
		{"email destroyed", func() error {
			_, err := s1.DestroyEmail(ctx, acct.ID, emailID)
			return err
		}},
	}
	for _, w := range writes {
		if err := w.do(); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		// The hook runs before the write returns.
		select {
		case <-local:
		default:
			t.Fatalf("%s: the listener on the writing node was not woken by the time the write returned", w.name)
		}
		took := waitWake(t, remote, 5*time.Second, w.name+", other node")
		t.Logf("%-16s other node woken after %v", w.name, took.Round(time.Millisecond))
		// What the listener then reads is the new state.
		want, _ := s1.State(ctx, acct.ID)
		if got, err := s2.State(ctx, acct.ID); err != nil || got != want {
			t.Fatalf("%s: node 2 reads state %d (%v), node 1 wrote %d", w.name, got, err, want)
		}
		drain(remote)
	}
	select {
	case <-bystander:
		t.Fatal("a listener of another account was woken")
	default:
	}

	// A read does not wake anybody, and neither does a write that fails.
	if _, err := s1.Mailboxes(ctx, acct.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.DestroyEmail(ctx, acct.ID, 999999); err == nil {
		t.Fatal("destroying an email that is not there succeeded")
	}
	select {
	case <-local:
		t.Fatal("woken by a read or a failed write")
	case <-remote:
		t.Fatal("other node woken by a read or a failed write")
	case <-time.After(300 * time.Millisecond):
	}
}

// ModSeqs leaves out accounts that do not exist.
func TestModSeqs(t *testing.T) {
	s := open(t)
	a, b := account(t, s), account(t, s)
	if _, err := s.Deliver(ctx, a.ID, Message{Raw: msg("<m1@example.test>", "x")}); err != nil {
		t.Fatal(err)
	}
	// Level none may answer from a copy that is a moment behind.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := s.ModSeqs(ctx, []string{a.ID, b.ID, "nobody"})
		if err != nil {
			t.Fatal(err)
		}
		sa, _ := s.State(ctx, a.ID)
		sb, _ := s.State(ctx, b.ID)
		if len(got) == 2 && got[a.ID] == sa && got[b.ID] == sb {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ModSeqs = %v, want %s:%d %s:%d", got, a.ID, sa, b.ID, sb)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mailboxByRole(t *testing.T, s *Store, account, role string) Mailbox {
	t.Helper()
	boxes, err := s.Mailboxes(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range boxes {
		if m.Role == role {
			return m
		}
	}
	t.Fatalf("no mailbox with role %s", role)
	return Mailbox{}
}
