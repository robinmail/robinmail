package store

import (
	"context"
	"fmt"
	"time"

	"github.com/robinmail/robinmail/internal/db"
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
	return s.scanAccount(res[0].Values[0]), nil
}

// Stages of a session.
const (
	// StageFull is a login that is complete.
	StageFull = "full"
	// StageTOTP is a login that gave its password, or came back from an
	// OIDC provider, and still owes a TOTP code. It opens nothing.
	StageTOTP = "totp"
)

// Session is a web login.
type Session struct {
	// ID names the session to its owner: in the list, and to revoke it.
	// It is not the token and logs nobody in.
	ID        string
	AccountID string
	Stage     string
	// Method is how the user proved who they are: "password",
	// "password+totp", "password+recovery", "oidc:<provider>" and so on.
	Method    string
	CreatedAt time.Time
	// SeenAt and SeenIP are the last request the server noted. They are
	// written at most every few minutes (TouchSession).
	SeenAt time.Time
	// ExpiresAt ends the session whatever happens; Idle ends it when
	// nothing was heard of it for that long.
	ExpiresAt time.Time
	Idle      time.Duration
	IP        string
	SeenIP    string
	// Agent is the browser's User-Agent at login, cut.
	Agent    string
	Remember bool
}

const sessionColumns = `id, account_id, stage, method, created_at, seen_at, expires_at, idle, ip, seen_ip, agent, remember`

func scanSession(row []any) Session {
	return Session{ID: asString(row[0]), AccountID: asString(row[1]), Stage: asString(row[2]), Method: asString(row[3]),
		CreatedAt: time.Unix(asInt(row[4]), 0), SeenAt: time.Unix(asInt(row[5]), 0), ExpiresAt: time.Unix(asInt(row[6]), 0),
		Idle: time.Duration(asInt(row[7])) * time.Second, IP: asString(row[8]), SeenIP: asString(row[9]),
		Agent: asString(row[10]), Remember: asInt(row[11]) != 0}
}

// sqlSessionLive is true for a session that has not ended by either of its
// lifetimes.
const sqlSessionLive = `expires_at > :now AND seen_at + idle > :now`

// CreateSession stores a web login. tokenHash is a hash of the secret the
// browser holds; the secret itself never reaches the database. The session
// is created as given, with CreatedAt and SeenAt set to now. replaces is
// the token hash of a session this one takes the place of, which is
// deleted in the same batch: a login never leaves the session it came with
// behind (session rotation). Empty is none.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, sess Session, replaces string) error {
	b := s.newBatch(sess.AccountID)
	b.guardAccount()
	// The token is 32 random bytes: a row with its hash can only be this
	// one, written by a first run of this batch. The delete is a delete.
	b.idempotent()
	if replaces != "" {
		b.add(`DELETE FROM session WHERE token_hash = :old`, "old", replaces)
	}
	b.add(`INSERT OR IGNORE INTO session (token_hash, id, account_id, stage, method, created_at, seen_at, expires_at, idle, ip, seen_ip, agent, remember)
		VALUES (:hash, :id, :a, :stage, :method, :now, :now, :expires, :idle, :ip, :ip, :agent, :remember)`,
		"hash", tokenHash, "id", sess.ID, "stage", sess.Stage, "method", sess.Method, "expires", sess.ExpiresAt.Unix(),
		"idle", int64(sess.Idle/time.Second), "ip", sess.IP, "agent", sess.Agent, "remember", boolInt(sess.Remember))
	_, err := s.run(ctx, b)
	return err
}

// SessionByToken returns a session that has not ended, of any stage, and
// its account, or ErrNoSession.
func (s *Store) SessionByToken(ctx context.Context, tokenHash string) (Session, Account, error) {
	res, err := s.query(ctx, "",
		q(`SELECT `+sessionColumns+` FROM session WHERE token_hash = :hash AND `+sqlSessionLive, "hash", tokenHash, "now", s.Now().Unix()),
		q(`SELECT `+accountColumns+` FROM account
			WHERE id = (SELECT account_id FROM session WHERE token_hash = :hash AND `+sqlSessionLive+`)`, "hash", tokenHash, "now", s.Now().Unix()))
	if err != nil {
		return Session{}, Account{}, err
	}
	if len(res[0].Values) == 0 || len(res[1].Values) == 0 {
		return Session{}, Account{}, ErrNoSession
	}
	return scanSession(res[0].Values[0]), s.scanAccount(res[1].Values[0]), nil
}

// SessionAccount returns the account of a complete login, or ErrNoSession
// if there is none with that hash, it has ended, or it still owes its
// second step.
func (s *Store) SessionAccount(ctx context.Context, tokenHash string) (Account, error) {
	sess, acct, err := s.SessionByToken(ctx, tokenHash)
	if err != nil {
		return Account{}, err
	}
	if sess.Stage != StageFull {
		return Account{}, ErrNoSession
	}
	return acct, nil
}

// TouchSession notes that a session was used, now and from ip. Callers do
// this every few minutes at most: it is a write.
func (s *Store) TouchSession(ctx context.Context, tokenHash, ip string) error {
	// Sets a row to given values.
	_, err := s.db.ExecuteIdempotent(ctx, db.Stmt(
		`UPDATE session SET seen_at = max(seen_at, ?2), seen_ip = ?3 WHERE token_hash = ?1`, tokenHash, s.Now().Unix(), ip))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// DeleteSession ends a session. One that does not exist is not an error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := s.db.ExecuteIdempotent(ctx, db.Stmt(`DELETE FROM session WHERE token_hash = ?`, tokenHash)); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Sessions lists the complete logins of an account that have not ended,
// the one used last first.
func (s *Store) Sessions(ctx context.Context, account string) ([]Session, error) {
	res, err := s.query(ctx, account, q(`SELECT `+sessionColumns+` FROM session
		WHERE account_id = :a AND stage = 'full' AND `+sqlSessionLive+` ORDER BY seen_at DESC, id`, "now", s.Now().Unix()))
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanSession(row))
	}
	return out, nil
}

// RevokeSession ends one session of an account by its id and reports
// whether there was one. It moves the account's auth_seq, so an event
// stream that is open under the session ends too.
func (s *Store) RevokeSession(ctx context.Context, account, id string) (bool, error) {
	// A delete; a second run finds nothing and moves auth_seq once more,
	// which costs open connections one more check.
	res, err := s.db.ExecuteIdempotent(ctx,
		db.Stmt(`DELETE FROM session WHERE account_id = ?1 AND id = ?2`, account, id),
		db.Stmt(sqlBumpAuthSeq, account))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	s.authChanged(account)
	return res[0].RowsAffected > 0, nil
}

// RevokeSessions ends all sessions of an account except the one with the
// given token hash (empty: all) and returns how many.
func (s *Store) RevokeSessions(ctx context.Context, account, exceptTokenHash string) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx,
		db.Stmt(`DELETE FROM session WHERE account_id = ?1 AND token_hash <> ?2`, account, exceptTokenHash),
		db.Stmt(sqlBumpAuthSeq, account))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	s.authChanged(account)
	return res[0].RowsAffected, nil
}

// sqlBumpAuthSeq takes the account id as ?1.
const sqlBumpAuthSeq = `UPDATE account SET auth_seq = auth_seq + 1 WHERE id = ?1`

// authChanged tells this node's listeners that a login of the account was
// withdrawn. Other nodes find out by polling auth_seq.
func (s *Store) authChanged(account string) {
	if s.OnAuthChange != nil {
		s.OnAuthChange(account)
	}
}

// TrimSessions removes the sessions that ended before the given time, by
// either lifetime, and returns how many.
func (s *Store) TrimSessions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, db.Stmt(
		`DELETE FROM session WHERE expires_at <= ?1 OR seen_at + idle <= ?1`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}
