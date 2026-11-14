package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = bytes.Repeat([]byte{7}, KeySize)

// verifier decrypts with testKey and compares the hash, like the store's.
func verifier(ctx context.Context, key string, r io.Reader) error {
	account, sum, err := SplitKey(key)
	if err != nil {
		return err
	}
	plain, err := Decrypt(testKey, account, sum, io.NopCloser(r))
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, plain); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return ErrCorrupt
	}
	return nil
}

// fakePeer is another node: a blob directory of its own behind the Peer
// interface, with switches for the ways a node can fail.
type fakePeer struct {
	id      string
	local   *Local
	down    atomic.Bool   // refuses everything
	slow    chan struct{} // when set, a push waits for it
	garbage atomic.Bool   // serves damaged files
	pushes  atomic.Int64
	fetches atomic.Int64
}

func (p *fakePeer) ID() string { return p.id }

func (p *fakePeer) Push(ctx context.Context, key string, size int64, file io.Reader) error {
	p.pushes.Add(1)
	if p.down.Load() {
		return errors.New("connection refused")
	}
	if p.slow != nil {
		select {
		case <-p.slow:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.local.PutVerified(ctx, key, file, verifier)
}

func (p *fakePeer) Fetch(ctx context.Context, key string) (io.ReadCloser, error) {
	p.fetches.Add(1)
	if p.down.Load() {
		return nil, errors.New("connection refused")
	}
	f, err := p.local.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if p.garbage.Load() {
		data, _ := io.ReadAll(f)
		f.Close()
		data[len(data)-1] ^= 1
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return f, nil
}

type fakePeers struct {
	list []Peer
	err  error
}

func (f *fakePeers) Peers(context.Context) ([]Peer, error) { return f.list, f.err }

// group makes a node with n-1 peers.
func group(t *testing.T, n int) (*Replicated, []*fakePeer) {
	t.Helper()
	r := &Replicated{Local: NewLocal(t.TempDir()), Verify: verifier, Pause: time.Hour}
	fp := &fakePeers{}
	var peers []*fakePeer
	for i := 1; i < n; i++ {
		p := &fakePeer{id: string(rune('a' + i)), local: NewLocal(t.TempDir())}
		peers = append(peers, p)
		fp.list = append(fp.list, p)
	}
	r.Peers = fp
	t.Cleanup(r.Wait)
	return r, peers
}

// sealed returns a blob's key and its encrypted file.
func sealed(t *testing.T, content string) (string, []byte) {
	t.Helper()
	sum := sha([]byte(content))
	enc, err := Encrypt(testKey, "acct", sum, bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(enc)
	if err != nil {
		t.Fatal(err)
	}
	return Key("acct", sum), data
}

func fileOf(t *testing.T, l *Local, key string) []byte {
	t.Helper()
	f, err := l.Get(context.Background(), key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	return data
}

// A write on three nodes ends on all three, byte for byte the same file.
func TestReplicatedPutReachesEveryNode(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 3)
	key, file := sealed(t, "hello")
	if err := r.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	for _, l := range []*Local{r.Local, peers[0].local, peers[1].local} {
		if !bytes.Equal(fileOf(t, l, key), file) {
			t.Error("a node does not have the file as it was written")
		}
	}
}

// Put returns once 2 nodes have the file and does not wait for the third.
func TestReplicatedPutAcknowledgesAtTwo(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 3)
	peers[1].slow = make(chan struct{})
	key, file := sealed(t, "two of three")
	done := make(chan error, 1)
	go func() { done <- r.Put(ctx, key, bytes.NewReader(file)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Put waited for the slow node")
	}
	if fileOf(t, peers[0].local, key) == nil || fileOf(t, peers[1].local, key) != nil {
		t.Error("expected the file on the fast peer only")
	}
	// The push to the slow node was not dropped: it finishes.
	close(peers[1].slow)
	r.Wait()
	if fileOf(t, peers[1].local, key) == nil {
		t.Error("the slow node never got the file")
	}
}

// One node down: the write still succeeds on the other two. Both peers
// down: it fails, and says so, because one copy is not what was promised.
func TestReplicatedPutWithNodesDown(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 3)
	peers[0].down.Store(true)
	key, file := sealed(t, "one down")
	if err := r.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatalf("with one of three nodes down: %v", err)
	}
	r.Wait()
	if fileOf(t, peers[1].local, key) == nil {
		t.Error("the live peer does not have the file")
	}

	peers[1].down.Store(true)
	key2, file2 := sealed(t, "two down")
	err := r.Put(ctx, key2, bytes.NewReader(file2))
	if !errors.Is(err, ErrNotReplicated) {
		t.Fatalf("with two of three nodes down: %v, want ErrNotReplicated", err)
	}
	// A node that failed is left alone for a while, but not when that
	// would make the write impossible: both were tried again.
	if peers[0].pushes.Load() != 2 || peers[1].pushes.Load() != 2 {
		t.Errorf("pushes: %d and %d, want 2 and 2", peers[0].pushes.Load(), peers[1].pushes.Load())
	}

	// With one peer back, the paused dead one is not tried for every blob.
	peers[1].down.Store(false)
	r.pause(peers[1].id, false)
	before := peers[0].pushes.Load()
	for _, c := range []string{"x1", "x2", "x3"} {
		k, f := sealed(t, c)
		if err := r.Put(ctx, k, bytes.NewReader(f)); err != nil {
			t.Fatal(err)
		}
	}
	r.Wait()
	if peers[0].pushes.Load() != before {
		t.Errorf("the dead node was tried %d more times", peers[0].pushes.Load()-before)
	}
}

// How many copies are needed follows the size of the cluster and Copies.
func TestReplicatedCopies(t *testing.T) {
	ctx := context.Background()
	key, file := sealed(t, "copies")

	// A cluster of one node: its own disk is all there is.
	r, _ := group(t, 1)
	if err := r.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Errorf("single node: %v", err)
	}
	// Two nodes: both must have it.
	r, peers := group(t, 2)
	peers[0].down.Store(true)
	if err := r.Put(ctx, key, bytes.NewReader(file)); !errors.Is(err, ErrNotReplicated) {
		t.Errorf("two nodes, one down: %v", err)
	}
	// Copies 3 of 3: one node down fails the write.
	r, peers = group(t, 3)
	r.Copies = 3
	peers[1].down.Store(true)
	if err := r.Put(ctx, key, bytes.NewReader(file)); !errors.Is(err, ErrNotReplicated) {
		t.Errorf("copies 3, one down: %v", err)
	}
	// Copies 1: acknowledged at once, still pushed to everyone.
	r, peers = group(t, 3)
	r.Copies = 1
	peers[0].down.Store(true)
	peers[1].down.Store(true)
	if err := r.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Errorf("copies 1: %v", err)
	}
	r.Wait()
	if peers[0].pushes.Load() != 1 {
		t.Error("copies 1 did not push at all")
	}
	// Not knowing the members is an error, not a cluster of one.
	r, _ = group(t, 3)
	r.Peers = &fakePeers{err: errors.New("rqlite is down")}
	if err := r.Put(ctx, key, bytes.NewReader(file)); err == nil {
		t.Error("Put succeeded without knowing the other nodes")
	}
}

// A push renews the file on the receiving node, also when that node has
// the same content already. The sweeper depends on it (Replicated.Sweep).
func TestPushRenewsTheFile(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 2)
	key, file := sealed(t, "again")
	if err := peers[0].local.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	path, _ := peers[0].local.Path(key)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(path, old, old)
	if err := r.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Errorf("the peer's copy still has its old time: %v %v", fi.ModTime(), err)
	}
}

// A read on a node without the file fetches it from a peer, checks it and
// keeps it.
func TestReplicatedGetFallsBack(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 3)
	key, file := sealed(t, "elsewhere")
	if err := peers[1].local.Put(ctx, key, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	f, err := r.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, file) {
		t.Error("the fetched file differs")
	}
	if !r.Local.Has(key) {
		t.Error("the fetched file was not kept")
	}
	n := peers[1].fetches.Load()
	if f, err = r.Get(ctx, key); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if peers[1].fetches.Load() != n {
		t.Error("a second read asked the peer again")
	}

	// Nobody has it.
	missing, _ := sealed(t, "nowhere")
	if _, err := r.Get(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob no node has: %v", err)
	}
	// Peers that cannot be asked are an error, not "not found".
	peers[0].down.Store(true)
	peers[1].down.Store(true)
	if _, err := r.Get(ctx, missing); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("with the peers down: %v", err)
	}
}

// A damaged or wrong file from a peer is never stored; the next peer is
// asked.
func TestFetchedFileIsVerified(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 3)
	key, file := sealed(t, "verify me")
	for _, p := range peers {
		p.local.Put(ctx, key, bytes.NewReader(file))
	}
	peers[0].garbage.Store(true)
	f, err := r.Get(ctx, key)
	if err != nil {
		t.Fatalf("with one bad and one good peer: %v", err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, file) || peers[1].fetches.Load() != 1 {
		t.Error("did not get the good copy from the second peer")
	}

	// Only bad copies: an error, and nothing on disk.
	r, peers = group(t, 2)
	peers[0].local.Put(ctx, key, bytes.NewReader(file))
	peers[0].garbage.Store(true)
	if _, err := r.Get(ctx, key); !errors.Is(err, ErrCorrupt) {
		t.Errorf("only a bad copy: %v, want ErrCorrupt", err)
	}
	if r.Local.Has(key) {
		t.Error("a file that failed the check is on disk")
	}
	// The same for a file sealed under another name.
	other, _ := sealed(t, "another blob")
	wrong := &fakePeer{id: "liar", local: NewLocal(t.TempDir())}
	path, _ := wrong.local.Path(other)
	os.MkdirAll(path[:len(path)-65], 0o700)
	os.WriteFile(path, file, 0o600) // the file of "verify me" under the other name
	r.Peers = &fakePeers{list: []Peer{wrong}}
	if _, err := r.Get(ctx, other); !errors.Is(err, ErrCorrupt) {
		t.Errorf("file under a wrong name: %v", err)
	}
}

// PutVerified gives a file its name only after the check.
func TestPutVerified(t *testing.T) {
	ctx := context.Background()
	l := NewLocal(t.TempDir())
	key, file := sealed(t, "checked")
	if err := l.PutVerified(ctx, key, bytes.NewReader(file[:len(file)-3]), verifier); !errors.Is(err, ErrCorrupt) {
		t.Errorf("cut file: %v", err)
	}
	if err := l.PutVerified(ctx, key, bytes.NewReader(nil), verifier); !errors.Is(err, ErrCorrupt) {
		t.Errorf("empty file: %v", err)
	}
	have, _ := l.List("acct")
	path, _ := l.Path(key)
	leftovers, _ := os.ReadDir(path[:len(path)-65])
	if l.Has(key) || len(have) != 0 || len(leftovers) != 0 {
		t.Errorf("a refused file left something behind: %v %v", have, leftovers)
	}
	if err := l.PutVerified(ctx, key, bytes.NewReader(file), verifier); err != nil {
		t.Fatal(err)
	}
	if have, _ = l.List("acct"); !l.Has(key) || len(have) != 1 {
		t.Errorf("after a good put: %v", have)
	}
}

type catalog map[string]map[string]bool

func (c catalog) Accounts(context.Context) ([]string, error) {
	var out []string
	for a := range c {
		out = append(out, a)
	}
	return out, nil
}
func (c catalog) AccountBlobs(_ context.Context, a string) (map[string]bool, error) { return c[a], nil }

// Repair brings a node with an empty disk up to date, fetches nothing it
// has, ignores what nothing refers to, and reports what no node has.
func TestRepair(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 3)
	cat := catalog{"acct": {}, "empty": {}}
	var keys []string
	for i, c := range []string{"r1", "r2", "r3", "r4"} {
		key, file := sealed(t, c)
		keys = append(keys, key)
		cat["acct"][key[len("acct/"):]] = true
		peers[i%2].local.Put(ctx, key, bytes.NewReader(file)) // spread over both peers
	}
	stray, file := sealed(t, "unreferenced")
	peers[0].local.Put(ctx, stray, bytes.NewReader(file))
	own, file := sealed(t, "have it")
	cat["acct"][own[len("acct/"):]] = true
	r.Local.Put(ctx, own, bytes.NewReader(file))

	res, err := r.Repair(ctx, cat)
	if err != nil || res != (RepairResult{Missing: 4, Fetched: 4}) {
		t.Fatalf("Repair = %+v, %v", res, err)
	}
	for _, k := range keys {
		if !r.Local.Has(k) {
			t.Errorf("%s was not fetched", k)
		}
	}
	if r.Local.Has(stray) {
		t.Error("fetched a blob nothing refers to")
	}
	if res, err = r.Repair(ctx, cat); err != nil || res != (RepairResult{}) {
		t.Errorf("second Repair = %+v, %v", res, err)
	}

	lost, _ := sealed(t, "lost everywhere")
	cat["acct"][lost[len("acct/"):]] = true
	res, err = r.Repair(ctx, cat)
	if err == nil || res != (RepairResult{Missing: 1}) {
		t.Errorf("Repair with a blob no node has = %+v, %v", res, err)
	}
}

// Many writes at once, one node failing half of the time: every
// acknowledged blob is on two nodes.
func TestReplicatedConcurrentPuts(t *testing.T) {
	ctx := context.Background()
	r, peers := group(t, 3)
	r.Pause = time.Nanosecond
	var wg sync.WaitGroup
	var mu sync.Mutex
	acked := map[string]bool{}
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%8 == 0 {
				peers[0].down.Store(i%16 == 0)
			}
			key, file := sealed(t, string(rune('A'+i))+"-concurrent")
			if err := r.Put(ctx, key, bytes.NewReader(file)); err == nil {
				mu.Lock()
				acked[key] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Counted before the background pushes are waited for: this is what an
	// acknowledgement promises at the moment it is given.
	for key := range acked {
		n := 0
		for _, l := range []*Local{r.Local, peers[0].local, peers[1].local} {
			if l.Has(key) {
				n++
			}
		}
		if n < 2 {
			t.Errorf("%s was acknowledged with %d copies", key, n)
		}
	}
	if len(acked) != 64 {
		t.Errorf("%d of 64 writes acknowledged with two nodes always up", len(acked))
	}
}
