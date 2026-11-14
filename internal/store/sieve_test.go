package store

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestSieveScripts(t *testing.T) {
	s := open(t)
	a := account(t, s)
	one, state, err := s.CreateSieveScript(ctx, a.ID, "vacation", []byte("keep;\r\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "first script", []any{one.ID, one.Name, one.Size, one.Active, state}, []any{int64(1), "vacation", int64(7), false, int64(2)})
	two, _, err := s.CreateSieveScript(ctx, a.ID, "Filters ✉", []byte("discard;\r\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The content is a blob of the account: encrypted on disk, readable
	// back, served by its hash, and kept by the sweeper.
	if got, err := s.SieveScriptContent(ctx, a.ID, one); err != nil || string(got) != "keep;\r\n" {
		t.Fatalf("content: %q, %v", got, err)
	}
	if r, size, err := s.OpenBlob(ctx, a.ID, one.Blob); err != nil || size != 7 {
		t.Fatalf("OpenBlob: %d, %v", size, err)
	} else {
		r.Close()
	}
	refs, err := blobRefs{s}.AccountBlobs(ctx, a.ID)
	if err != nil || !refs[one.Blob] || !refs[two.Blob] {
		t.Errorf("scripts are not among the referenced blobs: %v, %v", refs, err)
	}
	if ok, err := (blobRefs{s}).Referenced(ctx, a.ID, one.Blob); err != nil || !ok {
		t.Errorf("Referenced: %v, %v", ok, err)
	}

	for _, c := range []struct {
		name string
		err  error
	}{{"vacation", ErrSieveName}, {"", ErrSieveBadName}, {"line\nbreak", ErrSieveBadName}, {"nul\x00", ErrSieveBadName},
		{string(make([]byte, MaxSieveNameSize+1)), ErrSieveBadName}} {
		if _, _, err := s.CreateSieveScript(ctx, a.ID, c.name, []byte("keep;"), 0); !errors.Is(err, c.err) {
			t.Errorf("create %q: %v, want %v", c.name, err, c.err)
		}
	}
	if _, _, err := s.CreateSieveScript(ctx, a.ID, "stale", []byte("keep;"), 1); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("ifInState: %v", err)
	}

	// No script is active until one is made so; then exactly one is.
	if _, _, ok, err := s.ActiveSieveScript(ctx, a.ID); ok || err != nil {
		t.Fatalf("active before any activation: %v, %v", ok, err)
	}
	before := state2(t, s, a)
	if _, err := s.ActivateSieveScript(ctx, a.ID, one.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateSieveScript(ctx, a.ID, two.ID, 0); err != nil {
		t.Fatal(err)
	}
	list, err := s.SieveScripts(ctx, a.ID)
	if err != nil || len(list) != 2 || list[0].Active || !list[1].Active {
		t.Fatalf("after activating the second: %+v, %v", list, err)
	}
	blob, read, ok, err := s.ActiveSieveScript(ctx, a.ID)
	if err != nil || !ok || blob != two.Blob {
		t.Fatalf("ActiveSieveScript: %s, %v, %v", blob, ok, err)
	}
	if content, err := read(); err != nil || string(content) != "discard;\r\n" {
		t.Fatalf("the active script: %q, %v", content, err)
	}
	// Back to the first and to the second again: whichever row comes
	// first in the table, there is never a moment with two active ones.
	for _, id := range []int64{one.ID, two.ID} {
		if _, err := s.ActivateSieveScript(ctx, a.ID, id, 0); err != nil {
			t.Fatalf("activating %d: %v", id, err)
		}
	}
	// Both scripts changed: one stopped being active, one started.
	set, _, _, err := s.ChangesOf(ctx, a.ID, "SieveScript", before, 0)
	if err != nil || !reflect.DeepEqual(set.Updated, []int64{one.ID, two.ID}) {
		t.Errorf("changes: %+v, %v", set, err)
	}
	all, err := s.Changes(ctx, a.ID, before)
	if err != nil || all.Other["SieveScript"] == nil || len(all.Other["SieveScript"].Updated) != 2 {
		t.Errorf("Changes: %+v, %v", all.Other, err)
	}

	// The active script cannot be destroyed; a renamed and rewritten one
	// keeps its id.
	if _, err := s.DestroySieveScript(ctx, a.ID, two.ID, 0); !errors.Is(err, ErrSieveActive) {
		t.Errorf("destroy the active script: %v", err)
	}
	name := "holiday"
	if _, err := s.UpdateSieveScript(ctx, a.ID, one.ID, &name, []byte("stop;\r\n"), 0); err != nil {
		t.Fatal(err)
	}
	taken := "Filters ✉"
	if _, err := s.UpdateSieveScript(ctx, a.ID, one.ID, &taken, nil, 0); !errors.Is(err, ErrSieveName) {
		t.Errorf("rename to a taken name: %v", err)
	}
	if _, err := s.UpdateSieveScript(ctx, a.ID, 99, &name, nil, 0); !errors.Is(err, ErrNoSieveScript) {
		t.Errorf("update a script that is not there: %v", err)
	}
	list, _ = s.SieveScripts(ctx, a.ID)
	if list[0].Name != "holiday" || list[0].Size != 7 || list[0].Blob == one.Blob {
		t.Errorf("after the update: %+v", list[0])
	}
	if _, err := s.ActivateSieveScript(ctx, a.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.ActiveSieveScript(ctx, a.ID); ok {
		t.Error("a script is active after deactivating")
	}
	if _, err := s.DestroySieveScript(ctx, a.ID, two.ID, 0); err != nil {
		t.Errorf("destroy: %v", err)
	}
	if _, err := s.ActivateSieveScript(ctx, a.ID, two.ID, 0); !errors.Is(err, ErrNoSieveScript) {
		t.Errorf("activate a destroyed script: %v", err)
	}

	// The limit.
	for i := 0; ; i++ {
		_, _, err := s.CreateSieveScript(ctx, a.ID, "n"+string(rune('a'+i)), []byte("keep;"), 0)
		if errors.Is(err, ErrSieveLimit) {
			if i != MaxSieveScripts-1 {
				t.Errorf("the limit came at %d scripts", i+1)
			}
			break
		}
		if err != nil || i > MaxSieveScripts {
			t.Fatalf("script %d: %v", i, err)
		}
	}
}

func state2(t *testing.T, s *Store, a Account) int64 { return state(t, s, a) }

// One reply per key per period, also when several nodes ask at once.
func TestVacationOnce(t *testing.T) {
	s := open(t)
	a := account(t, s)
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	week := 7 * 24 * time.Hour
	ask := func(key string) bool {
		t.Helper()
		ok, err := s.VacationOnce(ctx, a.ID, key, week)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !ask("anna") || ask("anna") || !ask("bert") {
		t.Fatal("the first reply to each sender goes, the second does not")
	}
	now = now.Add(week - time.Second)
	if ask("anna") {
		t.Error("a reply within the period")
	}
	now = now.Add(time.Second)
	if !ask("anna") || ask("anna") {
		t.Error("after the period one reply goes again")
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	yes := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.VacationOnce(ctx, a.ID, "race", week); err == nil && ok {
				mu.Lock()
				yes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if yes != 1 {
		t.Errorf("%d of 16 concurrent askers got a yes", yes)
	}
	// Trimming at a time long past leaves the other tests' rows alone.
	if _, err := s.TrimVacationSent(ctx, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Error(err)
	}
}

func TestMailboxPaths(t *testing.T) {
	list := []Mailbox{{ID: 1, Name: "Inbox", Role: RoleInbox}, {ID: 2, Name: "Work"}, {ID: 3, ParentID: 2, Name: "Invoices"},
		{ID: 4, ParentID: 1, Name: "Lists"}, {ID: 5, Name: "Junk", Role: RoleJunk}}
	for path, id := range map[string]int64{"INBOX": 1, "inbox": 1, "Work": 2, "Work/Invoices": 3, "work/invoices": 3,
		"INBOX/Lists": 4, "Junk": 5, "Nope": 0, "Invoices": 0, "": 0} {
		if got := FindMailbox(list, path); got != id {
			t.Errorf("FindMailbox(%q) = %d, want %d", path, got, id)
		}
	}
}
