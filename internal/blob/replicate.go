package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Peer is another node of the cluster, as far as blobs go.
type Peer interface {
	ID() string
	// Push gives the peer the encrypted file of a blob. It returns nil once
	// the peer has verified the file and has it durably on its disk.
	Push(ctx context.Context, key string, size int64, file io.Reader) error
	// Fetch asks the peer for the encrypted file. ErrNotFound if it has
	// none.
	Fetch(ctx context.Context, key string) (io.ReadCloser, error)
}

// Peers knows the other nodes.
type Peers interface {
	// Peers returns every registered node except this one, running or not.
	Peers(ctx context.Context) ([]Peer, error)
}

// ErrNotReplicated is returned by Replicated.Put when the file did not reach
// enough nodes. The local file stays: nothing refers to it, and a sweep
// removes it in time.
var ErrNotReplicated = errors.New("blob: not stored on enough nodes")

// Replicated is a Store that keeps every blob on every node of the cluster.
//
//   - Put writes the file to the local disk and pushes it, as it is, to all
//     other nodes at once. It returns when Copies nodes, this one included,
//     have it durably; the remaining pushes go on in the background. With
//     fewer nodes registered than Copies, all of them must have it.
//   - Get reads the local file. If there is none it asks the other nodes
//     one by one, verifies what it gets, keeps it and serves it.
//   - Repair fetches every blob the database refers to that this disk
//     lacks. It is what brings a new or returning node up to date, and what
//     covers a push that failed.
//
// Nodes exchange the encrypted files. A received file gets its name only
// after Verify accepted it.
//
// Fill the exported fields before use.
type Replicated struct {
	Local *Local
	Peers Peers
	// Copies is how many nodes must have a file before Put returns. [2]
	Copies int
	// Verify checks a file received from a peer. Required.
	Verify Verifier
	Logger *slog.Logger
	// PushTimeout bounds one push to one peer. [60s]
	PushTimeout time.Duration
	// Pause is how long a peer is left alone after a failed push, so a dead
	// node costs one failed connection now and then, not one per blob. [3s]
	Pause time.Duration

	mu      sync.Mutex
	paused  map[string]time.Time // peer id -> until when
	running int                  // pushes in flight
	idle    chan struct{}        // closed when running drops to 0
}

func (r *Replicated) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Replicated) copies() int {
	if r.Copies > 0 {
		return r.Copies
	}
	return 2
}

func (r *Replicated) isPaused(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Before(r.paused[id])
}

func (r *Replicated) pause(id string, on bool) {
	d := r.Pause
	if d == 0 {
		d = 3 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused == nil {
		r.paused = map[string]time.Time{}
	}
	if on {
		r.paused[id] = time.Now().Add(d)
	} else {
		delete(r.paused, id)
	}
}

// Put stores the file here and on the other nodes. See Replicated.
func (r *Replicated) Put(ctx context.Context, key string, src io.Reader) error {
	if err := r.Local.Put(ctx, key, src); err != nil {
		return err
	}
	peers, err := r.Peers.Peers(ctx)
	if err != nil {
		return fmt.Errorf("blob: looking up the other nodes: %w", err)
	}
	need := min(r.copies(), len(peers)+1) - 1 // copies wanted on other nodes
	if len(peers) == 0 {
		return nil
	}
	// A peer that just failed is left out at first.
	var targets, resting []Peer
	for _, p := range peers {
		if r.isPaused(p.ID()) {
			resting = append(resting, p)
		} else {
			targets = append(targets, p)
		}
	}

	// The pushes outlive this call: the ones still running when enough
	// nodes have the file must finish, or the last node would only ever
	// get blobs through Repair.
	timeout := r.PushTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	results := make(chan error, len(peers))
	var pushes sync.WaitGroup
	start := func(list []Peer) {
		pushes.Add(len(list))
		r.track(len(list))
		for _, p := range list {
			go func() {
				defer r.track(-1)
				defer pushes.Done()
				err := r.push(pctx, p, key)
				r.pause(p.ID(), err != nil)
				if err != nil {
					r.log().Warn("blob push failed", "peer", p.ID(), "blob", key, "err", err)
				}
				results <- err
			}()
		}
	}
	start(targets)
	defer func() {
		go func() {
			pushes.Wait()
			cancel()
		}()
	}()

	have, left := 0, len(targets)
	var last error
	for have < need {
		if left == 0 {
			// Not enough: now the peers that were left out get a try too.
			if len(resting) == 0 {
				break
			}
			start(resting)
			left, resting = len(resting), nil
		}
		select {
		case err := <-results:
			left--
			if err == nil {
				have++
			} else {
				last = err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if have < need {
		return fmt.Errorf("%w: %d of %d, last error: %w", ErrNotReplicated, have+1, need+1, last)
	}
	return nil
}

// track counts the pushes that are running, for Wait.
func (r *Replicated) track(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running += n
	if r.running == 0 && r.idle != nil {
		close(r.idle)
		r.idle = nil
	}
}

// Wait returns when no push is running in the background any more. For
// shutdown and for tests.
func (r *Replicated) Wait() {
	r.mu.Lock()
	if r.running == 0 {
		r.mu.Unlock()
		return
	}
	if r.idle == nil {
		r.idle = make(chan struct{})
	}
	idle := r.idle
	r.mu.Unlock()
	<-idle
}

func (r *Replicated) push(ctx context.Context, p Peer, key string) error {
	f, err := r.Local.Get(ctx, key)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.(*os.File).Stat()
	if err != nil {
		return err
	}
	return p.Push(ctx, key, fi.Size(), f)
}

// Get opens the local file, after fetching it from another node if this
// one does not have it.
func (r *Replicated) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f, err := r.Local.Get(ctx, key)
	if !errors.Is(err, ErrNotFound) {
		return f, err
	}
	if err := r.fetch(ctx, key); err != nil {
		return nil, err
	}
	return r.Local.Get(ctx, key)
}

// fetch gets the file from the first peer that has a good copy.
func (r *Replicated) fetch(ctx context.Context, key string) error {
	peers, err := r.Peers.Peers(ctx)
	if err != nil {
		return fmt.Errorf("blob: looking up the other nodes: %w", err)
	}
	var failed error
	for _, p := range peers {
		body, err := p.Fetch(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err == nil {
			err = r.Local.PutVerified(ctx, key, body, r.Verify)
			body.Close()
		}
		if err == nil {
			r.log().Info("blob fetched", "peer", p.ID(), "blob", key)
			return nil
		}
		r.log().Warn("blob fetch failed", "peer", p.ID(), "blob", key, "err", err)
		failed = err
		if ctx.Err() != nil {
			break
		}
	}
	if failed != nil {
		return fmt.Errorf("blob: fetching %s: %w", key, failed)
	}
	return ErrNotFound
}

func (r *Replicated) Delete(ctx context.Context, key string) error { return r.Local.Delete(ctx, key) }

// Sweep sweeps this node's disk. Every node does that for itself, and none
// needs to know what another is doing. What keeps a sweep here from
// harming a delivery on another node:
//
// A delivery there Puts the blob before it writes the row. That Put
// rewrites the file on the delivering node and on every node the push
// reaches, also where an old copy of the same content lay, so those copies
// are young and no sweep touches them for a day. Put only returns, and the
// row is only written, once Copies nodes hold such a young copy. A node
// the push did not reach may still hold an old copy and may sweep it in
// that very moment: it then has no copy, which is the same as never having
// had one, and gets the blob back through Get or Repair from the nodes
// that were acknowledged.
func (r *Replicated) Sweep(ctx context.Context, before time.Time, refs References) (SweepResult, error) {
	return r.Local.Sweep(ctx, before, refs)
}

// Catalog lists what the database refers to, for Repair.
type Catalog interface {
	// Accounts returns the ids of all accounts.
	Accounts(ctx context.Context) ([]string, error)
	// AccountBlobs returns the hashes of all blobs the account refers to.
	AccountBlobs(ctx context.Context, account string) (map[string]bool, error)
}

// RepairResult counts what a Repair did.
type RepairResult struct {
	Missing int // referenced blobs this disk lacked
	Fetched int // of those, fetched from another node
}

// Repair compares what the database refers to with what is on this disk
// and fetches what is missing: the anti-entropy pass. A blob no reachable
// node has is reported in the error after the rest was done.
func (r *Replicated) Repair(ctx context.Context, cat Catalog) (RepairResult, error) {
	var res RepairResult
	accounts, err := cat.Accounts(ctx)
	if err != nil {
		return res, err
	}
	var failed error
	for _, account := range accounts {
		want, err := cat.AccountBlobs(ctx, account)
		if err != nil {
			return res, err
		}
		if len(want) == 0 {
			continue
		}
		have, err := r.Local.List(account)
		if err != nil {
			return res, err
		}
		for sum := range want {
			if have[sum] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			res.Missing++
			if err := r.fetch(ctx, Key(account, sum)); err != nil {
				failed = fmt.Errorf("blob %s: %w", Key(account, sum), err)
				continue
			}
			res.Fetched++
		}
	}
	if failed != nil {
		return res, fmt.Errorf("blob: %d of %d missing blobs could not be fetched, last: %w", res.Missing-res.Fetched, res.Missing, failed)
	}
	return res, nil
}
