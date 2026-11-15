package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/rqlite"
)

// What the administrator's commands need and nothing else does: the
// outbound queue as a whole, removing an account, totals, backup and
// restore.

// ErrQueueBusy is returned for a queue row a node is working on right now.
var ErrQueueBusy = errors.New("a node is working on that message right now")

// QueueEntry is a row of the outbound queue with whose it is.
type QueueEntry struct {
	Submission
	Address      string // the account's address
	ClaimedUntil time.Time
}

const sqlQueueEntry = `SELECT s.account_id, s.id, s.email_id, s.thread_id, s.identity_id, s.message_id, s.blob, s.size, s.mail_from,
	s.recipients, s.undo, s.queue, s.created_at, s.send_at, s.next_try, s.tries, s.claimed_by, s.modseq, a.address, s.claimed_until
	FROM submission s JOIN account a ON a.id = s.account_id`

func scanQueueEntry(row []any) QueueEntry {
	return QueueEntry{Submission: scanSubmission(row), Address: asString(row[18]), ClaimedUntil: time.Unix(asInt(row[19]), 0).UTC()}
}

// Queue lists the messages that still have work: the one that is due
// first comes first.
func (s *Store) Queue(ctx context.Context, limit int) ([]QueueEntry, error) {
	res, err := s.query(ctx, "", q(sqlQueueEntry+` WHERE s.queue = 'active' ORDER BY s.next_try, s.account_id, s.id LIMIT :limit`, "limit", limit))
	if err != nil {
		return nil, err
	}
	out := make([]QueueEntry, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanQueueEntry(row))
	}
	return out, nil
}

// Queued returns one message of the queue, done or not.
func (s *Store) Queued(ctx context.Context, account string, id int64) (QueueEntry, error) {
	res, err := s.query(ctx, account, q(sqlQueueEntry+` WHERE s.account_id = :a AND s.id = :id`, "id", id))
	if err != nil {
		return QueueEntry{}, err
	}
	if len(res[0].Values) == 0 {
		return QueueEntry{}, ErrNoSubmission
	}
	return scanQueueEntry(res[0].Values[0]), nil
}

// QueueStats counts the queue.
type QueueStats struct {
	Active  int       // messages with work left
	Due     int       // of those, due now and not being worked on
	Claimed int       // of those, being worked on
	Waiting int       // of those, still in their undo time or waiting for the next try
	Oldest  time.Time // when the oldest of them was submitted; zero without any
}

func (s *Store) QueueStats(ctx context.Context) (QueueStats, error) {
	res, err := s.query(ctx, "", q(`SELECT count(*), coalesce(sum(next_try <= :now AND claimed_until <= :now), 0),
		coalesce(sum(claimed_until > :now), 0), coalesce(min(created_at), 0) FROM submission WHERE queue = 'active'`, "now", s.Now().Unix()))
	if err != nil {
		return QueueStats{}, err
	}
	row := res[0].Values[0]
	st := QueueStats{Active: int(asInt(row[0])), Due: int(asInt(row[1])), Claimed: int(asInt(row[2]))}
	st.Waiting = st.Active - st.Due - st.Claimed
	if at := asInt(row[3]); at > 0 {
		st.Oldest = time.Unix(at, 0).UTC()
	}
	return st, nil
}

// RetryQueued makes messages due now instead of at their next try: one
// message, or with an empty account every message that waits. A message a
// node is working on is left alone. It returns how many it moved.
func (s *Store) RetryQueued(ctx context.Context, account string, id int64) (int64, error) {
	b := s.newBatch(account)
	b.idempotent() // sets rows to a given value
	where := `queue = 'active' AND claimed_until <= :now AND next_try > :now`
	if account != "" {
		b.base["id"] = id
		b.guard(ErrNoSubmission, `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND queue = 'active')`)
		b.guard(ErrQueueBusy, `NOT EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND claimed_until > :now)`)
		where += ` AND account_id = :a AND id = :id`
	}
	// send_at as well: a message in its undo time goes now.
	b.add(`UPDATE submission SET next_try = :now, send_at = min(send_at, :now) WHERE ` + where)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return res[len(res)-1].RowsAffected, nil
}

// DropQueued gives a message of the queue up: every recipient that still
// waited is failed with the reason, the message leaves the queue, and
// nothing is sent to anyone about it. The sender sees the failure where
// they see the state of what they sent (JMAP's deliveryStatus). It
// returns the recipients that were given up.
func (s *Store) DropQueued(ctx context.Context, account string, id int64, reason string) ([]string, error) {
	e, err := s.Queued(ctx, account, id)
	if err != nil {
		return nil, err
	}
	if e.Queue != QueueActive {
		return nil, ErrNoSubmission
	}
	var dropped []string
	old, _ := json.Marshal(e.Recipients)
	for i := range e.Recipients {
		if r := &e.Recipients[i]; r.State == RcptQueued {
			r.State, r.Reply, r.Status, r.MTA, r.At = RcptFailed, reason, "5.0.0", "", s.Now().Unix()
			dropped = append(dropped, r.Address)
		}
	}
	rj, _ := json.Marshal(e.Recipients)
	b := s.newBatch(account)
	b.base["id"] = id
	// The row as it was read, and nobody working on it: otherwise what a
	// node found out meanwhile would be overwritten.
	b.guard(ErrQueueBusy, `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND queue = 'active'
		AND claimed_until <= :now AND recipients = :old)`, "old", string(old))
	const visible = sqlJMAPSubmission + `)`
	b.bumped = true // when JMAP shows the row
	b.add(`UPDATE account SET modseq = modseq + 1 WHERE id = :a AND ` + visible)
	b.add(`INSERT OR IGNORE INTO changes (account_id, modseq, type, id, op, at)
		SELECT :a, ` + sqlModseq + `, 'EmailSubmission', :id, 'updated', :now WHERE ` + visible)
	b.add(`UPDATE submission SET recipients = :recipients, queue = 'done', blob = '', undo = CASE WHEN undo = 'pending' THEN 'canceled' ELSE undo END,
		claimed_by = '', claimed_until = 0, modseq = `+sqlModseq+` WHERE account_id = :a AND id = :id`, "recipients", string(rj))
	b.add(`DELETE FROM submission WHERE account_id = :a AND id = :id AND destroyed = 1`)
	if _, err := s.run(ctx, b); err != nil {
		return nil, err
	}
	return dropped, nil
}

// RemoveAccount deletes an account and everything it has: mail, mailboxes,
// filters, queued messages, sessions, passwords. Aliases that deliver to
// it lose that target. The log of auth events keeps its lines. The blob
// files stay on the disks until the next sweep finds them unreferenced.
//
// It is one batch, so the account is there or gone; for an account with a
// great many messages it is a large one.
func (s *Store) RemoveAccount(ctx context.Context, account string) error {
	res, err := s.query(ctx, account, q(`SELECT m.name FROM sqlite_master m JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND p.name = 'account_id' AND m.name NOT IN ('auth_event') ORDER BY m.name`),
		q(`SELECT address FROM account WHERE id = :a`))
	if err != nil {
		return err
	}
	if len(res[1].Values) == 0 {
		return ErrNoAccount
	}
	address := asString(res[1].Values[0][0])
	b := s.newBatch(account)
	b.guardAccount()
	for _, row := range res[0].Values {
		// Names from sqlite_master, of our own tables.
		b.add(`DELETE FROM "` + asString(row[0]) + `" WHERE account_id = :a`)
	}
	b.add(`DELETE FROM alias WHERE target = :address`, "address", address)
	b.add(`DELETE FROM account WHERE id = :a`)
	if _, err := s.run(ctx, b); err != nil {
		return err
	}
	s.keys.Delete(account)
	return nil
}

// Totals are the sizes of the server.
type Totals struct {
	Domains, Accounts, Aliases int
	Emails                     int64
	Bytes                      int64 // of the emails, as the quota counts them
	Schema                     int
}

func (s *Store) Totals(ctx context.Context) (Totals, error) {
	res, err := s.query(ctx, "", q(`SELECT (SELECT count(*) FROM domain), (SELECT count(*) FROM account),
		(SELECT count(DISTINCT address) FROM alias), (SELECT coalesce(sum(used_count), 0) FROM account),
		(SELECT coalesce(sum(used_bytes), 0) FROM account), (SELECT max(version) FROM schema_version)`))
	if err != nil {
		return Totals{}, err
	}
	r := res[0].Values[0]
	return Totals{Domains: int(asInt(r[0])), Accounts: int(asInt(r[1])), Aliases: int(asInt(r[2])),
		Emails: asInt(r[3]), Bytes: asInt(r[4]), Schema: int(asInt(r[5]))}, nil
}

// Ping asks the database for one row through the Raft log: it answers
// only when the cluster has a leader that this node can reach.
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.db.Query(ctx, rqlite.Strong, rqlite.Stmt(`SELECT 1`)); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// SchemaVersion is the schema this binary works with.
func SchemaVersion() int { return schemaVersion }

// ---- backup and restore ----

// BackupDatabase writes the whole database to w as one SQLite file: a
// copy rqlite makes at one point of its log, so every table in it is from
// the same moment.
func (s *Store) BackupDatabase(ctx context.Context, w io.Writer) error {
	if err := s.db.Backup(ctx, w); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// LoadDatabase replaces the database by the SQLite file in r, brings its
// schema up to this binary's and forgets the nodes the backup knew: they
// were the old cluster's.
func (s *Store) LoadDatabase(ctx context.Context, r io.ReadSeeker) error {
	if err := s.db.Load(ctx, r); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	s.keys.Clear()
	if err := s.Init(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(`DELETE FROM node`),
		// Claims of the old cluster's nodes: free at once.
		rqlite.Stmt(`UPDATE submission SET claimed_by = '', claimed_until = 0 WHERE claimed_until > 0`)); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// Empty reports whether the database holds no domain and no account: the
// only kind a backup is restored into.
func (s *Store) Empty(ctx context.Context) (bool, error) {
	res, err := s.db.Query(ctx, rqlite.Strong, rqlite.Stmt(`SELECT (SELECT count(*) FROM domain) + (SELECT count(*) FROM account)`))
	if err != nil {
		return false, fmt.Errorf("store: %w", err)
	}
	return asInt(res[0].Values[0][0]) == 0, nil
}

// BlobCatalog lists every blob the database refers to, by account.
func (s *Store) BlobCatalog(ctx context.Context) (map[string]map[string]bool, error) {
	refs := blobRefs{s}
	accounts, err := refs.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]bool, len(accounts))
	for _, a := range accounts {
		if out[a], err = refs.AccountBlobs(ctx, a); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AccountKeyFingerprints returns the fingerprints of the master keys the
// accounts' keys are wrapped under.
func (s *Store) AccountKeyFingerprints(ctx context.Context) ([]string, error) {
	use, err := s.AccountKeyUse(ctx)
	if err != nil {
		return nil, err
	}
	if use.Unreadable > 0 {
		return nil, fmt.Errorf("store: %d accounts have a key that no master key of the file unwraps", use.Unreadable)
	}
	out := make([]string, 0, len(use.ByKey))
	for fp := range use.ByKey {
		out = append(out, fp)
	}
	return out, nil
}

// BlobFile opens a blob as it is on disk: encrypted under the account's
// key. In a cluster a file this node lacks is fetched from another.
func (s *Store) BlobFile(ctx context.Context, account, sum string) (io.ReadCloser, error) {
	return s.blobs.Get(ctx, blob.Key(account, sum))
}

// PutBlobFile stores a blob file that BlobFile gave, after checking that
// it is the blob its name says (VerifyBlob): a restore puts back only what
// it can read.
func (s *Store) PutBlobFile(ctx context.Context, account, sum string, open func() (io.ReadCloser, error)) error {
	key := blob.Key(account, sum)
	f, err := open()
	if err != nil {
		return err
	}
	err = s.VerifyBlob(ctx, key, f)
	f.Close()
	if err != nil {
		return err
	}
	if f, err = open(); err != nil {
		return err
	}
	defer f.Close()
	return s.blobs.Put(ctx, key, f)
}
