# robinmail: plan

robinmail is a JMAP-first mail server in Go. It receives mail, stores it, sends
it, and serves it over JMAP (RFC 8620, RFC 8621). The webmail is a Void app in
`web/` that talks JMAP. Spam filtering and SPF/DKIM/DMARC are delegated to
Rspamd. All state lives in rqlite and S3-compatible storage, so robinmail nodes
are stateless and can run in several availability zones.

It replaces owl (`../owl`), whose per-folder Maildir store does not fit JMAP.
This is a learning project. There is no production target yet.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Language | Go, one binary `cmd/robinmail` | As in owl. |
| Dependencies | Standard library plus `golang.org/x` only | owl's rule, kept. rqlite and S3 are reached over HTTP with small clients of our own. |
| Mail API | JMAP core and mail | One standard API for the webmail, a later app and existing clients. |
| Metadata | One rqlite cluster (SQLite replicated with Raft), one database for all accounts | Self-hosted, replicated, automatic leader failover. No replication, lease or consensus code of our own. |
| Message files | Immutable blobs in S3-compatible storage, keyed by account and SHA-256 | Garage or MinIO self-hosted across zones, R2 in the cloud, a local-disk backend for development. Own SigV4 client. |
| Nodes | Stateless. Any node serves any account | No owner per account and no forwarding. rqlite's leader orders the writes. |
| Mailboxes | An Email can be in several mailboxes (labels) | Full JMAP `mailboxIds`. |
| IMAP | Last phase, but UIDs exist from day one | No store migration later. |
| Webmail | Void app in `web/`, self-hosted on workerd, plain Tailwind | Public repo, so no Tailwind Plus components. |
| Origin | One hostname. robinmail serves HTTPS and proxies everything outside `/jmap`, `/.well-known/jmap` and `/auth` to workerd | Same origin: no CORS, `SameSite=Strict` works. |
| Web auth | Session cookie (`Secure`, `HttpOnly`, `SameSite=Strict`) plus an `Origin` check on every JMAP request. Sessions are rows in rqlite | The client never holds a token. Any node accepts the cookie. |
| Spam and mail auth | Rspamd over HTTP | As in owl, including its lessons (below). |
| Config | JSON | As in owl. |
| Passwords | Argon2id, PHC string | As in owl. |

Not in scope: POP3, Sieve, CalDAV/CardDAV, quotas, full-text search (first
version filters on indexed columns only), migration tooling.

Accepted trade-off: every write goes through the one rqlite leader, also from
another zone. Fine at this scale. If it ever is not, accounts can be spread
over several rqlite clusters, because every table is keyed by account.

## Store

### Where data lives

```
rqlite      all tables below
S3 bucket   <account-id>/<sha256>     raw message or uploaded blob, immutable
S3 bucket   acme/                     ACME account and certificates, shared by nodes
<data_dir>/blobs/                     blob backend "local", development only
```

A node keeps nothing on its own disk except its config.

### Tables

Every mail table starts with `account_id`.

| Table | Columns | Notes |
|---|---|---|
| `domain` | name | domains we accept mail for |
| `account` | id, address, password_hash, modseq, next_email, next_mailbox, next_thread | id is random and never changes; counters live here |
| `session` | token_hash, account_id, expires_at | web login |
| `mailbox` | id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq | a rename changes `name` only |
| `email` | id, blob, size, received_at, thread_id, message_id, sender, subject, preview, has_attachment, modseq | header fields cached for lists and sorting |
| `email_mailbox` | email_id, mailbox_id, uid | one row per label; the IMAP UID belongs to the row |
| `keyword` | email_id, keyword | `$seen`, `$flagged`, `$draft`, `$answered`, custom |
| `thread_ref` | message_id, thread_id | every Message-ID seen in `Message-ID`, `In-Reply-To`, `References` |
| `changes` | modseq, type, id, op | type Email, Mailbox or Thread; op created, updated or destroyed |
| `delivered` | key | queue id plus recipient, for idempotent delivery; trimmed after a week |
| `submission` | id, email_id, envelope, state, next_try, claimed_by, claimed_until | outbound queue (phase 5) |

Rules:

- **Ids.** Email, Mailbox and Thread ids are per-account counters, encoded as
  short strings. An Email id is independent of mailbox and blob, so moves and
  label changes keep it.
- **modseq.** Every write transaction increments the account's `modseq` once,
  stamps the rows it touched and appends to `changes`. The JMAP state string is
  the `modseq`. `/changes` reads `changes` after the given state. Old rows are
  trimmed after 30 days; an older state gets `cannotCalculateChanges`.
- **Transactions.** rqlite runs a batch of statements atomically but has no
  interactive transactions: no read, decide in Go, then write. So each store
  operation is one batch whose statements compute what they need in SQL
  (take the counter, look up the thread, take `uidnext`). JMAP `ifInState`
  becomes `WHERE modseq = ?`.
- **Threads.** Assigned in the delivery batch: the first `thread_ref` hit among
  the message's ids wins, otherwise a new thread. Never changed afterwards. Two
  threads that turn out to be related are not merged.
- **UIDs.** Adding an Email to a mailbox takes that mailbox's `uidnext`. A move
  is remove plus add, so the message gets a new UID in the target, as IMAP
  expects. In IMAP an Email with three labels shows up in three folders.
  `modseq` doubles as `CONDSTORE` later.
- **Blobs.** Written and confirmed in S3 before the batch that references
  them. A crash leaves an unreferenced blob, never a row without a blob. A
  sweeper removes unreferenced blobs older than a day.
- **Delivery is idempotent** through `delivered`, so a sender's retry after a
  partial failure makes no second copy (owl did).
- **Reads** use rqlite's `weak` consistency. Change polling for push may use
  `none` with a freshness bound.

## Layout

```
cmd/robinmail        the binary: server and offline commands
internal/address     address parsing                    port as is
internal/tlsconf     TLS config, cert reload            port as is
internal/config      JSON config                        port, new fields
internal/smtp        ESMTP server, later client         port as is
internal/rspamd      Rspamd client                      port as is
internal/users       Argon2id hash and verify           port the hashing; storage moves to rqlite
internal/delivery    recipient to mailbox               port; talks to a store interface, mailbox by role
internal/rqlite      rqlite HTTP client                 new
internal/blob        blob backends: S3, local           new
internal/store       schema and all mail operations     new, replaces owl's store
internal/message     MIME to JMAP body structure        new
internal/jmap        JMAP core and mail, protocol only  new
internal/web         HTTPS, login, sessions, proxy      new
internal/queue       outbound delivery                  new
internal/imap        IMAP server                        new
web/                 Void webmail
contrib/rspamd       Rspamd config                      port as is
contrib/compose      rqlite, Garage, Rspamd for dev     new
```

owl's `internal/fsutil` is only needed by the local blob backend. Ported code
keeps its tests. Protocol packages know nothing about storage and call
interfaces that `cmd/robinmail` wires up. Store tests run against a real
`rqlited` and are skipped when none is available.

Carried over from owl: the rules for all code (hostile input, size limits and
deadlines, fuzz tests for parsers, rate-limited logins, no plaintext auth) and
the Rspamd lessons: defer on greylist only when the `GREYLIST` symbol fired,
send the FCrDNS hostname, score headers need
`contrib/rspamd/local.d/milter_headers.conf`.

## Phases

Estimates assume focused work. About 15 weeks in total.

### Phase 0: spikes — 2 days

- rqlite: write the delivery batch (counters, thread lookup, UID, change log)
  as one atomic request and confirm it holds under concurrent deliveries.
- A Void app self-hosted on workerd behind a Go reverse proxy.
- Exit: two short notes in this file, and the decisions table corrected.

### Phase 1: store and ported packages — 2 weeks

- Port the packages marked above.
- `internal/rqlite`, `internal/blob` (local and S3), `internal/store` with the
  full schema.
- Commands: `user add|list`, `mailbox list`, `deliver`, `list`, `cat`.
- Exit: deliver from the command line, move and label a message, and see the
  ids hold and `changes` fill; tests cover threads, UIDs and trimming.

### Phase 2: inbound SMTP, Rspamd, delivery — 4 days

- `serve` with the ported SMTP listener, Rspamd scan, delivery to Inbox or Junk.
- Exit: owl's end-to-end test passes here: `swaks` mail lands with score
  headers, GTUBE gets `550`.

### Phase 3: JMAP read side and webmail — 5 weeks

- `internal/jmap`, 2 weeks: session, request handling with result references
  and limits, `Mailbox/get|changes|query`, `Thread/get`,
  `Email/get|query|changes`, `Email/set` for keywords, `mailboxIds` and
  destroy, `Mailbox/set`, blob download.
- `internal/message`, 1 week: MIME to body structure, charsets through
  `x/text`, sanitised HTML view for a sandboxed iframe (allowlist on
  `x/net/html`, CSP, remote images off by default).
- `internal/web`: login, sessions, `Origin` check, proxy to workerd.
- `web/`, 2 weeks: login, mailbox list, message list, thread view, labels,
  attachment download, mark read, flag, move, delete.
- Exit: browse and manage mail in the browser; the same session answers raw
  JMAP from `curl`.

### Phase 4: cluster — 1 week

- Three rqlite nodes, three robinmail nodes and Garage in Docker Compose.
- robinmail follows rqlite leader changes and retries safely.
- Exit: kill the rqlite leader and a robinmail node during a stream of
  deliveries: no acknowledged message is lost, none is doubled, and the webmail
  keeps working on another node.

### Phase 5: outbound, submission, compose — 3 weeks

- Submission on 465 and 587, outbound through Rspamd for DKIM, queue in
  `submission` (a node claims a row for a limited time), MX delivery with
  STARTTLS, MTA-STS, retries, bounces.
- JMAP `Email/set` create, blob upload, `EmailSubmission/set`, `Identity/get`.
- Webmail: compose, reply, forward, attachments, drafts.
- Exit: send between two robinmail clusters; an unreachable host is retried
  and bounces; a killed node does not lose queued mail.

### Phase 6: push and IMAP — 3 weeks

- JMAP push over EventSource. A node polls `modseq` for the accounts that have
  a connection open on it.
- IMAP4rev1 on 993 with `IDLE`, `UIDPLUS`, `MOVE`, `SPECIAL-USE`, `CONDSTORE`.
- Exit: Thunderbird and iOS Mail sync, move, flag and see new mail arrive.

## Open questions

- Both spikes in phase 0.
- Which S3 store to run in the cluster test: Garage is assumed.
- Where phase 5 gets tested against real providers: which host and domain.
