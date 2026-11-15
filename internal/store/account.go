package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"

	"github.com/robinmail/robinmail/internal/rqlite"
)

// Account is one mail user. ID is random and never changes, also if the
// address ever does. State is the modseq: the JMAP state string.
type Account struct {
	ID           string
	Address      string
	PasswordHash string
	CreatedAt    time.Time
	State        int64
	// AuthSeq moves whenever a way into the account is withdrawn: a changed
	// password, a revoked session or app password, the first app password.
	// Connections that stay open compare it to know when to check theirs.
	AuthSeq           int64
	PasswordChangedAt time.Time
	// AppPasswords is how many app passwords the account has. With any, the
	// account password no longer opens IMAP, SMTP and JMAP over Basic auth.
	AppPasswords int
	// TOTP says whether the web login asks for a code.
	TOTP bool
	// Quota is what the account may hold and holds (quota.go).
	Quota Quota
}

// NoPassword is the password_hash of an account that cannot log in with a
// password: one that an OIDC login made. No password verifies against it.
const NoPassword = "!"

// defaultMailboxes are created with every account, with ids 1 to 5.
var defaultMailboxes = []struct{ name, role string }{
	{"Inbox", RoleInbox}, {"Drafts", RoleDrafts}, {"Sent", RoleSent}, {"Junk", RoleJunk}, {"Trash", RoleTrash},
}

func newAccountID() string {
	var b [10]byte
	rand.Read(b[:])
	return strings.ToLower(base32.StdEncoding.EncodeToString(b[:]))
}

// CreateAccount creates the account with its default mailboxes. address is
// the canonical (lowercase) address, passwordHash a PHC string. The domain of
// the address has to be in the domain table (ErrNoDomain); the check is part
// of the batch, so a domain cannot be removed from under a new account.
func (s *Store) CreateAccount(ctx context.Context, address, passwordHash string) (Account, error) {
	id := newAccountID()
	key, wrapped, err := s.master.NewAccountKey(id)
	if err != nil {
		return Account{}, fmt.Errorf("store: %w", err)
	}
	b := s.newBatch(id)
	b.guard(ErrNoDomain, `EXISTS (SELECT 1 FROM domain WHERE name = :domain)`,
		"domain", address[strings.LastIndexByte(address, '@')+1:])
	b.unique(ErrAccountExists)
	b.add(`INSERT INTO account (id, address, password_hash, blob_key, created_at, modseq, next_email,
			next_mailbox, next_thread, uidvalidity, seq)
		VALUES (:a, :address, :hash, :blob_key, :now, 1, 1, :next, 1, :now, (SELECT COALESCE(max(seq), 0) + 1 FROM account))`,
		"address", address, "hash", passwordHash, "blob_key", wrapped, "next", len(defaultMailboxes)+1)
	for i, m := range defaultMailboxes {
		b.add(`INSERT INTO mailbox (account_id, id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq)
			VALUES (:a, :id, 0, :name, :role, :id, :now, 1, 1)`, "id", i+1, "name", m.name, "role", m.role)
	}
	b.add(`INSERT INTO identity (account_id, id, name, email, modseq) VALUES (:a, 1, '', :address, 1)`, "address", address)
	b.change("Mailbox", `id AS id, 'created' AS op FROM mailbox WHERE account_id = :a`)
	b.change("Identity", `1 AS id, 'created' AS op`)
	if _, err := s.run(ctx, b); err != nil {
		return Account{}, err
	}
	s.keys.Store(id, key)
	now := time.Unix(b.base["now"].(int64), 0)
	return Account{ID: id, Address: address, PasswordHash: passwordHash, CreatedAt: now, State: 1}, nil
}

const accountColumns = `id, address, password_hash, created_at, modseq, auth_seq, password_changed_at,
	(SELECT count(*) FROM app_password p WHERE p.account_id = account.id),
	EXISTS (SELECT 1 FROM totp t WHERE t.account_id = account.id AND t.confirmed_at > 0),
	quota_bytes, quota_count, used_bytes, used_count`

func (s *Store) scanAccount(row []any) Account {
	a := scanAccount(row)
	a.Quota = s.quotaOf(ownLimit(row[9]), ownLimit(row[10]), asInt(row[11]), asInt(row[12]))
	return a
}

func scanAccount(row []any) Account {
	a := Account{ID: asString(row[0]), Address: asString(row[1]), PasswordHash: asString(row[2]),
		CreatedAt: time.Unix(asInt(row[3]), 0), State: asInt(row[4]), AuthSeq: asInt(row[5]),
		AppPasswords: int(asInt(row[7])), TOTP: asInt(row[8]) != 0}
	if at := asInt(row[6]); at > 0 {
		a.PasswordChangedAt = time.Unix(at, 0)
	}
	return a
}

// Account looks an account up by address.
func (s *Store) Account(ctx context.Context, address string) (Account, error) {
	res, err := s.query(ctx, "", q(`SELECT `+accountColumns+` FROM account WHERE address = :address`, "address", address))
	if err != nil {
		return Account{}, err
	}
	if len(res[0].Values) == 0 {
		return Account{}, ErrNoAccount
	}
	return s.scanAccount(res[0].Values[0]), nil
}

// Accounts lists all accounts of users by address. The system accounts
// (alias.go) are not among them.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	res, err := s.query(ctx, "", q(`SELECT `+accountColumns+` FROM account WHERE `+sqlUserAccount+` ORDER BY address`))
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, s.scanAccount(row))
	}
	return out, nil
}

// State returns the account's current modseq.
func (s *Store) State(ctx context.Context, account string) (int64, error) {
	res, err := s.query(ctx, account, q(`SELECT modseq FROM account WHERE id = :a`))
	if err != nil {
		return 0, err
	}
	if len(res[0].Values) == 0 {
		return 0, ErrNoAccount
	}
	return asInt(res[0].Values[0][0]), nil
}

// ModSeqs returns the current modseq of each of the given accounts; one
// that does not exist is left out. It is what internal/notify polls to find
// changes made on other nodes, and is read at level none: the rqlite node
// that is asked answers from its own copy, without the leader. A copy that
// is behind shows a change one poll later.
func (s *Store) ModSeqs(ctx context.Context, accounts []string) (map[string]int64, error) {
	return s.seqs(ctx, "modseq", accounts)
}

// AuthSeqs is ModSeqs for auth_seq: what a second internal/notify hub
// polls, so that open IMAP sessions and event streams hear, on every node,
// that a login was withdrawn.
func (s *Store) AuthSeqs(ctx context.Context, accounts []string) (map[string]int64, error) {
	return s.seqs(ctx, "auth_seq", accounts)
}

func (s *Store) seqs(ctx context.Context, column string, accounts []string) (map[string]int64, error) {
	out := map[string]int64{}
	for start := 0; start < len(accounts); start += maxIDsPerRead {
		chunk := accounts[start:min(start+maxIDsPerRead, len(accounts))]
		vals, kv := values("i", chunk)
		b := &batch{base: map[string]any{}}
		b.add(`SELECT id, `+column+` FROM account WHERE id IN (VALUES `+vals+`)`, kv...)
		res, err := s.db.Query(ctx, rqlite.None, b.stmts...)
		if err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		for _, row := range res[0].Values {
			out[asString(row[0])] = asInt(row[1])
		}
	}
	return out, nil
}

// blobKey returns the key that encrypts the account's blobs. Keys never
// change, so they are cached once unwrapped.
func (s *Store) blobKey(ctx context.Context, account string) ([]byte, error) {
	if key, ok := s.keys.Load(account); ok {
		return key.([]byte), nil
	}
	res, err := s.query(ctx, account, q(`SELECT blob_key FROM account WHERE id = :a`))
	if err != nil {
		return nil, err
	}
	if len(res[0].Values) == 0 {
		return nil, ErrNoAccount
	}
	key, err := s.master.Unwrap(account, asString(res[0].Values[0][0]))
	if err != nil {
		return nil, fmt.Errorf("store: account %s: %w", account, err)
	}
	s.keys.Store(account, key)
	return key, nil
}
