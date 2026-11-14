package store

import (
	"context"

	"github.com/mikepage/robinmail/internal/search"
)

// sqlFTSBase is the first rowid of the account :a in email_fts; its emails
// are the 2^32 rowids from there.
const sqlFTSBase = `((SELECT seq FROM account WHERE id = :a) * 4294967296)`

// sqlFTSRow is the rowid in email_fts of the email whose id the SQL
// expression id yields.
func sqlFTSRow(id string) string { return `(` + sqlFTSBase + ` + ` + id + `)` }

// indexEmail adds the email with the given id (a SQL expression) to the
// full-text index. The statement is part of the batch that stores the
// email, so the two exist together or not at all.
func (b *batch) indexEmail(id string, doc indexDoc) {
	b.add(`INSERT INTO email_fts (rowid, subject, addr, body, attach, hdr)
		SELECT `+sqlFTSRow(id)+`, :ix_subject, :ix_addr, :ix_body, :ix_attach, :ix_hdr
		WHERE EXISTS (SELECT 1 FROM email WHERE account_id = :a AND id = `+id+`)`,
		"ix_subject", doc.subject, "ix_addr", doc.addresses, "ix_body", doc.body,
		"ix_attach", doc.attachments, "ix_hdr", doc.headers)
}

// Columns of email_fts that each kind of condition looks in.
const (
	ftsTextColumns   = `{subject addr body attach}`
	ftsBodyColumns   = `{body}`
	ftsHeaderColumns = `{hdr}`
)

// fts is the condition "the email is among those the index finds for expr
// in these columns". An empty expression finds nothing: a search for
// something without a letter or digit in it.
func (b *sqlBuilder) fts(columns, expr string) string {
	if expr == "" {
		return "0"
	}
	return `e.id IN (SELECT rowid - ` + sqlFTSBase + ` FROM email_fts WHERE email_fts MATCH ` +
		b.param(columns+` : (`+expr+`)`) + ` AND rowid BETWEEN ` + sqlFTSBase + ` AND ` + sqlFTSBase + ` + 4294967295)`
}

// textConditions are the conditions of a filter that the index answers.
func (b *sqlBuilder) textConditions(f *Filter) []string {
	var c []string
	if f.Text != nil {
		c = append(c, b.fts(ftsTextColumns, search.FTSQuery(search.ParseQuery(*f.Text))))
	}
	if f.Body != nil {
		c = append(c, b.fts(ftsBodyColumns, search.FTSQuery(search.ParseQuery(*f.Body))))
	}
	if len(f.Header) > 0 {
		value := ""
		if len(f.Header) > 1 {
			value = f.Header[1]
		}
		c = append(c, b.fts(ftsHeaderColumns, search.HeaderFTS(f.Header[0], value)))
	}
	if f.BodyPhrase != nil {
		c = append(c, b.fts(ftsBodyColumns, search.FTSQuery([]search.Term{search.Phrase(*f.BodyPhrase)})))
	}
	if f.TextPhrase != nil {
		// In hdr every word stands behind its field's marker, so a phrase
		// of several words is only looked for in the subject, the
		// addresses and the body; a single word is found in any field.
		t := search.Phrase(*f.TextPhrase)
		columns := ftsTextColumns
		if len(t) == 1 {
			columns = `{subject addr body attach hdr}`
		}
		c = append(c, b.fts(columns, search.FTSQuery([]search.Term{t})))
	}
	return c
}

// SearchMailbox is IMAP's SEARCH BODY (bodyOnly) and SEARCH TEXT on the
// index: the ids of the emails in the mailbox that have text as a phrase
// of words. indexed is false when text has no word in it; the index cannot
// answer then.
func (s *Store) SearchMailbox(ctx context.Context, account string, mailbox int64, text string, bodyOnly bool) (ids []int64, indexed bool, err error) {
	if len(search.Phrase(text)) == 0 {
		return nil, false, nil
	}
	f := Filter{InMailbox: &mailbox}
	if bodyOnly {
		f.BodyPhrase = &text
	} else {
		f.TextPhrase = &text
	}
	res, err := s.QueryEmails(ctx, account, EmailQuery{Filter: &f, Limit: -1})
	if err != nil {
		return nil, false, err
	}
	return res.IDs, true, nil
}
