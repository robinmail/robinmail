package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// What IMAP needs beyond what JMAP uses: a mailbox seen as a list of UIDs,
// and changes to many messages of a mailbox at once, named by UID.
//
// Each write below is one batch, like every other write. They work on a
// set of emails that only SQL knows (the messages of a mailbox with a UID
// in some ranges, that have a keyword, that have not changed since a
// modseq), and later statements of the batch must name that same set after
// earlier ones changed what defined it. So each batch first stamps the
// emails it is about with the new modseq, which every email it touches
// gets anyway, and from then on the set is "the emails with this modseq".
// A batch that would touch nothing fails on a guard instead (errNothing):
// it must not bump the modseq, and a stamp of nothing would leave the set
// to be whatever the write before it stamped.

// KeywordDeleted is IMAP's \Deleted flag as a keyword. JMAP has no such
// flag (RFC 8621 4.1.1 lists what maps to what); it is stored like any
// other keyword, shows in JMAP under this name, and means: EXPUNGE takes
// this message out of the mailbox.
const KeywordDeleted = "$deleted"

// errNothing is what a batch fails with when there is nothing to do.
var errNothing = errors.New("store: nothing to do")

// UIDRange is an inclusive range of IMAP UIDs.
type UIDRange struct{ Lo, Hi uint32 }

// maxUIDRanges bounds the ranges of one batch. Callers with more split.
const maxUIDRanges = 400

// uidCond is a SQL condition on x.uid for the ranges, with its parameters.
// No ranges is every UID.
func uidCond(ranges []UIDRange) (string, []any) {
	if len(ranges) == 0 {
		return `1`, nil
	}
	parts := make([]string, len(ranges))
	var kv []any
	for i, r := range ranges {
		lo, hi := "ulo"+strconv.Itoa(i), "uhi"+strconv.Itoa(i)
		if r.Lo == r.Hi {
			parts[i] = `x.uid = :` + lo
			kv = append(kv, lo, int64(r.Lo))
			continue
		}
		parts[i] = `x.uid BETWEEN :` + lo + ` AND :` + hi
		kv = append(kv, lo, int64(r.Lo), hi, int64(r.Hi))
	}
	return `(` + strings.Join(parts, ` OR `) + `)`, kv
}

// splitRanges cuts a long list of ranges into pieces a batch can carry.
func splitRanges(ranges []UIDRange) [][]UIDRange {
	if len(ranges) <= maxUIDRanges {
		return [][]UIDRange{ranges}
	}
	var out [][]UIDRange
	for start := 0; start < len(ranges); start += maxUIDRanges {
		out = append(out, ranges[start:min(start+maxUIDRanges, len(ranges))])
	}
	return out
}

// sqlStamped is the set of emails the running batch stamped.
const sqlStamped = `(SELECT s.id FROM email s WHERE s.account_id = :a AND s.modseq = ` + sqlModseq + `)`

// stamp marks the emails of mailbox :m that meet cond (on email_mailbox x)
// as the set this batch works on. It adds the guard that there are any,
// the bump and the stamp; :m must be in the batch's base.
func (b *batch) stamp(cond string, kv ...any) {
	const in = `x.account_id = :a AND x.mailbox_id = :m AND `
	b.guard(errNothing, `EXISTS (SELECT 1 FROM email_mailbox x WHERE `+in+cond+`)`, kv...)
	b.bump()
	b.add(`UPDATE email SET modseq = `+sqlModseq+` WHERE account_id = :a
		AND id IN (SELECT x.email_id FROM email_mailbox x WHERE `+in+cond+`)`, kv...)
}

// touchMailboxes gives every mailbox that has a Mailbox change in this
// batch's modseq that modseq.
func (b *batch) touchMailboxes() {
	b.add(`UPDATE mailbox SET modseq = ` + sqlModseq + ` WHERE account_id = :a AND id IN (
		SELECT c.id FROM changes c WHERE c.account_id = :a AND c.modseq = ` + sqlModseq + ` AND c.type = 'Mailbox')`)
}

// MailboxMessage is a message as a mailbox has it.
type MailboxMessage struct {
	UID        uint32
	EmailID    int64
	Modseq     int64
	Size       int64
	ReceivedAt time.Time
	Keywords   []string
}

// MailboxView is a mailbox at one moment.
type MailboxView struct {
	UIDValidity uint32
	UIDNext     uint32
	// Modseq is the account's modseq at the time of the read: no message
	// of the mailbox has a higher one.
	Modseq int64
	// UIDs are all UIDs in the mailbox, ascending.
	UIDs []uint32
	// Changed are the messages whose modseq is above the one asked for,
	// by UID: with 0, all of them.
	Changed []MailboxMessage
}

// ViewMailbox reads a mailbox as IMAP sees it: all its UIDs, and the
// messages that are new or changed since the given modseq. Both come from
// one snapshot. A session keeps the result and asks again with the Modseq
// it got: UIDs it knew that are gone were expunged, Changed has what is
// new and what has other flags.
func (s *Store) ViewMailbox(ctx context.Context, account string, mailbox, since int64) (MailboxView, error) {
	res, err := s.query(ctx, account,
		q(`SELECT m.uidvalidity, m.uidnext, (SELECT modseq FROM account WHERE id = :a)
			FROM mailbox m WHERE m.account_id = :a AND m.id = :m`, "m", mailbox),
		q(`SELECT uid FROM email_mailbox WHERE account_id = :a AND mailbox_id = :m ORDER BY uid`, "m", mailbox),
		q(`SELECT x.uid, e.id, e.modseq, e.size, e.received_at,
				(SELECT group_concat(k.keyword, ' ') FROM keyword k WHERE k.account_id = e.account_id AND k.email_id = e.id)
			FROM email_mailbox x JOIN email e ON e.account_id = x.account_id AND e.id = x.email_id
			WHERE x.account_id = :a AND x.mailbox_id = :m AND e.modseq > :since ORDER BY x.uid`, "m", mailbox, "since", since))
	if err != nil {
		return MailboxView{}, err
	}
	if len(res[0].Values) == 0 {
		return MailboxView{}, ErrNoMailbox
	}
	head := res[0].Values[0]
	v := MailboxView{UIDValidity: uint32(asInt(head[0])), UIDNext: uint32(asInt(head[1])), Modseq: asInt(head[2]),
		UIDs: make([]uint32, 0, len(res[1].Values)), Changed: make([]MailboxMessage, 0, len(res[2].Values))}
	for _, row := range res[1].Values {
		v.UIDs = append(v.UIDs, uint32(asInt(row[0])))
	}
	for _, row := range res[2].Values {
		v.Changed = append(v.Changed, MailboxMessage{UID: uint32(asInt(row[0])), EmailID: asInt(row[1]), Modseq: asInt(row[2]),
			Size: asInt(row[3]), ReceivedAt: time.Unix(asInt(row[4]), 0).UTC(), Keywords: strings.Fields(asString(row[5]))})
	}
	return v, nil
}

// MailboxStatus is what IMAP's STATUS and LIST want of a mailbox.
type MailboxStatus struct {
	Mailbox
	Deleted int64 // messages with $deleted
	Size    int64 // octets of all messages
	// HighestModseq is the account's modseq.
	HighestModseq int64
}

// MailboxStatuses lists the account's mailboxes with their counts, without
// the thread counts of Mailboxes, which cost more than IMAP has use for.
func (s *Store) MailboxStatuses(ctx context.Context, account string) ([]MailboxStatus, error) {
	res, err := s.query(ctx, account,
		q(`SELECT modseq FROM account WHERE id = :a`),
		q(`SELECT m.id, m.parent_id, m.name, m.role, m.sort_order, m.uidvalidity, m.uidnext, m.modseq, m.subscribed,
				(SELECT count(*) FROM email_mailbox x WHERE x.account_id = m.account_id AND x.mailbox_id = m.id),
				(SELECT count(*) FROM email_mailbox x WHERE x.account_id = m.account_id AND x.mailbox_id = m.id AND `+sqlUnseen+`),
				(SELECT count(*) FROM email_mailbox x JOIN keyword k ON k.account_id = x.account_id AND k.email_id = x.email_id
					AND k.keyword = '`+KeywordDeleted+`' WHERE x.account_id = m.account_id AND x.mailbox_id = m.id),
				(SELECT COALESCE(sum(e.size), 0) FROM email_mailbox x JOIN email e ON e.account_id = x.account_id AND e.id = x.email_id
					WHERE x.account_id = m.account_id AND x.mailbox_id = m.id)
			FROM mailbox m WHERE m.account_id = :a ORDER BY m.sort_order, m.name, m.id`))
	if err != nil {
		return nil, err
	}
	if len(res[0].Values) == 0 {
		return nil, ErrNoAccount
	}
	state := asInt(res[0].Values[0][0])
	out := make([]MailboxStatus, 0, len(res[1].Values))
	for _, row := range res[1].Values {
		out = append(out, MailboxStatus{
			Mailbox: Mailbox{ID: asInt(row[0]), ParentID: asInt(row[1]), Name: asString(row[2]), Role: asString(row[3]),
				SortOrder: asInt(row[4]), UIDValidity: uint32(asInt(row[5])), UIDNext: uint32(asInt(row[6])),
				Modseq: asInt(row[7]), Subscribed: asInt(row[8]) != 0, Total: asInt(row[9]), Unread: asInt(row[10])},
			Deleted: asInt(row[11]), Size: asInt(row[12]), HighestModseq: state})
	}
	return out, nil
}

// How SetFlags changes the keywords.
const (
	FlagsAdd = iota
	FlagsRemove
	FlagsReplace
)

// FlagUpdate is IMAP's STORE: a change to the keywords of the messages of
// one mailbox with a UID in UIDs.
type FlagUpdate struct {
	Mailbox  int64
	UIDs     []UIDRange
	Mode     int
	Keywords []string
	// UnchangedSince, when not negative, leaves out every message whose
	// modseq is above it (RFC 7162 3.1.3).
	UnchangedSince int64
}

// SetFlags changes the keywords of many messages in one batch. It returns
// the new state, or 0 when no message needed a change, and the UIDs that
// were left out because of UnchangedSince. A message that has the asked
// keywords already is not touched and keeps its modseq.
//
// Keywords belong to the Email, not to its place in a mailbox: a flag set
// here shows in every mailbox the Email is in.
func (s *Store) SetFlags(ctx context.Context, account string, u FlagUpdate) (state int64, modified []uint32, err error) {
	keywords, err := cleanKeywords(u.Keywords)
	if err != nil {
		return 0, nil, err
	}
	for _, ranges := range splitRanges(u.UIDs) {
		st, mod, err := s.setFlags(ctx, account, u, ranges, keywords)
		if err != nil {
			return 0, nil, err
		}
		if st != 0 {
			state = st
		}
		modified = append(modified, mod...)
	}
	return state, modified, nil
}

func (s *Store) setFlags(ctx context.Context, account string, u FlagUpdate, ranges []UIDRange, keywords []string) (int64, []uint32, error) {
	inRange, kv := uidCond(ranges)
	kv = append(kv, "us", u.UnchangedSince)
	const emailModseq = `(SELECT e.modseq FROM email e WHERE e.account_id = x.account_id AND e.id = x.email_id)`
	// Which messages need a change at all.
	const have = `(SELECT count(*) FROM keyword k WHERE k.account_id = x.account_id AND k.email_id = x.email_id`
	vals, kkv := values("k", keywords)
	var differs string
	switch {
	case len(keywords) == 0 && u.Mode == FlagsReplace:
		differs = have + `) > 0`
	case len(keywords) == 0:
		differs = `0`
	case u.Mode == FlagsAdd:
		differs = have + ` AND k.keyword IN (VALUES ` + vals + `)) < :n`
	case u.Mode == FlagsRemove:
		differs = have + ` AND k.keyword IN (VALUES ` + vals + `)) > 0`
	default:
		differs = `(` + have + ` AND k.keyword IN (VALUES ` + vals + `)) <> :n OR ` + have + `) <> :n)`
	}
	if len(keywords) > 0 {
		kv = append(append(kv, kkv...), "n", len(keywords))
	}
	fresh := `(:us < 0 OR ` + emailModseq + ` <= :us)`

	// The messages UnchangedSince leaves out, read with or without the
	// write.
	const sqlStale = `SELECT x.uid FROM email_mailbox x WHERE x.account_id = :a AND x.mailbox_id = :m AND `
	stale := func() ([]uint32, error) {
		if u.UnchangedSince < 0 {
			return nil, nil
		}
		res, err := s.query(ctx, account, q(sqlStale+inRange+` AND NOT `+fresh+` ORDER BY x.uid`, append(kv, "m", u.Mailbox)...))
		if err != nil {
			return nil, err
		}
		out := make([]uint32, 0, len(res[0].Values))
		for _, row := range res[0].Values {
			out = append(out, uint32(asInt(row[0])))
		}
		return out, nil
	}

	b := s.newBatch(account)
	b.base["m"] = u.Mailbox
	b.guardAccount()
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`)
	staleAt := len(b.stmts)
	b.add(sqlStale+inRange+` AND NOT `+fresh+` ORDER BY x.uid`, kv...)
	b.stamp(inRange+` AND `+fresh+` AND `+differs, kv...)
	if u.Mode == FlagsReplace {
		if len(keywords) == 0 {
			b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id IN ` + sqlStamped)
		} else {
			b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id IN `+sqlStamped+`
				AND keyword NOT IN (VALUES `+vals+`)`, kkv...)
		}
	}
	if len(keywords) > 0 && u.Mode != FlagsRemove {
		b.add(`INSERT OR IGNORE INTO keyword (account_id, email_id, keyword)
			SELECT :a, s.id, v.column1 FROM email s, (VALUES `+vals+`) v
			WHERE s.account_id = :a AND s.modseq = `+sqlModseq, kkv...)
	}
	if len(keywords) > 0 && u.Mode == FlagsRemove {
		b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id IN `+sqlStamped+`
			AND keyword IN (VALUES `+vals+`)`, kkv...)
	}
	b.change("Email", `s.id AS id, 'updated' AS op FROM email s WHERE s.account_id = :a AND s.modseq = `+sqlModseq)
	// Keywords change the unread count of every mailbox the email is in.
	b.change("Mailbox", `y.mailbox_id AS id, 'counts' AS op FROM email_mailbox y
		WHERE y.account_id = :a AND y.email_id IN `+sqlStamped)
	b.touchMailboxes()
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if errors.Is(err, errNothing) {
		mod, err := stale()
		return 0, mod, err
	}
	if err != nil {
		return 0, nil, err
	}
	var modified []uint32
	for _, row := range res[staleAt].Values {
		modified = append(modified, uint32(asInt(row[0])))
	}
	return asInt(res[len(res)-1].Values[0][0]), modified, nil
}

// UIDPair is a message's UID in the mailbox it was copied from and in the
// one it was copied to.
type UIDPair struct{ From, To uint32 }

// CopyResult is what a copy or move did.
type CopyResult struct {
	// State is the new state, or 0 when no message was named.
	State       int64
	UIDValidity uint32 // of the target
	Pairs       []UIDPair
}

// CopyMessages is IMAP's COPY and MOVE for the messages of mailbox from
// with a UID in uids, in one batch.
//
// A mailbox is a label, so a copy adds the label "to" to the emails: no
// second email is made. A copy into the mailbox the messages are in
// changes nothing; a move into it gives them new UIDs, which is what a
// client that moves expects to see (the old UIDs are gone). Each gets the target's next UID, in the order of
// their UIDs in from. An email that is in the target already stays as it
// is there and is reported with the UID it has. A move also takes the
// label "from" away, so the email keeps its id (and JMAP sees an update,
// not a destroy and a create).
func (s *Store) CopyMessages(ctx context.Context, account string, from int64, uids []UIDRange, to int64, move bool) (CopyResult, error) {
	var out CopyResult
	for _, ranges := range splitRanges(uids) {
		r, err := s.copyMessages(ctx, account, from, ranges, to, move)
		if err != nil {
			return out, err
		}
		if r.State != 0 {
			out.State = r.State
		}
		out.UIDValidity = r.UIDValidity
		out.Pairs = append(out.Pairs, r.Pairs...)
	}
	return out, nil
}

func (s *Store) copyMessages(ctx context.Context, account string, from int64, ranges []UIDRange, to int64, move bool) (CopyResult, error) {
	if from == to && move {
		return s.renumber(ctx, account, from, ranges)
	}
	if from == to {
		// The label is there already.
		return s.sameMailbox(ctx, account, from, ranges)
	}
	inRange, kv := uidCond(ranges)
	b := s.newBatch(account)
	b.base["m"], b.base["to"] = from, to
	b.guardAccount()
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`)
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :to)`)
	b.stamp(inRange, kv...)
	const inTarget = `EXISTS (SELECT 1 FROM email_mailbox y WHERE y.account_id = :a AND y.email_id = x.email_id AND y.mailbox_id = :to)`
	b.add(`INSERT INTO email_mailbox (account_id, email_id, mailbox_id, uid)
		SELECT :a, x.email_id, :to,
			(SELECT uidnext FROM mailbox WHERE account_id = :a AND id = :to) + row_number() OVER (ORDER BY x.uid) - 1
		FROM email_mailbox x
		WHERE x.account_id = :a AND x.mailbox_id = :m AND x.email_id IN ` + sqlStamped + ` AND NOT ` + inTarget)
	b.add(`UPDATE mailbox SET uidnext = max(uidnext,
			(SELECT max(uid) + 1 FROM email_mailbox WHERE account_id = :a AND mailbox_id = :to))
		WHERE account_id = :a AND id = :to`)
	pairsAt := len(b.stmts)
	b.add(`SELECT x.uid, y.uid FROM email_mailbox x
		JOIN email_mailbox y ON y.account_id = x.account_id AND y.email_id = x.email_id AND y.mailbox_id = :to
		WHERE x.account_id = :a AND x.mailbox_id = :m AND x.email_id IN ` + sqlStamped + ` ORDER BY x.uid`)
	if move {
		b.add(`DELETE FROM email_mailbox WHERE account_id = :a AND mailbox_id = :m AND email_id IN ` + sqlStamped)
		b.change("Mailbox", `:m AS id, 'counts' AS op`)
	}
	b.change("Mailbox", `:to AS id, 'counts' AS op`)
	b.change("Email", `s.id AS id, 'updated' AS op FROM email s WHERE s.account_id = :a AND s.modseq = `+sqlModseq)
	b.touchMailboxes()
	b.add(`SELECT (SELECT modseq FROM account WHERE id = :a), uidvalidity FROM mailbox WHERE account_id = :a AND id = :to`)
	res, err := s.run(ctx, b)
	if errors.Is(err, errNothing) {
		return s.targetOnly(ctx, account, to)
	}
	if err != nil {
		return CopyResult{}, err
	}
	last := res[len(res)-1].Values[0]
	out := CopyResult{State: asInt(last[0]), UIDValidity: uint32(asInt(last[1]))}
	for _, row := range res[pairsAt].Values {
		out.Pairs = append(out.Pairs, UIDPair{From: uint32(asInt(row[0])), To: uint32(asInt(row[1]))})
	}
	return out, nil
}

// renumber is a move of messages into the mailbox they are in: each gets
// the mailbox's next UID, in the order they had. The rows are replaced:
// the new ones come from a SELECT over the old ones, which SQLite reads
// in full before it writes when both are the same table.
func (s *Store) renumber(ctx context.Context, account string, mailbox int64, ranges []UIDRange) (CopyResult, error) {
	inRange, kv := uidCond(ranges)
	b := s.newBatch(account)
	b.base["m"] = mailbox
	b.guardAccount()
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`)
	b.stamp(inRange, kv...)
	const renumbered = `(SELECT uidnext FROM mailbox WHERE account_id = :a AND id = :m) + row_number() OVER (ORDER BY x.uid) - 1
		FROM email_mailbox x WHERE x.account_id = :a AND x.mailbox_id = :m AND x.email_id IN ` + sqlStamped
	pairsAt := len(b.stmts)
	b.add(`SELECT x.uid, ` + renumbered + ` ORDER BY x.uid`)
	b.add(`INSERT OR REPLACE INTO email_mailbox (account_id, email_id, mailbox_id, uid) SELECT :a, x.email_id, :m, ` + renumbered)
	b.add(`UPDATE mailbox SET uidnext = (SELECT max(uid) + 1 FROM email_mailbox WHERE account_id = :a AND mailbox_id = :m)
		WHERE account_id = :a AND id = :m`)
	b.change("Mailbox", `:m AS id, 'counts' AS op`)
	b.change("Email", `s.id AS id, 'updated' AS op FROM email s WHERE s.account_id = :a AND s.modseq = `+sqlModseq)
	b.touchMailboxes()
	b.add(`SELECT (SELECT modseq FROM account WHERE id = :a), uidvalidity FROM mailbox WHERE account_id = :a AND id = :m`)
	res, err := s.run(ctx, b)
	if errors.Is(err, errNothing) {
		return s.targetOnly(ctx, account, mailbox)
	}
	if err != nil {
		return CopyResult{}, err
	}
	last := res[len(res)-1].Values[0]
	out := CopyResult{State: asInt(last[0]), UIDValidity: uint32(asInt(last[1]))}
	for _, row := range res[pairsAt].Values {
		out.Pairs = append(out.Pairs, UIDPair{From: uint32(asInt(row[0])), To: uint32(asInt(row[1]))})
	}
	return out, nil
}

// targetOnly is the result of a copy that named no message.
func (s *Store) targetOnly(ctx context.Context, account string, to int64) (CopyResult, error) {
	res, err := s.query(ctx, account, q(`SELECT uidvalidity FROM mailbox WHERE account_id = :a AND id = :to`, "to", to))
	if err != nil {
		return CopyResult{}, err
	}
	if len(res[0].Values) == 0 {
		return CopyResult{}, ErrNoMailbox
	}
	return CopyResult{UIDValidity: uint32(asInt(res[0].Values[0][0]))}, nil
}

// sameMailbox is a copy of messages into the mailbox they are in: nothing
// changes, every message is its own copy.
func (s *Store) sameMailbox(ctx context.Context, account string, mailbox int64, ranges []UIDRange) (CopyResult, error) {
	inRange, kv := uidCond(ranges)
	out, err := s.targetOnly(ctx, account, mailbox)
	if err != nil {
		return out, err
	}
	res, err := s.query(ctx, account, q(`SELECT x.uid FROM email_mailbox x
		WHERE x.account_id = :a AND x.mailbox_id = :m AND `+inRange+` ORDER BY x.uid`, append(kv, "m", mailbox)...))
	if err != nil {
		return out, err
	}
	for _, row := range res[0].Values {
		uid := uint32(asInt(row[0]))
		out.Pairs = append(out.Pairs, UIDPair{From: uid, To: uid})
	}
	return out, nil
}

// Expunge is IMAP's EXPUNGE: it takes the messages with $deleted out of
// the mailbox, all of them or, with uids, those named (UID EXPUNGE, RFC
// 4315). It returns the new state, or 0 when there were none.
//
// What that means for an Email with labels: an Email that is in other
// mailboxes too only loses this label, and its $deleted keyword with it,
// so that the copies elsewhere are not swept up by the next EXPUNGE there.
// An Email that is in no other mailbox is destroyed. It is not moved to
// Trash: mail clients move to Trash themselves, before they expunge, and
// EXPUNGE in Trash has to mean gone.
func (s *Store) Expunge(ctx context.Context, account string, mailbox int64, uids []UIDRange) (int64, error) {
	var state int64
	pieces := splitRanges(uids)
	for _, ranges := range pieces {
		st, err := s.expunge(ctx, account, mailbox, ranges)
		if err != nil {
			return 0, err
		}
		if st != 0 {
			state = st
		}
	}
	return state, nil
}

func (s *Store) expunge(ctx context.Context, account string, mailbox int64, ranges []UIDRange) (int64, error) {
	inRange, kv := uidCond(ranges)
	// The stamped emails that are in no other mailbox.
	const doomed = `(SELECT e.id FROM email e WHERE e.account_id = :a AND e.modseq = ` + sqlModseq + `
		AND NOT EXISTS (SELECT 1 FROM email_mailbox y
			WHERE y.account_id = :a AND y.email_id = e.id AND y.mailbox_id <> :m))`
	b := s.newBatch(account)
	b.base["m"] = mailbox
	b.guardAccount()
	b.guard(ErrNoMailbox, `EXISTS (SELECT 1 FROM mailbox WHERE account_id = :a AND id = :m)`)
	b.stamp(inRange+` AND EXISTS (SELECT 1 FROM keyword k
		WHERE k.account_id = x.account_id AND k.email_id = x.email_id AND k.keyword = '`+KeywordDeleted+`')`, kv...)

	b.change("Email", `s.id AS id, CASE WHEN s.id IN `+doomed+` THEN 'destroyed' ELSE 'updated' END AS op
		FROM email s WHERE s.account_id = :a AND s.modseq = `+sqlModseq)
	b.change("Thread", `e.thread_id AS id, CASE WHEN EXISTS (SELECT 1 FROM email o
				WHERE o.account_id = :a AND o.thread_id = e.thread_id AND o.id NOT IN `+doomed+`)
			THEN 'updated' ELSE 'destroyed' END AS op
		FROM email e WHERE e.account_id = :a AND e.id IN `+doomed)
	// This mailbox; the other mailboxes of an email that stays (it loses a
	// keyword); and mailboxes that share a thread with a destroyed email
	// (their thread counts).
	b.change("Mailbox", `:m AS id, 'counts' AS op`)
	b.change("Mailbox", `y.mailbox_id AS id, 'counts' AS op FROM email_mailbox y
		WHERE y.account_id = :a AND y.email_id IN `+sqlStamped)
	b.change("Mailbox", `y.mailbox_id AS id, 'counts' AS op FROM email_mailbox y
		JOIN email o ON o.account_id = y.account_id AND o.id = y.email_id
		WHERE y.account_id = :a AND o.thread_id IN (
			SELECT e.thread_id FROM email e WHERE e.account_id = :a AND e.id IN `+doomed+`)`)
	b.touchMailboxes()
	b.add(`DELETE FROM thread_ref WHERE account_id = :a
		AND thread_id IN (SELECT e.thread_id FROM email e WHERE e.account_id = :a AND e.id IN ` + doomed + `)
		AND NOT EXISTS (SELECT 1 FROM email o
			WHERE o.account_id = :a AND o.thread_id = thread_ref.thread_id AND o.id NOT IN ` + doomed + `)`)
	b.add(`DELETE FROM keyword WHERE account_id = :a AND email_id IN ` + sqlStamped + `
		AND (keyword = '` + KeywordDeleted + `' OR email_id IN ` + doomed + `)`)
	b.add(`DELETE FROM email WHERE account_id = :a AND modseq = ` + sqlModseq + `
		AND NOT EXISTS (SELECT 1 FROM email_mailbox y
			WHERE y.account_id = :a AND y.email_id = email.id AND y.mailbox_id <> :m)`)
	// The rows of the emails that stay, and of the ones just destroyed.
	b.add(`DELETE FROM email_mailbox WHERE account_id = :a AND mailbox_id = :m AND (
		email_id IN ` + sqlStamped + `
		OR NOT EXISTS (SELECT 1 FROM email e WHERE e.account_id = :a AND e.id = email_mailbox.email_id))`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if errors.Is(err, errNothing) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}

// MailboxUIDValidity returns a mailbox's UIDVALIDITY.
func (s *Store) MailboxUIDValidity(ctx context.Context, account string, mailbox int64) (uint32, error) {
	r, err := s.targetOnly(ctx, account, mailbox)
	return r.UIDValidity, err
}
