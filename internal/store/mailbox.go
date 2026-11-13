package store

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mailbox is a label. ParentID 0 is the top level. Role is "" for a mailbox
// the user made.
type Mailbox struct {
	ID          int64
	ParentID    int64
	Name        string
	Role        string
	SortOrder   int64
	UIDValidity uint32
	UIDNext     uint32
	Modseq      int64
	Total       int64 // emails in the mailbox
	Unread      int64 // of which without $seen
}

const mailboxColumns = `m.id, m.parent_id, m.name, m.role, m.sort_order, m.uidvalidity, m.uidnext, m.modseq,
	(SELECT count(*) FROM email_mailbox x WHERE x.account_id = m.account_id AND x.mailbox_id = m.id),
	(SELECT count(*) FROM email_mailbox x WHERE x.account_id = m.account_id AND x.mailbox_id = m.id
		AND NOT EXISTS (SELECT 1 FROM keyword k WHERE k.account_id = x.account_id AND k.email_id = x.email_id AND k.keyword = '$seen'))`

func scanMailbox(row []any) Mailbox {
	return Mailbox{ID: asInt(row[0]), ParentID: asInt(row[1]), Name: asString(row[2]), Role: asString(row[3]),
		SortOrder: asInt(row[4]), UIDValidity: uint32(asInt(row[5])), UIDNext: uint32(asInt(row[6])),
		Modseq: asInt(row[7]), Total: asInt(row[8]), Unread: asInt(row[9])}
}

// Mailboxes lists the account's mailboxes by sort order, then name.
func (s *Store) Mailboxes(ctx context.Context, account string) ([]Mailbox, error) {
	res, err := s.query(ctx, account,
		q(`SELECT 1 FROM account WHERE id = :a`),
		q(`SELECT `+mailboxColumns+` FROM mailbox m WHERE m.account_id = :a ORDER BY m.sort_order, m.name, m.id`))
	if err != nil {
		return nil, err
	}
	if len(res[0].Values) == 0 {
		return nil, ErrNoAccount
	}
	out := make([]Mailbox, 0, len(res[1].Values))
	for _, row := range res[1].Values {
		out = append(out, scanMailbox(row))
	}
	return out, nil
}

// validMailboxName accepts 1 to 255 bytes of UTF-8 without control
// characters and without space at either end.
func validMailboxName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// userSortOrder puts mailboxes the user made after the default ones.
const userSortOrder = 10

// CreateMailbox adds a mailbox under parentID (0 for the top level).
func (s *Store) CreateMailbox(ctx context.Context, account string, parentID int64, name string) (Mailbox, error) {
	if !validMailboxName(name) {
		return Mailbox{}, ErrMailboxName
	}
	b := s.newBatch(account)
	b.guardAccount()
	if parentID != 0 {
		b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :parent)`, "parent", parentID)
	}
	b.bump("next_mailbox")
	b.unique(ErrMailboxExists)
	b.add(`INSERT INTO mailbox (account_id, id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq)
		SELECT a.id, a.next_mailbox - 1, :parent, :name, NULL, :sort, :now, 1, a.modseq FROM account a WHERE a.id = :a`,
		"parent", parentID, "name", name, "sort", userSortOrder)
	b.change("Mailbox", `next_mailbox - 1 AS id, 'created' AS op FROM account WHERE id = :a`)
	b.add(`SELECT ` + mailboxColumns + ` FROM mailbox m
		WHERE m.account_id = :a AND m.id = (SELECT next_mailbox - 1 FROM account WHERE id = :a)`)
	res, err := s.run(ctx, b)
	if err != nil {
		return Mailbox{}, err
	}
	return scanMailbox(res[len(res)-1].Values[0]), nil
}

// RenameMailbox changes the name and nothing else: the id, the emails and
// their UIDs stay. It returns the new state.
func (s *Store) RenameMailbox(ctx context.Context, account string, id int64, name string) (int64, error) {
	if !validMailboxName(name) {
		return 0, ErrMailboxName
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`, "m", id)
	b.bump()
	b.unique(ErrMailboxExists)
	b.add(`UPDATE mailbox SET name = :name, modseq = `+sqlModseq+` WHERE account_id = :a AND id = :m`, "m", id, "name", name)
	b.change("Mailbox", `:m AS id, 'updated' AS op`, "m", id)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}
