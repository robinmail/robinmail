package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mikepage/robinmail/internal/rqlite"
)

// schemaVersion goes up with every change to the tables below. Init brings
// an older database up to date with the statements in migrations and
// refuses a newer one.
const schemaVersion = 13

// Every mail table starts with account_id, so accounts could be spread over
// several rqlite clusters later.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`,

	// Never holds a row. See batch.guard.
	`CREATE TABLE IF NOT EXISTS guard (ok INTEGER NOT NULL CHECK (ok = 0))`,

	`CREATE TABLE IF NOT EXISTS domain (
		name TEXT NOT NULL PRIMARY KEY
	) WITHOUT ROWID`,

	// id is random and never changes. blob_key encrypts the account's blobs
	// and is stored wrapped by the master key. The counters hand out Email,
	// Mailbox and Thread ids. Change log rows up to trimmed_modseq are gone.
	// uidvalidity is the last UIDVALIDITY a mailbox of the account got.
	`CREATE TABLE IF NOT EXISTS account (
		id             TEXT NOT NULL PRIMARY KEY,
		address        TEXT NOT NULL UNIQUE,
		password_hash  TEXT NOT NULL,
		blob_key       TEXT NOT NULL,
		created_at     INTEGER NOT NULL,
		modseq         INTEGER NOT NULL,
		trimmed_modseq INTEGER NOT NULL DEFAULT 0,
		next_email     INTEGER NOT NULL,
		next_mailbox   INTEGER NOT NULL,
		next_thread    INTEGER NOT NULL,
		next_submission INTEGER NOT NULL DEFAULT 1,
		next_identity   INTEGER NOT NULL DEFAULT 2,
		uidvalidity     INTEGER NOT NULL DEFAULT 0,
		auth_seq        INTEGER NOT NULL DEFAULT 0,
		password_changed_at INTEGER NOT NULL DEFAULT 0,
		seq            INTEGER NOT NULL DEFAULT 0,
		next_sieve     INTEGER NOT NULL DEFAULT 1,
		quota_bytes    INTEGER,
		quota_count    INTEGER,
		used_bytes     INTEGER NOT NULL DEFAULT 0,
		used_count     INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`,
	sqlAccountSeq,

	sqlSessionTable, sqlSessionExpires, sqlSessionAccount,
	sqlAppPasswordTable, sqlTOTPTable, sqlRecoveryCodeTable, sqlOIDCLinkTable,
	sqlAuthEventTable, sqlAuthEventAt, sqlAuthEventAccount,

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
		subscribed  INTEGER NOT NULL DEFAULT 1,
		PRIMARY KEY (account_id, id),
		UNIQUE (account_id, parent_id, name)
	) WITHOUT ROWID`,
	`CREATE UNIQUE INDEX IF NOT EXISTS mailbox_role ON mailbox (account_id, role) WHERE role IS NOT NULL`,

	// blob is the SHA-256 of the raw message, which with the account names
	// the blob file.
	// The header fields are cached here for lists, filters and sorting:
	// sender, recipients, cc and bcc are the From, To, Cc and Bcc fields as
	// text, sent_at is the Date field (0 if it has none that parses).
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
		recipients     TEXT NOT NULL DEFAULT '',
		cc             TEXT NOT NULL DEFAULT '',
		bcc            TEXT NOT NULL DEFAULT '',
		sent_at        INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, id)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS email_thread ON email (account_id, thread_id)`,
	`CREATE INDEX IF NOT EXISTS email_blob ON email (account_id, blob)`,
	`CREATE INDEX IF NOT EXISTS email_received ON email (account_id, received_at)`,

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

	sqlSubmissionTable,
	sqlSubmissionDue,
	sqlSubmissionMessage,
	sqlIdentityTable,
	sqlUploadTable,
	sqlSTSTable,

	sqlNodeTable,
	sqlLoginFailureTable,
	sqlEmailFTS, sqlEmailFTSDelete,
	sqlAliasTable, sqlAliasDomain, sqlAliasTarget,
	sqlSieveTable, sqlSieveActive, sqlVacationSentTable, sqlVacationSentAt,
	sqlVacationTable,
	sqlQuotaInsert, sqlQuotaDelete,
	sqlTLSResultTable, sqlTLSReportSentTable, sqlTLSReportInTable, sqlTLSReportInID,

	`INSERT INTO schema_version (version) SELECT ` + fmt.Sprint(schemaVersion) + ` WHERE NOT EXISTS (SELECT 1 FROM schema_version)`,
}

// migrations[v] takes a database from version v to v+1. Each runs as one
// batch together with the new version number.
var migrations = map[int64][]string{
	// 3: columns for JMAP. Mailbox/set can unsubscribe; Email/query filters
	// and sorts on To, Cc, Bcc and the Date field. Emails stored before
	// this have the new columns empty until "robinmail reindex".
	2: {
		`ALTER TABLE mailbox ADD COLUMN subscribed INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE email ADD COLUMN recipients TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email ADD COLUMN cc TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email ADD COLUMN bcc TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email ADD COLUMN sent_at INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS email_received ON email (account_id, received_at)`,
		`CREATE INDEX IF NOT EXISTS session_expires ON session (expires_at)`,
	},
	// 4: the cluster. Nodes find each other through the node table; login
	// failures are counted for all nodes together.
	3: {sqlNodeTable, sqlLoginFailureTable},
	// 5: sending. The submission table of the first schema was never used
	// and is replaced; accounts get a counter for submissions and one for
	// identities, and every account its first identity.
	4: {
		`DROP INDEX IF EXISTS submission_next`,
		`DROP TABLE IF EXISTS submission`,
		sqlSubmissionTable, sqlSubmissionDue, sqlSubmissionMessage, sqlIdentityTable, sqlUploadTable, sqlSTSTable,
		`ALTER TABLE account ADD COLUMN next_submission INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE account ADD COLUMN next_identity INTEGER NOT NULL DEFAULT 2`,
		`INSERT OR IGNORE INTO identity (account_id, id, name, email, modseq) SELECT id, 1, '', address, modseq FROM account`,
	},
	// 6: IMAP. An account remembers the last UIDVALIDITY it gave a mailbox,
	// so that a mailbox that is deleted and made again under the same name
	// never gets the one it had (RFC 3501 2.3.1.1).
	5: {
		`ALTER TABLE account ADD COLUMN uidvalidity INTEGER NOT NULL DEFAULT 0`,
		`UPDATE account SET uidvalidity = COALESCE((SELECT max(m.uidvalidity) FROM mailbox m WHERE m.account_id = account.id), 0)`,
	},
	// 7: authentication. Sessions get an id, a stage, a device and two
	// lifetimes; the table is made again, so everyone logs in once more.
	// App passwords, TOTP with recovery codes, the subject an OIDC provider
	// knows an account by, and the log of auth events are new. auth_seq
	// moves whenever a way into the account is withdrawn.
	6: {
		`ALTER TABLE account ADD COLUMN auth_seq INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE account ADD COLUMN password_changed_at INTEGER NOT NULL DEFAULT 0`,
		`DROP INDEX IF EXISTS session_expires`,
		`DROP TABLE IF EXISTS session`,
		sqlSessionTable, sqlSessionExpires, sqlSessionAccount,
		sqlAppPasswordTable, sqlTOTPTable, sqlRecoveryCodeTable, sqlOIDCLinkTable,
		sqlAuthEventTable, sqlAuthEventAt, sqlAuthEventAccount,
	},
	// 8: full-text search. Accounts get a number, which with the email id
	// makes the rowid of an email in the index. Mail stored before this is
	// not in the index until "robinmail reindex".
	7: {
		`ALTER TABLE account ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`,
		`UPDATE account SET seq = (SELECT count(*) FROM account b WHERE b.id <= account.id)`,
		sqlAccountSeq,
		sqlEmailFTS, sqlEmailFTSDelete,
	},
	// 9: aliases.
	8: {sqlAliasTable, sqlAliasDomain, sqlAliasTarget},
	// 10: Sieve scripts, and what vacation replies went to whom.
	9: {
		`ALTER TABLE account ADD COLUMN next_sieve INTEGER NOT NULL DEFAULT 1`,
		sqlSieveTable, sqlSieveActive, sqlVacationSentTable, sqlVacationSentAt,
	},
	// 11: JMAP's VacationResponse.
	10: {sqlVacationTable},
	// 12: quotas. The account row counts its emails and their octets;
	// two triggers on email keep the counters. quota_bytes and
	// quota_count are the account's own limits: NULL is the default of the
	// configuration, 0 no limit.
	11: {
		`ALTER TABLE account ADD COLUMN quota_bytes INTEGER`,
		`ALTER TABLE account ADD COLUMN quota_count INTEGER`,
		`ALTER TABLE account ADD COLUMN used_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE account ADD COLUMN used_count INTEGER NOT NULL DEFAULT 0`,
		`UPDATE account SET
			used_bytes = (SELECT COALESCE(sum(e.size), 0) FROM email e WHERE e.account_id = account.id),
			used_count = (SELECT count(*) FROM email e WHERE e.account_id = account.id)`,
		sqlQuotaInsert, sqlQuotaDelete,
	},
	// 13: TLS reporting (RFC 8460).
	12: {sqlTLSResultTable, sqlTLSReportSentTable, sqlTLSReportInTable, sqlTLSReportInID},
}

const (
	// seq numbers the accounts: 1, 2, ... It only exists to make integer
	// row ids for the full-text index (sqlFTSRow).
	sqlAccountSeq = `CREATE UNIQUE INDEX IF NOT EXISTS account_seq ON account (seq)`

	// The full-text index (FTS5). It is contentless: it holds the words of
	// each message and where they stand, not the text, and what a column
	// holds cannot be read back. That is still enough to learn what a
	// message says, so unlike the blobs this is not encrypted at rest (see
	// PLAN.md, phase 8). The rowid of an email is its account's seq times
	// 2^32 plus its id, so one account's rows are one range of rowids.
	// hdr holds the header fields in the form of search.HeaderDoc.
	sqlEmailFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS email_fts USING fts5(
		subject, addr, body, attach, hdr,
		content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2')`
	// Every way an email row goes (destroy, expunge, a mailbox destroyed
	// with its mail) takes its index entry along.
	sqlEmailFTSDelete = `CREATE TRIGGER IF NOT EXISTS email_fts_delete AFTER DELETE ON email BEGIN
		DELETE FROM email_fts WHERE rowid = (SELECT seq FROM account WHERE id = old.account_id) * 4294967296 + old.id;
	END`

	// Aliases: one row per target. address is an address of ours or a
	// pattern with one "*" ("*@domain" is the catch-all); domain is its
	// domain; wildcard says it is a pattern. target is an address anywhere.
	sqlAliasTable = `CREATE TABLE IF NOT EXISTS alias (
		address    TEXT NOT NULL,
		target     TEXT NOT NULL,
		domain     TEXT NOT NULL,
		wildcard   INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (address, target)
	) WITHOUT ROWID`
	sqlAliasDomain = `CREATE INDEX IF NOT EXISTS alias_domain ON alias (domain, wildcard)`
	sqlAliasTarget = `CREATE INDEX IF NOT EXISTS alias_target ON alias (target)`

	// Sieve scripts (RFC 9661). blob is the script, a blob of the account.
	// At most one script of an account has active = 1.
	sqlSieveTable = `CREATE TABLE IF NOT EXISTS sieve_script (
		account_id TEXT NOT NULL,
		id         INTEGER NOT NULL,
		name       TEXT NOT NULL,
		blob       TEXT NOT NULL,
		size       INTEGER NOT NULL,
		active     INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		modseq     INTEGER NOT NULL,
		PRIMARY KEY (account_id, id),
		UNIQUE (account_id, name)
	) WITHOUT ROWID`
	sqlSieveActive = `CREATE UNIQUE INDEX IF NOT EXISTS sieve_active ON sieve_script (account_id) WHERE active = 1`

	// Automatic replies that went out (RFC 5230 4.2): key is a hash of the
	// reply and of whom it was for; at is when. A reply with the same key
	// is not sent again within its period.
	sqlVacationSentTable = `CREATE TABLE IF NOT EXISTS vacation_sent (
		account_id TEXT NOT NULL,
		key        TEXT NOT NULL,
		at         INTEGER NOT NULL,
		PRIMARY KEY (account_id, key)
	) WITHOUT ROWID`
	sqlVacationSentAt = `CREATE INDEX IF NOT EXISTS vacation_sent_at ON vacation_sent (at)`

	// The vacation response of an account (RFC 8621 8): at most one row.
	// from_at and to_at bound when it applies; NULL is no bound. A NULL
	// subject or body is the server's to fill in.
	sqlVacationTable = `CREATE TABLE IF NOT EXISTS vacation (
		account_id TEXT NOT NULL PRIMARY KEY,
		enabled    INTEGER NOT NULL DEFAULT 0,
		from_at    INTEGER,
		to_at      INTEGER,
		subject    TEXT,
		text_body  TEXT,
		html_body  TEXT,
		modseq     INTEGER NOT NULL
	) WITHOUT ROWID`

	// The counters of an account's quota move with its emails, in the
	// batch that adds or removes them (quota.go).
	sqlQuotaInsert = `CREATE TRIGGER IF NOT EXISTS email_quota_insert AFTER INSERT ON email BEGIN
		UPDATE account SET used_bytes = used_bytes + new.size, used_count = used_count + 1 WHERE id = new.account_id;
	END`
	sqlQuotaDelete = `CREATE TRIGGER IF NOT EXISTS email_quota_delete AFTER DELETE ON email BEGIN
		UPDATE account SET used_bytes = used_bytes - old.size, used_count = used_count - 1 WHERE id = old.account_id;
	END`

	// TLS reporting (tlsrpt.go). tls_result counts the sessions of
	// outbound deliveries per UTC day, recipient domain, policy and
	// outcome. tls_report_sent marks the reports that were made, so that
	// one node makes each. tls_report_in keeps reports sent to us.
	sqlTLSResultTable = `CREATE TABLE IF NOT EXISTS tls_result (
		day    INTEGER NOT NULL,
		domain TEXT NOT NULL,
		policy TEXT NOT NULL,
		result TEXT NOT NULL DEFAULT '',
		mx     TEXT NOT NULL DEFAULT '',
		ip     TEXT NOT NULL DEFAULT '',
		count  INTEGER NOT NULL,
		PRIMARY KEY (day, domain, policy, result, mx, ip)
	) WITHOUT ROWID`
	sqlTLSReportSentTable = `CREATE TABLE IF NOT EXISTS tls_report_sent (
		day     INTEGER NOT NULL,
		domain  TEXT NOT NULL,
		at      INTEGER NOT NULL,
		outcome TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (day, domain)
	) WITHOUT ROWID`
	sqlTLSReportInTable = `CREATE TABLE IF NOT EXISTS tls_report_in (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		received_at  INTEGER NOT NULL,
		organization TEXT NOT NULL DEFAULT '',
		report_id    TEXT NOT NULL,
		domain       TEXT NOT NULL DEFAULT '',
		start_at     INTEGER NOT NULL,
		end_at       INTEGER NOT NULL,
		successful   INTEGER NOT NULL DEFAULT 0,
		failed       INTEGER NOT NULL DEFAULT 0,
		report       TEXT NOT NULL
	)`
	sqlTLSReportInID = `CREATE UNIQUE INDEX IF NOT EXISTS tls_report_in_id ON tls_report_in (organization, report_id)`

	// A web login. token_hash is the SHA-256 of the token in the cookie, so
	// a copy of this table logs nobody in. id is what the user's list of
	// sessions shows and revokes by. stage is 'full', or 'totp' for a login
	// that has given its password and still owes the second step. A session
	// ends at expires_at whatever happens, and before that when nothing was
	// heard of it for idle seconds after seen_at.
	sqlSessionTable = `CREATE TABLE IF NOT EXISTS session (
		token_hash TEXT NOT NULL PRIMARY KEY,
		id         TEXT NOT NULL UNIQUE,
		account_id TEXT NOT NULL,
		stage      TEXT NOT NULL DEFAULT 'full',
		method     TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		seen_at    INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		idle       INTEGER NOT NULL,
		ip         TEXT NOT NULL DEFAULT '',
		seen_ip    TEXT NOT NULL DEFAULT '',
		agent      TEXT NOT NULL DEFAULT '',
		remember   INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`
	sqlSessionExpires = `CREATE INDEX IF NOT EXISTS session_expires ON session (expires_at)`
	sqlSessionAccount = `CREATE INDEX IF NOT EXISTS session_account ON session (account_id)`

	// A password for one device or program, for IMAP, SMTP submission and
	// JMAP with Basic auth. hash is the SHA-256 of the secret, which the
	// server made and showed once. scope lists the protocols it opens,
	// separated by spaces; empty is all of them. A revoked one is deleted.
	sqlAppPasswordTable = `CREATE TABLE IF NOT EXISTS app_password (
		account_id TEXT NOT NULL,
		id         TEXT NOT NULL,
		name       TEXT NOT NULL,
		hash       TEXT NOT NULL UNIQUE,
		scope      TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		used_at    INTEGER NOT NULL DEFAULT 0,
		used_ip    TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (account_id, id)
	) WITHOUT ROWID`

	// The TOTP secret of an account, sealed with the account's key.
	// confirmed_at is 0 until the user has shown a code from it; until then
	// the login does not ask for one. last_step is the newest time step a
	// code was accepted for: a code works once, on whatever node.
	sqlTOTPTable = `CREATE TABLE IF NOT EXISTS totp (
		account_id   TEXT NOT NULL PRIMARY KEY,
		secret       TEXT NOT NULL,
		created_at   INTEGER NOT NULL,
		confirmed_at INTEGER NOT NULL DEFAULT 0,
		last_step    INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`

	// Recovery codes, as SHA-256. Using one deletes its row.
	sqlRecoveryCodeTable = `CREATE TABLE IF NOT EXISTS recovery_code (
		account_id TEXT NOT NULL,
		hash       TEXT NOT NULL,
		PRIMARY KEY (account_id, hash)
	) WITHOUT ROWID`

	// The subject ("sub") under which an OIDC provider knew an account at
	// its first login through that provider. A later login that maps to
	// the account with another subject is refused: the address was given
	// to someone else at the provider.
	sqlOIDCLinkTable = `CREATE TABLE IF NOT EXISTS oidc_link (
		account_id TEXT NOT NULL,
		provider   TEXT NOT NULL,
		subject    TEXT NOT NULL,
		linked_at  INTEGER NOT NULL,
		PRIMARY KEY (account_id, provider)
	) WITHOUT ROWID`

	// What happened at the doors: logins that worked and that failed,
	// second steps, changes to passwords, app passwords and sessions, and
	// what an administrator did. account_id is empty when the address
	// belongs to nobody. Trimmed after a retention.
	sqlAuthEventTable = `CREATE TABLE IF NOT EXISTS auth_event (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		at         INTEGER NOT NULL,
		account_id TEXT NOT NULL DEFAULT '',
		address    TEXT NOT NULL DEFAULT '',
		event      TEXT NOT NULL,
		ip         TEXT NOT NULL DEFAULT '',
		detail     TEXT NOT NULL DEFAULT ''
	)`
	sqlAuthEventAt      = `CREATE INDEX IF NOT EXISTS auth_event_at ON auth_event (at)`
	sqlAuthEventAccount = `CREATE INDEX IF NOT EXISTS auth_event_account ON auth_event (account_id, id)`

	// The robinmail nodes of the cluster. A node writes its own row when it
	// starts and renews seen_at while it runs. address is where the other
	// nodes reach it; cert is the SHA-256 of the certificate it shows them
	// and expects to be recognised by (internal/cluster).
	sqlNodeTable = `CREATE TABLE IF NOT EXISTS node (
		id         TEXT NOT NULL PRIMARY KEY,
		address    TEXT NOT NULL,
		cert       TEXT NOT NULL,
		started_at INTEGER NOT NULL DEFAULT 0,
		seen_at    INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`

	// The outbound queue: one row per submitted message. recipients is a
	// JSON list with the state of every recipient. email_id is 0 for a
	// message that came in over SMTP submission: it is no Email of the
	// account and JMAP does not show the row. blob is the message as it
	// goes out (signed, without Bcc), a blob of the account, and is emptied
	// when the row is done. undo is the JMAP undoStatus; queue is 'active'
	// while there is work and 'done' after. A node works on a row while its
	// claim lasts (claimed_by, claimed_until). submit_key makes the insert
	// safe to repeat. destroyed marks a row the JMAP client destroyed while
	// it still had work.
	sqlSubmissionTable = `CREATE TABLE IF NOT EXISTS submission (
		account_id    TEXT NOT NULL,
		id            INTEGER NOT NULL,
		email_id      INTEGER NOT NULL DEFAULT 0,
		thread_id     INTEGER NOT NULL DEFAULT 0,
		identity_id   INTEGER NOT NULL DEFAULT 0,
		message_id    TEXT NOT NULL DEFAULT '',
		blob          TEXT NOT NULL DEFAULT '',
		size          INTEGER NOT NULL DEFAULT 0,
		mail_from     TEXT NOT NULL DEFAULT '',
		recipients    TEXT NOT NULL,
		rcpt_count    INTEGER NOT NULL,
		undo          TEXT NOT NULL DEFAULT 'pending',
		queue         TEXT NOT NULL DEFAULT 'active',
		created_at    INTEGER NOT NULL,
		send_at       INTEGER NOT NULL,
		next_try      INTEGER NOT NULL DEFAULT 0,
		tries         INTEGER NOT NULL DEFAULT 0,
		claimed_by    TEXT NOT NULL DEFAULT '',
		claimed_until INTEGER NOT NULL DEFAULT 0,
		destroyed     INTEGER NOT NULL DEFAULT 0,
		submit_key    TEXT NOT NULL,
		modseq        INTEGER NOT NULL,
		PRIMARY KEY (account_id, id),
		UNIQUE (account_id, submit_key)
	) WITHOUT ROWID`
	sqlSubmissionDue     = `CREATE INDEX IF NOT EXISTS submission_due ON submission (queue, next_try)`
	sqlSubmissionMessage = `CREATE INDEX IF NOT EXISTS submission_message ON submission (account_id, message_id)`

	// The identities a user sends as (RFC 8621 6). Identity 1 is made with
	// the account. reply_to and bcc are JSON lists of addresses, or empty.
	sqlIdentityTable = `CREATE TABLE IF NOT EXISTS identity (
		account_id     TEXT NOT NULL,
		id             INTEGER NOT NULL,
		name           TEXT NOT NULL DEFAULT '',
		email          TEXT NOT NULL,
		reply_to       TEXT NOT NULL DEFAULT '',
		bcc            TEXT NOT NULL DEFAULT '',
		text_signature TEXT NOT NULL DEFAULT '',
		html_signature TEXT NOT NULL DEFAULT '',
		modseq         INTEGER NOT NULL,
		PRIMARY KEY (account_id, id)
	) WITHOUT ROWID`

	// Blobs a client uploaded (RFC 8620 6.1) and may refer to for a while.
	sqlUploadTable = `CREATE TABLE IF NOT EXISTS upload (
		account_id TEXT NOT NULL,
		blob       TEXT NOT NULL,
		size       INTEGER NOT NULL,
		type       TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		PRIMARY KEY (account_id, blob)
	) WITHOUT ROWID`

	// MTA-STS policies of other domains (RFC 8461), cached for all nodes.
	// mx holds the patterns, one per line.
	sqlSTSTable = `CREATE TABLE IF NOT EXISTS mta_sts (
		domain     TEXT NOT NULL PRIMARY KEY,
		policy_id  TEXT NOT NULL,
		mode       TEXT NOT NULL,
		mx         TEXT NOT NULL,
		fetched_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	) WITHOUT ROWID`

	// Failed logins per key ("a:" + address, "i:" + IP) in a window that
	// starts with the first failure, shared by the nodes.
	sqlLoginFailureTable = `CREATE TABLE IF NOT EXISTS login_failure (
		key     TEXT NOT NULL PRIMARY KEY,
		started INTEGER NOT NULL,
		count   INTEGER NOT NULL
	) WITHOUT ROWID`
)

// Init creates the tables in an empty database, migrates an older one and
// checks the schema version. Call it once at start. It is safe to run on
// several nodes at the same time.
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
		if _, err := s.db.ExecuteIdempotent(ctx, stmts...); err != nil {
			return fmt.Errorf("store: creating schema: %w", err)
		}
		if version, err = s.version(ctx); err != nil {
			return err
		}
	}
	for version < schemaVersion {
		steps, ok := migrations[version]
		if !ok {
			break
		}
		// The guard makes the batch fail if another node migrated first.
		stmts := []rqlite.Statement{rqlite.Stmt(
			`INSERT INTO guard (ok) SELECT 1 WHERE NOT ((SELECT max(version) FROM schema_version) = ?)`, version)}
		for _, sql := range steps {
			stmts = append(stmts, rqlite.Stmt(sql))
		}
		stmts = append(stmts, rqlite.Stmt(`UPDATE schema_version SET version = ?`, version+1))
		_, merr := s.db.ExecuteIdempotent(ctx, stmts...)
		now, err := s.version(ctx)
		if err != nil {
			return err
		}
		if merr != nil && now == version {
			return fmt.Errorf("store: migrating schema from version %d: %w", version, merr)
		}
		version = now
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
