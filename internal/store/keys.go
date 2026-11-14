package store

import (
	"context"
	"fmt"

	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/rqlite"
)

// Master key rotation. An account's blob key is stored wrapped under a
// master key (blob.MasterKey); replacing the master key means wrapping
// every account's key again under the new one. The blob keys themselves,
// and so the blob files, stay as they are.
//
// A master key file can hold the new key next to the old one, and a server
// unwraps with whichever fits. So the order is: give every node a file
// with both, then rewrap, then take the old key out of the files. At no
// point is there a row that a running node cannot read.

// KeyUse says how many accounts have their key wrapped under each master
// key, by fingerprint; Unreadable counts those no key of the file unwraps.
type KeyUse struct {
	ByKey      map[string]int
	Unreadable int
}

// AccountKeyUse reads through the Raft log: what it says decides whether an
// old master key can be thrown away.
func (s *Store) AccountKeyUse(ctx context.Context) (KeyUse, error) {
	use := KeyUse{ByKey: map[string]int{}}
	res, err := s.db.Query(ctx, rqlite.Strong, rqlite.Stmt(`SELECT id, blob_key FROM account`))
	if err != nil {
		return use, fmt.Errorf("store: %w", err)
	}
	for _, row := range res[0].Values {
		fp, err := s.master.WrappedBy(asString(row[0]), asString(row[1]))
		if err != nil {
			use.Unreadable++
			continue
		}
		use.ByKey[fp]++
	}
	return use, nil
}

// RewrapAccountKeys wraps the key of every account under the current
// master key and returns how many it changed and how many already were.
//
// Each account is one statement that replaces the wrapped key only if it
// still is the one that was read, so it can be stopped anywhere and run
// again, and run on two nodes at once: an account is either still under
// the old key or under the new one, and both are in every node's file.
func (s *Store) RewrapAccountKeys(ctx context.Context) (changed, already int, err error) {
	res, err := s.db.Query(ctx, rqlite.Strong, rqlite.Stmt(`SELECT id, blob_key FROM account ORDER BY id`))
	if err != nil {
		return 0, 0, fmt.Errorf("store: %w", err)
	}
	for _, row := range res[0].Values {
		if err := ctx.Err(); err != nil {
			return changed, already, err
		}
		id, old := asString(row[0]), asString(row[1])
		wrapped, differs, err := s.master.Rewrap(id, old)
		if err != nil {
			return changed, already, fmt.Errorf("store: account %s: %w", id, err)
		}
		if !differs {
			already++
			continue
		}
		// Safe to send twice: the second time the row no longer has the
		// old value and nothing happens.
		if _, err := s.db.ExecuteIdempotent(ctx,
			rqlite.Stmt(`UPDATE account SET blob_key = ? WHERE id = ? AND blob_key = ?`, wrapped, id, old)); err != nil {
			return changed, already, fmt.Errorf("store: account %s: %w", id, err)
		}
		changed++
	}
	return changed, already, nil
}

// UseMasterKey replaces the master key of the store. For the commands that
// change the key file, before anything else uses the store: it is not safe
// while other calls run.
func (s *Store) UseMasterKey(m *blob.MasterKey) { s.master = m }
