package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/robinmail/robinmail/internal/blob"
)

// Message is a raw message to store in one account.
type Message struct {
	Raw []byte
	// Key makes the delivery idempotent: a second delivery with the same key
	// to the same account stores nothing and returns ErrDuplicate. Empty
	// turns that off.
	Key string
	// MailboxID is the target mailbox. If it is 0 the target is the mailbox
	// with Role, or the inbox when the account has no such mailbox. An empty
	// Role is the inbox.
	MailboxID int64
	Role      string
	// MailboxIDs, when not empty, puts the email in all of these mailboxes
	// and replaces MailboxID and Role.
	MailboxIDs []int64
	// IfInState, when not 0, is the state the account must be in.
	IfInState int64
	// ReceivedAt defaults to now.
	ReceivedAt time.Time
	Keywords   []string
	// IgnoreQuota stores the message also when the account is full: for a
	// notification the server itself owes the account (a bounce), which
	// would otherwise be lost.
	IgnoreQuota bool
}

// sqlNewEmail is the id of the email a delivery batch is creating, valid
// after the bump.
const sqlNewEmail = `(SELECT next_email - 1 FROM account WHERE id = :a)`

// Deliver stores a message: the encrypted blob first, then one transaction that takes
// the email id, assigns the thread, takes the mailbox's next UID and writes
// the change log. A crash in between leaves a blob nobody references, never
// an email without a blob.
//
// The thread is that of the first known Message-ID among the message's own,
// its References and its In-Reply-To; otherwise a new one. It never changes
// afterwards. Every delivery takes a thread id from the counter, also when
// it joins a thread, so thread ids have gaps.
func (s *Store) Deliver(ctx context.Context, account string, m Message) (Email, error) {
	keywords, err := cleanKeywords(m.Keywords)
	if err != nil {
		return Email{}, err
	}
	sum := sha256.Sum256(m.Raw)
	blobID := hex.EncodeToString(sum[:])
	info := summarize(m.Raw)
	received := m.ReceivedAt
	if received.IsZero() {
		received = s.Now()
	}
	role := m.Role
	if role == "" {
		role = RoleInbox
	}

	key, err := s.blobKey(ctx, account)
	if err != nil {
		return Email{}, err
	}
	targets := m.MailboxIDs
	if len(targets) == 0 && m.MailboxID != 0 {
		targets = []int64{m.MailboxID}
	}
	sealed, err := blob.Encrypt(key, account, blobID, bytes.NewReader(m.Raw))
	if err != nil {
		return Email{}, fmt.Errorf("store: %w", err)
	}
	if err := s.blobs.Put(ctx, blob.Key(account, blobID), sealed); err != nil {
		return Email{}, fmt.Errorf("store: %w", err)
	}

	attachment := 0
	if info.hasAttachment {
		attachment = 1
	}

	b := s.newBatch(account)
	b.guardAccount()
	b.ifState(m.IfInState)
	if !m.IgnoreQuota {
		// Before the insert, so it sees what the account held until now.
		b.guardQuota(s.DefaultQuota, len(m.Raw))
	}
	var into string // a SQL condition on mailbox m: the mailboxes the email goes in
	if len(targets) > 0 {
		vals, kv := values("t", targets)
		for k := 0; k < len(kv); k += 2 {
			b.base[kv[k].(string)] = kv[k+1]
		}
		into = `m.id IN (VALUES ` + vals + `)`
		b.guard(ErrNoMailbox, `(SELECT count(*) FROM mailbox m WHERE m.account_id = :a AND `+into+`) = :targets`, "targets", len(uniqueInts(targets)))
	} else {
		b.base["role"] = role
		into = `m.id = (SELECT id FROM mailbox WHERE account_id = :a AND role IN (:role, 'inbox') ORDER BY role = 'inbox' LIMIT 1)`
		b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox m WHERE m.account_id = :a AND `+into+`)`)
	}
	if m.Key != "" {
		// A repeated key fails here and rolls everything back. That also
		// makes the batch safe to send again after an unknown outcome: if
		// the first run was applied, the second is refused as a whole.
		b.idempotent()
		b.unique(ErrDuplicate)
		b.add(`INSERT INTO delivered (account_id, key, at) VALUES (:a, :key, :now)`, "key", m.Key)
	}
	b.bump("next_email", "next_thread")

	thread := `a.next_thread - 1`
	refVals, refKV := values("r", info.refs)
	if len(info.refs) > 0 {
		thread = `COALESCE((SELECT min(thread_id) FROM thread_ref
			WHERE account_id = a.id AND message_id IN (VALUES ` + refVals + `)), a.next_thread - 1)`
	}
	b.add(`INSERT INTO email (account_id, id, blob, size, received_at, thread_id, message_id, sender, subject,
			preview, has_attachment, modseq, recipients, cc, bcc, sent_at)
		SELECT a.id, a.next_email - 1, :blob, :size, :received, `+thread+`, :message_id, :sender, :subject,
			:preview, :attachment, a.modseq, :recipients, :cc, :bcc, :sent_at
		FROM account a WHERE a.id = :a`,
		append([]any{"blob", blobID, "size", len(m.Raw), "received", received.Unix(), "message_id", info.messageID,
			"sender", info.sender, "subject", info.subject, "preview", info.preview, "attachment", attachment,
			"recipients", info.recipients, "cc", info.cc, "bcc", info.bcc, "sent_at", info.sentAt},
			refKV...)...)
	b.indexEmail(sqlNewEmail, info.index)
	if len(info.refs) > 0 {
		b.add(`INSERT OR IGNORE INTO thread_ref (account_id, message_id, thread_id)
			SELECT e.account_id, v.column1, e.thread_id FROM email e, (VALUES `+refVals+`) v
			WHERE e.account_id = :a AND e.id = `+sqlNewEmail, refKV...)
	}

	b.add(`INSERT INTO email_mailbox (account_id, email_id, mailbox_id, uid)
		SELECT m.account_id, ` + sqlNewEmail + `, m.id, m.uidnext FROM mailbox m
		WHERE m.account_id = :a AND ` + into)
	b.add(`UPDATE mailbox SET uidnext = uidnext + 1, modseq = ` + sqlModseq + `
		WHERE account_id = :a AND id IN (SELECT mailbox_id FROM email_mailbox WHERE account_id = :a AND email_id = ` + sqlNewEmail + `)`)
	if len(keywords) > 0 {
		vals, kv := values("k", keywords)
		b.add(`INSERT INTO keyword (account_id, email_id, keyword)
			SELECT :a, `+sqlNewEmail+`, v.column1 FROM (VALUES `+vals+`) v`, kv...)
	}

	b.change("Email", `next_email - 1 AS id, 'created' AS op FROM account WHERE id = :a`)
	b.change("Mailbox", `mailbox_id AS id, 'counts' AS op FROM email_mailbox
		WHERE account_id = :a AND email_id = `+sqlNewEmail)
	b.change("Thread", `e.thread_id AS id,
		CASE WHEN EXISTS (SELECT 1 FROM email o WHERE o.account_id = :a AND o.thread_id = e.thread_id AND o.id <> e.id)
			THEN 'updated' ELSE 'created' END AS op
		FROM email e WHERE e.account_id = :a AND e.id = `+sqlNewEmail)
	b.add(`SELECT ` + emailColumns + ` FROM email e WHERE e.account_id = :a AND e.id = ` + sqlNewEmail)

	res, err := s.run(ctx, b)
	if err != nil {
		return Email{}, err
	}
	rows := res[len(res)-1].Values
	if len(rows) != 1 {
		return Email{}, fmt.Errorf("store: delivery returned %d rows", len(rows))
	}
	return scanEmail(rows[0]), nil
}

func uniqueInts(list []int64) map[int64]bool {
	out := map[int64]bool{}
	for _, v := range list {
		out[v] = true
	}
	return out
}
