package store

import (
	"bytes"
	gocontext "context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/rqlite"
)

func randomID(prefix string) string {
	var r [6]byte
	rand.Read(r[:])
	return prefix + hex.EncodeToString(r[:])
}

func TestNodeTable(t *testing.T) {
	s := open(t)
	id := randomID("t")
	t.Cleanup(func() { s.RemoveNode(ctx, id) })
	find := func() (Node, bool) {
		nodes, err := s.Nodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if n.ID == id {
				return n, true
			}
		}
		return Node{}, false
	}

	// A command registers: known, but never seen running.
	if err := s.RegisterNode(ctx, Node{ID: id, Address: "10.0.0.1:7443", Cert: "aa"}, false); err != nil {
		t.Fatal(err)
	}
	n, ok := find()
	want(t, "after a command registered", []any{ok, n.Address, n.Cert, n.SeenAt.Unix(), n.StartedAt.Unix()},
		[]any{true, "10.0.0.1:7443", "aa", int64(0), int64(0)})

	// The server registers: started and seen now. Twice gives the same row.
	now := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if err := s.RegisterNode(ctx, Node{ID: id, Address: "10.0.0.2:7443", Cert: "bb"}, true); err != nil {
			t.Fatal(err)
		}
	}
	n, _ = find()
	want(t, "after the server registered", []any{n.Address, n.Cert, n.SeenAt.Unix(), n.StartedAt.Unix()},
		[]any{"10.0.0.2:7443", "bb", now.Unix(), now.Unix()})
	// A command on the same node afterwards leaves the times alone.
	s.RegisterNode(ctx, Node{ID: id, Address: "10.0.0.2:7443", Cert: "bb"}, false)
	n, _ = find()
	want(t, "seen_at after a command", n.SeenAt.Unix(), now.Unix())

	now = now.Add(5 * time.Second)
	alive, err := s.Heartbeat(ctx, id, "bb")
	n, _ = find()
	want(t, "heartbeat", []any{alive, err, n.SeenAt.Unix(), n.StartedAt.Unix()}, []any{true, nil, now.Unix(), now.Unix() - 5})
	// Another process under the same id took the row.
	alive, err = s.Heartbeat(ctx, id, "aa")
	want(t, "heartbeat with another certificate", []any{alive, err}, []any{false, nil})

	removed, err := s.RemoveNode(ctx, id)
	want(t, "RemoveNode", []any{removed, err}, []any{true, nil})
	removed, _ = s.RemoveNode(ctx, id)
	want(t, "RemoveNode again", removed, false)
	alive, _ = s.Heartbeat(ctx, id, "bb")
	_, ok = find()
	want(t, "after removal", []any{alive, ok}, []any{false, false})
}

// The failure counters all nodes share: a window that starts at the first
// failure, counts per key, and ends by itself.
func TestLoginFailures(t *testing.T) {
	s := open(t)
	addr, ip := randomID("a:"), randomID("i:")
	t.Cleanup(func() { s.ClearLoginFailures(ctx, addr, ip) })
	const window = 15 * time.Minute
	now := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return now }
	read := func() map[string]LoginFailure {
		t.Helper()
		m, err := s.LoginFailures(ctx, window, addr, ip, randomID("a:"))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	want(t, "no failures", len(read()), 0)

	for i := 0; i < 3; i++ {
		wantErr(t, "AddLoginFailure", s.AddLoginFailure(ctx, window, addr, ip), nil)
		now = now.Add(time.Minute)
	}
	wantErr(t, "AddLoginFailure", s.AddLoginFailure(ctx, window, ip), nil)
	start := time.Unix(1_800_000_000, 0)
	want(t, "counts", read(), map[string]LoginFailure{addr: {Count: 3, Started: start}, ip: {Count: 4, Started: start}})

	// Many nodes counting at once lose nothing.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.AddLoginFailure(ctx, window, addr) }()
	}
	wg.Wait()
	want(t, "after 20 concurrent failures", read()[addr].Count, 23)

	// A success clears the address, not the IP.
	wantErr(t, "ClearLoginFailures", s.ClearLoginFailures(ctx, addr), nil)
	m := read()
	_, hasAddr := m[addr]
	want(t, "after clearing the address", []any{hasAddr, m[ip].Count}, []any{false, 4})

	// The window ends 15 minutes after its first failure, and the next
	// failure starts a new one at 1.
	now = start.Add(window)
	want(t, "after the window", len(read()), 0)
	wantErr(t, "AddLoginFailure", s.AddLoginFailure(ctx, window, ip), nil)
	want(t, "new window", read(), map[string]LoginFailure{ip: {Count: 1, Started: now}})

	// Trimming removes ended windows only. The table is shared with every
	// other test that runs against this database, and with their servers,
	// so the trim here is at a time long past: a trim at this test's clock,
	// which is ahead of the real one, once removed the live counters of
	// the two-node login test in cmd/robinmail, and its node B then let
	// the eleventh login in.
	old, past := randomID("a:"), time.Unix(1_000_000_000, 0)
	t.Cleanup(func() { s.ClearLoginFailures(ctx, old) })
	future := now
	now = past
	wantErr(t, "AddLoginFailure", s.AddLoginFailure(ctx, window, old), nil)
	_, err := s.TrimLoginFailures(ctx, past.Add(-time.Second))
	wantErr(t, "TrimLoginFailures", err, nil)
	m, err = s.LoginFailures(ctx, window, old)
	want(t, "a window that started after the cut stays", []any{m[old].Count, err}, []any{1, nil})
	_, err = s.TrimLoginFailures(ctx, past)
	wantErr(t, "TrimLoginFailures", err, nil)
	m, err = s.LoginFailures(ctx, window, old)
	want(t, "trimmed", []any{len(m), err}, []any{0, nil})
	now = future
	want(t, "a later window is not trimmed", read(), map[string]LoginFailure{ip: {Count: 1, Started: now}})
}

// VerifyBlob accepts the file a delivery wrote and nothing else.
func TestVerifyBlob(t *testing.T) {
	s := open(t)
	a, b := account(t, s), account(t, s)
	e := deliver(t, s, a, Message{Raw: msg("v1@x", "verify")})
	file, err := os.ReadFile(blobFile(t, s, a.ID, e.Blob))
	if err != nil {
		t.Fatal(err)
	}
	key := blob.Key(a.ID, e.Blob)
	wantErr(t, "the real file", s.VerifyBlob(ctx, key, bytes.NewReader(file)), nil)

	damaged := bytes.Clone(file)
	damaged[len(damaged)/2] ^= 0x80
	other := deliver(t, s, a, Message{Raw: msg("v2@x", "other")})
	for name, c := range map[string]struct {
		key  string
		file []byte
		want error
	}{
		"damaged":                {key, damaged, blob.ErrCorrupt},
		"cut short":              {key, file[:len(file)-1], blob.ErrCorrupt},
		"empty":                  {key, nil, blob.ErrCorrupt},
		"under another hash":     {blob.Key(a.ID, other.Blob), file, blob.ErrCorrupt},
		"under another account":  {blob.Key(b.ID, e.Blob), file, blob.ErrCorrupt},
		"for an unknown account": {blob.Key("nosuchaccount", e.Blob), file, ErrNoAccount},
	} {
		if err := s.VerifyBlob(ctx, c.key, bytes.NewReader(c.file)); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if err := s.VerifyBlob(ctx, "not a key", bytes.NewReader(file)); err == nil {
		t.Error("accepted an invalid key")
	}

	// Sealed with the right key under a name that is not its hash: every
	// chunk authenticates, only the hash comparison catches it.
	k, _ := s.blobKey(ctx, a.ID)
	lie := sha256.Sum256([]byte("what the name promises"))
	name := hex.EncodeToString(lie[:])
	enc, _ := blob.Encrypt(k, a.ID, name, bytes.NewReader([]byte("something else")))
	forged, _ := io.ReadAll(enc)
	wantErr(t, "content that does not match its name", s.VerifyBlob(ctx, blob.Key(a.ID, name), bytes.NewReader(forged)), blob.ErrCorrupt)
}

// link is the other stores of a test cluster seen as blob peers, without a
// network in between: what matters here is the store logic.
type link struct {
	id    string
	local *blob.Local
	store *Store
	down  bool
}

func (l *link) ID() string { return l.id }
func (l *link) Push(_ gocontext.Context, key string, _ int64, f io.Reader) error {
	if l.down {
		return errors.New("node is down")
	}
	return l.local.PutVerified(ctx, key, f, l.store.VerifyBlob)
}
func (l *link) Fetch(_ gocontext.Context, key string) (io.ReadCloser, error) {
	if l.down {
		return nil, errors.New("node is down")
	}
	return l.local.Get(ctx, key)
}

type links []*link

func (ls links) Peers(gocontext.Context) ([]blob.Peer, error) {
	out := make([]blob.Peer, len(ls))
	for i, l := range ls {
		out[i] = l
	}
	return out, nil
}

// cluster3 is three stores on one rqlite, each with a blob directory of its
// own, replicating to the other two.
func cluster3(t *testing.T) (stores [3]*Store, ls [3]*link) {
	t.Helper()
	base := os.Getenv("ROBINMAIL_TEST_RQLITE")
	if base == "" {
		t.Skip("ROBINMAIL_TEST_RQLITE is not set")
	}
	open(t) // schema and domain
	master, _ := blob.NewMasterKey(testMasterKey)
	var reps [3]*blob.Replicated
	for i := range stores {
		db, err := rqlite.New(base, nil)
		if err != nil {
			t.Fatal(err)
		}
		local := blob.NewLocal(t.TempDir())
		reps[i] = &blob.Replicated{Local: local, Pause: time.Millisecond}
		stores[i] = New(db, reps[i], master)
		reps[i].Verify = stores[i].VerifyBlob
		ls[i] = &link{id: fmt.Sprint("n", i), local: local, store: stores[i]}
		locals[stores[i]] = local
	}
	for i := range stores {
		reps[i].Peers = links{ls[(i+1)%3], ls[(i+2)%3]}
	}
	t.Cleanup(func() {
		for i, s := range stores {
			reps[i].Wait()
			delete(locals, s)
		}
	})
	return stores, ls
}

func settle(s *Store) { s.blobs.(*blob.Replicated).Wait() }

// A delivery on one node of three: the blob is on two nodes when Deliver
// returns, any node can read the message, a node that was away reads it
// by fetching the blob, and a wiped node is filled again by RepairBlobs.
func TestClusterDelivery(t *testing.T) {
	st, ls := cluster3(t)
	a := account(t, st[0])

	e := deliver(t, st[0], a, Message{Raw: msg("c1@x", "to all"), Key: "c1"})
	settle(st[0])
	for i, s := range st {
		if !exists(blobFile(t, s, a.ID, e.Blob)) {
			t.Errorf("node %d lacks the blob", i)
		}
		raw, _, err := s.Raw(ctx, a.ID, e.ID)
		if err != nil || !bytes.Equal(raw, msg("c1@x", "to all")) {
			t.Errorf("node %d cannot read the message: %v", i, err)
		}
	}

	// Node 2 is away for a delivery on node 1.
	ls[2].down = true
	e2 := deliver(t, st[1], a, Message{Raw: msg("c2@x", "while away"), Key: "c2"})
	settle(st[1])
	want(t, "blob on the nodes", []bool{exists(blobFile(t, st[0], a.ID, e2.Blob)), exists(blobFile(t, st[1], a.ID, e2.Blob)),
		exists(blobFile(t, st[2], a.ID, e2.Blob))}, []bool{true, true, false})
	// With both others away nothing is acknowledged, and nothing stored.
	ls[0].down = true
	_, err := st[1].Deliver(ctx, a.ID, Message{Raw: msg("c3@x", "alone"), Key: "c3"})
	if !errors.Is(err, blob.ErrNotReplicated) {
		t.Fatalf("delivery with one node left: %v", err)
	}
	ids, _ := st[1].EmailIDs(ctx, a.ID)
	want(t, "emails after the refused delivery", len(ids), 2)
	ls[0].down, ls[2].down = false, false
	// The sender's retry of that message is a first delivery, not a duplicate.
	deliver(t, st[1], a, Message{Raw: msg("c3@x", "alone"), Key: "c3"})

	// Node 2 is back and is asked for the message it never got.
	raw, _, err := st[2].Raw(ctx, a.ID, e2.ID)
	if err != nil || !bytes.Equal(raw, msg("c2@x", "while away")) {
		t.Fatalf("read through the node that missed the blob: %v", err)
	}
	if !exists(blobFile(t, st[2], a.ID, e2.Blob)) {
		t.Error("the fetched blob was not kept")
	}

	// Node 0 loses its disk.
	settle(st[1])
	for _, id := range []Email{e, e2} {
		os.Remove(blobFile(t, st[0], a.ID, id.Blob))
	}
	// The test database holds other tests' accounts, whose blobs are in
	// directories long gone: those stay missing and are reported. Ours
	// must come back.
	res, err := st[0].RepairBlobs(ctx)
	if res.Fetched != 2 {
		t.Fatalf("RepairBlobs = %+v, %v", res, err)
	}
	for _, id := range []int64{e.ID, e2.ID} {
		if err := st[0].VerifyEmail(ctx, a.ID, id); err != nil {
			t.Error(err)
		}
	}
	res, _ = st[0].RepairBlobs(ctx)
	want(t, "fetched by a second repair", res.Fetched, 0)

	// A store without other nodes says so.
	_, err = open(t).RepairBlobs(ctx)
	wantErr(t, "RepairBlobs on a single node", err, ErrNoRepair)
}

// Every node sweeps its own disk. A blob that lies around unreferenced and
// old on all nodes is delivered again on node 0 while nodes 1 and 2 sweep:
// whatever the timing, the acknowledged message is readable everywhere
// afterwards, and no sweep removed a copy the delivery had just renewed.
func TestClusterSweepDuringDelivery(t *testing.T) {
	st, ls := cluster3(t)
	a := account(t, st[0])
	for round := 0; round < 15; round++ {
		raw := msg(fmt.Sprintf("s%d@x", round), "comes back")
		first := deliver(t, st[0], a, Message{Raw: raw})
		settle(st[0])
		if _, err := st[0].DestroyEmail(ctx, a.ID, first.ID); err != nil {
			t.Fatal(err)
		}
		for _, s := range st {
			age(t, blobFile(t, s, a.ID, first.Blob), 48*time.Hour)
		}
		// Odd rounds: the push to node 2 fails, so node 2 keeps its old
		// copy and its sweep is free to take it.
		ls[2].down = round%2 == 1

		var wg sync.WaitGroup
		var again Email
		var derr error
		wg.Add(3)
		go func() { defer wg.Done(); again, derr = st[0].Deliver(ctx, a.ID, Message{Raw: raw}) }()
		for _, s := range st[1:] {
			go func() {
				defer wg.Done()
				if _, err := s.SweepBlobs(ctx, time.Now().Add(-BlobGrace)); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if derr != nil {
			t.Fatal(derr)
		}
		// The promise of the acknowledgement: two nodes hold it right now.
		copies := 0
		for _, s := range st[:2] {
			if exists(blobFile(t, s, a.ID, again.Blob)) {
				copies++
			}
		}
		want(t, fmt.Sprintf("round %d: copies on the nodes that acknowledged", round), copies, 2)
		settle(st[0])
		ls[2].down = false
		for i, s := range st {
			if err := s.VerifyEmail(ctx, a.ID, again.ID); err != nil {
				t.Errorf("round %d, node %d: %v", round, i, err)
			}
		}
		// And later sweeps leave it alone on every node.
		for i, s := range st {
			age(t, blobFile(t, s, a.ID, again.Blob), 48*time.Hour)
			if res, err := s.SweepBlobs(ctx, time.Now().Add(-BlobGrace)); err != nil || res.Removed != 0 {
				t.Errorf("round %d, node %d: sweep of a referenced blob = %+v, %v", round, i, res, err)
			}
		}
	}
}
