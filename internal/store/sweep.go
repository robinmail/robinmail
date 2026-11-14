package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// SweepBlobs removes the blob files on this node that no email, upload, queued
// submission or Sieve script refers to
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
	res, err := r.s.query(ctx, account, q(`SELECT blob FROM email WHERE account_id = :a
		UNION SELECT blob FROM upload WHERE account_id = :a
		UNION SELECT blob FROM submission WHERE account_id = :a AND blob <> ''
		UNION SELECT blob FROM sieve_script WHERE account_id = :a`))
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
		rqlite.Stmt(`SELECT 1 FROM email WHERE account_id = ? AND blob = ?
			UNION ALL SELECT 1 FROM upload WHERE account_id = ? AND blob = ?
			UNION ALL SELECT 1 FROM submission WHERE account_id = ? AND blob = ?
			UNION ALL SELECT 1 FROM sieve_script WHERE account_id = ? AND blob = ? LIMIT 1`,
			account, sum, account, sum, account, sum, account, sum))
	if err != nil {
		return false, err
	}
	return len(res[0].Values) > 0, nil
}

// VerifyBlob is the blob.Verifier of a cluster: it decides whether an
// encrypted file that another node sent is the blob the key names. It
// decrypts the file with the account's key and compares the SHA-256 of the
// plaintext with the name.
//
// Decrypting already proves a lot: every chunk is authenticated with the
// account's key and with the account id and hash as associated data, so
// only a node that holds the master key can have made a file that gets
// this far. The hash on top catches a sender that sealed the wrong
// content under a name. Every node has the master key anyway (it serves
// every account), so a check that needs no key would buy nothing.
func (s *Store) VerifyBlob(ctx context.Context, key string, r io.Reader) error {
	account, sum, err := blob.SplitKey(key)
	if err != nil {
		return err
	}
	k, err := s.blobKey(ctx, account)
	if err != nil {
		return err
	}
	plain, err := blob.Decrypt(k, account, sum, io.NopCloser(r))
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, plain); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return blob.ErrCorrupt
	}
	return nil
}

// ErrNoRepair is returned by RepairBlobs when the blob store is not a
// replicated one: there is nobody to fetch from.
var ErrNoRepair = errors.New("store: this blob store has no other nodes to repair from")

// repairer is what blob.Replicated has.
type repairer interface {
	Repair(ctx context.Context, cat blob.Catalog) (blob.RepairResult, error)
}

// RepairBlobs fetches, from the other nodes, every blob an email refers to
// that this node's disk lacks. Every node runs it at start and from time
// to time.
func (s *Store) RepairBlobs(ctx context.Context) (blob.RepairResult, error) {
	rp, ok := s.blobs.(repairer)
	if !ok {
		return blob.RepairResult{}, ErrNoRepair
	}
	res, err := rp.Repair(ctx, blobRefs{s})
	if err != nil {
		return res, fmt.Errorf("store: repairing blobs: %w", err)
	}
	return res, nil
}

// Accounts lists the account ids, for blob.Catalog.
func (r blobRefs) Accounts(ctx context.Context) ([]string, error) {
	res, err := r.s.query(ctx, "", q(`SELECT id FROM account ORDER BY id`))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, asString(row[0]))
	}
	return out, nil
}

// ScrubBlobs reads every blob file on this node that the database refers
// to and checks it against its name (VerifyBlob: it decrypts and hashes).
// In a cluster a file that fails is replaced by a good copy from another
// node; on a single node it is reported. Every node runs it for its own
// disk, from time to time.
func (s *Store) ScrubBlobs(ctx context.Context) (blob.ScrubResult, error) {
	var res blob.ScrubResult
	var err error
	switch b := s.blobs.(type) {
	case *blob.Replicated:
		res, err = b.Scrub(ctx, blobRefs{s})
	case *blob.Local:
		res, err = b.Scrub(ctx, blobRefs{s}, s.VerifyBlob, nil)
	default:
		return res, errors.New("store: this blob store cannot be scrubbed")
	}
	if err != nil {
		return res, fmt.Errorf("store: scrubbing blobs: %w", err)
	}
	return res, nil
}
