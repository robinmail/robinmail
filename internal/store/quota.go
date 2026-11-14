package store

import (
	"context"
	"errors"
)

// Quotas: how much mail an account may hold, in octets and in messages.
//
// What counts is the Emails of the account: each once, whatever number of
// mailboxes it is in, by the size of its message. Uploads, the queue and
// Sieve scripts do not count (they are small or go by themselves).
//
// The count holds under concurrency because nothing in Go counts. The
// account row carries used_bytes and used_count, and two triggers on the
// email table move them with every insert and delete, inside whatever
// batch does it. The limit is a guard of the batch that stores an Email
// (Deliver): rqlite runs batches one after another, so the guard always
// sees what the batches before it left, on whatever node they came from.

// ErrOverQuota is returned when storing an Email would take the account
// past its limit.
var ErrOverQuota = errors.New("the account is over its quota")

// Limits are the limits of an account or the defaults for all accounts. 0
// is no limit.
type Limits struct {
	Bytes    int64
	Messages int64
}

// Quota is an account's limits and what it uses.
type Quota struct {
	// Limits in effect: the account's own where it has them, else the
	// default. 0 is no limit.
	Limits
	// Own are the account's own limits; nil is "the default".
	OwnBytes, OwnMessages *int64
	UsedBytes             int64
	UsedMessages          int64
}

// Room reports whether a message of the given size fits, and if not
// whether it ever could: never is true when the message alone is larger
// than the limit.
func (q Quota) Room(size int64) (fits, never bool) {
	if q.Bytes > 0 && size > q.Bytes {
		return false, true
	}
	if q.Bytes > 0 && q.UsedBytes+size > q.Bytes {
		return false, false
	}
	if q.Messages > 0 && q.UsedMessages+1 > q.Messages {
		return false, false
	}
	return true, false
}

// sqlQuotaRoom is true when an Email of :size octets fits the account :a.
// :qbytes and :qmessages are the defaults.
const sqlQuotaRoom = `(SELECT (COALESCE(quota_bytes, :qbytes) = 0 OR used_bytes + :size <= COALESCE(quota_bytes, :qbytes))
	AND (COALESCE(quota_count, :qmessages) = 0 OR used_count + 1 <= COALESCE(quota_count, :qmessages))
	FROM account WHERE id = :a)`

// sqlQuotaRoomBytes is the octet half of sqlQuotaRoom.
const sqlQuotaRoomBytes = `(SELECT COALESCE(quota_bytes, :qbytes) = 0 OR used_bytes + :size <= COALESCE(quota_bytes, :qbytes)
	FROM account WHERE id = :a)`

// guardQuota makes the batch fail with ErrOverQuota unless an Email of
// that size fits.
func (b *batch) guardQuota(def Limits, size int) {
	b.guard(ErrOverQuota, sqlQuotaRoom, "qbytes", def.Bytes, "qmessages", def.Messages, "size", size)
}

func ownLimit(v any) *int64 {
	if v == nil {
		return nil
	}
	n := asInt(v)
	return &n
}

func (s *Store) quotaOf(ownBytes, ownMessages *int64, usedBytes, usedMessages int64) Quota {
	q := Quota{Limits: s.DefaultQuota, OwnBytes: ownBytes, OwnMessages: ownMessages, UsedBytes: usedBytes, UsedMessages: usedMessages}
	if ownBytes != nil {
		q.Bytes = *ownBytes
	}
	if ownMessages != nil {
		q.Messages = *ownMessages
	}
	return q
}

// Quota returns an account's limits and use.
func (s *Store) Quota(ctx context.Context, account string) (Quota, error) {
	res, err := s.query(ctx, account, q(`SELECT quota_bytes, quota_count, used_bytes, used_count FROM account WHERE id = :a`))
	if err != nil {
		return Quota{}, err
	}
	if len(res[0].Values) == 0 {
		return Quota{}, ErrNoAccount
	}
	row := res[0].Values[0]
	return s.quotaOf(ownLimit(row[0]), ownLimit(row[1]), asInt(row[2]), asInt(row[3])), nil
}

// SetQuota gives an account limits of its own: nil is the default of the
// configuration, 0 no limit. Lowering a limit below what the account
// holds removes nothing; it only stops new mail.
func (s *Store) SetQuota(ctx context.Context, account string, bytes, messages *int64) error {
	val := func(p *int64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	if (bytes != nil && *bytes < 0) || (messages != nil && *messages < 0) {
		return errors.New("store: a quota cannot be negative")
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.bump()
	b.add(`UPDATE account SET quota_bytes = :bytes, quota_count = :messages WHERE id = :a`, "bytes", val(bytes), "messages", val(messages))
	b.change("Quota", `1 AS id, 'updated' AS op`)
	_, err := s.run(ctx, b)
	return err
}

// RecountQuota sets an account's counters to what its emails add up to,
// and reports whether they were off. The triggers keep them right; this is
// for a database that was changed by hand.
func (s *Store) RecountQuota(ctx context.Context, account string) (changed bool, err error) {
	b := s.newBatch(account)
	b.idempotent() // sets the counters to what the emails say
	b.guardAccount()
	b.add(`UPDATE account SET
			used_bytes = (SELECT COALESCE(sum(size), 0) FROM email WHERE account_id = :a),
			used_count = (SELECT count(*) FROM email WHERE account_id = :a)
		WHERE id = :a AND (used_bytes <> (SELECT COALESCE(sum(size), 0) FROM email WHERE account_id = :a)
			OR used_count <> (SELECT count(*) FROM email WHERE account_id = :a))`)
	res, err := s.run(ctx, b)
	if err != nil {
		return false, err
	}
	return res[1].RowsAffected > 0, nil
}
