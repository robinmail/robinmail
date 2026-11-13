package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mikepage/robinmail/internal/blob"
)

// blobFile is where the local blob store keeps a blob.
func blobFile(t *testing.T, s *Store, account, sum string) string {
	t.Helper()
	path, err := locals[s].Path(blob.Key(account, sum))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestSweepBlobs(t *testing.T) {
	s := open(t)
	a := account(t, s)
	keep := deliver(t, s, a, Message{Raw: msg("w1@x", "keep")})
	gone := deliver(t, s, a, Message{Raw: msg("w2@x", "gone")})
	shared1 := deliver(t, s, a, Message{Raw: msg("w3@x", "shared")})
	shared2 := deliver(t, s, a, Message{Raw: msg("w3@x", "shared")}) // the same content twice
	fresh := deliver(t, s, a, Message{Raw: msg("w4@x", "fresh")})
	want(t, "shared blob", shared1.Blob, shared2.Blob)
	for _, e := range []Email{gone, shared1, fresh} {
		if _, err := s.DestroyEmail(ctx, a.ID, e.ID); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{}
	for name, e := range map[string]Email{"keep": keep, "gone": gone, "shared": shared2, "fresh": fresh} {
		files[name] = blobFile(t, s, a.ID, e.Blob)
	}
	// Everything but "fresh" is two days old.
	for _, name := range []string{"keep", "gone", "shared"} {
		age(t, files[name], 48*time.Hour)
	}
	dir := filepath.Dir(files["gone"])
	oldTmp, newTmp := filepath.Join(dir, ".tmp-old"), filepath.Join(dir, ".tmp-new")
	os.WriteFile(oldTmp, []byte("x"), 0o600)
	os.WriteFile(newTmp, []byte("x"), 0o600)
	age(t, oldTmp, 48*time.Hour)

	res, err := s.SweepBlobs(ctx, time.Now().Add(-BlobGrace))
	if err != nil {
		t.Fatal(err)
	}
	want(t, "result", res, blob.SweepResult{Removed: 1, Temp: 1})
	want(t, "files left", map[string]bool{"keep": exists(files["keep"]), "gone": exists(files["gone"]),
		"shared": exists(files["shared"]), "fresh": exists(files["fresh"]), "old tmp": exists(oldTmp), "new tmp": exists(newTmp)},
		map[string]bool{"keep": true, "gone": false, "shared": true, "fresh": true, "old tmp": false, "new tmp": true})
	if err := s.VerifyEmail(ctx, a.ID, keep.ID); err != nil {
		t.Error(err)
	}
	if err := s.VerifyEmail(ctx, a.ID, shared2.ID); err != nil {
		t.Error(err)
	}

	// A claim that a dead sweep left behind is settled by the next one:
	// put back when the blob is referenced, removed when it is not.
	claimed := filepath.Join(filepath.Dir(files["keep"]), ".sweep-"+keep.Blob)
	if err := os.Rename(files["keep"], claimed); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, ".sweep-"+gone.Blob)
	os.WriteFile(orphan, []byte("x"), 0o600)
	age(t, orphan, 48*time.Hour)
	res, err = s.SweepBlobs(ctx, time.Now().Add(-BlobGrace))
	if err != nil {
		t.Fatal(err)
	}
	want(t, "second result", res, blob.SweepResult{Removed: 1, Restored: 1})
	if !exists(files["keep"]) || exists(claimed) || exists(orphan) {
		t.Error("leftover claims were not settled")
	}
	if err := s.VerifyEmail(ctx, a.ID, keep.ID); err != nil {
		t.Error(err)
	}
}

// The race the sweeper has to survive: a message is destroyed, its blob is
// old enough to go, and the same content is delivered again while the
// sweep runs. Whatever the interleaving, an email that was acknowledged
// must have its blob afterwards.
func TestSweepAgainstDelivery(t *testing.T) {
	s := open(t)
	a := account(t, s)
	const rounds, messages = 30, 8
	raws := make([][]byte, messages)
	for i := range raws {
		raws[i] = msg(fmt.Sprintf("race%d@x", i), fmt.Sprintf("race %d", i))
	}
	for round := 0; round < rounds; round++ {
		// Old, unreferenced blobs of all messages.
		for _, raw := range raws {
			e := deliver(t, s, a, Message{Raw: raw})
			if _, err := s.DestroyEmail(ctx, a.ID, e.ID); err != nil {
				t.Fatal(err)
			}
			age(t, blobFile(t, s, a.ID, e.Blob), 48*time.Hour)
		}
		var wg sync.WaitGroup
		delivered := make([]Email, messages)
		errs := make([]error, messages)
		for i, raw := range raws {
			wg.Add(1)
			go func() {
				defer wg.Done()
				delivered[i], errs[i] = s.Deliver(ctx, a.ID, Message{Raw: raw})
			}()
		}
		wg.Add(2)
		for i := 0; i < 2; i++ { // two sweeps at once, as two nodes' loops could
			go func() {
				defer wg.Done()
				if _, err := s.SweepBlobs(ctx, time.Now().Add(-BlobGrace)); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		for i, e := range delivered {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if err := s.VerifyEmail(ctx, a.ID, e.ID); err != nil {
				t.Fatalf("round %d: delivered email lost its blob: %v", round, err)
			}
			if _, err := s.DestroyEmail(ctx, a.ID, e.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// And the claim itself: a file that is renewed after the first look
	// but before the claim is put back, not removed.
	e := deliver(t, s, a, Message{Raw: raws[0]})
	path := blobFile(t, s, a.ID, e.Blob)
	s.DestroyEmail(ctx, a.ID, e.ID)
	age(t, path, 48*time.Hour)
	refs := &racingRefs{blobRefs: blobRefs{s}, during: func() {
		// Between the sweep's two looks: the same content arrives again.
		if _, err := s.Deliver(ctx, a.ID, Message{Raw: raws[0]}); err != nil {
			t.Error(err)
		}
	}}
	res, err := locals[s].Sweep(ctx, time.Now().Add(-BlobGrace), refs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 || !exists(path) {
		t.Errorf("a blob delivered during the sweep was removed: %+v", res)
	}
	ids, _ := s.EmailIDs(ctx, a.ID)
	if err := s.VerifyEmail(ctx, a.ID, ids[len(ids)-1]); err != nil {
		t.Error(err)
	}
}

// racingRefs runs during once, right after the sweep took its first look
// at the account, which found the blob unreferenced.
type racingRefs struct {
	blobRefs
	during func()
	once   sync.Once
}

func (r *racingRefs) AccountBlobs(c context.Context, account string) (map[string]bool, error) {
	m, err := r.blobRefs.AccountBlobs(c, account)
	r.once.Do(r.during)
	return m, err
}
