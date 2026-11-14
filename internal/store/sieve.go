package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// SieveScript is a Sieve script of an account (RFC 9661 2). The script
// itself is a blob of the account, encrypted like a message. At most one
// script of an account is active: the one that runs when mail arrives.
type SieveScript struct {
	ID        int64
	Name      string
	Blob      string // SHA-256 of the script
	Size      int64
	Active    bool
	CreatedAt time.Time
	Modseq    int64
}

// Limits on the scripts of one account.
const (
	MaxSieveScripts = 16
	// RFC 9661 1.3.1: "for compatibility with ManageSieve, this MUST be
	// at least 512 (up to 128 Unicode characters)".
	MaxSieveNameSize = 512
)

var (
	ErrNoSieveScript = errors.New("no such sieve script")
	ErrSieveName     = errors.New("a sieve script with that name already exists")
	ErrSieveBadName  = errors.New("not a name for a sieve script")
	ErrSieveLimit    = errors.New("too many sieve scripts")
	// ErrSieveActive is returned when the active script is destroyed:
	// deactivate it first (RFC 9661 2.2, scriptIsActive).
	ErrSieveActive = errors.New("the sieve script is active")
)

const (
	sieveScriptPrefix = 's'
	sieveColumns      = `id, name, blob, size, active, created_at, modseq`
)

func SieveScriptID(n int64) string              { return formatID(sieveScriptPrefix, n) }
func ParseSieveScriptID(s string) (int64, bool) { return parseID(sieveScriptPrefix, s) }

func scanSieve(row []any) SieveScript {
	return SieveScript{ID: asInt(row[0]), Name: asString(row[1]), Blob: asString(row[2]), Size: asInt(row[3]),
		Active: asInt(row[4]) != 0, CreatedAt: time.Unix(asInt(row[5]), 0).UTC(), Modseq: asInt(row[6])}
}

// validSieveName holds a script name to RFC 9661 2 (and RFC 5804 1.6, where
// the rule comes from): UTF-8 without control characters, not empty, of
// bounded length.
func validSieveName(name string) bool {
	if name == "" || len(name) > MaxSieveNameSize || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

// SieveScripts lists the scripts of an account by id.
func (s *Store) SieveScripts(ctx context.Context, account string) ([]SieveScript, error) {
	res, err := s.query(ctx, account, q(`SELECT `+sieveColumns+` FROM sieve_script WHERE account_id = :a ORDER BY id`))
	if err != nil {
		return nil, err
	}
	out := make([]SieveScript, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanSieve(row))
	}
	return out, nil
}

// SieveScriptContent reads a script.
func (s *Store) SieveScriptContent(ctx context.Context, account string, script SieveScript) ([]byte, error) {
	return s.readBlob(ctx, account, script.Blob)
}

// CreateSieveScript stores a script, not active. The caller has checked
// that it is a valid script.
func (s *Store) CreateSieveScript(ctx context.Context, account, name string, content []byte, ifState int64) (SieveScript, int64, error) {
	if !validSieveName(name) {
		return SieveScript{}, 0, ErrSieveBadName
	}
	sum := sha256.Sum256(content)
	blobID := hex.EncodeToString(sum[:])
	if err := s.putBlob(ctx, account, blobID, content); err != nil {
		return SieveScript{}, 0, err
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrSieveLimit, `(SELECT count(*) FROM sieve_script WHERE account_id = :a) < :max`, "max", MaxSieveScripts)
	b.bump("next_sieve")
	b.unique(ErrSieveName)
	b.add(`INSERT INTO sieve_script (account_id, id, name, blob, size, active, created_at, modseq)
		SELECT a.id, a.next_sieve - 1, :name, :blob, :size, 0, :now, a.modseq FROM account a WHERE a.id = :a`,
		"name", name, "blob", blobID, "size", len(content))
	b.change("SieveScript", `next_sieve - 1 AS id, 'created' AS op FROM account WHERE id = :a`)
	b.add(`SELECT ` + sieveColumns + ` FROM sieve_script WHERE account_id = :a AND id = (SELECT next_sieve - 1 FROM account WHERE id = :a)`)
	res, err := s.run(ctx, b)
	if err != nil {
		return SieveScript{}, 0, err
	}
	script := scanSieve(res[len(res)-1].Values[0])
	return script, script.Modseq, nil
}

// UpdateSieveScript gives a script another name (name not nil) or other
// content (content not nil).
func (s *Store) UpdateSieveScript(ctx context.Context, account string, id int64, name *string, content []byte, ifState int64) (int64, error) {
	if name != nil && !validSieveName(*name) {
		return 0, ErrSieveBadName
	}
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoSieveScript, `EXISTS (SELECT 1 FROM sieve_script WHERE account_id = :a AND id = :id)`)
	b.bump()
	if name != nil {
		b.unique(ErrSieveName)
		b.add(`UPDATE sieve_script SET name = :name, modseq = `+sqlModseq+` WHERE account_id = :a AND id = :id`, "name", *name)
	}
	if content != nil {
		sum := sha256.Sum256(content)
		blobID := hex.EncodeToString(sum[:])
		if err := s.putBlob(ctx, account, blobID, content); err != nil {
			return 0, err
		}
		b.add(`UPDATE sieve_script SET blob = :blob, size = :size, modseq = `+sqlModseq+` WHERE account_id = :a AND id = :id`,
			"blob", blobID, "size", len(content))
	}
	b.change("SieveScript", `:id AS id, 'updated' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// ActivateSieveScript makes one script the active one, or with id 0 none.
// The script that was active stops being so in the same batch.
func (s *Store) ActivateSieveScript(ctx context.Context, account string, id, ifState int64) (int64, error) {
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	if id != 0 {
		b.guard(ErrNoSieveScript, `EXISTS (SELECT 1 FROM sieve_script WHERE account_id = :a AND id = :id)`)
	}
	b.bump()
	b.change("SieveScript", `id AS id, 'updated' AS op FROM sieve_script WHERE account_id = :a AND active <> (id = :id)`)
	// Two statements, the old one off first: the index that keeps it to
	// one active script is checked row by row.
	b.add(`UPDATE sieve_script SET active = 0, modseq = ` + sqlModseq + ` WHERE account_id = :a AND active = 1 AND id <> :id`)
	b.add(`UPDATE sieve_script SET active = 1, modseq = ` + sqlModseq + ` WHERE account_id = :a AND active = 0 AND id = :id`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// DestroySieveScript removes a script that is not active.
func (s *Store) DestroySieveScript(ctx context.Context, account string, id, ifState int64) (int64, error) {
	b := s.newBatch(account)
	b.base["id"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoSieveScript, `EXISTS (SELECT 1 FROM sieve_script WHERE account_id = :a AND id = :id)`)
	b.guard(ErrSieveActive, `NOT EXISTS (SELECT 1 FROM sieve_script WHERE account_id = :a AND id = :id AND active = 1)`)
	b.bump()
	b.add(`DELETE FROM sieve_script WHERE account_id = :a AND id = :id`)
	b.change("SieveScript", `:id AS id, 'destroyed' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// ActiveSieveScript returns the script that runs when mail arrives for the
// account: the SHA-256 it is stored under, which changes when the script
// does and so names a parsed copy, and a function that reads it. ok is
// false when the account has no active script.
func (s *Store) ActiveSieveScript(ctx context.Context, account string) (blobID string, read func() ([]byte, error), ok bool, err error) {
	res, err := s.query(ctx, account, q(`SELECT blob FROM sieve_script WHERE account_id = :a AND active = 1`))
	if err != nil {
		return "", nil, false, err
	}
	if len(res[0].Values) == 0 {
		return "", nil, false, nil
	}
	blobID = asString(res[0].Values[0][0])
	return blobID, func() ([]byte, error) { return s.readBlob(ctx, account, blobID) }, true, nil
}

// VacationOnce answers whether an automatic reply may go out now: it may
// when none with the same key went out within the period. Asking counts:
// a yes is remembered, so the next question within the period gets a no.
// It is one statement, so of several nodes that ask at once for the same
// key one gets the yes. The key names the reply and whom it is for (RFC
// 5230 4.2); the caller hashes them into it.
func (s *Store) VacationOnce(ctx context.Context, account, key string, period time.Duration) (bool, error) {
	b := s.newBatch(account)
	b.add(`INSERT INTO vacation_sent (account_id, key, at) VALUES (:a, :key, :now)
		ON CONFLICT (account_id, key) DO UPDATE SET at = excluded.at WHERE vacation_sent.at <= :cutoff`,
		"key", key, "cutoff", s.Now().Add(-period).Unix())
	res, err := s.run(ctx, b)
	if err != nil {
		return false, err
	}
	return res[0].RowsAffected > 0, nil
}

// VacationSentRetention is how long the record of an automatic reply is
// kept: longer than the longest period of "once per sender".
const VacationSentRetention = 45 * 24 * time.Hour

// TrimVacationSent forgets the replies sent before the given time.
func (s *Store) TrimVacationSent(ctx context.Context, before time.Time) (int64, error) {
	b := s.newBatch("")
	b.idempotent() // a delete by condition
	b.add(`DELETE FROM vacation_sent WHERE at < :before`, "before", before.Unix())
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return res[0].RowsAffected, nil
}

// MailboxPaths maps every mailbox of an account to its full name, the
// levels joined by "/", as IMAP shows it; the inbox is "INBOX".
func MailboxPaths(list []Mailbox) map[int64]string {
	byID := make(map[int64]Mailbox, len(list))
	for _, m := range list {
		byID[m.ID] = m
	}
	out := make(map[int64]string, len(list))
	for _, m := range list {
		path := m.Name
		if m.Role == RoleInbox && m.ParentID == 0 {
			path = "INBOX"
		}
		for p, depth := m.ParentID, 0; p != 0 && depth < 64; depth++ {
			parent := byID[p]
			name := parent.Name
			if parent.Role == RoleInbox && parent.ParentID == 0 {
				name = "INBOX"
			}
			path = name + "/" + path
			p = parent.ParentID
		}
		out[m.ID] = path
	}
	return out
}

// FindMailbox finds a mailbox by its full name: exactly, or else without
// regard to case. 0 when there is none.
func FindMailbox(list []Mailbox, path string) int64 {
	paths := MailboxPaths(list)
	for _, exact := range []bool{true, false} {
		for _, m := range list {
			if p := paths[m.ID]; p == path || (!exact && strings.EqualFold(p, path)) {
				return m.ID
			}
		}
	}
	return 0
}

// FlagKeyword turns an IMAP flag, as a Sieve script writes it (RFC 5232),
// into the keyword it is stored as: "\Seen" is "$seen", "\Flagged"
// "$flagged", "\Answered" "$answered", "\Draft" "$draft", "\Deleted"
// "$deleted"; a keyword stays what it is, in lower case. ok is false for
// what cannot be a keyword, such as another flag with a backslash.
func FlagKeyword(flag string) (string, bool) {
	switch strings.ToLower(flag) {
	case `\seen`:
		return "$seen", true
	case `\flagged`:
		return "$flagged", true
	case `\answered`:
		return "$answered", true
	case `\draft`:
		return "$draft", true
	case `\deleted`:
		return "$deleted", true
	}
	if !validKeyword(flag) {
		return "", false
	}
	return strings.ToLower(flag), true
}
