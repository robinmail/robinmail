package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mikepage/robinmail/internal/rqlite"
)

// AccountByID looks an account up by its id.
func (s *Store) AccountByID(ctx context.Context, id string) (Account, error) {
	res, err := s.query(ctx, id, q(`SELECT `+accountColumns+` FROM account WHERE id = :a`))
	if err != nil {
		return Account{}, err
	}
	if len(res[0].Values) == 0 {
		return Account{}, ErrNoAccount
	}
	return scanAccount(res[0].Values[0]), nil
}

// CreateSession stores a web login. tokenHash is a hash of the secret the
// browser holds; the secret itself never reaches the database.
func (s *Store) CreateSession(ctx context.Context, tokenHash, account string, expires time.Time) error {
	b := s.newBatch(account)
	b.guardAccount()
	// The token is 32 random bytes: a row with its hash can only be this
	// one, written by a first run of this batch.
	b.idempotent()
	b.add(`INSERT OR IGNORE INTO session (token_hash, account_id, expires_at) VALUES (:hash, :a, :expires)`,
		"hash", tokenHash, "expires", expires.Unix())
	_, err := s.run(ctx, b)
	return err
}

// SessionAccount returns the account a session belongs to, or ErrNoSession
// if there is none with that hash or it has expired.
func (s *Store) SessionAccount(ctx context.Context, tokenHash string) (Account, error) {
	res, err := s.query(ctx, "", q(`SELECT `+accountColumns+` FROM account
		WHERE id = (SELECT account_id FROM session WHERE token_hash = :hash AND expires_at > :now)`,
		"hash", tokenHash, "now", s.Now().Unix()))
	if err != nil {
		return Account{}, err
	}
	if len(res[0].Values) == 0 {
		return Account{}, ErrNoSession
	}
	return scanAccount(res[0].Values[0]), nil
}

// DeleteSession ends a session. One that does not exist is not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(`DELETE FROM session WHERE token_hash = ?`, tokenHash)); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// TrimSessions removes the sessions that expired before the given time and
// returns how many.
func (s *Store) TrimSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(`DELETE FROM session WHERE expires_at <= ?`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}
