package store

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
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
// the canonical (lowercase) address, passwordHash a PHC string.
func (s *Store) CreateAccount(ctx context.Context, address, passwordHash string) (Account, error) {
	id := newAccountID()
	key, wrapped, err := s.master.NewAccountKey(id)
	if err != nil {
		return Account{}, fmt.Errorf("store: %w", err)
	}
	b := s.newBatch(id)
	b.unique(ErrAccountExists)
	b.add(`INSERT INTO account (id, address, password_hash, blob_key, created_at, modseq, next_email,
			next_mailbox, next_thread)
		VALUES (:a, :address, :hash, :blob_key, :now, 1, 1, :next, 1)`,
		"address", address, "hash", passwordHash, "blob_key", wrapped, "next", len(defaultMailboxes)+1)
	for i, m := range defaultMailboxes {
		b.add(`INSERT INTO mailbox (account_id, id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq)
			VALUES (:a, :id, 0, :name, :role, :id, :now, 1, 1)`, "id", i+1, "name", m.name, "role", m.role)
	}
	b.change("Mailbox", `id AS id, 'created' AS op FROM mailbox WHERE account_id = :a`)
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
