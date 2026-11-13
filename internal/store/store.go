// Package store holds all mail state: accounts, mailboxes, emails, threads
// and the change log in rqlite, and the raw messages as encrypted files in a
// blob store.
//
// rqlite has no interactive transactions, so every write here is one batch of
// statements that rqlite runs atomically. A batch cannot read, decide in Go
// and then write: it computes what it needs in SQL (take the counter, look up
// the thread, take uidnext). Preconditions are "guard" statements that fail,
// and so roll the batch back, when a row is missing.
//
// Every write to an account increments its modseq exactly once, stamps the
// rows it touched and appends to the changes table. The modseq is the JMAP
// state string.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/mikepage/robinmail/internal/blob"
	"github.com/mikepage/robinmail/internal/rqlite"
)

var (
	ErrNoAccount     = errors.New("no such account")
	ErrAccountExists = errors.New("account already exists")
	ErrNoMailbox     = errors.New("no such mailbox")
	ErrMailboxExists = errors.New("a mailbox with that name already exists")
	ErrMailboxName   = errors.New("invalid mailbox name")
	ErrNoEmail       = errors.New("no such email")
	ErrKeyword       = errors.New("invalid keyword")
	// ErrDuplicate is returned by Deliver when the delivery key was used
	// before: the message is already stored.
	ErrDuplicate = errors.New("already delivered")
	// ErrNoMailboxes is returned when an update would leave an email in no
	// mailbox at all. Destroy it instead.
	ErrNoMailboxes = errors.New("an email must be in at least one mailbox")
	// ErrCannotCalculateChanges is returned by Changes when the given state
	// is older than the change log reaches, or is not a state of the account.
	ErrCannotCalculateChanges = errors.New("cannot calculate changes from that state")
)

// Mailbox roles (RFC 8621 2, the IANA "IMAP Mailbox Name Attributes").
const (
	RoleInbox  = "inbox"
	RoleDrafts = "drafts"
	RoleSent   = "sent"
	RoleJunk   = "junk"
	RoleTrash  = "trash"
)

// Store is safe for concurrent use. Its only state is a cache of account
// keys, which never change, so any number of nodes can work on the same
// rqlite.
type Store struct {
	db     *rqlite.Client
	blobs  blob.Store
	master *blob.MasterKey
	keys   sync.Map // account id -> []byte, the unwrapped blob key
	// Now is the clock, replaceable in tests.
	Now func() time.Time
}

// New returns a store. master wraps the keys that encrypt each account's
// blobs; every node of a cluster needs the same one.
func New(db *rqlite.Client, blobs blob.Store, master *blob.MasterKey) *Store {
	return &Store{db: db, blobs: blobs, master: master, Now: time.Now}
}

// Email, Mailbox and Thread ids are per-account counters. To the outside they
// are short strings: a letter for the type and the number in base 36.
const (
	emailPrefix   = 'e'
	mailboxPrefix = 'm'
	threadPrefix  = 't'
)

func EmailID(n int64) string   { return formatID(emailPrefix, n) }
func MailboxID(n int64) string { return formatID(mailboxPrefix, n) }
func ThreadID(n int64) string  { return formatID(threadPrefix, n) }

func ParseEmailID(s string) (int64, bool)   { return parseID(emailPrefix, s) }
func ParseMailboxID(s string) (int64, bool) { return parseID(mailboxPrefix, s) }
func ParseThreadID(s string) (int64, bool)  { return parseID(threadPrefix, s) }

func formatID(prefix byte, n int64) string {
	return string(prefix) + strconv.FormatInt(n, 36)
}

// parseID accepts exactly what formatID produces, so every id has one
// spelling.
func parseID(prefix byte, s string) (int64, bool) {
	if len(s) < 2 || len(s) > 14 || s[0] != prefix {
		return 0, false
	}
	n, err := strconv.ParseInt(s[1:], 36, 64)
	if err != nil || n <= 0 || formatID(prefix, n) != s {
		return 0, false
	}
	return n, true
}

// SQL fragments shared by the batches. :a is always the account id.
const (
	// sqlModseq is the modseq of the running write, valid after the bump.
	sqlModseq = `(SELECT modseq FROM account WHERE id = :a)`
)

// batch collects the statements of one write. All of them share the
// parameters in base; add and guard take extra ones as name, value pairs.
type batch struct {
	base  map[string]any
	stmts []rqlite.Statement
	fail  map[int]error // statement index -> what its failure means
}

func (s *Store) newBatch(account string) *batch {
	return &batch{
		base: map[string]any{"a": account, "now": s.Now().Unix()},
		fail: map[int]error{},
	}
}

var paramRE = regexp.MustCompile(`:[a-z][a-z0-9_]*`)

// add appends a statement. Only the parameters the SQL names are sent. A
// name without a value is a bug in this package: SQLite would bind NULL.
func (b *batch) add(sql string, kv ...any) {
	if len(kv)%2 != 0 {
		panic("store: odd number of parameter arguments")
	}
	named := map[string]any{}
	for _, name := range paramRE.FindAllString(sql, -1) {
		name = name[1:]
		v, ok := b.base[name]
		for i := 0; i < len(kv); i += 2 {
			if kv[i].(string) == name {
				v, ok = kv[i+1], true
			}
		}
		if !ok {
			panic("store: no value for parameter :" + name)
		}
		named[name] = v
	}
	b.stmts = append(b.stmts, rqlite.Statement{SQL: sql, Named: named})
}

// guard makes the batch fail with err unless cond, a SQL expression, holds.
// The guard table only accepts ok = 0, so inserting a 1 breaks its CHECK
// constraint and rqlite rolls the batch back. The table stays empty.
func (b *batch) guard(err error, cond string, kv ...any) {
	b.fail[len(b.stmts)] = err
	b.add(`INSERT INTO guard (ok) SELECT 1 WHERE NOT (`+cond+`)`, kv...)
}

// unique makes a UNIQUE violation in the next statement mean err.
func (b *batch) unique(err error) { b.fail[len(b.stmts)] = err }

// guardAccount is the first statement of every batch.
func (b *batch) guardAccount() {
	b.guard(ErrNoAccount, `EXISTS (SELECT 1 FROM account WHERE id = :a)`)
}

// bump increments the account's modseq, plus the named counters.
func (b *batch) bump(counters ...string) {
	sql := `UPDATE account SET modseq = modseq + 1`
	for _, c := range counters {
		sql += `, ` + c + ` = ` + c + ` + 1`
	}
	b.add(sql + ` WHERE id = :a`)
}

// change appends to the change log. sel is the rest of a SELECT that yields
// id and op, for example "5, 'updated'" or "id, 'created' FROM mailbox ...".
// A second change to the same object in one batch is ignored: within one
// modseq an object has one op.
func (b *batch) change(typ, sel string, kv ...any) {
	b.add(`INSERT OR IGNORE INTO changes (account_id, modseq, type, id, op, at)
		SELECT :a, `+sqlModseq+`, '`+typ+`', c.id, c.op, :now FROM (SELECT `+sel+`) c`, kv...)
}

// run executes the batch and translates a failed statement.
func (s *Store) run(ctx context.Context, b *batch) ([]rqlite.Result, error) {
	res, err := s.db.Execute(ctx, b.stmts...)
	if err == nil {
		return res, nil
	}
	var e *rqlite.Error
	if errors.As(err, &e) {
		if mapped, ok := b.fail[e.Index]; ok && (e.Unique() || e.Check()) {
			return nil, mapped
		}
	}
	return nil, fmt.Errorf("store: %w", err)
}

// query runs read-only statements with weak consistency: the leader answers
// after checking, locally, that it still is the leader.
func (s *Store) query(ctx context.Context, account string, stmts ...[]any) ([]rqlite.Result, error) {
	b := &batch{base: map[string]any{"a": account}}
	for _, st := range stmts {
		b.add(st[0].(string), st[1:]...)
	}
	res, err := s.db.Query(ctx, rqlite.Weak, b.stmts...)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return res, nil
}

func q(sql string, kv ...any) []any { return append([]any{sql}, kv...) }

// Column helpers. rqlite returns integers as int64 and text as string.
func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
