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

	"github.com/robinmail/robinmail/internal/blob"
	"github.com/robinmail/robinmail/internal/rqlite"
)

var (
	ErrNoAccount     = errors.New("no such account")
	ErrAccountExists = errors.New("account already exists")
	ErrNoDomain      = errors.New("no such domain")
	ErrDomainExists  = errors.New("domain already exists")
	ErrDomainInUse   = errors.New("domain still has accounts")
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
	// ErrStateMismatch is returned by a write that was given a state to
	// expect (JMAP ifInState) when the account has moved on.
	ErrStateMismatch = errors.New("the account is not in the expected state")
	// Mailbox/set refusals.
	ErrMailboxHasChild = errors.New("the mailbox has child mailboxes")
	ErrMailboxHasEmail = errors.New("the mailbox has emails")
	ErrMailboxRole     = errors.New("a mailbox with a role cannot be destroyed")
	ErrMailboxLoop     = errors.New("a mailbox cannot be moved under itself")
	ErrNoBlob          = errors.New("no such blob")
	ErrNoSession       = errors.New("no such session")
	// Authentication (auth.go).
	ErrNoAppPassword   = errors.New("no such app password")
	ErrAppPasswords    = errors.New("too many app passwords")
	ErrNoTOTP          = errors.New("the account has no TOTP secret")
	ErrTOTPEnabled     = errors.New("TOTP is already on")
	ErrTOTPNotPending  = errors.New("no TOTP enrolment is waiting to be confirmed")
	ErrOIDCSubject     = errors.New("the account is linked to another subject at this provider")
	ErrPasswordChanged = errors.New("the password was changed meanwhile")
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
	// systems holds the ids of the system accounts known to exist.
	systems sync.Map
	// DefaultQuota are the limits of accounts that have none of their
	// own; zero is no limit. Set it before the store is used, and to the
	// same on every node.
	DefaultQuota Limits
	// Now is the clock, replaceable in tests.
	Now func() time.Time
	// OnChange, when set, is called with the account after every write that
	// moved an account's modseq. It is how this node's listeners (JMAP
	// event streams, IMAP sessions) hear of a change at once; see
	// internal/notify. Set it before the store is used.
	OnChange func(account string)
	// OnAuthChange, when set, is called with the account after a write
	// that withdrew a way into it (auth_seq moved): a revoked session or
	// app password, a changed password. Set it before the store is used.
	OnAuthChange func(account string)
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
	// again marks a batch that may be sent a second time when rqlite did
	// not say whether the first was applied. See run.
	again bool
	// bumped marks a batch that moves the account's modseq.
	bumped bool
}

// idempotent marks the batch as safe to run twice. Call it only with a
// comment that says why it is.
func (b *batch) idempotent() { b.again = true }

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

// ifState makes the batch fail with ErrStateMismatch unless the account is
// in the given state. 0 is no expectation. Being a statement of the batch,
// it holds for the write itself, not for a moment before it.
func (b *batch) ifState(state int64) {
	if state != 0 {
		b.guard(ErrStateMismatch, `(SELECT modseq FROM account WHERE id = :a) = :ifstate`, "ifstate", state)
	}
}

// bump increments the account's modseq, plus the named counters.
func (b *batch) bump(counters ...string) {
	b.bumped = true
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
//
// When a leader dies in the middle of a write, rqlite cannot say whether the
// batch was applied. Most batches here are not safe to send again: they take
// a counter and bump the modseq, so a second run would do the work twice, or
// they carry a guard ("no mailbox of that name yet", ifInState) that a
// second run would trip over its own first run and report as a refusal.
// Those fail with rqlite.ErrUnknownOutcome and the caller's caller decides:
// a JMAP client gets serverFail and syncs, a command prints the error.
//
// The batches marked idempotent are sent again. How each write qualifies:
//
//	Deliver with a key     again: the delivered row is its guard; a second run
//	                       fails as a whole with ErrDuplicate, which callers
//	                       take as "stored"
//	Deliver without a key  not again: a second run stores a second email
//	CreateAccount          not again: would report ErrAccountExists for itself
//	AddDomain/RemoveDomain not again: would report ErrDomainExists/ErrNoDomain
//	NewMailbox, UpdateMailbox, DestroyMailbox, UpdateEmail, DestroyEmail
//	                       not again: counters, modseq, and guards as above
//	Reindex                again: sets columns to values computed from the blob
//	CreateSession          again: INSERT OR IGNORE of a random token's hash
//	DeleteSession, TrimSessions, TrimChanges, TrimDelivered, TrimLoginFailures,
//	TrimAuthEvents         again: deletes by condition; only the count of the
//	                       second run is lower
//	TouchSession, UseAppPassword
//	                       again: set a row to given values
//	RevokeSession, RevokeSessions, RevokeAppPassword, DisableTOTP
//	                       again: deletes; auth_seq moves once more, which
//	                       only makes open connections check once more
//	CreateAppPassword      again: INSERT OR IGNORE of a random id and hash
//	SetPendingTOTP, ConfirmTOTP, ReplaceRecoveryCodes, SetPassword
//	                       not again: guarded by the state they start from
//	UseTOTPStep, UseRecoveryCode
//	                       not again: the answer is whether this run took
//	                       the step or the code; a second run would say no
//	AddAuthEvent           not again: it appends
//	LinkOIDC               again: INSERT OR IGNORE, then a check
//	RegisterNode, Heartbeat, RemoveNode
//	                       again: set a row to given values
//	AddLoginFailure        not again: it counts
//	CreateSieveScript, UpdateSieveScript, ActivateSieveScript,
//	DestroySieveScript     not again: counters, modseq, and guards
//	CountTLSResult         not again: it counts
//	ClaimTLSReport         not again: its answer is whether this run claimed
//	NoteTLSReport, SaveTLSReport, TrimTLSReports
//	                       again: a row set to given values, INSERT OR
//	                       IGNORE by the report's id, deletes
//	SetQuota               not again: modseq
//	RecountQuota           again: sets the counters to what the emails say
//	SetVacationResponse    not again: modseq, and the ifInState guard
//	VacationOnce           not again: its answer is whether this run took
//	                       the turn; a second run would say no
//	TrimVacationSent       again: a delete by condition
//	AddAlias               again: INSERT OR IGNORE of given rows
//	RemoveAlias            not again: its answer is whether this run removed
//	Forward, Submit        again: a random key in the row (and the caller's
//	                       key in delivered) refuses a second run as a whole
//	ClearLoginFailures     again: a delete
//	Init                   again: CREATE IF NOT EXISTS; a migration is guarded
//	                       by the version it starts from
func (s *Store) run(ctx context.Context, b *batch) ([]rqlite.Result, error) {
	exec := s.db.Execute
	if b.again {
		exec = s.db.ExecuteIdempotent
	}
	res, err := exec(ctx, b.stmts...)
	if err == nil {
		if b.bumped && s.OnChange != nil {
			if account, _ := b.base["a"].(string); account != "" {
				s.OnChange(account)
			}
		}
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
//
// Read consistency, per kind of read:
//
//	weak    everything a user or a delivery reads: accounts, sessions,
//	        domains, mailboxes, emails, changes, the node list. It costs no
//	        Raft round trip. A leader that was just cut off from the others
//	        can still answer for about a second (until its lease runs out)
//	        from data that misses the newest writes. Nothing breaks on
//	        that: JMAP state strings make a client catch up, a delivery's
//	        own checks are guards inside its write batch, and a logged-out
//	        session or a removed domain is honoured a second late.
//	strong  the one read that decides a deletion: the blob sweeper's
//	        "is this still referenced". It goes through the Raft log, so
//	        only a leader that still has a quorum answers.
//	none    the login failure counters: any node answers from its own copy,
//	        without asking the leader, unless it lost the leader more than
//	        5 seconds ago. A count that is a moment old is good enough for a
//	        rate limit, and it keeps a login from waiting on the leader.
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
