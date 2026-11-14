package store

import (
	"context"
	"time"
)

// VacationResponse is the automatic reply of an account as JMAP has it
// (RFC 8621 8): one per account. A nil date has no bound on that side; a
// nil subject or body is the server's to fill in.
type VacationResponse struct {
	Enabled  bool
	From     *time.Time
	To       *time.Time
	Subject  *string
	TextBody *string
	HTMLBody *string
}

// Active reports whether the reply is on at a moment: enabled, on or
// after From and before To.
func (v VacationResponse) Active(now time.Time) bool {
	return v.Enabled && (v.From == nil || !now.Before(*v.From)) && (v.To == nil || now.Before(*v.To))
}

// VacationResponse returns the account's vacation response. An account
// that never set one has the zero value: not enabled.
func (s *Store) VacationResponse(ctx context.Context, account string) (VacationResponse, error) {
	res, err := s.query(ctx, account, q(`SELECT enabled, from_at, to_at, subject, text_body, html_body FROM vacation WHERE account_id = :a`))
	if err != nil {
		return VacationResponse{}, err
	}
	var v VacationResponse
	if len(res[0].Values) == 0 {
		return v, nil
	}
	row := res[0].Values[0]
	v.Enabled = asInt(row[0]) != 0
	date := func(x any) *time.Time {
		if x == nil {
			return nil
		}
		t := time.Unix(asInt(x), 0).UTC()
		return &t
	}
	text := func(x any) *string {
		if x == nil {
			return nil
		}
		s := asString(x)
		return &s
	}
	v.From, v.To, v.Subject, v.TextBody, v.HTMLBody = date(row[1]), date(row[2]), text(row[3]), text(row[4]), text(row[5])
	return v, nil
}

// SetVacationResponse replaces the account's vacation response and returns
// the new state.
func (s *Store) SetVacationResponse(ctx context.Context, account string, v VacationResponse, ifState int64) (int64, error) {
	date := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return t.Unix()
	}
	text := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	b := s.newBatch(account)
	b.guardAccount()
	b.ifState(ifState)
	b.bump()
	b.add(`INSERT INTO vacation (account_id, enabled, from_at, to_at, subject, text_body, html_body, modseq)
		VALUES (:a, :enabled, :from_at, :to_at, :subject, :text_body, :html_body, `+sqlModseq+`)
		ON CONFLICT (account_id) DO UPDATE SET enabled = excluded.enabled, from_at = excluded.from_at, to_at = excluded.to_at,
			subject = excluded.subject, text_body = excluded.text_body, html_body = excluded.html_body, modseq = excluded.modseq`,
		"enabled", boolInt(v.Enabled), "from_at", date(v.From), "to_at", date(v.To),
		"subject", text(v.Subject), "text_body", text(v.TextBody), "html_body", text(v.HTMLBody))
	b.change("VacationResponse", `1 AS id, 'updated' AS op`)
	b.add(`SELECT modseq FROM account WHERE id = :a`)
	res, err := s.run(ctx, b)
	if err != nil {
		return 0, err
	}
	return asInt(res[len(res)-1].Values[0][0]), nil
}
