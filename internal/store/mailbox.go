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
	Subscribed  bool
	Total       int64 // emails in the mailbox
	Unread      int64 // of which without $seen
	// TotalThreads counts the threads with an email in the mailbox;
	// UnreadThreads those of them with an email without $seen in any
	// mailbox (RFC 8621 2).
	TotalThreads  int64
	UnreadThreads int64
}

const sqlUnseen = `NOT EXISTS (SELECT 1 FROM keyword k WHERE k.account_id = x.account_id AND k.email_id = x.email_id AND k.keyword = '$seen')`

const mailboxColumns = `m.id, m.parent_id, m.name, m.role, m.sort_order, m.uidvalidity, m.uidnext, m.modseq, m.subscribed,
	(SELECT count(*) FROM email_mailbox x WHERE x.account_id = m.account_id AND x.mailbox_id = m.id),
	(SELECT count(*) FROM email_mailbox x WHERE x.account_id = m.account_id AND x.mailbox_id = m.id AND ` + sqlUnseen + `),
	(SELECT count(DISTINCT e.thread_id) FROM email_mailbox x
		JOIN email e ON e.account_id = x.account_id AND e.id = x.email_id
		WHERE x.account_id = m.account_id AND x.mailbox_id = m.id),
	(SELECT count(DISTINCT e.thread_id) FROM email_mailbox x
		JOIN email e ON e.account_id = x.account_id AND e.id = x.email_id
		WHERE x.account_id = m.account_id AND x.mailbox_id = m.id AND EXISTS (
			SELECT 1 FROM email u WHERE u.account_id = e.account_id AND u.thread_id = e.thread_id AND NOT EXISTS (
				SELECT 1 FROM keyword k WHERE k.account_id = u.account_id AND k.email_id = u.id AND k.keyword = '$seen')))`

func scanMailbox(row []any) Mailbox {
	return Mailbox{ID: asInt(row[0]), ParentID: asInt(row[1]), Name: asString(row[2]), Role: asString(row[3]),
		SortOrder: asInt(row[4]), UIDValidity: uint32(asInt(row[5])), UIDNext: uint32(asInt(row[6])),
		Modseq: asInt(row[7]), Subscribed: asInt(row[8]) != 0, Total: asInt(row[9]), Unread: asInt(row[10]),
		TotalThreads: asInt(row[11]), UnreadThreads: asInt(row[12])}
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

// NewMailbox describes a mailbox to create.
type NewMailbox struct {
	ParentID   int64 // 0 for the top level
	Name       string
	SortOrder  int64
	Subscribed bool
	IfInState  int64 // when not 0, the state the account must be in
}

// CreateMailbox adds a subscribed mailbox under parentID (0 for the top
// level), sorted after the default ones.
func (s *Store) CreateMailbox(ctx context.Context, account string, parentID int64, name string) (Mailbox, error) {
	return s.NewMailbox(ctx, account, NewMailbox{ParentID: parentID, Name: name, SortOrder: userSortOrder, Subscribed: true})
}

// NewMailbox adds a mailbox.
func (s *Store) NewMailbox(ctx context.Context, account string, n NewMailbox) (Mailbox, error) {
	if !validMailboxName(n.Name) {
		return Mailbox{}, ErrMailboxName
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.ifState(n.IfInState)
	if n.ParentID != 0 {
		b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :parent)`, "parent", n.ParentID)
	}
	b.bump("next_mailbox")
	// UIDVALIDITY is the time of creation, or one more than the last one
	// the account gave out when that is not later: a mailbox deleted and
	// made again in the same second must not get the value it had, or a
	// client would take its cache of the old mailbox for the new one (RFC
	// 3501 2.3.1.1).
	b.add(`UPDATE account SET uidvalidity = max(uidvalidity + 1, :now) WHERE id = :a`)
	b.unique(ErrMailboxExists)
	b.add(`INSERT INTO mailbox (account_id, id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq, subscribed)
		SELECT a.id, a.next_mailbox - 1, :parent, :name, NULL, :sort, a.uidvalidity, 1, a.modseq, :subscribed FROM account a WHERE a.id = :a`,
		"parent", n.ParentID, "name", n.Name, "sort", n.SortOrder, "subscribed", boolInt(n.Subscribed))
	b.change("Mailbox", `next_mailbox - 1 AS id, 'created' AS op FROM account WHERE id = :a`)
	b.add(`SELECT ` + mailboxColumns + ` FROM mailbox m
		WHERE m.account_id = :a AND m.id = (SELECT next_mailbox - 1 FROM account WHERE id = :a)`)
	res, err := s.run(ctx, b)
	if err != nil {
		return Mailbox{}, err
	}
	return scanMailbox(res[len(res)-1].Values[0]), nil
}

// MailboxUpdate changes a mailbox. A nil field stays as it is.
type MailboxUpdate struct {
	Name       *string
	ParentID   *int64
	SortOrder  *int64
	Subscribed *bool
	IfInState  int64
}

// sqlAncestors is every mailbox from :parent up to the top level. UNION
// drops doubles, so it ends even if the table ever held a loop.
const sqlAncestors = `WITH RECURSIVE up(id) AS (
	SELECT :parent
	UNION
	SELECT m.parent_id FROM mailbox m JOIN up ON m.id = up.id WHERE m.account_id = :a AND m.parent_id <> 0)
	SELECT 1 FROM up WHERE id = :m`

// UpdateMailbox renames, moves, reorders or (un)subscribes a mailbox in one
// transaction and returns the new state. The id, the emails and their UIDs
// stay. Moving a mailbox under itself or one of its descendants fails with
// ErrMailboxLoop.
func (s *Store) UpdateMailbox(ctx context.Context, account string, id int64, u MailboxUpdate) (int64, error) {
	if u.Name != nil && !validMailboxName(*u.Name) {
		return 0, ErrMailboxName
	}
	b := s.newBatch(account)
	b.base["m"] = id
	b.guardAccount()
	b.ifState(u.IfInState)
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`)
	var name, parent, sort, subscribed any
	if u.Name != nil {
		name = *u.Name
	}
	if u.ParentID != nil {
		parent = *u.ParentID
		if *u.ParentID != 0 {
			b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :parent)`, "parent", parent)
			b.guard(ErrMailboxLoop, `NOT EXISTS (`+sqlAncestors+`)`, "parent", parent)
		}
	}
	if u.SortOrder != nil {
		sort = *u.SortOrder
	}
	if u.Subscribed != nil {
		subscribed = boolInt(*u.Subscribed)
	}
	b.bump()
	b.unique(ErrMailboxExists)
	b.add(`UPDATE mailbox SET name = COALESCE(:name, name), parent_id = COALESCE(:parent, parent_id),
			sort_order = COALESCE(:sort, sort_order), subscribed = COALESCE(:subscribed, subscribed), modseq = `+sqlModseq+`
		WHERE account_id = :a AND id = :m`, "name", name, "parent", parent, "sort", sort, "subscribed", subscribed)
	b.change("Mailbox", `:m AS id, 'updated' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// DestroyMailbox removes a mailbox and returns the new state. It refuses a
// mailbox with a role (delivery needs those), one with children
// (ErrMailboxHasChild) and, unless removeEmails is set, one with emails
// (ErrMailboxHasEmail). With removeEmails, an email that is in no other
// mailbox is destroyed and one that is only loses this label, as RFC 8621
// 2.5 says for onDestroyRemoveEmails. All of it is one transaction.
func (s *Store) DestroyMailbox(ctx context.Context, account string, id int64, removeEmails bool, ifState int64) (int64, error) {
	// The emails that are in this mailbox and no other.
	const doomed = `(SELECT x.email_id FROM email_mailbox x WHERE x.account_id = :a AND x.mailbox_id = :m
		AND NOT EXISTS (SELECT 1 FROM email_mailbox y
			WHERE y.account_id = :a AND y.email_id = x.email_id AND y.mailbox_id <> :m))`
	b := s.newBatch(account)
	b.base["m"] = id
	b.guardAccount()
	b.ifState(ifState)
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`)
	b.guard(ErrMailboxRole, `(SELECT role FROM mailbox WHERE account_id = :a AND id = :m) IS NULL`)
	b.guard(ErrMailboxHasChild, `NOT EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND parent_id = :m)`)
	if !removeEmails {
		b.guard(ErrMailboxHasEmail, `NOT EXISTS (SELECT 1 FROM email_mailbox WHERE account_id = :a AND mailbox_id = :m)`)
	}
	b.bump()
	if removeEmails {
		b.change("Email", `x.email_id AS id, CASE WHEN x.email_id IN `+doomed+` THEN 'destroyed' ELSE 'updated' END AS op
			FROM email_mailbox x WHERE x.account_id = :a AND x.mailbox_id = :m`)
		b.change("Thread", `e.thread_id AS id, CASE WHEN EXISTS (SELECT 1 FROM email o
				WHERE o.account_id = :a AND o.thread_id = e.thread_id AND o.id NOT IN `+doomed+`)
			THEN 'updated' ELSE 'destroyed' END AS op
			FROM email e WHERE e.account_id = :a AND e.id IN `+doomed)
		// Mailboxes that share a thread with a destroyed email: their
		// thread counts may change.
		b.change("Mailbox", `x.mailbox_id AS id, 'counts' AS op FROM email_mailbox x
			JOIN email o ON o.account_id = x.account_id AND o.id = x.email_id
			WHERE x.account_id = :a AND x.mailbox_id <> :m AND o.thread_id IN (
				SELECT e.thread_id FROM email e WHERE e.account_id = :a AND e.id IN `+doomed+`)`)
		b.add(`UPDATE mailbox SET modseq = ` + sqlModseq + ` WHERE account_id = :a AND id IN (
			SELECT c.id FROM changes c WHERE c.account_id = :a AND c.modseq = ` + sqlModseq + ` AND c.type = 'Mailbox')`)
		b.add(`DELETE FROM thread_ref WHERE account_id = :a
			AND thread_id IN (SELECT e.thread_id FROM email e WHERE e.account_id = :a AND e.id IN ` + doomed + `)
			AND NOT EXISTS (SELECT 1 FROM email o
				WHERE o.account_id = :a AND o.thread_id = thread_ref.thread_id AND o.id NOT IN ` + doomed + `)`)
		b.add(`UPDATE email SET modseq = ` + sqlModseq + ` WHERE account_id = :a
			AND id IN (SELECT email_id FROM email_mailbox WHERE account_id = :a AND mailbox_id = :m)`)
		b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id IN ` + doomed)
		b.add(`DELETE FROM email WHERE account_id = :a AND id IN ` + doomed)
		b.add(`DELETE FROM email_mailbox WHERE account_id = :a AND mailbox_id = :m`)
	}
	b.change("Mailbox", `:m AS id, 'destroyed' AS op`)
	b.add(`DELETE FROM mailbox WHERE account_id = :a AND id = :m`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// RenameMailbox changes the name and nothing else: the id, the emails and
// their UIDs stay. It returns the new state.
func (s *Store) RenameMailbox(ctx context.Context, account string, id int64, name string) (int64, error) {
	return s.UpdateMailbox(ctx, account, id, MailboxUpdate{Name: &name})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
