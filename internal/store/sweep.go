package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/rqlite"
)

// BlobGrace is how old an unreferenced blob file must be before a sweep
// removes it. A delivery writes the file before the row that refers to it;
// this is far longer than that ever takes.
const BlobGrace = 24 * time.Hour

// ErrNoSweep is returned by SweepBlobs when the blob store cannot be swept.
var ErrNoSweep = errors.New("store: this blob store cannot be swept")

// SweepBlobs removes the blob files on this node that no email refers to
// and that were written before the given time. Email/set destroy and
// DestroyEmail leave the file, since another email of the account may
// share it; this is what removes it in the end. See blob.Local.Sweep for
// why a concurrent delivery of the same content is safe.
//
// Every node of a cluster runs it for its own disk.
func (s *Store) SweepBlobs(ctx context.Context, before time.Time) (blob.SweepResult, error) {
	sw, ok := s.blobs.(blob.Sweeper)
	if !ok {
		return blob.SweepResult{}, ErrNoSweep
	}
	res, err := sw.Sweep(ctx, before, blobRefs{s})
	if err != nil {
		return res, fmt.Errorf("store: sweeping blobs: %w", err)
	}
	return res, nil
}

type blobRefs struct{ s *Store }

func (r blobRefs) AccountBlobs(ctx context.Context, account string) (map[string]bool, error) {
	res, err := r.s.query(ctx, account, q(`SELECT DISTINCT blob FROM email WHERE account_id = :a`))
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(res[0].Values))
	for _, row := range res[0].Values {
		out[asString(row[0])] = true
	}
	return out, nil
}

// Referenced reads through the Raft log: the answer decides whether a file
// is deleted, so it must not come from a node that only thinks it leads.
func (r blobRefs) Referenced(ctx context.Context, account, sum string) (bool, error) {
	res, err := r.s.db.Query(ctx, rqlite.Strong,
		rqlite.Stmt(`SELECT 1 FROM email WHERE account_id = ? AND blob = ? LIMIT 1`, account, sum))
	if err != nil {
		return false, err
	}
	return len(res[0].Values) > 0, nil
}
