package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/mikepage/robinmail/internal/rqlite"
)

// How long the change log and the delivery keys are kept.
const (
	ChangesRetention   = 30 * 24 * time.Hour
	DeliveredRetention = 7 * 24 * time.Hour
)

// ChangeSet lists the ids of one type that changed, each id in one list.
type ChangeSet struct {
	Created   []int64
	Updated   []int64
	Destroyed []int64
}

// Changes is what happened to an account between two states.
type Changes struct {
	OldState int64
	NewState int64
	Email    ChangeSet
	Mailbox  ChangeSet
	Thread   ChangeSet
}

// Changes returns what changed after state since. An object created and
// destroyed in between is left out; one created and then updated is only
// "created"; one updated and then destroyed is only "destroyed".
func (s *Store) Changes(ctx context.Context, account string, since int64) (Changes, error) {
	res, err := s.query(ctx, account,
		q(`SELECT modseq, trimmed_modseq FROM account WHERE id = :a`),
		q(`SELECT type, id, op FROM changes WHERE account_id = :a AND modseq > :since ORDER BY modseq`, "since", since))
	if err != nil {
		return Changes{}, err
	}
	if len(res[0].Values) == 0 {
		return Changes{}, ErrNoAccount
	}
	state, trimmed := asInt(res[0].Values[0][0]), asInt(res[0].Values[0][1])
	if since < trimmed || since > state || since < 0 {
		return Changes{}, ErrCannotCalculateChanges
	}

	type key struct {
		typ string
		id  int64
	}
	type span struct{ first, last string }
	seen := map[key]*span{}
	var order []key
	for _, row := range res[1].Values {
		k, op := key{asString(row[0]), asInt(row[1])}, asString(row[2])
		if sp, ok := seen[k]; ok {
			sp.last = op
			continue
		}
		seen[k] = &span{op, op}
		order = append(order, k)
	}
	c := Changes{OldState: since, NewState: state}
	for _, k := range order {
		var set *ChangeSet
		switch k.typ {
		case "Email":
			set = &c.Email
		case "Mailbox":
			set = &c.Mailbox
		case "Thread":
			set = &c.Thread
		default:
			return Changes{}, fmt.Errorf("store: unknown change type %q", k.typ)
		}
		sp := seen[k]
		switch {
		case sp.first == "created" && sp.last == "destroyed":
		case sp.first == "created":
			set.Created = append(set.Created, k.id)
		case sp.last == "destroyed":
			set.Destroyed = append(set.Destroyed, k.id)
		default:
			set.Updated = append(set.Updated, k.id)
		}
	}
	for _, set := range []*ChangeSet{&c.Email, &c.Mailbox, &c.Thread} {
		for _, list := range [][]int64{set.Created, set.Updated, set.Destroyed} {
			sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
		}
	}
	return c, nil
}

// TrimChanges removes change log rows written before the given time, for
// all accounts, and returns how many. Each account remembers the highest
// modseq it lost, so Changes can refuse a state from before that.
func (s *Store) TrimChanges(ctx context.Context, before time.Time) (int64, error) {
	t := before.Unix()
	res, err := s.db.Execute(ctx,
		rqlite.Stmt(`UPDATE account SET trimmed_modseq = max(trimmed_modseq,
				(SELECT max(c.modseq) FROM changes c WHERE c.account_id = account.id AND c.at < ?))
			WHERE EXISTS (SELECT 1 FROM changes c WHERE c.account_id = account.id AND c.at < ?)`, t, t),
		rqlite.Stmt(`DELETE FROM changes WHERE at < ?`, t))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[1].RowsAffected, nil
}

// TrimDelivered forgets delivery keys stored before the given time and
// returns how many.
func (s *Store) TrimDelivered(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.Execute(ctx, rqlite.Stmt(`DELETE FROM delivered WHERE at < ?`, before.Unix()))
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return res[0].RowsAffected, nil
}
