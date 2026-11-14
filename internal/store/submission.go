package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/rqlite"
)

var (
	ErrNoSubmission = errors.New("no such submission")
	ErrNoIdentity   = errors.New("no such identity")
	// ErrIdentityDefault is returned for the identity made with the account.
	ErrIdentityDefault = errors.New("the first identity of an account cannot be destroyed")
	// ErrRateLimited is returned by Submit when the account has sent to too
	// many recipients in the last hour.
	ErrRateLimited = errors.New("too many recipients in the last hour")
	// ErrCannotUnsend is returned by CancelSubmission once a node has
	// started on the message.
	ErrCannotUnsend = errors.New("the message is already on its way")
	// ErrNotClaimed is returned when a submission is not this caller's to
	// work on: another node holds it, it is not due, or the claim was lost.
	ErrNotClaimed = errors.New("the submission is not claimed by this node")
)

const (
	submissionPrefix = 's'
	identityPrefix   = 'i'
)

func SubmissionID(n int64) string { return formatID(submissionPrefix, n) }
func IdentityID(n int64) string   { return formatID(identityPrefix, n) }

func ParseSubmissionID(s string) (int64, bool) { return parseID(submissionPrefix, s) }
func ParseIdentityID(s string) (int64, bool)   { return parseID(identityPrefix, s) }

// States of one recipient of a submission.
const (
	RcptQueued    = "queued"
	RcptDelivered = "delivered"
	RcptFailed    = "failed"
)

// The undoStatus of RFC 8621 7, and the two states of the queue.
const (
	UndoPending  = "pending"
	UndoFinal    = "final"
	UndoCanceled = "canceled"

	QueueActive = "active"
	QueueDone   = "done"
)

// Recipient is one RCPT TO of a submission and what became of it.
type Recipient struct {
	Address string `json:"a"`
	State   string `json:"s"`
	// Reply is the last SMTP reply for this recipient, or our own words
	// when no server said anything ("connection refused").
	Reply string `json:"r,omitempty"`
	// Status is the enhanced status code (RFC 3463) that goes with Reply.
	Status string `json:"c,omitempty"`
	// MTA is the host that gave the reply.
	MTA string `json:"m,omitempty"`
	// At is when the last attempt was, in Unix seconds.
	At int64 `json:"t,omitempty"`
	// Remote is true when the state comes from a delivery status
	// notification another system sent us later.
	Remote bool `json:"n,omitempty"`
	// Mailbox is true when the message is known to have reached the
	// recipient's mailbox: it was delivered on this server, or a
	// notification said so. Otherwise "delivered" means that another
	// server took it.
	Mailbox bool `json:"b,omitempty"`
}

// Submission is one message in the outbound queue.
type Submission struct {
	Account    string
	ID         int64
	EmailID    int64 // 0: submitted over SMTP, not an Email of the account
	ThreadID   int64
	IdentityID int64
	MessageID  string
	Blob       string // the message as it goes out; empty once done
	Size       int64
	MailFrom   string // empty: the null reverse-path
	Recipients []Recipient
	Undo       string
	Queue      string
	CreatedAt  time.Time
	SendAt     time.Time
	NextTry    time.Time
	Tries      int
	ClaimedBy  string
	Modseq     int64
}

// Done reports whether every recipient has a final state.
func (s Submission) Done() bool {
	for _, r := range s.Recipients {
		if r.State == RcptQueued {
			return false
		}
	}
	return true
}

const submissionColumns = `account_id, id, email_id, thread_id, identity_id, message_id, blob, size, mail_from, recipients,
	undo, queue, created_at, send_at, next_try, tries, claimed_by, modseq`

func scanSubmission(row []any) Submission {
	s := Submission{Account: asString(row[0]), ID: asInt(row[1]), EmailID: asInt(row[2]), ThreadID: asInt(row[3]),
		IdentityID: asInt(row[4]), MessageID: asString(row[5]), Blob: asString(row[6]), Size: asInt(row[7]),
		MailFrom: asString(row[8]), Undo: asString(row[10]), Queue: asString(row[11]),
		CreatedAt: time.Unix(asInt(row[12]), 0).UTC(), SendAt: time.Unix(asInt(row[13]), 0).UTC(),
		NextTry: time.Unix(asInt(row[14]), 0).UTC(), Tries: int(asInt(row[15])), ClaimedBy: asString(row[16]),
		Modseq: asInt(row[17])}
	json.Unmarshal([]byte(asString(row[9])), &s.Recipients)
	return s
}

// NewSubmission is a message to queue.
type NewSubmission struct {
	// EmailID is the Email this submits (JMAP), or 0. The email must exist.
	EmailID    int64
	IdentityID int64 // 0: none; otherwise it must exist
	// Raw is the message as it goes on the wire.
	Raw      []byte
	MailFrom string
	Rcpts    []string
	// SendAt is the first moment a node may start on it; until then a JMAP
	// client can cancel. Zero is now.
	SendAt time.Time
	// MaxPerHour, when not 0, refuses the submission (ErrRateLimited) if
	// the account would pass that many recipients within an hour.
	MaxPerHour int
	IfInState  int64
}

// Submit puts a message in the queue: the blob first, then one batch that
// takes the submission id and checks the rate limit. The limit is counted in
// the table itself, so it holds for all nodes together.
func (s *Store) Submit(ctx context.Context, account string, n NewSubmission) (Submission, error) {
	if len(n.Rcpts) == 0 {
		return Submission{}, errors.New("store: a submission needs a recipient")
	}
	sum := sha256.Sum256(n.Raw)
	blobID := hex.EncodeToString(sum[:])
	if err := s.putBlob(ctx, account, blobID, n.Raw); err != nil {
		return Submission{}, err
	}
	rcpts := make([]Recipient, len(n.Rcpts))
	for i, r := range n.Rcpts {
		rcpts[i] = Recipient{Address: r, State: RcptQueued}
	}
	rj, _ := json.Marshal(rcpts)
	var kb [16]byte
	rand.Read(kb[:])
	key := hex.EncodeToString(kb[:])
	now := s.Now()
	sendAt := n.SendAt
	if sendAt.Before(now) {
		sendAt = now
	}
	info := summarize(n.Raw)

	b := s.newBatch(account)
	// The key is random and unique per call: if the first run was applied,
	// a second one fails on it as a whole, and the row is read back below.
	b.idempotent()
	b.guardAccount()
	b.ifState(n.IfInState)
	if n.EmailID != 0 {
		b.guard(ErrNoEmail, `EXISTS (SELECT 1 FROM email WHERE account_id = :a AND id = :email)`, "email", n.EmailID)
	}
	if n.IdentityID != 0 {
		b.guard(ErrNoIdentity, `EXISTS (SELECT 1 FROM identity WHERE account_id = :a AND id = :identity)`, "identity", n.IdentityID)
	}
	if n.MaxPerHour > 0 {
		b.guard(ErrRateLimited, `(SELECT COALESCE(SUM(rcpt_count), 0) FROM submission
			WHERE account_id = :a AND created_at > :since) + :n <= :max`,
			"since", now.Add(-time.Hour).Unix(), "n", len(rcpts), "max", n.MaxPerHour)
	}
	b.bump("next_submission")
	b.unique(ErrDuplicate)
	b.add(`INSERT INTO submission (account_id, id, email_id, thread_id, identity_id, message_id, blob, size, mail_from,
			recipients, rcpt_count, created_at, send_at, next_try, submit_key, modseq)
		SELECT a.id, a.next_submission - 1, :email,
			COALESCE((SELECT thread_id FROM email WHERE account_id = :a AND id = :email), 0),
			:identity, :message_id, :blob, :size, :mail_from, :recipients, :n, :now, :send_at, :send_at, :key, a.modseq
		FROM account a WHERE a.id = :a`,
		"email", n.EmailID, "identity", n.IdentityID, "message_id", info.messageID, "blob", blobID, "size", len(n.Raw),
		"mail_from", n.MailFrom, "recipients", string(rj), "n", len(rcpts), "send_at", sendAt.Unix(), "key", key)
	if n.EmailID != 0 {
		b.change("EmailSubmission", `next_submission - 1 AS id, 'created' AS op FROM account WHERE id = :a`)
	}
	_, runErr := s.run(ctx, b)
	// A run that was sent twice reports what the second one met: the key,
	// or a guard that the first run itself made untrue. The row decides.
	res, err := s.query(ctx, account, q(`SELECT `+submissionColumns+` FROM submission
		WHERE account_id = :a AND submit_key = :key`, "key", key))
	if err != nil {
		return Submission{}, err
	}
	if len(res[0].Values) == 1 {
		return scanSubmission(res[0].Values[0]), nil
	}
	if runErr == nil || errors.Is(runErr, ErrDuplicate) {
		runErr = fmt.Errorf("store: the submission was not stored")
	}
	return Submission{}, runErr
}

// putBlob encrypts data and stores it as a blob of the account.
func (s *Store) putBlob(ctx context.Context, account, blobID string, data []byte) error {
	key, err := s.blobKey(ctx, account)
	if err != nil {
		return err
	}
	sealed, err := blob.Encrypt(key, account, blobID, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := s.blobs.Put(ctx, blob.Key(account, blobID), sealed); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// readBlob reads a blob of the account without asking what refers to it.
func (s *Store) readBlob(ctx context.Context, account, blobID string) ([]byte, error) {
	key, err := s.blobKey(ctx, account)
	if err != nil {
		return nil, err
	}
	f, err := s.blobs.Get(ctx, blob.Key(account, blobID))
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", blobID, err)
	}
	r, err := blob.Decrypt(key, account, blobID, f)
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", blobID, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", blobID, err)
	}
	return data, nil
}

// SubmissionMessage reads the message of a submission that is not done.
func (s *Store) SubmissionMessage(ctx context.Context, sub Submission) ([]byte, error) {
	if sub.Blob == "" {
		return nil, ErrNoBlob
	}
	return s.readBlob(ctx, sub.Account, sub.Blob)
}

// Submissions lists the submissions JMAP shows: those of an Email that the
// client has not destroyed. With ids only those, in no particular order.
func (s *Store) Submissions(ctx context.Context, account string, ids []int64) ([]Submission, error) {
	sql := `SELECT ` + submissionColumns + ` FROM submission WHERE account_id = :a AND email_id <> 0 AND destroyed = 0`
	var kv []any
	if ids != nil {
		if len(ids) == 0 {
			return nil, nil
		}
		var vals string
		vals, kv = values("i", ids)
		sql += ` AND id IN (VALUES ` + vals + `)`
	}
	res, err := s.query(ctx, account, q(sql+` ORDER BY id`, kv...))
	if err != nil {
		return nil, err
	}
	out := make([]Submission, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanSubmission(row))
	}
	return out, nil
}

// Submission reads one row of the queue, whatever kind.
func (s *Store) Submission(ctx context.Context, account string, id int64) (Submission, error) {
	res, err := s.query(ctx, account, q(`SELECT `+submissionColumns+` FROM submission
		WHERE account_id = :a AND id = :id`, "id", id))
	if err != nil {
		return Submission{}, err
	}
	if len(res[0].Values) == 0 {
		return Submission{}, ErrNoSubmission
	}
	return scanSubmission(res[0].Values[0]), nil
}

const sqlVisibleSubmission = `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND email_id <> 0 AND destroyed = 0)`

// CancelSubmission sets undoStatus to canceled. That works until a node has
// started on the message (ErrCannotUnsend after).
func (s *Store) CancelSubmission(ctx context.Context, account string, id, ifState int64) (int64, error) {
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoSubmission, sqlVisibleSubmission)
	b.guard(ErrCannotUnsend, `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id
		AND undo = 'pending' AND tries = 0 AND claimed_until <= :now)`)
	b.bump()
	b.add(`UPDATE submission SET undo = 'canceled', queue = 'done', blob = '', modseq = ` + sqlModseq + `
		WHERE account_id = :a AND id = :id`)
	b.change("EmailSubmission", `:id AS id, 'updated' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// DestroySubmission removes the submission from what JMAP shows. It does
// not stop delivery (RFC 8621 7.5): a row with work left stays in the queue
// until that is done.
func (s *Store) DestroySubmission(ctx context.Context, account string, id, ifState int64) (int64, error) {
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoSubmission, sqlVisibleSubmission)
	b.bump()
	b.change("EmailSubmission", `:id AS id, 'destroyed' AS op`)
	b.add(`UPDATE submission SET destroyed = 1 WHERE account_id = :a AND id = :id`)
	b.add(`DELETE FROM submission WHERE account_id = :a AND id = :id AND queue = 'done'`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// SubmissionRef names a row of the queue.
type SubmissionRef struct {
	Account string
	ID      int64
}

// DueSubmissions lists rows a node may claim now, the longest waiting
// first. domains, when not empty, limits them to senders in those domains:
// a cluster has no use for that, tests that share a database do.
func (s *Store) DueSubmissions(ctx context.Context, now time.Time, limit int, domains []string) ([]SubmissionRef, error) {
	sql := `SELECT account_id, id FROM submission WHERE queue = 'active' AND next_try <= :t AND claimed_until <= :t`
	kv := []any{"t", now.Unix(), "limit", limit}
	if len(domains) > 0 {
		vals, dkv := values("d", domains)
		sql += ` AND account_id IN (SELECT id FROM account WHERE ` + sqlAddressDomain + ` IN (VALUES ` + vals + `))`
		kv = append(kv, dkv...)
	}
	res, err := s.query(ctx, "", q(sql+` ORDER BY next_try, account_id, id LIMIT :limit`, kv...))
	if err != nil {
		return nil, err
	}
	out := make([]SubmissionRef, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, SubmissionRef{asString(row[0]), asInt(row[1])})
	}
	return out, nil
}

// sqlJMAPSubmission is true for the row :id when JMAP shows it.
const sqlJMAPSubmission = `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND email_id <> 0 AND destroyed = 0`

// ClaimSubmission makes the row this caller's until the given time. token
// names the claim; every later write for the row must show it. The claim
// is one guarded update: of several nodes that try at once, one gets it,
// and a row whose node died is free again when its claim has run out.
//
// The first claim ends the time in which the message can be cancelled
// (undoStatus becomes final).
//
// After an unknown outcome the batch is not sent again: if it was applied,
// the claim simply runs out and the row is taken later.
func (s *Store) ClaimSubmission(ctx context.Context, ref SubmissionRef, token string, now, until time.Time) (Submission, error) {
	b := s.newBatch(ref.Account)
	b.base["id"], b.base["t"] = ref.ID, now.Unix()
	b.guard(ErrNotClaimed, `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id
		AND queue = 'active' AND next_try <= :t AND claimed_until <= :t)`)
	const first = sqlJMAPSubmission + ` AND undo = 'pending')`
	b.add(`UPDATE account SET modseq = modseq + 1 WHERE id = :a AND ` + first)
	b.add(`INSERT OR IGNORE INTO changes (account_id, modseq, type, id, op, at)
		SELECT :a, ` + sqlModseq + `, 'EmailSubmission', :id, 'updated', :now WHERE ` + first)
	b.add(`UPDATE submission SET claimed_by = :token, claimed_until = :until, tries = tries + 1,
			modseq = CASE WHEN undo = 'pending' THEN `+sqlModseq+` ELSE modseq END, undo = 'final'
		WHERE account_id = :a AND id = :id`, "token", token, "until", until.Unix())
	b.add(`SELECT ` + submissionColumns + ` FROM submission WHERE account_id = :a AND id = :id`)
	res, err := s.run(ctx, b)
	if err != nil {
		return Submission{}, err
	}
	rows := res[len(res)-1].Values
	if len(rows) != 1 {
		return Submission{}, ErrNotClaimed
	}
	return scanSubmission(rows[0]), nil
}

// RenewSubmission extends a claim. ErrNotClaimed means it was lost: stop
// working on the row.
func (s *Store) RenewSubmission(ctx context.Context, ref SubmissionRef, token string, until time.Time) error {
	b := s.newBatch(ref.Account)
	b.base["id"] = ref.ID
	b.idempotent() // sets the end of our own claim to a given time
	b.guard(ErrNotClaimed, `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND claimed_by = :token)`, "token", token)
	b.add(`UPDATE submission SET claimed_until = :until WHERE account_id = :a AND id = :id AND claimed_by = :token`,
		"token", token, "until", until.Unix())
	_, err := s.run(ctx, b)
	return err
}

// SaveSubmission records what an attempt found out. With release the claim
// ends: the row is done when no recipient is left queued, and due again at
// nextTry otherwise. Without release only the recipients are written, so
// that a node that dies later in the attempt does not lose what it knew.
//
// The caller holds the claim, so reading the row, changing the recipients
// and writing them back is safe: the guard refuses the write once the
// claim is someone else's.
func (s *Store) SaveSubmission(ctx context.Context, sub Submission, token string, release bool, nextTry time.Time) error {
	rj, _ := json.Marshal(sub.Recipients)
	b := s.newBatch(sub.Account)
	b.base["id"], b.base["token"] = sub.ID, token
	b.guard(ErrNotClaimed, `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND claimed_by = :token)`)
	const visible = sqlJMAPSubmission + `)`
	b.add(`UPDATE account SET modseq = modseq + 1 WHERE id = :a AND ` + visible)
	b.add(`INSERT OR IGNORE INTO changes (account_id, modseq, type, id, op, at)
		SELECT :a, ` + sqlModseq + `, 'EmailSubmission', :id, 'updated', :now WHERE ` + visible)
	set := `recipients = :recipients, modseq = ` + sqlModseq
	kv := []any{"recipients", string(rj)}
	if release {
		set += `, claimed_by = '', claimed_until = 0, next_try = :next`
		kv = append(kv, "next", nextTry.Unix())
		if sub.Done() {
			set += `, queue = 'done', blob = ''`
		}
	}
	b.add(`UPDATE submission SET `+set+` WHERE account_id = :a AND id = :id`, kv...)
	if release {
		// Rows nobody will look at again: destroyed by the client while
		// they had work, or submitted over SMTP. The latter stay a day for
		// the rate limit, see TrimSubmissions.
		b.add(`DELETE FROM submission WHERE account_id = :a AND id = :id AND queue = 'done' AND destroyed = 1`)
	}
	_, err := s.run(ctx, b)
	return err
}

// ApplyDSN records what a delivery status notification from another system
// says about recipients of a message this account sent (found by its
// Message-ID). It returns false when no submission matches. Only recipients
// we handed over ("delivered" as far as we know) change: a relay further on
// may still fail or confirm them.
func (s *Store) ApplyDSN(ctx context.Context, account, messageID string, reports []Recipient) (bool, error) {
	if messageID == "" || len(reports) == 0 {
		return false, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		res, err := s.query(ctx, account, q(`SELECT `+submissionColumns+` FROM submission
			WHERE account_id = :a AND message_id = :m AND queue = 'done' ORDER BY id DESC LIMIT 1`, "m", messageID))
		if err != nil {
			return false, err
		}
		if len(res[0].Values) == 0 {
			return false, nil
		}
		sub := scanSubmission(res[0].Values[0])
		changed := false
		for _, rep := range reports {
			for i := range sub.Recipients {
				r := &sub.Recipients[i]
				if !strings.EqualFold(r.Address, rep.Address) || r.State != RcptDelivered || r.Mailbox || rep.State == RcptQueued {
					continue
				}
				r.State, r.Reply, r.Status, r.MTA, r.At, r.Remote = rep.State, rep.Reply, rep.Status, rep.MTA, rep.At, true
				r.Mailbox = rep.State == RcptDelivered
				changed = true
			}
		}
		if !changed {
			return false, nil
		}
		rj, _ := json.Marshal(sub.Recipients)
		b := s.newBatch(account)
		b.base["id"] = sub.ID
		// Nobody claims a done row; the modseq it was read with guards
		// against another notification written in between.
		b.guard(ErrNotClaimed, `EXISTS (SELECT 1 FROM submission WHERE account_id = :a AND id = :id AND modseq = :seen)`, "seen", sub.Modseq)
		b.bump()
		b.add(`UPDATE submission SET recipients = :r, modseq = `+sqlModseq+` WHERE account_id = :a AND id = :id`, "r", string(rj))
		if sub.EmailID != 0 {
			b.change("EmailSubmission", `:id AS id, 'updated' AS op WHERE `+sqlVisibleSubmission)
		}
		_, err = s.run(ctx, b)
		if errors.Is(err, ErrNotClaimed) {
			continue
		}
		return err == nil, err
	}
	return false, nil
}

// SubmissionRetention is how long a finished submission that JMAP does not
// show is kept: longer than the hour the rate limit looks back.
const SubmissionRetention = 24 * time.Hour

// TrimSubmissions removes finished rows that came in over SMTP and were
// created before the given time, and returns how many. Submissions made
// over JMAP are the client's to destroy.
func (s *Store) TrimSubmissions(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(
		`DELETE FROM submission WHERE queue = 'done' AND email_id = 0 AND created_at < ?`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}

// Identity is one sender identity of an account (RFC 8621 6). ReplyTo and
// Bcc hold JSON lists of {name, email}, or are empty for none.
type Identity struct {
	ID            int64
	Name          string
	Email         string
	ReplyTo       string
	Bcc           string
	TextSignature string
	HTMLSignature string
}

// MayDelete is false for the identity made with the account.
func (i Identity) MayDelete() bool { return i.ID != 1 }

// Identities lists the identities of an account.
func (s *Store) Identities(ctx context.Context, account string) ([]Identity, error) {
	res, err := s.query(ctx, account, q(`SELECT id, name, email, reply_to, bcc, text_signature, html_signature
		FROM identity WHERE account_id = :a ORDER BY id`))
	if err != nil {
		return nil, err
	}
	out := make([]Identity, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, Identity{ID: asInt(row[0]), Name: asString(row[1]), Email: asString(row[2]), ReplyTo: asString(row[3]),
			Bcc: asString(row[4]), TextSignature: asString(row[5]), HTMLSignature: asString(row[6])})
	}
	return out, nil
}

// CreateIdentity adds an identity and returns it with the new state.
// Whether the account may send as the address is the caller's to check.
func (s *Store) CreateIdentity(ctx context.Context, account string, i Identity, ifState int64) (Identity, int64, error) {
	b := s.newBatch(account)
	b.guardAccount()
	b.ifState(ifState)
	b.bump("next_identity")
	b.add(`INSERT INTO identity (account_id, id, name, email, reply_to, bcc, text_signature, html_signature, modseq)
		SELECT a.id, a.next_identity - 1, :name, :email, :reply_to, :bcc, :text, :html, a.modseq FROM account a WHERE a.id = :a`,
		"name", i.Name, "email", i.Email, "reply_to", i.ReplyTo, "bcc", i.Bcc, "text", i.TextSignature, "html", i.HTMLSignature)
	b.change("Identity", `next_identity - 1 AS id, 'created' AS op FROM account WHERE id = :a`)
	b.add(`SELECT next_identity - 1, modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return Identity{}, 0, err
	}
	row := res[len(res)-1].Values[0]
	i.ID = asInt(row[0])
	return i, asInt(row[1]), nil
}

// IdentityUpdate changes the fields that are not nil. The address of an
// identity never changes.
type IdentityUpdate struct {
	Name, ReplyTo, Bcc, TextSignature, HTMLSignature *string
}

// UpdateIdentity changes an identity and returns the new state.
func (s *Store) UpdateIdentity(ctx context.Context, account string, id int64, u IdentityUpdate, ifState int64) (int64, error) {
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoIdentity, `EXISTS (SELECT 1 FROM identity WHERE account_id = :a AND id = :id)`)
	b.bump()
	set := `modseq = ` + sqlModseq
	var kv []any
	for _, f := range []struct {
		col string
		v   *string
	}{{"name", u.Name}, {"reply_to", u.ReplyTo}, {"bcc", u.Bcc}, {"text_signature", u.TextSignature}, {"html_signature", u.HTMLSignature}} {
		if f.v != nil {
			set += `, ` + f.col + ` = :` + f.col
			kv = append(kv, f.col, *f.v)
		}
	}
	b.add(`UPDATE identity SET `+set+` WHERE account_id = :a AND id = :id`, kv...)
	b.change("Identity", `:id AS id, 'updated' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// DestroyIdentity removes an identity, except the first one.
func (s *Store) DestroyIdentity(ctx context.Context, account string, id, ifState int64) (int64, error) {
	if id == 1 {
		return 0, ErrIdentityDefault
	}
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoIdentity, `EXISTS (SELECT 1 FROM identity WHERE account_id = :a AND id = :id)`)
	b.bump()
	b.add(`DELETE FROM identity WHERE account_id = :a AND id = :id`)
	b.change("Identity", `:id AS id, 'destroyed' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// UploadRetention is how long an uploaded blob that nothing refers to is
// kept (RFC 8620 6.1 asks for at least an hour).
const UploadRetention = 24 * time.Hour

// Upload stores data as a blob of the account and returns its id, the
// SHA-256 of the data. The blob can be downloaded and used in Email/set
// and Email/import until it is trimmed.
func (s *Store) Upload(ctx context.Context, account string, data []byte, typ string) (string, error) {
	sum := sha256.Sum256(data)
	blobID := hex.EncodeToString(sum[:])
	if err := s.putBlob(ctx, account, blobID, data); err != nil {
		return "", err
	}
	b := s.newBatch(account)
	b.idempotent() // sets one row to given values
	b.guardAccount()
	b.add(`INSERT OR REPLACE INTO upload (account_id, blob, size, type, created_at) VALUES (:a, :blob, :size, :type, :now)`,
		"blob", blobID, "size", len(data), "type", typ)
	if _, err := s.run(ctx, b); err != nil {
		return "", err
	}
	return blobID, nil
}

// TrimUploads forgets uploads made before the given time. Their files go
// with the next sweep, unless an email refers to the same content.
func (s *Store) TrimUploads(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(`DELETE FROM upload WHERE created_at < ?`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}

// STSPolicy is a cached MTA-STS policy of a recipient domain (RFC 8461).
type STSPolicy struct {
	Domain    string
	ID        string // the id of the TXT record the policy was fetched for
	Mode      string // enforce, testing or none
	MX        []string
	FetchedAt time.Time
	ExpiresAt time.Time
}

// STSPolicy returns the cached policy of a domain, expired or not.
func (s *Store) STSPolicy(ctx context.Context, domain string) (STSPolicy, bool, error) {
	res, err := s.query(ctx, "", q(`SELECT domain, policy_id, mode, mx, fetched_at, expires_at FROM mta_sts WHERE domain = :d`, "d", domain))
	if err != nil {
		return STSPolicy{}, false, err
	}
	if len(res[0].Values) == 0 {
		return STSPolicy{}, false, nil
	}
	row := res[0].Values[0]
	p := STSPolicy{Domain: asString(row[0]), ID: asString(row[1]), Mode: asString(row[2]),
		FetchedAt: time.Unix(asInt(row[4]), 0), ExpiresAt: time.Unix(asInt(row[5]), 0)}
	if mx := asString(row[3]); mx != "" {
		p.MX = strings.Split(mx, "\n")
	}
	return p, true, nil
}

// SaveSTSPolicy caches a policy for all nodes.
func (s *Store) SaveSTSPolicy(ctx context.Context, p STSPolicy) error {
	_, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(
		`INSERT OR REPLACE INTO mta_sts (domain, policy_id, mode, mx, fetched_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		p.Domain, p.ID, p.Mode, strings.Join(p.MX, "\n"), p.FetchedAt.Unix(), p.ExpiresAt.Unix()))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

// TrimSTSPolicies removes policies that expired before the given time.
func (s *Store) TrimSTSPolicies(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecuteIdempotent(ctx, rqlite.Stmt(`DELETE FROM mta_sts WHERE expires_at < ?`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}
