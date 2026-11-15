package raftdb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
	"google.golang.org/protobuf/proto"
)

// logStore keeps what Raft must find again after a restart: the entries,
// the HardState (term, vote, commit), the metadata of the last snapshot,
// and this node's own facts (its Raft id, the members it learnt from
// "robinmail node join"). It is a SQLite file of its own, raft.db, written
// with synchronous=FULL: Raft may only send what it has made durable.
//
// In memory the entries are in a raft.MemoryStorage, which is what Raft
// reads; the file is how they survive.
type logStore struct {
	mu sync.Mutex
	db *sql.DB
}

const logSchema = `
CREATE TABLE IF NOT EXISTS entry (idx INTEGER PRIMARY KEY, data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS state (k TEXT PRIMARY KEY, v BLOB NOT NULL);`

func openLog(path string) (*logStore, error) {
	d, err := openSQLite(path, "FULL", false, 1)
	if err != nil {
		return nil, fmt.Errorf("raft log %s: %w", path, err)
	}
	if _, err := d.Exec(logSchema); err != nil {
		d.Close()
		return nil, fmt.Errorf("raft log %s: %w", path, err)
	}
	return &logStore{db: d}, nil
}

func (l *logStore) close() error { return l.db.Close() }

func (l *logStore) get(k string) ([]byte, error) {
	var v []byte
	err := l.db.QueryRow(`SELECT v FROM state WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

func (l *logStore) set(k string, v []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.db.Exec(`INSERT INTO state (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

func (l *logStore) del(k string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.db.Exec(`DELETE FROM state WHERE k = ?`, k)
	return err
}

// getJSON and setJSON keep a value as JSON.
func (l *logStore) getJSON(k string, v any) (bool, error) {
	b, err := l.get(k)
	if err != nil || b == nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (l *logStore) setJSON(k string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return l.set(k, b)
}

// empty reports whether Raft never wrote anything here.
func (l *logStore) empty() (bool, error) {
	var n int
	err := l.db.QueryRow(`SELECT (SELECT count(*) FROM entry) + (SELECT count(*) FROM state WHERE k IN ('hard', 'snap'))`).Scan(&n)
	return n == 0, err
}

// save makes one Ready durable, in one transaction: a snapshot (and the
// entries it covers go), new entries (and those they replace go), the
// HardState.
func (l *logStore) save(hs *pb.HardState, entries []*pb.Entry, snap *pb.Snapshot) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if snap != nil && !raft.IsEmptySnap(snap) {
		meta, err := proto.Marshal(snap.GetMetadata())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO state (k, v) VALUES ('snap', ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, meta); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM entry WHERE idx <= ?`, int64(snap.GetMetadata().GetIndex())); err != nil {
			return err
		}
	}
	if len(entries) > 0 {
		// Entries from this index on are replaced: a new leader may
		// overwrite what an old one sent but never committed.
		if _, err := tx.Exec(`DELETE FROM entry WHERE idx >= ?`, int64(entries[0].GetIndex())); err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO entry (idx, data) VALUES (?, ?)`)
		if err != nil {
			return err
		}
		for _, e := range entries {
			data, err := proto.Marshal(e)
			if err != nil {
				stmt.Close()
				return err
			}
			if _, err := stmt.Exec(int64(e.GetIndex()), data); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}
	if hs != nil && !raft.IsEmptyHardState(hs) {
		data, err := proto.Marshal(hs)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO state (k, v) VALUES ('hard', ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// compact records a snapshot this node took itself, at an index its data
// file holds durably, and drops the entries up to keepFrom.
func (l *logStore) compact(meta *pb.SnapshotMetadata, keepFrom uint64) error {
	data, err := proto.Marshal(meta)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO state (k, v) VALUES ('snap', ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, data); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM entry WHERE idx < ?`, int64(keepFrom)); err != nil {
		return err
	}
	return tx.Commit()
}

// load fills a MemoryStorage with what the file holds and returns it with
// the snapshot's metadata.
func (l *logStore) load() (*raft.MemoryStorage, *pb.SnapshotMetadata, error) {
	ms := raft.NewMemoryStorage()
	meta := pb.EnsureSnapshotMetadata(nil)
	if b, err := l.get("snap"); err != nil {
		return nil, nil, err
	} else if b != nil {
		if err := proto.Unmarshal(b, meta); err != nil {
			return nil, nil, fmt.Errorf("raft log: snapshot metadata: %w", err)
		}
		meta = pb.EnsureSnapshotMetadata(meta)
		if err := ms.ApplySnapshot(&pb.Snapshot{Metadata: meta}); err != nil {
			return nil, nil, err
		}
	}
	if b, err := l.get("hard"); err != nil {
		return nil, nil, err
	} else if b != nil {
		hs := &pb.HardState{}
		if err := proto.Unmarshal(b, hs); err != nil {
			return nil, nil, fmt.Errorf("raft log: hard state: %w", err)
		}
		if err := ms.SetHardState(hs); err != nil {
			return nil, nil, err
		}
	}
	rows, err := l.db.Query(`SELECT data FROM entry WHERE idx > ? ORDER BY idx`, int64(meta.GetIndex()))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var batch []*pb.Entry
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, nil, err
		}
		e := &pb.Entry{}
		if err := proto.Unmarshal(data, e); err != nil {
			return nil, nil, fmt.Errorf("raft log: entry: %w", err)
		}
		batch = append(batch, e)
		if len(batch) == 1024 {
			if err := ms.Append(batch); err != nil {
				return nil, nil, err
			}
			batch = nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(batch) > 0 {
		if err := ms.Append(batch); err != nil {
			return nil, nil, err
		}
	}
	return ms, meta, nil
}
