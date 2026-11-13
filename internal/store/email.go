package store

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/blob"
)

// Email is one stored message. Its ID does not depend on the mailboxes it is
// in or on its blob, so moving and labelling keep it.
type Email struct {
	ID            int64
	ThreadID      int64
	Blob          string // SHA-256 of the raw message, hex
	Size          int64
	ReceivedAt    time.Time
	MessageID     string
	Sender        string
	Subject       string
	Preview       string
	HasAttachment bool
	Modseq        int64
	// Mailboxes maps each mailbox the email is in to its IMAP UID there.
	Mailboxes map[int64]uint32
	Keywords  []string // sorted
}

// HasKeyword reports whether the email carries the keyword, e.g. "$seen".
func (e Email) HasKeyword(k string) bool {
	for _, have := range e.Keywords {
		if have == k {
			return true
		}
	}
	return false
}

const emailColumns = `e.id, e.thread_id, e.blob, e.size, e.received_at, e.message_id, e.sender, e.subject, e.preview,
	e.has_attachment, e.modseq,
	(SELECT group_concat(x.mailbox_id || ':' || x.uid, ' ') FROM email_mailbox x WHERE x.account_id = e.account_id AND x.email_id = e.id),
	(SELECT group_concat(k.keyword, ' ') FROM keyword k WHERE k.account_id = e.account_id AND k.email_id = e.id)`

func scanEmail(row []any) Email {
	e := Email{ID: asInt(row[0]), ThreadID: asInt(row[1]), Blob: asString(row[2]), Size: asInt(row[3]),
		ReceivedAt: time.Unix(asInt(row[4]), 0).UTC(), MessageID: asString(row[5]), Sender: asString(row[6]),
		Subject: asString(row[7]), Preview: asString(row[8]), HasAttachment: asInt(row[9]) != 0, Modseq: asInt(row[10]),
		Mailboxes: map[int64]uint32{}}
	for _, pair := range strings.Fields(asString(row[11])) {
		m, uid, _ := strings.Cut(pair, ":")
		id, _ := strconv.ParseInt(m, 10, 64)
		u, _ := strconv.ParseUint(uid, 10, 32)
		e.Mailboxes[id] = uint32(u)
	}
	e.Keywords = strings.Fields(asString(row[12]))
	sort.Strings(e.Keywords)
	return e
}

// Email reads one email's metadata.
func (s *Store) Email(ctx context.Context, account string, id int64) (Email, error) {
	res, err := s.query(ctx, account,
		q(`SELECT `+emailColumns+` FROM email e WHERE e.account_id = :a AND e.id = :e`, "e", id))
	if err != nil {
		return Email{}, err
	}
	if len(res[0].Values) == 0 {
		return Email{}, ErrNoEmail
	}
	return scanEmail(res[0].Values[0]), nil
}

// Emails lists the emails in a mailbox in UID order.
func (s *Store) Emails(ctx context.Context, account string, mailboxID int64) ([]Email, error) {
	res, err := s.query(ctx, account,
		q(`SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m`, "m", mailboxID),
		q(`SELECT `+emailColumns+` FROM email_mailbox em
			JOIN email e ON e.account_id = em.account_id AND e.id = em.email_id
			WHERE em.account_id = :a AND em.mailbox_id = :m ORDER BY em.uid`, "m", mailboxID))
	if err != nil {
		return nil, err
	}
	if len(res[0].Values) == 0 {
		return nil, ErrNoMailbox
	}
	out := make([]Email, 0, len(res[1].Values))
	for _, row := range res[1].Values {
		out = append(out, scanEmail(row))
	}
	return out, nil
}

// ThreadEmails returns the ids of the emails in a thread, oldest first.
func (s *Store) ThreadEmails(ctx context.Context, account string, threadID int64) ([]int64, error) {
	res, err := s.query(ctx, account,
		q(`SELECT id FROM email WHERE account_id = :a AND thread_id = :t ORDER BY received_at, id`, "t", threadID))
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, asInt(row[0]))
	}
	return out, nil
}

// Open returns the raw message. The caller closes it.
func (s *Store) Open(ctx context.Context, account string, id int64) (io.ReadCloser, Email, error) {
	e, err := s.Email(ctx, account, id)
	if err != nil {
		return nil, Email{}, err
	}
	r, err := s.blobs.Get(ctx, blob.Key(account, e.Blob))
	if err != nil {
		return nil, Email{}, fmt.Errorf("store: email %s: %w", EmailID(id), err)
	}
	return r, e, nil
}

// Update describes a change to one email. All of it is applied in one
// transaction, removals before additions.
type Update struct {
	// AddMailboxes labels the email. A mailbox it is already in is left
	// alone; a new one gives the email that mailbox's next UID.
	AddMailboxes []int64
	// RemoveMailboxes takes labels away. The email must end up in at least
	// one mailbox.
	RemoveMailboxes []int64
	// ReplaceKeywords drops all keywords first, so AddKeywords becomes the
	// whole set.
	ReplaceKeywords bool
	AddKeywords     []string
	RemoveKeywords  []string

	// requireIn makes the update fail unless the email is in this mailbox.
	requireIn int64
}

func (u Update) empty() bool {
	return len(u.AddMailboxes) == 0 && len(u.RemoveMailboxes) == 0 && !u.ReplaceKeywords &&
		len(u.AddKeywords) == 0 && len(u.RemoveKeywords) == 0
}

// validKeyword follows RFC 8621 4.1.1: 1 to 255 printable ASCII characters
// without ( ) { ] % * " \, stored in lowercase.
func validKeyword(k string) bool {
	if k == "" || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`(){]%*"\`, c) >= 0 {
			return false
		}
	}
	return true
}

func cleanKeywords(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, k := range in {
		if !validKeyword(k) {
			return nil, fmt.Errorf("%w: %q", ErrKeyword, k)
		}
		k = strings.ToLower(k)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out, nil
}

// values builds "(:p0),(:p1)" and the matching parameters for a list.
func values[T any](prefix string, list []T) (sql string, kv []any) {
	parts := make([]string, len(list))
	for i, v := range list {
		name := prefix + strconv.Itoa(i)
		parts[i] = "(:" + name + ")"
		kv = append(kv, name, v)
	}
	return strings.Join(parts, ","), kv
}

const sqlInMailbox = `EXISTS (SELECT 1 FROM email_mailbox WHERE account_id = :a AND email_id = :e AND mailbox_id = :m)`

// UpdateEmail changes the mailboxes and keywords of an email and returns the
// new state.
func (s *Store) UpdateEmail(ctx context.Context, account string, id int64, u Update) (int64, error) {
	add, err := cleanKeywords(u.AddKeywords)
	if err != nil {
		return 0, err
	}
	remove, err := cleanKeywords(u.RemoveKeywords)
	if err != nil {
		return 0, err
	}
	if u.empty() {
		return s.State(ctx, account)
	}

	b := s.newBatch(account)
	b.base["e"] = id
	b.guardAccount()
	b.guard(ErrNoEmail, `EXISTS (SELECT 1 FROM email WHERE account_id = :a AND id = :e)`)
	if u.requireIn != 0 {
		b.guard(ErrNoEmail, sqlInMailbox, "m", u.requireIn)
	}
	for _, m := range u.AddMailboxes {
		b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`, "m", m)
	}
	b.bump()

	for _, m := range u.RemoveMailboxes {
		b.change("Mailbox", `:m AS id, 'updated' AS op WHERE `+sqlInMailbox, "m", m)
		b.add(`UPDATE mailbox SET modseq = `+sqlModseq+` WHERE account_id = :a AND id = :m AND `+sqlInMailbox, "m", m)
		b.add(`DELETE FROM email_mailbox WHERE account_id = :a AND email_id = :e AND mailbox_id = :m`, "m", m)
	}
	for _, m := range u.AddMailboxes {
		// The three statements share one condition: the email is not in the
		// mailbox yet. The UPDATE takes the UID, the INSERT uses it.
		b.change("Mailbox", `:m AS id, 'updated' AS op WHERE NOT `+sqlInMailbox, "m", m)
		b.add(`UPDATE mailbox SET uidnext = uidnext + 1, modseq = `+sqlModseq+`
			WHERE account_id = :a AND id = :m AND NOT `+sqlInMailbox, "m", m)
		b.add(`INSERT INTO email_mailbox (account_id, email_id, mailbox_id, uid)
			SELECT :a, :e, m.id, m.uidnext - 1 FROM mailbox m
			WHERE m.account_id = :a AND m.id = :m AND NOT `+sqlInMailbox, "m", m)
	}

	if u.ReplaceKeywords {
		b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id = :e`)
	}
	if len(add) > 0 {
		vals, kv := values("k", add)
		b.add(`INSERT OR IGNORE INTO keyword (account_id, email_id, keyword)
			SELECT :a, :e, v.column1 FROM (VALUES `+vals+`) v`, kv...)
	}
	if len(remove) > 0 {
		vals, kv := values("k", remove)
		b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id = :e AND keyword IN (VALUES `+vals+`)`, kv...)
	}
	if u.ReplaceKeywords || len(add) > 0 || len(remove) > 0 {
		// Keywords change a mailbox's unread count.
		b.change("Mailbox", `mailbox_id AS id, 'updated' AS op FROM email_mailbox WHERE account_id = :a AND email_id = :e`)
		b.add(`UPDATE mailbox SET modseq = ` + sqlModseq + ` WHERE account_id = :a
			AND id IN (SELECT mailbox_id FROM email_mailbox WHERE account_id = :a AND email_id = :e)`)
	}

	b.guard(ErrNoMailboxes, `EXISTS (SELECT 1 FROM email_mailbox WHERE account_id = :a AND email_id = :e)`)
	b.add(`UPDATE email SET modseq = ` + sqlModseq + ` WHERE account_id = :a AND id = :e`)
	b.change("Email", `:e AS id, 'updated' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// MoveEmail takes the email out of one mailbox and puts it in another, in one
// transaction. The email keeps its id and gets a new UID in the target, as
// IMAP expects. It fails with ErrNoEmail if the email is not in from.
func (s *Store) MoveEmail(ctx context.Context, account string, id, from, to int64) (int64, error) {
	if from == to {
		return 0, fmt.Errorf("store: move within mailbox %s", MailboxID(from))
	}
	return s.UpdateEmail(ctx, account, id, Update{
		RemoveMailboxes: []int64{from}, AddMailboxes: []int64{to}, requireIn: from})
}

// DestroyEmail removes the email from every mailbox and returns the new
// state. The blob stays: another email of the account may share it, and
// removing unreferenced blobs is the sweeper's job.
func (s *Store) DestroyEmail(ctx context.Context, account string, id int64) (int64, error) {
	const others = `EXISTS (SELECT 1 FROM email o WHERE o.account_id = :a AND o.thread_id = e.thread_id AND o.id <> :e)`
	b := s.newBatch(account)
	b.base["e"] = id
	b.guardAccount()
	b.guard(ErrNoEmail, `EXISTS (SELECT 1 FROM email WHERE account_id = :a AND id = :e)`)
	b.bump()
	b.change("Mailbox", `mailbox_id AS id, 'updated' AS op FROM email_mailbox WHERE account_id = :a AND email_id = :e`)
	b.add(`UPDATE mailbox SET modseq = ` + sqlModseq + ` WHERE account_id = :a
		AND id IN (SELECT mailbox_id FROM email_mailbox WHERE account_id = :a AND email_id = :e)`)
	b.change("Thread", `e.thread_id AS id, CASE WHEN `+others+` THEN 'updated' ELSE 'destroyed' END AS op
		FROM email e WHERE e.account_id = :a AND e.id = :e`)
	// A thread that loses its last email is gone, so a late reply starts a
	// new thread instead of reviving a destroyed id.
	b.add(`DELETE FROM thread_ref WHERE account_id = :a
		AND thread_id = (SELECT thread_id FROM email WHERE account_id = :a AND id = :e)
		AND NOT EXISTS (SELECT 1 FROM email o WHERE o.account_id = :a AND o.thread_id = thread_ref.thread_id AND o.id <> :e)`)
	b.change("Email", `:e AS id, 'destroyed' AS op`)
	b.add(`DELETE FROM email_mailbox WHERE account_id = :a AND email_id = :e`)
	b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id = :e`)
	b.add(`DELETE FROM email WHERE account_id = :a AND id = :e`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}
