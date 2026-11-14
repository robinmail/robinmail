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
| `account` | id, address, password_hash, blob_key, created_at, modseq, trimmed_modseq, next_email, next_mailbox, next_thread | id is random and never changes; counters live here; `blob_key` is the account's blob key wrapped by the master key; `trimmed_modseq` is the newest state the change log no longer covers |
| `session` | token_hash, account_id, expires_at | web login; the hash is SHA-256 of the token in the cookie |
| `mailbox` | id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq, subscribed | a rename changes `name` only |
| `email` | id, blob, size, received_at, thread_id, message_id, sender, recipients, cc, bcc, subject, sent_at, preview, has_attachment, modseq | header fields cached for lists, filters and sorting |
| `email_mailbox` | email_id, mailbox_id, uid | one row per label; the IMAP UID belongs to the row |
| `keyword` | email_id, keyword | `$seen`, `$flagged`, `$draft`, `$answered`, custom |
| `thread_ref` | message_id, thread_id | every Message-ID seen in `Message-ID`, `In-Reply-To`, `References` |
| `changes` | modseq, type, id, op, at | type Email, Mailbox or Thread; op created, updated or destroyed; `at` is for trimming |
| `delivered` | key, at | delivery key, for idempotent delivery; trimmed after a week |
| `submission` | id, email_id, envelope, state, next_try, claimed_by, claimed_until | outbound queue (phase 5) |
| `acme` | key, value | ACME account and certificates, shared by nodes (not built yet) |
| `guard` | ok | always empty; how a batch checks a precondition (below) |
| `schema_version` | version | checked at start; an older database is migrated in one guarded batch per version (so far 2 to 3) |

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
  delivery of the same content at that moment keeps its blob.
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
internal/blob        encrypted blob files on disk       new
internal/store       schema and all mail operations     new, replaces owl's store
internal/message     MIME to JMAP body structure, HTML view   new
internal/jmap        JMAP core and mail, protocol only  new
internal/web         HTTPS, login, sessions, static app new
internal/queue       outbound delivery                  new
internal/imap        IMAP server                        new
web/                 Vue webmail (Vite, Nuxt UI); src/jmap is the client, e2e the browser test
Makefile             web build, binary with the webmail, tests, dev
contrib/rspamd       Rspamd config                      port as is
contrib/compose      rqlite, Rspamd for dev             new; with dev-only Rspamd overrides
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
  guards inside the batch. Delivery reads the table at most once per 30
  seconds and keeps the last list when rqlite does not answer.
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

### Phase 4: cluster and blob replication

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

### Phase 5: outbound, submission, compose

- Submission on 465 and 587, outbound through Rspamd for DKIM, queue in
  `submission` (a node claims a row for a limited time), MX delivery with
  STARTTLS, MTA-STS, retries, bounces.
- JMAP `Email/set` create, blob upload, `EmailSubmission/set`, `Identity/get`.
- Webmail: compose, reply, forward, attachments, drafts.
- Exit: send between two robinmail clusters; an unreachable host is retried
  and bounces; a killed node does not lose queued mail.

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
- Login rate limits kept in rqlite so they hold across nodes.
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
- The domain list is cached for 30 seconds per node, so a removed domain
  still gets mail for that long.
- The Decisions say the name of a blob "is also the JMAP blobId". That
  holds for a message. A body part needs a blobId too and has no file of
  its own, so its id is `<message blob>-<partId>`. Phase 5's upload will
  make real blobs again; `Email/set` create then has to accept both kinds.
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
- Login limits and the Basic auth memory are per node and in memory until
  phase 7. A changed password keeps working over Basic for up to five
  minutes on a node that remembered the old one.
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
