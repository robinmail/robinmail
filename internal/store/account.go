package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"

	"github.com/mikepage/robinmail/internal/rqlite"
)

// Account is one mail user. ID is random and never changes, also if the
// address ever does. State is the modseq: the JMAP state string.
type Account struct {
	ID           string
	Address      string
	PasswordHash string
	CreatedAt    time.Time
	State        int64
}

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
			next_mailbox, next_thread, uidvalidity)
		VALUES (:a, :address, :hash, :blob_key, :now, 1, 1, :next, 1, :now)`,
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

const accountColumns = `id, address, password_hash, created_at, modseq`

func scanAccount(row []any) Account {
	return Account{ID: asString(row[0]), Address: asString(row[1]), PasswordHash: asString(row[2]),
		CreatedAt: time.Unix(asInt(row[3]), 0), State: asInt(row[4])}
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
	return scanAccount(res[0].Values[0]), nil
}

// Accounts lists all accounts by address.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	res, err := s.query(ctx, "", q(`SELECT `+accountColumns+` FROM account ORDER BY address`))
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(res[0].Values))
	for _, row := range res[0].Values {
		out = append(out, scanAccount(row))
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
	out := map[string]int64{}
	for start := 0; start < len(accounts); start += maxIDsPerRead {
		chunk := accounts[start:min(start+maxIDsPerRead, len(accounts))]
		vals, kv := values("i", chunk)
		b := &batch{base: map[string]any{}}
		b.add(`SELECT id, modseq FROM account WHERE id IN (VALUES `+vals+`)`, kv...)
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
