package store

import (
	"context"
	"fmt"
	"time"

	"github.com/robinmail/robinmail/internal/db"
)

// Node is a robinmail node of the cluster, as the node table has it.
type Node struct {
	ID string
	// Address is the host:port the other nodes reach it on.
	Address string
	// Cert is the SHA-256, in hex, of the certificate the node uses for
	// calls between nodes, as server and as client.
	Cert      string
	StartedAt time.Time
	// SeenAt is the node's last heartbeat, by its own clock. For people: no
	// decision in the code depends on comparing it with another clock.
	SeenAt time.Time
}

// RegisterNode writes the node's row: its address and certificate. A running
// server passes running = true, which also stamps started_at and seen_at; a
// command that only needs the other nodes to know its certificate passes
// false and leaves those alone.
//
// Idempotent: the row is set to the given values.
func (s *Store) RegisterNode(ctx context.Context, n Node, running bool) error {
	now := s.Now().Unix()
	stamp := int64(0)
	if running {
		stamp = now
	}
	_, err := s.db.ExecuteIdempotent(ctx, db.Stmt(`INSERT INTO node (id, address, cert, started_at, seen_at)
		VALUES (?1, ?2, ?3, ?4, ?4)
		ON CONFLICT (id) DO UPDATE SET address = ?2, cert = ?3,
			started_at = CASE WHEN ?4 > 0 THEN ?4 ELSE started_at END,
			seen_at = CASE WHEN ?4 > 0 THEN ?4 ELSE seen_at END`, n.ID, n.Address, n.Cert, stamp))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Heartbeat renews the node's seen_at. It reports false when the row is
// gone or carries another certificate: the node was removed, or a second
// node runs under the same id.
func (s *Store) Heartbeat(ctx context.Context, id, cert string) (bool, error) {
	res, err := s.db.ExecuteIdempotent(ctx,
		db.Stmt(`UPDATE node SET seen_at = ? WHERE id = ? AND cert = ?`, s.Now().Unix(), id, cert),
		db.Stmt(`SELECT 1 FROM node WHERE id = ? AND cert = ?`, id, cert))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	return len(res[1].Values) > 0, nil
}

// Nodes lists the registered nodes by id.
func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	res, err := s.query(ctx, "", q(`SELECT id, address, cert, started_at, seen_at FROM node ORDER BY id`))
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(res[0].Values))
	for _, r := range res[0].Values {
		out = append(out, Node{ID: asString(r[0]), Address: asString(r[1]), Cert: asString(r[2]),
			StartedAt: time.Unix(asInt(r[3]), 0), SeenAt: time.Unix(asInt(r[4]), 0)})
	}
	return out, nil
}

// RemoveNode takes a node out of the cluster: the others stop sending it
// blobs, stop counting it, and refuse its calls. A node that is still
// running notices at its next heartbeat and registers again, so stop it
// first. It reports whether there was such a node.
func (s *Store) RemoveNode(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecuteIdempotent(ctx,
		db.Stmt(`SELECT 1 FROM node WHERE id = ?`, id),
		db.Stmt(`DELETE FROM node WHERE id = ?`, id))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	return len(res[0].Values) > 0, nil
}

// LoginFailure is the count of failed logins for one key in its window.
type LoginFailure struct {
	Count   int
	Started time.Time // the first failure of the window
}

// LoginFailures returns the counters of the given keys whose window, which
// starts at the first failure, has not ended. A key without failures is not
// in the map.
//
// It is read at level none: the node that is asked answers from its own
// copy of the database, so a login does not wait for the leader. The count
// may miss failures of the last moments, which a rate limit can live with.
func (s *Store) LoginFailures(ctx context.Context, window time.Duration, keys ...string) (map[string]LoginFailure, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	vals, kv := values("k", keys)
	b := &batch{base: map[string]any{}}
	b.add(`SELECT key, started, count FROM login_failure WHERE key IN (VALUES `+vals+`) AND started > :cut`,
		append(kv, "cut", s.Now().Add(-window).Unix())...)
	res, err := s.db.Query(ctx, db.None, b.stmts...)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	out := map[string]LoginFailure{}
	for _, r := range res[0].Values {
		out[asString(r[0])] = LoginFailure{Count: int(asInt(r[2])), Started: time.Unix(asInt(r[1]), 0)}
	}
	return out, nil
}

// AddLoginFailure counts one failed login for each key, starting a new
// window for a key whose last one has ended.
//
// Not idempotent, so not sent again after an unknown outcome: a lost count
// is better than a double one, and the caller only logs the error.
func (s *Store) AddLoginFailure(ctx context.Context, window time.Duration, keys ...string) error {
	now := s.Now().Unix()
	cut := s.Now().Add(-window).Unix()
	stmts := make([]db.Statement, len(keys))
	for i, k := range keys {
		stmts[i] = db.Stmt(`INSERT INTO login_failure (key, started, count) VALUES (?1, ?2, 1)
			ON CONFLICT (key) DO UPDATE SET
				count = CASE WHEN started > ?3 THEN count + 1 ELSE 1 END,
				started = CASE WHEN started > ?3 THEN started ELSE ?2 END`, k, now, cut)
	}
	if _, err := s.db.Execute(ctx, stmts...); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// ClearLoginFailures forgets the failures of the keys: a login succeeded.
func (s *Store) ClearLoginFailures(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	vals, kv := values("k", keys)
	b := &batch{base: map[string]any{}}
	b.add(`DELETE FROM login_failure WHERE key IN (VALUES `+vals+`)`, kv...)
	if _, err := s.db.ExecuteIdempotent(ctx, b.stmts...); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// TrimLoginFailures removes the counters whose window ended before the
// given time and returns how many.
func (s *Store) TrimLoginFailures(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, db.Stmt(`DELETE FROM login_failure WHERE started <= ?`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}
