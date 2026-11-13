package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mikepage/robinmail/internal/rqlite"
)

// schemaVersion goes up with every change to the tables below. There are no
// migrations yet: Init refuses a database of another version.
const schemaVersion = 1

// Every mail table starts with account_id, so accounts could be spread over
// several rqlite clusters later.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`,

	// Never holds a row. See batch.guard.
	`CREATE TABLE IF NOT EXISTS guard (ok INTEGER NOT NULL CHECK (ok = 0))`,

	`CREATE TABLE IF NOT EXISTS domain (
		name TEXT NOT NULL PRIMARY KEY
	) WITHOUT ROWID`,

	// id is random and never changes. The counters hand out Email, Mailbox
	// and Thread ids. Change log rows up to trimmed_modseq are gone.
	`CREATE TABLE IF NOT EXISTS account (
		id             TEXT NOT NULL PRIMARY KEY,
		address        TEXT NOT NULL UNIQUE,
		password_hash  TEXT NOT NULL,
		created_at     INTEGER NOT NULL,
		modseq         INTEGER NOT NULL,
		trimmed_modseq INTEGER NOT NULL DEFAULT 0,
		next_email     INTEGER NOT NULL,
		next_mailbox   INTEGER NOT NULL,
		next_thread    INTEGER NOT NULL
	) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS session (
		token_hash TEXT NOT NULL PRIMARY KEY,
		account_id TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	) WITHOUT ROWID`,

	// parent_id 0 is the top level. A rename changes name only.
	`CREATE TABLE IF NOT EXISTS mailbox (
		account_id  TEXT NOT NULL,
		id          INTEGER NOT NULL,
		parent_id   INTEGER NOT NULL DEFAULT 0,
		name        TEXT NOT NULL,
		role        TEXT,
		sort_order  INTEGER NOT NULL DEFAULT 0,
		uidvalidity INTEGER NOT NULL,
		uidnext     INTEGER NOT NULL DEFAULT 1,
		modseq      INTEGER NOT NULL,
		PRIMARY KEY (account_id, id),
		UNIQUE (account_id, parent_id, name)
	) WITHOUT ROWID`,
	`CREATE UNIQUE INDEX IF NOT EXISTS mailbox_role ON mailbox (account_id, role) WHERE role IS NOT NULL`,

	// blob is the SHA-256 of the raw message; the blob key adds the account.
	// The header fields are cached here for lists and sorting.
	`CREATE TABLE IF NOT EXISTS email (
		account_id     TEXT NOT NULL,
		id             INTEGER NOT NULL,
		blob           TEXT NOT NULL,
		size           INTEGER NOT NULL,
		received_at    INTEGER NOT NULL,
		thread_id      INTEGER NOT NULL,
		message_id     TEXT NOT NULL DEFAULT '',
		sender         TEXT NOT NULL DEFAULT '',
		subject        TEXT NOT NULL DEFAULT '',
		preview        TEXT NOT NULL DEFAULT '',
		has_attachment INTEGER NOT NULL DEFAULT 0,
		modseq         INTEGER NOT NULL,
		PRIMARY KEY (account_id, id)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS email_thread ON email (account_id, thread_id)`,
	`CREATE INDEX IF NOT EXISTS email_blob ON email (account_id, blob)`,

	// One row per label. The IMAP UID belongs to the row, not to the email.
	`CREATE TABLE IF NOT EXISTS email_mailbox (
		account_id TEXT NOT NULL,
		email_id   INTEGER NOT NULL,
		mailbox_id INTEGER NOT NULL,
		uid        INTEGER NOT NULL,
		PRIMARY KEY (account_id, email_id, mailbox_id),
		UNIQUE (account_id, mailbox_id, uid)
	) WITHOUT ROWID`,

	`CREATE TABLE IF NOT EXISTS keyword (
		account_id TEXT NOT NULL,
		email_id   INTEGER NOT NULL,
		keyword    TEXT NOT NULL,
		PRIMARY KEY (account_id, email_id, keyword)
	) WITHOUT ROWID`,

	// Every Message-ID seen in Message-ID, In-Reply-To and References.
	`CREATE TABLE IF NOT EXISTS thread_ref (
		account_id TEXT NOT NULL,
		message_id TEXT NOT NULL,
		thread_id  INTEGER NOT NULL,
		PRIMARY KEY (account_id, message_id)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS thread_ref_thread ON thread_ref (account_id, thread_id)`,

	// type is Email, Mailbox or Thread; op is created, updated or destroyed.
	// at is when the row was written, for trimming.
	`CREATE TABLE IF NOT EXISTS changes (
		account_id TEXT NOT NULL,
		modseq     INTEGER NOT NULL,
		type       TEXT NOT NULL,
		id         INTEGER NOT NULL,
		op         TEXT NOT NULL,
		at         INTEGER NOT NULL,
		PRIMARY KEY (account_id, modseq, type, id)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS changes_at ON changes (at)`,

	// Delivery keys, so a repeated delivery stores nothing.
	`CREATE TABLE IF NOT EXISTS delivered (
		account_id TEXT NOT NULL,
		key        TEXT NOT NULL,
		at         INTEGER NOT NULL,
		PRIMARY KEY (account_id, key)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS delivered_at ON delivered (at)`,

	// Outbound queue. Nothing uses it before phase 5.
	`CREATE TABLE IF NOT EXISTS submission (
		account_id    TEXT NOT NULL,
		id            INTEGER NOT NULL,
		email_id      INTEGER NOT NULL,
		envelope      TEXT NOT NULL,
		state         TEXT NOT NULL,
		next_try      INTEGER NOT NULL DEFAULT 0,
		claimed_by    TEXT NOT NULL DEFAULT '',
		claimed_until INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, id)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS submission_next ON submission (state, next_try)`,

	`INSERT INTO schema_version (version) SELECT ` + fmt.Sprint(schemaVersion) + ` WHERE NOT EXISTS (SELECT 1 FROM schema_version)`,
}

// Init creates the tables in an empty database and checks the schema version
// of an existing one. Call it once at start. It is safe to run on several
// nodes at the same time.
func (s *Store) Init(ctx context.Context) error {
	version, err := s.version(ctx)
	if err != nil {
		return err
	}
	if version == 0 {
		stmts := make([]rqlite.Statement, len(schema))
		for i, sql := range schema {
			stmts[i] = rqlite.Stmt(sql)
		}
		if _, err := s.db.Execute(ctx, stmts...); err != nil {
			return fmt.Errorf("store: creating schema: %w", err)
		}
		if version, err = s.version(ctx); err != nil {
			return err
		}
	}
	if version != schemaVersion {
		return fmt.Errorf("store: database has schema version %d, this binary needs %d", version, schemaVersion)
	}
	return nil
}

// version returns 0 for a database without our tables.
func (s *Store) version(ctx context.Context) (int64, error) {
	res, err := s.db.Query(ctx, rqlite.Weak, rqlite.Stmt(`SELECT max(version) FROM schema_version`))
	var e *rqlite.Error
	if errors.As(err, &e) && strings.Contains(e.Message, "no such table") {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	if len(res[0].Values) == 0 {
		return 0, nil
	}
	return asInt(res[0].Values[0][0]), nil
}
