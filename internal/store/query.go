package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Filter is the filter of an Email/query (RFC 8621 4.4.1): either an
// operator over other filters or a condition, of which every field that is
// set must hold. Only what the email table and its indexes can answer is
// here; text search waits for phase 8.
type Filter struct {
	// Operator is "AND", "OR" or "NOT" (none of Conditions holds), or
	// empty for a condition.
	Operator   string
	Conditions []Filter

	InMailbox *int64
	// InMailboxOtherThan: the email is in a mailbox that is not one of
	// these. nil is not set; an empty list is set and matches every email.
	InMailboxOtherThan []int64
	Before             *time.Time // received before
	After              *time.Time // received at or after
	MinSize            *int64     // size at least
	MaxSize            *int64     // size below
	HasKeyword         *string
	NotKeyword         *string
	// The three below look at all emails of the email's thread.
	AllInThreadHaveKeyword  *string
	SomeInThreadHaveKeyword *string
	NoneInThreadHaveKeyword *string
	HasAttachment           *bool
	// Substring matches on the cached header text, ASCII case-insensitive.
	From, To, Cc, Bcc, Subject *string
}

// Sort is one comparator of an Email/query.
type Sort struct {
	// Property is one of SortProperties.
	Property string
	// Keyword is the keyword for the property "hasKeyword".
	Keyword   string
	Ascending bool
}

// SortProperties are the properties QueryEmails can sort by.
var SortProperties = []string{"receivedAt", "sentAt", "size", "from", "to", "subject", "hasKeyword"}

// ErrUnsupportedSort is returned for a sort property not in SortProperties.
var ErrUnsupportedSort = errors.New("unsupported sort property")

// EmailQuery asks for a window of the emails that match a filter.
type EmailQuery struct {
	Filter *Filter // nil: all emails
	Sort   []Sort  // empty: newest first
	// CollapseThreads keeps of each thread only the email that comes first
	// in the sort order.
	CollapseThreads bool
	Offset          int64
	Limit           int64 // below 0: no limit
	Total           bool  // also count all matches
}

// QueryResult is one window of ids, and the state they were read in.
type QueryResult struct {
	IDs   []int64
	Total int64 // only if asked for
	State int64
}

// sqlBuilder collects the parameters of a generated query.
type sqlBuilder struct {
	kv []any
	n  int
}

func (b *sqlBuilder) param(v any) string {
	name := "f" + strconv.Itoa(b.n)
	b.n++
	b.kv = append(b.kv, name, v)
	return ":" + name
}

const sqlEmailKeyword = `EXISTS (SELECT 1 FROM keyword k WHERE k.account_id = e.account_id AND k.email_id = e.id AND k.keyword = `

// sqlThreadKeyword counts nothing: it is the emails of e's thread that do
// (have = true) or do not carry a keyword.
func (b *sqlBuilder) threadKeyword(keyword string, have bool) string {
	not := ""
	if !have {
		not = "NOT "
	}
	return `EXISTS (SELECT 1 FROM email t WHERE t.account_id = e.account_id AND t.thread_id = e.thread_id AND ` + not +
		`EXISTS (SELECT 1 FROM keyword k WHERE k.account_id = t.account_id AND k.email_id = t.id AND k.keyword = ` +
		b.param(strings.ToLower(keyword)) + `))`
}

// where turns a filter into a SQL expression over the email row e.
func (b *sqlBuilder) where(f *Filter) (string, error) {
	if f == nil {
		return "1", nil
	}
	if f.Operator != "" {
		parts := make([]string, 0, len(f.Conditions))
		for i := range f.Conditions {
			p, err := b.where(&f.Conditions[i])
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		switch f.Operator {
		case "AND":
			if len(parts) == 0 {
				return "1", nil
			}
			return "(" + strings.Join(parts, " AND ") + ")", nil
		case "OR":
			if len(parts) == 0 {
				return "0", nil
			}
			return "(" + strings.Join(parts, " OR ") + ")", nil
		case "NOT":
			if len(parts) == 0 {
				return "1", nil
			}
			return "NOT (" + strings.Join(parts, " OR ") + ")", nil
		}
		return "", fmt.Errorf("store: unknown filter operator %q", f.Operator)
	}

	const inMailbox = `EXISTS (SELECT 1 FROM email_mailbox x WHERE x.account_id = e.account_id AND x.email_id = e.id`
	var c []string
	if f.InMailbox != nil {
		c = append(c, inMailbox+` AND x.mailbox_id = `+b.param(*f.InMailbox)+`)`)
	}
	if f.InMailboxOtherThan != nil {
		cond := inMailbox
		if len(f.InMailboxOtherThan) > 0 {
			names := make([]string, len(f.InMailboxOtherThan))
			for i, id := range f.InMailboxOtherThan {
				names[i] = "(" + b.param(id) + ")"
			}
			cond += ` AND x.mailbox_id NOT IN (VALUES ` + strings.Join(names, ",") + `)`
		}
		c = append(c, cond+`)`)
	}
	if f.Before != nil {
		c = append(c, `e.received_at < `+b.param(f.Before.Unix()))
	}
	if f.After != nil {
		c = append(c, `e.received_at >= `+b.param(f.After.Unix()))
	}
	if f.MinSize != nil {
		c = append(c, `e.size >= `+b.param(*f.MinSize))
	}
	if f.MaxSize != nil {
		c = append(c, `e.size < `+b.param(*f.MaxSize))
	}
	if f.HasKeyword != nil {
		c = append(c, sqlEmailKeyword+b.param(strings.ToLower(*f.HasKeyword))+`)`)
	}
	if f.NotKeyword != nil {
		c = append(c, `NOT `+sqlEmailKeyword+b.param(strings.ToLower(*f.NotKeyword))+`)`)
	}
	if f.AllInThreadHaveKeyword != nil {
		c = append(c, `NOT `+b.threadKeyword(*f.AllInThreadHaveKeyword, false))
	}
	if f.SomeInThreadHaveKeyword != nil {
		c = append(c, b.threadKeyword(*f.SomeInThreadHaveKeyword, true))
	}
	if f.NoneInThreadHaveKeyword != nil {
		c = append(c, `NOT `+b.threadKeyword(*f.NoneInThreadHaveKeyword, true))
	}
	if f.HasAttachment != nil {
		c = append(c, `e.has_attachment = `+b.param(boolInt(*f.HasAttachment)))
	}
	for _, t := range []struct {
		col string
		val *string
	}{{"sender", f.From}, {"recipients", f.To}, {"cc", f.Cc}, {"bcc", f.Bcc}, {"subject", f.Subject}} {
		if t.val != nil {
			c = append(c, `instr(lower(e.`+t.col+`), lower(`+b.param(*t.val)+`)) > 0`)
		}
	}
	if len(c) == 0 {
		return "1", nil
	}
	return "(" + strings.Join(c, " AND ") + ")", nil
}

// orderBy turns comparators into an ORDER BY list. The id comes last, so
// the order is total and a window is stable between calls.
func (b *sqlBuilder) orderBy(sorts []Sort) (string, error) {
	if len(sorts) == 0 {
		return `e.received_at DESC, e.id DESC`, nil
	}
	var parts []string
	for _, s := range sorts {
		var expr string
		switch s.Property {
		case "receivedAt":
			expr = `e.received_at`
		case "sentAt":
			expr = `e.sent_at`
		case "size":
			expr = `e.size`
		case "from":
			expr = `lower(e.sender)`
		case "to":
			expr = `lower(e.recipients)`
		case "subject":
			expr = `lower(e.subject)`
		case "hasKeyword":
			expr = sqlEmailKeyword + b.param(strings.ToLower(s.Keyword)) + `)`
		default:
			return "", fmt.Errorf("%w: %q", ErrUnsupportedSort, s.Property)
		}
		if !s.Ascending {
			expr += ` DESC`
		}
		parts = append(parts, expr)
	}
	return strings.Join(parts, ", ") + `, e.id`, nil
}

// QueryEmails runs an Email/query: filter, sort, optionally one email per
// thread, then a window. The ids, the total and the state come from one
// snapshot.
func (s *Store) QueryEmails(ctx context.Context, account string, query EmailQuery) (QueryResult, error) {
	var b sqlBuilder
	where, err := b.where(query.Filter)
	if err != nil {
		return QueryResult{}, err
	}
	order, err := b.orderBy(query.Sort)
	if err != nil {
		return QueryResult{}, err
	}
	// r numbers the matches in sort order. Collapsing keeps the first of
	// each thread by that number.
	matches := `WITH r AS (SELECT e.id AS id, e.thread_id AS thread_id, row_number() OVER (ORDER BY ` + order + `) AS pos
		FROM email e WHERE e.account_id = :a AND ` + where + `)`
	rows := `SELECT id, pos FROM r`
	if query.CollapseThreads {
		rows = `SELECT id, pos FROM (SELECT id, pos, row_number() OVER (PARTITION BY thread_id ORDER BY pos) AS nth FROM r) WHERE nth = 1`
	}
	page := matches + ` SELECT id FROM (` + rows + `) ORDER BY pos`
	kv := append([]any{}, b.kv...)
	if query.Limit >= 0 || query.Offset > 0 {
		limit := query.Limit
		if limit < 0 {
			limit = -1 // SQLite: no limit
		}
		page += ` LIMIT :limit OFFSET :offset`
		kv = append(kv, "limit", limit, "offset", max(query.Offset, 0))
	}
	stmts := [][]any{q(`SELECT modseq FROM account WHERE id = :a`), q(page, kv...)}
	if query.Total {
		stmts = append(stmts, q(matches+` SELECT count(*) FROM (`+rows+`)`, b.kv...))
	}
	res, err := s.query(ctx, account, stmts...)
	if err != nil {
		return QueryResult{}, err
	}
	if len(res[0].Values) == 0 {
		return QueryResult{}, ErrNoAccount
	}
	out := QueryResult{State: asInt(res[0].Values[0][0]), IDs: make([]int64, 0, len(res[1].Values))}
	for _, row := range res[1].Values {
		out.IDs = append(out.IDs, asInt(row[0]))
	}
	if query.Total {
		out.Total = asInt(res[2].Values[0][0])
	}
	return out, nil
}
