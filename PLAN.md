# robinmail: plan

robinmail is a JMAP-first mail server in Go. It receives mail, stores it, sends
it, and serves it over JMAP (RFC 8620, RFC 8621). The webmail is a Vue app in
`web/` that talks JMAP and ships inside the Go binary. Spam filtering and SPF/DKIM/DMARC are delegated to
Rspamd. Metadata lives in rqlite. Message files live on each node's own disk,
encrypted, and robinmail copies them between nodes itself, so a cluster can
run in several availability zones without any other storage service.

It replaces owl (`../owl`), whose per-folder Maildir store does not fit JMAP.
This is a learning project. There is no production target yet.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Language | Go, one binary `cmd/robinmail` | As in owl. |
| Dependencies | Standard library plus `golang.org/x` only | owl's rule, kept. rqlite is reached over HTTP with a small client of our own. |
| Mail API | JMAP core and mail | One standard API for the webmail, a later app and existing clients. |
| Metadata | One rqlite cluster (SQLite replicated with Raft), one database for all accounts | Self-hosted, replicated, automatic leader failover. No replication, lease or consensus code of our own. |
| Message files | Immutable blobs on the local disk of every node, named by account and SHA-256 of the plaintext | No storage service to run. robinmail replicates blobs itself (phase 4): every node holds every blob, a write is acknowledged once 2 nodes have it, and a background check fetches missing ones. |
| Encryption at rest | Every blob is encrypted with a key of its account (XChaCha20-Poly1305 from `golang.org/x/crypto`, in chunks). The account key is in the `account` row, wrapped by a master key from a key file every node has | Protects against stolen disks and backups. Does not protect against a compromised running server, which holds the master key. |
| Nodes | Not stateless: metadata is in rqlite, blobs are on each node's disk. Any node serves any account | No owner per account and no forwarding. rqlite's leader orders the writes. |
| Between nodes | Nodes find each other through the `node` table in rqlite and talk over mutual TLS: each has a self-signed certificate, and both sides accept exactly the certificates whose fingerprint is in that table | No CA and no shared secret to distribute. Who may join is decided by who can write to rqlite, which holds everything anyway. |
| Mailboxes | An Email can be in several mailboxes (labels) | Full JMAP `mailboxIds`. |
| IMAP | Last phase, but UIDs exist from day one | No store migration later. |
| Webmail | Vue single-page app in `web/`: plain Vite, Vue Router, Nuxt UI (without Nuxt). Built to static files and embedded with `go:embed` | The client only talks JMAP, so nothing needs to render on a server. No workerd container and no proxy; a node stays one binary. Nuxt UI is MIT and has the dashboard and inbox components a mail client needs. |
| Origin | One hostname. robinmail serves the API under `/jmap`, `/.well-known/jmap` and `/auth`, and the embedded webmail everywhere else | Same origin: no CORS, `SameSite=Strict` works. |
| Web auth | Session cookie (`Secure`, `HttpOnly`, `SameSite=Strict`) plus an `Origin` check on every JMAP request. Sessions are rows in rqlite | The client never holds a token. Any node accepts the cookie. |
| Spam and mail auth | Rspamd over HTTP | As in owl, including its lessons (below). |
| Config | JSON | As in owl. |
| Passwords | Argon2id, PHC string | As in owl. |
| Accounts and login | Built in. App passwords for IMAP and SMTP, TOTP for web login, optional OIDC login for the webmail (phase 7). No Keycloak or other identity service required | Mail clients log in with a password and do not speak OIDC, so robinmail needs its own accounts anyway. OIDC stays generic, so Keycloak, Authentik or Zitadel can be plugged in. |

Not in scope: POP3, CalDAV/CardDAV, migration tooling. Sieve, quotas and
full-text search wait for phase 8; until then queries filter on indexed
columns only.

Accepted trade-off: every write goes through the one rqlite leader, also from
another zone. Fine at this scale. If it ever is not, accounts can be spread
over several rqlite clusters, because every table is keyed by account.

## Store

### Where data lives

```
rqlite                                            all tables below
<data_dir>/blobs/<account-id>/<2 hex>/<sha256>    raw message or uploaded blob, encrypted, immutable
master key file                                   wraps the account keys; the same on every node
```

The name of a blob is the SHA-256 of its plaintext, which is also the JMAP
blobId. A node keeps its config, the master key file and its blobs on disk,
nothing else. ACME account and certificates go into an rqlite table, shared by
the nodes.

### Tables

Every mail table starts with `account_id`.

| Table | Columns | Notes |
|---|---|---|
| `domain` | name | domains we accept mail for; managed with `domain add\|list\|remove`, read by every node |
| `account` | id, address, password_hash, blob_key, created_at, modseq, trimmed_modseq, next_email, next_mailbox, next_thread, next_submission, next_identity | id is random and never changes; counters live here; `blob_key` is the account's blob key wrapped by the master key; `trimmed_modseq` is the newest state the change log no longer covers |
| `session` | token_hash, account_id, expires_at | web login; the hash is SHA-256 of the token in the cookie |
| `mailbox` | id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq, subscribed | a rename changes `name` only |
| `email` | id, blob, size, received_at, thread_id, message_id, sender, recipients, cc, bcc, subject, sent_at, preview, has_attachment, modseq | header fields cached for lists, filters and sorting |
| `email_mailbox` | email_id, mailbox_id, uid | one row per label; the IMAP UID belongs to the row |
| `keyword` | email_id, keyword | `$seen`, `$flagged`, `$draft`, `$answered`, custom |
| `thread_ref` | message_id, thread_id | every Message-ID seen in `Message-ID`, `In-Reply-To`, `References` |
| `changes` | modseq, type, id, op, at | type Email, Mailbox, Thread, EmailSubmission or Identity; op created, updated or destroyed; `at` is for trimming |
| `delivered` | key, at | delivery key, for idempotent delivery; trimmed after a week |
| `submission` | id, email_id, thread_id, identity_id, message_id, blob, size, mail_from, recipients, rcpt_count, undo, queue, created_at, send_at, next_try, tries, claimed_by, claimed_until, destroyed, submit_key, modseq | outbound queue: one row per message; `recipients` is JSON with the state of each; `blob` is the message as it goes out; a node works on a row while its claim lasts |
| `identity` | id, name, email, reply_to, bcc, text_signature, html_signature, modseq | JMAP identities; id 1 is made with the account |
| `upload` | blob, size, type, created_at | blobs a client uploaded; trimmed after a day |
| `mta_sts` | domain, policy_id, mode, mx, fetched_at, expires_at | MTA-STS policies of other domains, cached for all nodes; not per account |
| `acme` | key, value | ACME account and certificates, shared by nodes (not built yet) |
| `node` | id, address, cert, started_at, seen_at | the robinmail nodes: where the others reach each, the SHA-256 of its certificate, its heartbeat |
| `login_failure` | key, started, count | failed logins per address and per IP in a 15 minute window, for all nodes together |
| `guard` | ok | always empty; how a batch checks a precondition (below) |
| `schema_version` | version | checked at start; an older database is migrated in one guarded batch per version (so far 2 to 3, 3 to 4 and 4 to 5) |

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
  (take the counter, look up the thread, take `uidnext`). A precondition
  ("this mailbox exists", later JMAP `ifInState`) is a guard statement:
  `INSERT INTO guard SELECT 1 WHERE NOT (condition)`. The table only accepts
  0, so an unmet condition breaks its `CHECK` and rolls the batch back. rqlite
  reports which statement failed, and the store maps that position to an
  error such as `ErrNoMailbox`.
- **Threads.** Assigned in the delivery batch: the first `thread_ref` hit among
  the message's ids wins, otherwise a new thread. Never changed afterwards. Two
  threads that turn out to be related are not merged.
- **UIDs.** Adding an Email to a mailbox takes that mailbox's `uidnext`. A move
  is remove plus add, so the message gets a new UID in the target, as IMAP
  expects. In IMAP an Email with three labels shows up in three folders.
  `modseq` doubles as `CONDSTORE` later.
- **Blobs.** Written to a temp file, synced and renamed into place before the
  batch that references them. A crash leaves an unreferenced blob, never a
  row without a blob. A sweeper on every node removes that node's
  unreferenced blob files older than a day. It claims a file by renaming it
  aside, then looks at its age again and asks rqlite once more, so a
  delivery of the same content at that moment keeps its blob. In a cluster
  each node sweeps only its own disk. A delivery on another node rewrites
  the file, and so renews its time, on every node its push reaches, and is
  only acknowledged when 2 nodes hold such a fresh copy; a node the push
  missed may sweep its old copy and gets the blob back like any blob it
  never had.
- **Blob file format.** A 27-byte header (magic `RMB1`, chunk size, random
  nonce prefix), then the plaintext in 64 KiB chunks, each sealed on its own.
  The chunk's position and whether it is the last are part of its nonce;
  header, account id and SHA-256 are the associated data. So a changed,
  moved, missing or extra chunk fails, as does a file under another name, and
  a reader holds one chunk in memory. The file does not depend on the node
  that wrote it: replication copies it as it is, and a node can check a copy
  it received by decrypting it and comparing the hash with the name.
- **Delivery is idempotent** through `delivered`, so a sender's retry after a
  partial failure makes no second copy (owl did). The key is the SHA-256 of
  the envelope sender and the message as received. It cannot be our queue id:
  a retry gets a new one.
- **Reads** use rqlite's `weak` consistency: no Raft round trip, and a
  leader that was just cut off can answer from slightly old data for about
  a second. The sweeper's "is this blob still referenced" decides a
  deletion and uses `strong`. The login failure counters use `none` with a
  freshness bound of 5 seconds, so a login never waits for the leader.
  Change polling for push may use `none` too.
- **Writes and retries.** When a leader dies mid-write rqlite cannot say
  whether the batch was applied. A batch is sent again only if a second run
  is harmless: a delivery (its `delivered` row refuses the second run as a
  whole), deletes and trims, rows set to given values. Everything that
  takes a counter or carries a guard fails with "outcome unknown" instead
  and the caller decides. The list is at `run` in `internal/store`.
  Submitting a message is made repeatable by a random key in the row: a
  second run fails on it as a whole, and the row is read back.

## Layout

```
cmd/robinmail        the binary: server and offline commands
internal/address     address parsing                    port as is
internal/tlsconf     TLS config, cert reload            port as is
internal/config      JSON config                        port, new fields
internal/smtp        ESMTP server and client, AUTH      port as is; client and AUTH new
internal/rspamd      Rspamd client                      port as is
internal/users       Argon2id hash and verify           port the hashing; storage moves to rqlite
internal/delivery    recipient to mailbox; submission   port; talks to a store interface, mailbox by role
internal/rqlite      rqlite HTTP client for a cluster   new
internal/blob        encrypted blob files on disk, replicated between nodes   new
internal/cluster     node table, mutual TLS, blob transfer between nodes      new
internal/clustertest tests against the Docker cluster   new; "make cluster-test"
internal/store       schema and all mail operations     new, replaces owl's store
internal/message     MIME to JMAP body structure, HTML view   new
internal/jmap        JMAP core and mail, protocol only  new
internal/web         HTTPS, login, sessions, static app new
internal/queue       outbound delivery: MX, MTA-STS, retries, bounces   new
internal/imap        IMAP server                        new
web/                 Vue webmail (Vite, Nuxt UI); src/jmap is the client, e2e the browser test
Makefile             web build, binary with the webmail, tests, dev
Dockerfile           the binary with the webmail in an image
contrib/rspamd       Rspamd config, DKIM signing        port as is; dkim_signing.conf new
contrib/compose      rqlite, Rspamd for dev             new; with dev-only Rspamd overrides
                     cluster.yaml, cluster.sh: three rqlite and three robinmail nodes
contrib/dev          seed and demo scripts, sample mail new; a local instance in ./dev
```

owl's `internal/fsutil` is not ported: the blob store only needed its
atomic write, which is now a few lines in `internal/blob`. Ported code
keeps its tests. Protocol packages know nothing about storage and call
interfaces that `cmd/robinmail` wires up. Store tests run against a real
`rqlited` and are skipped when none is available.

Carried over from owl: the rules for all code (hostile input, size limits and
deadlines, fuzz tests for parsers, rate-limited logins, no plaintext auth) and
the Rspamd lessons: defer on greylist only when the `GREYLIST` symbol fired,
send the FCrDNS hostname, score headers need
`contrib/rspamd/local.d/milter_headers.conf`. Rspamd adds no score headers
to mail from local addresses (`skip_local`), which is all mail in
development; `contrib/compose` turns that off for its own Rspamd only.

Every protocol package also gets tests written from the RFC, not from our
own behaviour. Each names the RFC and section it checks and uses the RFC's
examples or test vectors where it has them. A known deviation keeps a test
that states it and why.

## Phases

Each phase is built by its own session and ends in something shown working.
The week estimates from owl's plan are gone: phases 0 and 1 took well under
an hour of session time each. What takes real time is review and testing
against real providers.

### Phase 0: spike — done

Question: can a delivery be one atomic rqlite request? Yes.
`spike/rqlite/main.go` shows how; phase 1 turns it into `internal/store` and
deletes it.

- One `POST /db/request?transaction` holds the whole delivery: insert the
  `delivered` key, bump the account counters, insert the email with its thread
  chosen by a subquery on `thread_ref`, take `uidnext`, write `changes`, and a
  final `SELECT` that returns the new ids.
- A statement error rolls the whole batch back. A repeated delivery key fails
  on the first statement with `UNIQUE constraint failed`, which is how a retry
  is recognised.
- Tested on rqlite 10.7.1, one node in Docker: 600 replies from 32 workers,
  each sent twice. 600 stored, 600 refused, ids, UIDs and modseq without gaps
  or doubles, every reply in its parent's thread.
- Not measured: speed on a three-node cluster. The 2500 requests a second seen
  here is one node on a laptop. Phase 4 measures it.
- Unused counter values: every delivery takes a thread id, also when it joins
  an existing thread. Thread ids have gaps; Email ids and UIDs do not.

### Phase 1: store and ported packages — done

- Port the packages marked above.
- `internal/rqlite`, `internal/blob` (encrypted files on local disk),
  `internal/store` with the full schema.
- Commands: `user add|list`, `mailbox list`, `deliver`, `list`, `cat`.
- Exit: deliver from the command line, move and label a message, and see the
  ids hold and `changes` fill; tests cover threads, UIDs and trimming.

Built as follows, where it differs from the above:

- More commands than planned, because the exit needs them: `mailbox
  create|rename`, `move`, `label`, `unlabel`, `flag`, `rm`, `changes`, `trim`.
  `cmd/robinmail/main_test.go` runs the exit against a real rqlite.
- Ids on the outside are a type letter plus the counter in base 36: `e1`,
  `m6`, `t1`.
- One store call, `UpdateEmail`, adds and removes mailboxes and keywords in
  one batch. A move is that call with one removal and one addition. An email
  cannot end up in no mailbox.
- Destroying the last email of a thread also drops its `thread_ref` rows, so
  a late reply starts a new thread instead of reviving a destroyed id.
- A keyword change marks the email's mailboxes as updated too: their unread
  count changed.
- `preview` and `has_attachment` are rough (text/plain only; multipart/mixed)
  until `internal/message` in phase 3.
- An S3 backend with its own SigV4 client was built first and then removed
  when blobs moved to local disk (it is in the git history).
- `key generate` writes the master key file (mode 0600, never overwritten).
  Every other command refuses to run without it, or if others can read it.
  `verify` decrypts blob files and checks them against their hash.
- Account keys are cached in memory once unwrapped; they never change.
- There is no master key rotation yet: that means rewrapping every
  `account.blob_key`.
- `internal/users` only hashes and verifies now. It refuses stored hashes
  with absurd Argon2 parameters.
- Tests: `contrib/compose` starts rqlite. Set `ROBINMAIL_TEST_RQLITE` (see
  `compose.yaml`); without it the tests that need rqlite are skipped.
- Not built: the blob sweeper (see open questions), mailbox destroy, and
  leader following and blob replication (phase 4). `serve` is phase 2.

### Phase 2: inbound SMTP, Rspamd, delivery — done

- `serve` with the ported SMTP listener, Rspamd scan, delivery to Inbox or Junk.
- Exit: owl's end-to-end test passes here: `swaks` mail lands with score
  headers, GTUBE gets `550`.

Built as follows, where it differs from or adds to the above:

- Accepted domains come from the `domain` table: `domain add|list|remove`.
  `domains` is gone from the config. `user add` needs the domain to exist
  and `domain remove` refuses one that still has users; both checks are
  guards inside the batch. Delivery read the table at most once per 30
  seconds; since phase 4 it reads it for every RCPT.
- `<Postmaster>` without a domain goes to the first domain, by name, that
  has a `postmaster` account. owl used the first domain in the config.
- Shutdown: on SIGINT or SIGTERM the listener closes, a session waiting for
  a command gets `421` at once, a session in the middle of a command
  finishes and answers it first, and after 30 seconds what is left is
  closed. A second signal ends the process.
- Trimming: every node runs `TrimChanges` and `TrimDelivered` daily, the
  first time at a random moment within ten minutes of start. Nobody picks a
  node: each trim is one atomic batch, so concurrent runs are safe and the
  later one finds nothing (`TestConcurrentTrim`).
- The exit test is `TestServeEndToEnd` in `cmd/robinmail`, with a small
  SMTP client in Go instead of `swaks`. It needs `ROBINMAIL_TEST_RQLITE`
  and `ROBINMAIL_TEST_RSPAMD` and is skipped without them or when Rspamd
  does not answer. Run against rqlite 10.7.1 and Rspamd 4.3.0: a normal
  message lands in the Inbox with `X-Spamd-Result` and `X-Rspamd-Action`,
  GTUBE gets `550 5.7.1 Gtube pattern`, the "add header" test pattern lands
  in Junk with `X-Spam: Yes`. `TestServeRspamdDown` covers `451` and
  `fail_open`.
- `contrib/compose` runs Rspamd with two development-only overrides in
  `contrib/compose/rspamd`: `skip_local = false`, because the test sends
  from 127.0.0.1, and `enable_test_patterns`, so the GTUBE variant that
  starts with `YJS` yields "add header".
- RFC tests (the rule above) were written for phases 1 and 2 together.
  They found and fixed:
  - `MAIL` and `RCPT` took almost anything between angle brackets. The
    parser now follows RFC 5321 4.1.2 and 4.1.3: dot-string or quoted
    local part, LDH domain labels (no underscore, no trailing dot),
    address literals with `IPv6:`, source routes checked and ignored, the
    64, 255 and 256 octet limits. Anything else is `501`.
  - Parameters: `KEY=` without a value, `SIZE=5abc`, `SIZE=+5`, a repeated
    `SIZE` or `BODY`, and `SMTPUTF8=x` were accepted.
  - A non-ASCII sender without `SMTPUTF8` got `553`; RFC 6531 3.5 says
    `550` for `MAIL` and `553` for `RCPT`, and `RCPT` did not check at all.
  - `RSET x` and `QUIT x` were accepted (`501` now), `VRFY` without an
    argument too, `EXPN` got `500` and `AUTH` `503` (both `502` now).
  - Reply text from a handler, such as Rspamd's reason, went out as it
    was: non-ASCII, and longer than the 512 octets of a reply line.
  - Pipelining: a reply was held back while the first part of the next
    command sat in the input buffer, and replies to `NOOP`, `VRFY` and
    unknown commands were buffered (RFC 2920 3.2).
  - A command timeout closed the connection without the `421`.
  - `Received` wrote an IPv6 peer as `[2001:db8::1]` instead of
    `[IPv6:2001:db8::1]`, and copied any EHLO argument into the line.
  - `Return-Path` for a non-ASCII reverse-path was MIME-encoded, and a
    `Return-Path` that came with the message was kept next to ours.
  - `internal/address` accepted addresses longer than a 256-octet path.
  - Argon2id used p=2; new hashes use RFC 9106's second recommended
    option (t=3, p=4, 64 MiB). The PHC parser accepted `v=19junk`, signs,
    leading zeros and line breaks inside the base64.
  - `References` and `In-Reply-To`: an id inside a comment or a quoted
    phrase was taken as a reference.
  - The config allowed limits below RFC 5321's minimums (64K octets per
    message, 100 recipients).
- Deviations kept on purpose, each with a test that says why: a bare CR or
  LF in `DATA` rejects the message; a command line may end in a bare LF;
  any printable EHLO argument is accepted; address sizes over the RFC's
  minimums are refused; `VRFY` answers `252`; `internal/address` knows only
  lowercase `a-z 0-9 . _ + -` local parts; threads ignore the subject.
- Not built: a bounce when delivery fails for some recipients of a
  message. RFC 5321 4.4 wants `250` plus a notification; we answer `451`
  and rely on the idempotent retry, as owl did. Needs phase 5's outbound.

### Phase 3: JMAP read side and webmail — done

- `internal/jmap`: session, request handling with result references
  and limits, `Mailbox/get|changes|query`, `Thread/get`,
  `Email/get|query|changes`, `Email/set` for keywords, `mailboxIds` and
  destroy, `Mailbox/set`, blob download.
- `internal/message`: MIME to body structure, charsets through
  `x/text`, sanitised HTML view for a sandboxed iframe (allowlist on
  `x/net/html`, CSP, remote images off by default).
- `internal/web`: login, sessions, `Origin` check, serving the embedded app
  (in development Vite's dev server proxies the API paths to Go instead).
- `web/`: login, mailbox list, message list, thread view, labels,
  attachment download, mark read, flag, move, delete.
- Exit: browse and manage mail in the browser; the same session answers raw
  JMAP from `curl`. The `curl` half is `contrib/dev/jmap-demo.sh`; the
  browser half is `make e2e`.

Built as follows (the Go side), where it differs from or adds to the above:

- Paths. `/.well-known/jmap` and `/jmap/session` (session resource),
  `/jmap/api`, `/jmap/download/{accountId}/{blobId}/{name}?type=`,
  `/jmap/view/{accountId}/{emailId}[?images=1]` (the HTML view, not part of
  JMAP), `/auth/login`, `/auth/logout`, `/auth/session`. `/jmap/upload/` and
  `/jmap/eventsource/` are named in the session, as the RFC requires, and
  answer 501 until phases 5 and 6. Everything else is the webmail from an
  `fs.FS`: a placeholder page now, `cmd/robinmail`'s `webStatic` later.
- More methods than listed, because a client needs them to stay in sync:
  `Mailbox/queryChanges`, `Thread/changes`, `Email/queryChanges`,
  `Core/echo`. Not built: `Email/set` create (answers `forbidden`; phase
  5), `Email/import`, `Email/copy`, `Email/parse`, `SearchSnippet/get`.
- `Email/query` is SQL over the email table. Filters: `inMailbox`,
  `inMailboxOtherThan`, `before`, `after`, `minSize`, `maxSize`,
  `hasKeyword`, `notKeyword`, `allInThreadHaveKeyword`,
  `someInThreadHaveKeyword`, `noneInThreadHaveKeyword`, `hasAttachment`,
  `from`, `to`, `cc`, `bcc`, `subject` (substring, ASCII case-insensitive,
  on the cached header text) and `AND`/`OR`/`NOT`. `text`, `body` and
  `header` answer `unsupportedFilter` until phase 8. Sorts: `receivedAt`,
  `sentAt`, `size`, `from`, `to`, `subject`, `hasKeyword`.
  `collapseThreads` is a window function. Without an anchor the window is
  cut in SQL; with an anchor or a position from the end the ids are
  fetched and cut in Go.
- `/queryChanges` reports every email that changed, and every email of a
  thread that changed, as removed and, if it is in the results now, as
  added at its index. The RFC allows that; it is not the smallest answer.
  `upToId` is accepted and not used.
- State strings are the account `modseq` for every type. `ifInState` is
  compared first and then travels as a guard inside the first write of the
  call. Each create, update and destroy of a `/set` is a batch of its own.
- Body part blob ids are `<blob of the message>-<partId>`; the part is
  found by parsing the message again. Only the blobId of an Email is a
  SHA-256 (see open questions).
- `bodyStructure` always includes `subParts`. Member names of a request
  are matched case-sensitively, which `encoding/json` does not do by
  itself.
- Delivery fills the cached columns through `internal/message`: the header
  text, `sent_at`, a preview from the text body or from the HTML, and
  `has_attachment` by the RFC's rule. Schema version 3 adds the columns
  with the first migration. `robinmail reindex` refreshes mail stored
  before.
- The HTML view drops `<style>`, `<script>`, forms, SVG and MathML with
  their content, keeps an allowlist of elements and attributes, filters
  `style` attributes per declaration, turns `cid:` images into `data:`
  URLs and leaves out remote images unless `?images=1`. It answers
  `X-Remote-Images: blocked|shown|none` so the webmail can offer to load
  them. Its CSP is `default-src 'none'` with `sandbox`; a download is
  always an attachment with `nosniff` and a sandbox CSP.
- Web: the cookie is `__Host-robinmail` (`Secure`, `HttpOnly`,
  `SameSite=Strict`) on HTTPS and `robinmail` without `Secure` on the plain
  listener. The `Origin` check: an `Origin` must be ours or a configured
  dev origin; without one `Sec-Fetch-Site` must be `same-origin`; a client
  that sends neither is not a browser and is let through, or `curl` and
  standard JMAP clients could not work. Requests for another `Host` get
  421. Basic auth on the JMAP paths, with a verified password remembered
  for five minutes. Login failures: 10 per address and 50 per IP in 15
  minutes, in memory, and at most four Argon2id checks at once.
- Config: `web.listen` (HTTPS, needs `tls`; with ACME it is the listener
  the certificate is validated over), `web.http_listen` (plain HTTP on a
  loopback address only), `web.dev_origins`. Without a listener the web
  side is off.
- The blob sweeper is built (rule "Blobs" above) and runs with the daily
  trim, which now also removes expired sessions. Commands: `sweep`,
  `reindex`.
- `contrib/dev/seed.sh` makes a local instance in `./dev` with a user and
  five messages; `contrib/dev/jmap-demo.sh` is the `curl` session.
- The tests found and fixed, in the new code:
  - A date with folding or comments around the colons of the time did not
    parse (RFC 5322 4.3; the example of appendix A.6.3).
  - Header values of nothing but `(`, `"`, `[` or `=?` cost quadratic
    time (the limits test); every such scan is bounded now.
  - A message id of only invalid UTF-8 became an empty id, and `<<>`
    gave a URL of `<` (fuzzing).
  - `methodCAlls` was taken for `methodCalls`, and a request without
    calls answered `"methodResponses": null` (fuzzing).
  - Three messages of the same second came oldest first in a list sorted
    newest first (the end-to-end test).
- Deviations kept on purpose, each with a test that says why: encoded
  words are also decoded inside quoted strings, when split inside a
  character, and when longer than 75 characters; an obsolete message id
  with white space inside is skipped; `bodyStructure` has `subParts`
  without being asked.

Built as follows (the webmail in `web/`):

- Stack as decided: Vue 3, Vue Router, TypeScript, Vite, Nuxt UI 4 through
  its Vite plugin (dashboard group, sidebar, panels). Runtime dependencies:
  `vue`, `vue-router`, `@nuxt/ui`, `tailwindcss`, `@iconify/vue`. Icons
  (Lucide) are bundled at build time; the page never asks an icon API.
- Build and embedding. `make` runs `npm run build`, copies `web/dist` to
  `internal/web/dist` and builds with the tag `webmail`, which embeds that
  directory (`internal/web/embed_webmail.go`). Neither directory is in git.
  Without the tag (`go build ./...`, no Node needed) the binary serves the
  placeholder. The static handler and the CSP of phase 3's Go side fit the
  built app unchanged: no inline script, `frame-src 'self'` for the view.
  `TestEmbeddedWebmail` (tag `webmail`) checks the real build through the
  handler. `make test`, `make e2e` and `make dev` are described in the
  `Makefile`.
- JMAP client of our own (`web/src/jmap`): session, batches with result
  references, typed per method. It uses only the path of the session's
  URLs, since the webmail is always same-origin. A list is one request:
  `Email/query` (collapsed threads) → `Email/get` → `Thread/get` →
  `Email/get` for every email of those threads.
- State (`web/src/store/mail.ts`). One state string per type. It moves on
  after `/changes` was merged, and after a `/set` of ours only when that
  `/set` started from the state we held; a `/get` never moves it. A
  refresh is one request of twelve calls (`Mailbox/changes`,
  `Email/changes`, `Thread/changes`, `Email/queryChanges`, each with the
  `/get` calls it needs), every 30 seconds while the page is visible and
  on focus. `cannotCalculateChanges` fetches everything held again.
- Actions change the page first and send the `Email/set` after; a refused
  or failed change is put back and reported. While a change waits, answers
  from the server are older than the page and are dropped. Changes are
  sent with `keepalive`, so closing the tab does not lose them.
  `ifInState` is used for `Mailbox/set` only (rename, delete, create): on
  `stateMismatch` the mailboxes are fetched again and the change is tried
  once more if the mailbox it is about did not change. Email changes are
  patches that hold in any state, so they carry none.
- What an action covers. A row is a thread. Read: every email of the
  thread; unread and flag: the newest; move and delete: the emails of the
  thread that are in the open mailbox. Delete replaces all mailboxes by
  Trash; in Trash it destroys. Deleting a mailbox with mail in it uses
  `onDestroyRemoveEmails` after saying so.
- Routes: `/login`, `/mail/<mailbox id>/<thread id>` with the filter in
  the query (`from`, `subject`, `unread`, `flagged`, `attachment`). A 401
  resets the store and returns to `/login?next=…`. Nothing about the
  session is in `localStorage`; only the colour mode and pane widths are.
- The HTML view sits in `<iframe sandbox="allow-popups
  allow-popups-to-escape-sandbox">`; a `HEAD` on the view reads
  `X-Remote-Images` for the "load remote images" bar. A text body comes
  from `bodyValues`.
- Keys: `j`/`k`, `u` (read/unread), `s` (flag), `#` or Delete, `/`
  (filter), Escape.
- Tests: Vitest for the client, the state handling and the store against
  an in-memory JMAP server (`web/src/test/fake.ts`); Playwright
  (`web/e2e`) against the real binary and the dev instance: login, HTML
  view, remote images, attachment, read, flag, reload, filter, mailbox
  create and delete, move, delete, destroy, paging, selection, expired
  session, phone width, no console errors and no request to another
  origin.
- The tests found and fixed, in the new code: a refresh queued before a
  waiting change put the old data back for a moment; a reload on a thread
  closed it (the first load cleared what the thread view had fetched);
  icons named in `.ts` files were not bundled and were asked from the
  Iconify API, which the CSP refused; a change was lost or half applied
  when the page went away right after the click.
- Not built: compose, reply, drafts (phase 5); push (phase 6); search in
  bodies (phase 8); drag and drop; an undo for delete.

### Phase 4: cluster and blob replication — done

- Three rqlite nodes and three robinmail nodes in Docker Compose.
- robinmail follows rqlite leader changes and retries safely.
- Blob replication between robinmail nodes: every node holds every blob. A
  write is acknowledged once 2 nodes have the file. A background check finds
  blobs a node misses and fetches them from another node. Nodes exchange the
  encrypted files as they are; the format from phase 1 does not change.
- Exit: kill the rqlite leader and a robinmail node during a stream of
  deliveries: no acknowledged message is lost, none is doubled, every
  acknowledged blob is readable from a surviving node, the killed node
  catches up on the blobs it missed, and the webmail keeps working on another
  node.

Built as follows:

- **rqlite client.** `rqlite.urls` lists several nodes, the nearest first.
  The client stays with the node that answered last and moves on when it
  stops answering. rqlite nodes hand a request to the leader themselves; a
  redirect, if one comes, is followed with method and body kept. Tried
  again, with a backoff of 50 ms to 1 s, for at most 15 seconds: every
  read, and every request that provably never reached the log (connection
  not made, redirect, "leader not found"). A write whose outcome is unknown
  is sent again only when its batch is idempotent or guarded (rule "Writes
  and retries"). 2 seconds to connect, 10 seconds per attempt. The client
  does not learn who leads: a request through a follower costs one hop
  inside rqlite.
- **Membership.** `node` in the config: `id`, `listen`, `address`,
  `blob_copies` (2), `repair_interval_seconds` (300). A node writes its row
  at start and renews `seen_at` every 5 seconds. The row stays when the
  node stops, so the others keep counting it; `node list`, `node remove`.
  Without a `node` section an instance runs alone, as before.
- **Between nodes: mutual TLS, pinned through the node table.** Each node
  makes an ECDSA P-256 key and self-signed certificate once and keeps them
  in its data directory (`node.key`, `node.crt`), so the server and the
  commands on a node are the same node. Calls are HTTP over TLS 1.3 with a
  certificate on both sides. A server accepts a client only if the
  SHA-256 of its certificate is in the node table; a client accepts a
  server only if it shows the certificate registered for the node it
  called. Chosen over a shared secret: nothing to distribute, and rqlite
  is already the place that must not be writable by strangers. An unknown
  certificate makes a node read the table again, at most once a second.
  Tested: no certificate, an unknown one, plain HTTP and a removed node get
  no answer; a server with the wrong certificate gets no blob.
- **Blob replication** (`blob.Replicated`). A write goes to the local disk
  and is pushed, as the encrypted file it is, to all other registered nodes
  at once (`PUT /v1/blob/<account>/<sha256>`). It returns when `blob_copies`
  nodes, this one included, have it durably; with fewer nodes registered,
  all of them. The pushes still running finish in the background. Too few
  copies fails the write: SMTP answers `451` and the sender retries. A
  node whose push failed is left out for 3 seconds, unless the write cannot
  succeed without it. A read that finds no local file asks the other nodes
  one by one (`GET`), keeps the file and serves it.
- **Verification.** A received file, pushed or fetched, gets its name only
  after the receiver decrypted it with the account's key and found the
  SHA-256 of the plaintext equal to the name. A check without a key was
  not worth having: every node holds the master key anyway.
- **Anti-entropy.** At start and every `repair_interval_seconds` a node
  lists, per account, the blobs emails refer to, compares that with its
  disk and fetches what is missing. This is how a new node fills up, how a
  node that was away or lost its disk catches up, and what covers a failed
  push. Unreferenced files are not fetched. Command: `repair`.
- **Per-node state that is now shared.** Login failures are counted in
  `login_failure` as well as in memory, with the same limits; a login
  without failures behind it costs one read the nearest rqlite node
  answers from its own copy, and no write. The in-memory counters remain
  as the fallback when rqlite does not answer. The domain list is no
  longer cached: every RCPT reads it. Trim and sweep run on every node
  unchanged (rule "Blobs" for why the sweep is safe). This was listed
  under phase 7.
- **Cluster in Docker.** `Dockerfile` builds the binary with the webmail.
  `contrib/compose/cluster.yaml` runs three rqlite and three robinmail
  nodes; `contrib/compose/cluster.sh up` makes a master key and a
  certificate, starts them and adds a domain and four users. Rspamd is an
  optional profile.
- **Exit tests** (`internal/clustertest`, `make cluster-test`, skipped
  without `ROBINMAIL_TEST_CLUSTER`). Senders deliver through all three
  nodes to four accounts and send the very same message again, to the next
  node, until it gets `250`. Run on rqlite 10.7.1, Docker Desktop on a
  laptop:
  - (a) rqlite leader killed for 15 seconds and started again: 6623
    messages got `250` in 26 seconds, none got `4xx`; the longest time
    without any `250` was 2.1 seconds, while robinmail waited for the new
    leader. Every one of them is stored exactly once and reads back as
    sent through each of the three robinmail nodes.
  - (b) a robinmail node killed: 5101 messages got `250`, 132
    attempts to the dead node got no answer and were sent again elsewhere;
    0 lost, 0 doubled, read back through both survivors while the node was
    still dead. Each survivor held a blob for every email. The session
    cookie from a login on the killed node worked on the other two
    (session, `Mailbox/get`, `Email/query`, `Email/get`, `Email/set`). By
    the time the restarted node answered and was counted it had all 11724
    blobs, and everything read back through it.
  - (c) a node whose blob directory was removed: 17 seconds
    later it had exactly the 11724 referenced blobs again (the pass ran
    every 15 seconds in this cluster); then the other two robinmail nodes
    were killed and it read all 11724 messages from its own disk.
  - (d) a blob file removed on one node and the message read through that
    node at once: answered in 5 ms with the message as sent, the file was back
    on disk and the node logged the fetch from a peer.
  - (e) throughput, each delivery a whole SMTP transaction that ends with
    the blob on two nodes and one Raft write: 239
    deliveries a second with 3 senders, 381 with 12, 469 with 48, with
    about 3000 to 4000 emails per account. Phase 0 saw 2500 bare requests
    a second on one rqlite node.
- **What a network partition does.**
  - A robinmail node that cannot reach a majority of rqlite: every write
    waits 15 seconds and fails, so SMTP answers `451` and nothing is
    acknowledged; reads fail too (logins, JMAP: 503). Nothing is served
    from a stale copy, except the login failure counters for 5 seconds.
  - A robinmail node that reaches rqlite but no other robinmail node:
    it cannot put a blob on a second node, so it answers `451` and writes
    no row. It still serves the mail whose blobs it has; a message whose
    blob it lacks fails to open until a peer is back.
  - Two robinmail nodes and an rqlite majority on one side: that side
    works as a cluster with one node down. The third catches up through
    the repair pass when it is back.
  - An rqlite leader that was just cut off answers `weak` reads for about
    a second from data that misses the newest writes (rule "Reads"). Its
    own writes cannot commit.
  - No side can acknowledge mail the other side does not get to know:
    acknowledging needs a Raft majority and a second blob copy.
- **Not handled.**
  - A blob that was acknowledged on 2 nodes and whose 2 disks are both
    lost before the third node fetched it is gone. `blob_copies: 3` closes
    that at the price of refusing mail whenever one node is down.
  - Damage to a file that is in place: the repair pass looks at names, not
    content. A read of a damaged file fails and does not fall back to
    another node. `verify` finds such files; removing the file and running
    `repair` fixes it. There is no periodic scrub.
  - A node that is gone for good stays in the node table until `node
    remove`. That is harmless (2 of the remaining nodes still acknowledge)
    but nothing does it automatically.
  - While only one node has registered, it acknowledges with one copy.
    Start the first two nodes before taking mail.
  - Mail in flight on a node that dies is not lost only because the sender
    retries. That is SMTP's contract, and phase 5's own queue needs the
    same care.
  - JMAP writes other than delivery are not sent again after an unknown
    outcome: a client can see `serverFail` during a leader change and has
    to sync and retry.
  - rqlite in `cluster.yaml` has no authentication and no TLS. Production
    needs both, since the node table is what admits a node.
  - Clock differences between nodes: `received_at`, session expiry and the
    blob grace period use each node's own clock.

### Phase 5: outbound, submission, compose — Go side done, webmail open

- Submission on 465 and 587, outbound through Rspamd for DKIM, queue in
  `submission` (a node claims a row for a limited time), MX delivery with
  STARTTLS, MTA-STS, retries, bounces.
- JMAP `Email/set` create, blob upload, `EmailSubmission/set`, `Identity/get`.
- Webmail: compose, reply, forward, attachments, drafts. **Open**: a
  session of its own, built on the JMAP methods listed below.
- Exit: send between two robinmail clusters; an unreachable host is retried
  and bounces; a killed node does not lose queued mail.

Built as follows (the Go side):

- **Submission** (`smtp.Server` with `Auth`, `delivery.Handler`). Config
  `submission {listen, listen_tls, max_recipients_per_hour, undo_seconds}`;
  both listeners need a certificate. 587 offers `AUTH` only after
  STARTTLS, 465 has TLS from the first byte. `AUTH PLAIN` and `LOGIN`
  check the account password through `web.Server.Login`, so SMTP shares
  the login failure counters (in memory and in rqlite) and the bound on
  Argon2id with the web login. Refusals: `538 5.7.11` for AUTH without TLS,
  `530 5.7.0` for MAIL without STARTTLS or without AUTH, `535 5.7.8`,
  `454 4.7.0` when limited or when the store is away, `550 5.7.1` for a
  sender that is not the user's.
- **What a submitted message must be and becomes** (`delivery.Submitter`,
  for SMTP and JMAP alike). Envelope sender, `From` and `Sender` must be
  the account's address, with or without a `+tag`; the null reverse-path
  is allowed. `Bcc` is removed, `Date` and `Message-ID` are added when
  missing, bare LF becomes CRLF. On top goes `Received: by <host>
  (robinmail) with ESMTPSA id <id> (<TLS>); <date>`: no FROM clause, so
  neither the address nor the EHLO name of the user's computer reaches a
  recipient (a deliberate deviation from RFC 5321 4.4, as large providers
  do it). A JMAP submission says `(JMAP)` instead of a WITH clause. Limits:
  the `smtp` message size and recipient count, and
  `max_recipients_per_hour` (300) per account, counted in the `submission`
  table inside the insert, so it holds for all nodes together.
- **Rspamd and DKIM.** The message is scanned with the account's address
  in `User`, which makes Rspamd's `dkim_signing` sign it. Over HTTP Rspamd
  returns the signature in the field `dkim-signature` of its reply, not
  among the milter headers; it is written as the field it is, above the
  message. `reject` refuses the submission (`550 5.7.1`,
  `forbiddenToSend`), `soft reject` and an unreachable Rspamd defer it;
  with `fail_open`, and without an Rspamd at all, mail goes out unsigned,
  with a warning at start. `contrib/rspamd/local.d/dkim_signing.conf`: one
  key per domain in `/var/lib/rspamd/dkim/<domain>.robinmail.key`, selector
  `robinmail`. `robinmail dns <domain> [key file]` makes the key if the
  file is not there (RSA 2048) and prints the MX, SPF, DKIM, DMARC and
  MTA-STS records. robinmail itself signs and verifies nothing.
- **Queue** (`internal/queue`, table `submission`). One row per message,
  with the recipients and their state as JSON and a blob of its own: the
  message as it goes out. Every node runs a worker (`outbound
  {concurrency, per_destination, routes}`) that lists due rows every 5
  seconds and claims one with a guarded update: claimed by a token, for 2
  minutes, renewed every 40 seconds while the attempt runs. Results are
  written under the claim; a node that lost it cannot write. A node that
  dies leaves a claim that runs out. Recipients are grouped per domain:
  one connection and one transaction per domain, at most 2 connections
  per domain per node, an idle connection is kept 30 seconds and reused
  after an `RSET`.
- **Where mail goes** (RFC 5321 5.1). MX records by preference, equal
  ones in random order; no MX means the domain's own address; a null MX
  (RFC 7505) fails with 5.1.10; a domain without MX and address with
  5.1.2; our own host name among the MX hosts drops it and everything not
  preferred over it, and 5.4.6 if nothing is left. Every address of a
  host is tried, so IPv4 covers for IPv6 and the reverse. A domain of
  ours (the `domain` table) is never looked up: the message is stored
  here through `delivery.Local`. `outbound.routes` sends a domain, or
  `*`, to a fixed `host:port` without DNS (a relay; development).
- **SMTP client** (`internal/smtp`). EHLO with HELO fallback, STARTTLS,
  PIPELINING (one write for MAIL, RCPT and DATA), SIZE, 8BITMIME, SMTPUTF8,
  multi-line replies, a result per recipient, the timeouts of RFC 5321
  4.5.3.2. A non-ASCII domain is sent as A-labels; a non-ASCII local part
  or header needs SMTPUTF8 and fails with 5.6.7 without it; 8-bit content
  to a server without 8BITMIME fails with 5.6.3 (we do not convert).
  Without a policy TLS is opportunistic: used when offered, certificate
  not checked, one try without TLS if the handshake fails.
- **MTA-STS** (RFC 8461). The TXT record is read for every delivery; a new
  id, or no cached policy, fetches
  `https://mta-sts.<domain>/.well-known/mta-sts.txt` (no redirects,
  `text/plain`, 64 kB). Policies are cached in `mta_sts` for all nodes; a
  cached one keeps applying when DNS or the fetch fails, until it
  expires. `enforce`: only hosts the policy names, STARTTLS required, TLS
  1.2 or later, certificate valid for the host; otherwise the message
  waits (4.7.0). `testing`: the same is checked and logged. DANE is not
  implemented: it needs a resolver that validates DNSSEC. TLS-RPT is
  phase 8.
- **Retries and bounces.** After a try a message waits 5 minutes, then
  twice as long each time up to 4 hours: 36 tries in five days, then it is
  given up with 4.4.7. A 5yz reply fails that recipient at once. What
  failed is reported to the sender as an RFC 3464 `multipart/report`
  (text, `message/delivery-status`, the original header) from
  `MAILER-DAEMON@<host>`, stored in the sender's Inbox with `Return-Path:
  <>`, in the thread of the message. Every sender is a local user, so a
  bounce never leaves the server. A message with the null reverse-path
  gets none.
- **Incoming notifications.** A DSN from elsewhere is ordinary mail in the
  Inbox. If it has the null reverse-path and returns the Message-ID of a
  message the account sent, its `failed` and `delivered` recipients are
  recorded in that submission's `deliveryStatus`.
- **JMAP.** Capability `urn:ietf:params:jmap:submission`
  (`maxDelayedSend` 0, no extensions). Upload: `POST
  /jmap/upload/{accountId}/` answers 201 with `blobId`, `type`, `size`; an
  upload nothing refers to is removed after a day. A blobId is the SHA-256
  of a message or upload, optionally followed by part ids:
  `<blob>-3` is part 3, `<blob>-3-2` part 2 of the message attached as
  part 3.
  - `Email/set` create: `mailboxIds`, `keywords`, `receivedAt`; the header
    properties `from`, `to`, `cc`, `bcc`, `replyTo`, `sender`, `subject`,
    `sentAt`, `messageId`, `inReplyTo`, `references` and any
    `header:Name:asForm[:all]`; the body as `bodyStructure`, or as
    `textBody`, `htmlBody` and `attachments`, from which
    mixed[alternative[text, related[html, inline images]], attachments]
    is made; parts from `bodyValues` (`partId`) or a blob (`blobId`), with
    `type`, `name`, `disposition`, `cid`, `language`, `location`. `Date`
    and `Message-ID` are added. Emails are immutable: a changed draft is a
    create and a destroy in one call.
  - `Email/import`, `Email/parse`, and `Email/copy`. RFC 8620 5.4 says
    the two accounts of a `/copy` "MUST" differ; a user here has one
    account, so it is offered within the account (deviation, with a test).
  - `Identity/get|changes|set`. Identity `i1` is the account's address and
    cannot be destroyed; others may only be for that address with a
    `+tag` (`forbiddenFrom` otherwise). `email` never changes.
  - `EmailSubmission/get|changes|query|queryChanges|set`. Create takes
    `identityId`, `emailId` and optionally `envelope` (without one: sender
    from `Sender` or `From`, recipients from `To`, `Cc`, `Bcc`).
    `onSuccessUpdateEmail` and `onSuccessDestroyEmail` run as an
    `Email/set` whose response follows. `undoStatus` is `pending` for
    `undo_seconds` (10) and can be set to `canceled` until a node claims
    the message; then `final` and `cannotUnsend`. `deliveryStatus` per
    recipient: `delivered` is `queued`, `no`, `yes` (in a mailbox on this
    server, or confirmed by a DSN) or `unknown` (another server took it);
    `smtpReply` is the last reply; `displayed` is always `unknown`.
    Destroying a submission removes the record, not the mail.
- **Message building** (`internal/message`). `Build` writes a tree of
  parts; text is 7bit when it is ASCII in lines of at most 78, otherwise
  quoted-printable, or base64 when most of it would be escaped; other
  types base64; an attached message is never re-encoded. Headers: RFC 2047
  encoded words of at most 75 characters that never split a character,
  quoted or encoded display names, folding at 78, RFC 2231 for parameter
  values that are long or not ASCII.
- **Commands and files.** `robinmail dns`; `contrib/dev/send-demo.sh`
  (two local servers, `curl`); the development instance of `seed.sh`
  routes every other domain to a dead port, so it cannot send real mail; `compose.yaml` has a second rqlite
  (`rqlite-b`, port 4003) and mounts the DKIM config; `cluster.yaml` has
  `robinmail-sink`, a robinmail with its own database for the cluster to
  exchange mail with. Schema version 5; the migration replaces the unused
  `submission` table.
- **Tests.** RFC tests for RFC 5321 (client, MX rules), 2920, 1870, 6152,
  6531, 3207, 4954, 4616, 8314, 7505, 8461, 3464, 3463, 2047, 2231, 2045
  and RFC 8621 4.6 to 4.9, 6 and 7; what `Build` writes is read back by
  our parser, `net/mail` and `mime/multipart`. No test asks a real name
  server or connects beyond loopback: DNS and the dialer are injected.
  - `TestSendBetweenTwoServers` (`cmd/robinmail`): two servers with a
    domain and a database each. Submission over 587 and 465, delivery to
    the other server, a reply over JMAP, a 5.1.1 bounce at once, local
    delivery without SMTP, MTA-STS `enforce` with a certificate that is
    and one that is not valid for the MX host.
  - `TestSendRetriesThenBounces`: the test moves the queue's clock; 36
    attempts on the schedule, then the bounce with 4.4.7.
  - `TestOutgoingMailIsSignedByRspamd`: through a real Rspamd with a key
    made by `robinmail dns`; the body hash and the RSA signature of what
    arrived verify against the printed public key.
  - `internal/queue`: a node that dies holding a claim (nothing happens
    until the claim runs out, then one delivery), three nodes on one
    queue (30 messages, each once), the exact backoff schedule.
  - `make cluster-test` (f): the other side is frozen so claims are held,
    60 messages are submitted through node 1, node 2 is killed with 8
    claims. 52 arrived within a second of letting the other side go, the
    8 of the dead node 2 minutes after the kill; 0 lost, 0 doubled; the
    reply from the other side was read through the restarted node.
- **The tests found and fixed**, in the new code:
  - Nothing was signed: the DKIM signature was looked for among Rspamd's
    milter headers (the real Rspamd test).
  - A recipient with an undeliverable domain was failed without being
    reported to the sender.
  - `Build` added a line end to every part of a multipart, so a text body
    came back one line end longer than it was created with.
- **Deviations kept on purpose**, each with a test or a comment that says
  why: no FROM clause in the submission `Received` line; `Email/copy`
  within one account; `header:Content-*` properties on a body part are
  refused except `Content-Description` (the part's own properties write
  those fields); a 4yz reply to a recipient is not tried on the next MX
  host but waits for the next try.
- **Not handled.**
  - A message can be delivered twice: when a node dies, or cannot reach
    rqlite, after the other server said 250 and before that is written
    down, the next attempt sends it again. SMTP cannot prevent that. A
    receiving robinmail drops the second copy (its delivery key).
  - A claim outlives its node by up to 2 minutes; mail a dying node held
    is that much late.
  - No DANE, no TLS-RPT, no REQUIRETLS, no DSN extension (RFC 3461: no
    `NOTIFY`, `RET`, `ENVID`), no "delayed" notice before the final
    bounce, no FUTURERELEASE.
  - A bounce quotes only the header of the original.
  - An incoming DSN is matched by the Message-ID it returns. Someone who
    knows that id and a recipient can make a sent message look failed to
    its sender; nothing else changes.
  - Finished JMAP submissions stay until the client destroys them;
    submissions over SMTP are trimmed after a day.
  - Mail between two users of the same server is not scanned on the way
    in: it was scanned when it was submitted.
  - No app passwords: SMTP AUTH takes the account password until phase 7.
  - Phase 2's gap is still there: when storing an inbound message fails
    for some of its recipients the sender gets `451`, not a notification.
  - No relay limit per IP or per connection beyond the SMTP server's own,
    and no limit on the size of the queue.

### Phase 6: push and IMAP

- JMAP push over EventSource. A node polls `modseq` for the accounts that have
  a connection open on it.
- IMAP4rev1 on 993 with `IDLE`, `UIDPLUS`, `MOVE`, `SPECIAL-USE`, `CONDSTORE`.
- Exit: Thunderbird and iOS Mail sync, move, flag and see new mail arrive.

### Phase 7: auth

- App passwords: one per device, for IMAP and SMTP submission, revocable.
  The account password then only opens the webmail.
- TOTP (RFC 6238) for web login, with recovery codes.
- Optional OIDC login for the webmail against any provider: authorization
  code flow with PKCE, ID token checked with our own JWT and JWKS code.
- Exit: Thunderbird logs in with an app password and not with the account
  password; web login asks for TOTP; a Keycloak container logs a user in
  through OIDC.

### Phase 8: completeness

- Aliases and catch-all addresses.
- JMAP `VacationResponse`.
- Filters: Sieve (RFC 5228) run at delivery, managed over JMAP (RFC 9661).
- Full-text search behind `Email/query` and IMAP `SEARCH`.
- Quotas (RFC 9425).
- MTA-STS policy serving, TLS-RPT, client autoconfig.
- Admin commands, backup and restore of rqlite plus blobs.
- Exit: each item shown working with a test.

## Open questions

- Where the master key comes from in production: a file is all there is. A
  lost key file loses all mail, so it needs a backup that is not next to the
  blobs.
- Delivery keys derived from content mean a byte-identical message from the
  same sender within a week is dropped as a retry. Intended, but worth
  knowing.
- Where phase 5 gets tested against real providers: which host and domain.
  Still open: everything sent so far went over loopback or a Docker
  network. What only real providers can show: whether Gmail, Outlook and
  the like accept the mail (PTR, SPF, DKIM alignment, their rate limits),
  how their greylisting and 4yz replies meet our backoff, what their
  MTA-STS policies and certificates do under `enforce`, and whether their
  bounces parse.
- The strict `MAIL FROM` grammar refuses senders that exist in the wild,
  mostly hosts with an underscore in their name. Postfix accepts those by
  default. The RFC says `501`; real traffic in phase 5 will show whether
  that costs wanted mail.
- RFC 9106's own test vectors use a secret and associated data, which
  `x/crypto`'s exported `argon2.IDKey` does not take. `x/crypto` runs them
  in its own tests; ours check the reference implementation's vectors
  without those two inputs. Good enough, or wrap Argon2 ourselves?
- `<Postmaster>` without a domain picks one domain's postmaster. With
  several domains of different owners that may be the wrong person.
- The Decisions say the name of a blob "is also the JMAP blobId". That
  holds for a message and for an upload. A body part has no file of its
  own, so its id is `<blob>-<partId>`, and `Email/set` create, `/import`
  and `/parse` accept both kinds.
- The HTML view drops `<style>` blocks, so mail that is laid out with a
  style sheet instead of attributes looks plain. Keeping them needs a CSS
  sanitiser of our own. Worth it once the webmail shows real mail.
- `Email/get` reads and decrypts the whole message for any property that
  comes from a header, also for a list of thirty rows. Fast on local
  disk, but a list could be answered from the email row alone if the
  parsed `from` and `to` were cached there as JSON.
- `Email/query` sorts all matches of an account before it cuts the window,
  and `Mailbox/get` counts emails and threads on every call. Fine for
  thousands of messages; phase 4 should measure it with more.
- The Basic auth memory is per node. A changed password keeps working
  over Basic for up to five minutes on a node that remembered the old one.
- The repair pass reads the blob list of every account and lists every
  blob directory on each node every five minutes. Fine for the 20,000
  emails of the cluster test; with millions it wants to work from the
  change log instead ("what was delivered since my last pass").
- Every RCPT now reads the domain table and every node writes a heartbeat
  every 5 seconds. Both are small; at a real load a short cache that is
  invalidated through a version row would bring the domain read back to
  one per change.
- A delivery through an rqlite follower is forwarded to the leader inside
  rqlite. The client could ask who leads and go there directly.
- Reads at `weak` can be about a second stale right after a leader was cut
  off. If that ever matters for a read (it does not for any today),
  `linearizable` costs one round trip to a quorum.
- On the plain listener the session cookie has no `Secure` flag. That
  listener only binds to loopback, but a TLS-terminating proxy in front of
  it would get cookies a browser also sends over HTTP. The plan has no
  proxy; if one ever appears, it needs its own setting.
- A JMAP request may be 10 MB and four run at once per account, each held
  in memory. No limit across accounts yet.
- The HTML view has a fixed height (65% of the window) and scrolls inside
  it: a frame without scripts and without our origin cannot tell the page
  how tall it is. A short message leaves white space. The way out is a
  height the server computes, or a view that is allowed one fixed script.
- Each create, update and destroy of a `/set` is its own batch, and the
  request's context ends when the client goes away. A browser that
  disconnects in the middle leaves the `/set` half applied (seen in the
  end-to-end test: one of two destroys). The webmail now sends changes
  with `keepalive`; the server could also finish a `/set` it started.
- The webmail's assets are served uncompressed: 680 kB of script and
  210 kB of CSS, 195 kB and 26 kB with gzip. Compress at build time and
  serve by `Accept-Encoding`, or leave it to phase 8.
- The navigation shows `unreadEmails`, which the webmail can keep exact
  while a change is on its way, not `unreadThreads`, which fits collapsed
  threads better but only the server can count.
- `go build ./...` walks `web/node_modules` when it exists. No npm package
  there ships Go files now; one that does would become part of `./...`.
- DKIM keys are files on every Rspamd. A cluster has to copy them by
  hand. Rspamd can also take the key with the request
  (`use_http_headers`), which would let robinmail keep keys in rqlite;
  not tried.
- `undo_seconds` is a server setting (10). The webmail cannot ask for a
  longer or shorter hold per message; FUTURERELEASE (`HOLDFOR`) would be
  the standard way.
- `deliveryStatus` says `unknown` for mail another server accepted, as RFC
  8621 defines it. A user will read that as "sent"; the webmail has to
  word it.
- The queue asks rqlite for due rows every 5 seconds on every node, also
  when it is empty. A submission could wake the local worker at once; the
  5 seconds are what a send waits today at most.
- The MTA-STS TXT record is looked up for every delivery to a domain, and
  the resolver's cache is the only one in front of it.
- A bounce is stored in the Inbox without going through Rspamd or any
  filter; with Sieve (phase 8) it should pass through the user's rules.
- The tests of all packages share one rqlite database. Queue rows are now
  purged by the tests that make them, but a `serve` on the development
  database still works whatever is queued there.
