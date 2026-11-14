# robinmail: plan

robinmail is a JMAP-first mail server in Go. It receives mail, stores it, sends
it, and serves it over JMAP (RFC 8620, RFC 8621) and, for mail clients, over
IMAP (RFC 3501, RFC 9051). The webmail is a Vue app in
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
| Encryption at rest | Every blob is encrypted with a key of its account (XChaCha20-Poly1305 from `golang.org/x/crypto`, in chunks). The account key is in the `account` row, wrapped by a master key from a key file every node has. What rqlite holds is not encrypted by robinmail: the cached header fields and previews, and since phase 8 the full-text index, from which the words of every message can be read | Protects the blobs against stolen disks and backups. Does not protect against a compromised running server, which holds the master key, nor does it cover rqlite's files: those need an encrypted disk. |
| Nodes | Not stateless: metadata is in rqlite, blobs are on each node's disk. Any node serves any account | No owner per account and no forwarding. rqlite's leader orders the writes. |
| Between nodes | Nodes find each other through the `node` table in rqlite and talk over mutual TLS: each has a self-signed certificate, and both sides accept exactly the certificates whose fingerprint is in that table | No CA and no shared secret to distribute. Who may join is decided by who can write to rqlite, which holds everything anyway. |
| Mailboxes | An Email can be in several mailboxes (labels) | Full JMAP `mailboxIds`. |
| IMAP | Built in phase 6 on UIDs that exist from day one. A label is a folder; a message with three labels is in three folders | No store migration later. Mail clients only speak IMAP. |
| Webmail | Vue single-page app in `web/`: plain Vite, Vue Router, Nuxt UI (without Nuxt). Built to static files and embedded with `go:embed` | The client only talks JMAP, so nothing needs to render on a server. No workerd container and no proxy; a node stays one binary. Nuxt UI is MIT and has the dashboard and inbox components a mail client needs. |
| Origin | One hostname. robinmail serves the API under `/jmap`, `/.well-known/jmap` and `/auth`, and the embedded webmail everywhere else | Same origin: no CORS, `SameSite=Strict` works. |
| Web auth | Session cookie (`Secure`, `HttpOnly`, `SameSite=Strict`, `__Host-`) plus an `Origin` check on every request to `/auth` and JMAP, and JSON with its content type for everything that changes state. No CSRF token. Sessions are rows in rqlite | The client never holds a token. Any node accepts the cookie. The three checks leave no cross-site request for a token to stop (phase 7). |
| Spam and mail auth | Rspamd over HTTP | As in owl, including its lessons (below). |
| Config | JSON | As in owl. |
| Passwords | Argon2id, PHC string | As in owl. |
| Accounts and login | Built in. App passwords for IMAP, SMTP and JMAP with Basic auth, TOTP for web login, optional OIDC login for the webmail (phase 7). No Keycloak or other identity service required | Mail clients log in with a password and do not speak OIDC, so robinmail needs its own accounts anyway. OIDC stays generic, so Keycloak, Authentik or Zitadel can be plugged in. |
| Auth code | JWT, JWK, PKCE, OIDC, TOTP and the QR code are our own, on the standard library (`internal/jose`, `internal/oidc`, `internal/totp`, `internal/qr`) | The dependency rule above. Each is small when it only does what robinmail needs: verify two signature algorithms, one flow, one kind of code. |

Not in scope: POP3, CalDAV/CardDAV, migration tooling.

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
| `sieve_script` | id, name, blob, size, active, created_at, modseq | Sieve scripts (RFC 9661); the script is a blob of the account; at most one per account is active |
| `vacation` | enabled, from_at, to_at, subject, text_body, html_body, modseq | JMAP's VacationResponse: at most one row per account |
| `vacation_sent` | key, at | automatic replies that went out: a hash of the reply and of whom it was for; trimmed after 45 days |
| `alias` | address, target, domain, wildcard, created_at | one row per target of an alias; `address` is an address or a pattern with one `*` (`*@domain` is the catch-all); not per account |
| `account` | id, address, password_hash, blob_key, created_at, modseq, trimmed_modseq, next_email, next_mailbox, next_thread, next_submission, next_identity, uidvalidity, auth_seq, password_changed_at, seq, next_sieve, quota_bytes, quota_count, used_bytes, used_count | id is random and never changes; counters live here; `uidvalidity` is the last UIDVALIDITY a mailbox of the account got; `blob_key` is the account's blob key wrapped by the master key; `trimmed_modseq` is the newest state the change log no longer covers; `auth_seq` moves when a way into the account is withdrawn; `password_hash` is `!` for an account that an OIDC login made; `seq` numbers the accounts and makes the row ids of `email_fts`; `quota_*` are the account's own limits (NULL: the default of the config, 0: none) and `used_*` what its emails add up to, kept by triggers |
| `session` | token_hash, id, account_id, stage, method, created_at, seen_at, expires_at, idle, ip, seen_ip, agent, remember | web login; the hash is SHA-256 of the token in the cookie; `id` is what the user's list shows and revokes by; `stage` is `full`, or `totp` for a login that still owes its code; ends at `expires_at`, or `idle` seconds after `seen_at` |
| `app_password` | id, name, hash, scope, created_at, used_at, used_ip | a password for one device; `hash` is SHA-256 of the 160 random bits the server made; `scope` lists `imap`, `smtp`, `jmap` (empty: all); revoking deletes the row |
| `totp` | secret, created_at, confirmed_at, last_step | one row per account; the secret is sealed with the account's blob key; not asked for until confirmed; `last_step` is the newest time step a code was accepted for |
| `recovery_code` | hash | SHA-256 of 80 random bits; using a code deletes its row |
| `oidc_link` | provider, subject, linked_at | the `sub` under which a provider knew the account at its first login through it |
| `auth_event` | id, at, account_id, address, event, ip, detail | the log of auth events; not per account (a failed login may name nobody); trimmed after `auth.audit_retention_days` |
| `mailbox` | id, parent_id, name, role, sort_order, uidvalidity, uidnext, modseq, subscribed | a rename changes `name` only |
| `email` | id, blob, size, received_at, thread_id, message_id, sender, recipients, cc, bcc, subject, sent_at, preview, has_attachment, modseq | header fields cached for lists, filters and sorting |
| `email_mailbox` | email_id, mailbox_id, uid | one row per label; the IMAP UID belongs to the row |
| `email_fts` | rowid, subject, addr, body, attach, hdr | the full-text index: an FTS5 table, contentless (the words and their places, not the text); the rowid is the account's `seq` times 2^32 plus the email id; a trigger on `email` removes a row with its email; not per account and not encrypted |
| `keyword` | email_id, keyword | `$seen`, `$flagged`, `$draft`, `$answered`, custom; `$deleted` is IMAP's `\Deleted` |
| `thread_ref` | message_id, thread_id | every Message-ID seen in `Message-ID`, `In-Reply-To`, `References` |
| `changes` | modseq, type, id, op, at | type Email, Mailbox, Thread, EmailSubmission, Identity, SieveScript, VacationResponse or Quota; op created, updated or destroyed; `at` is for trimming |
| `delivered` | key, at | delivery key, for idempotent delivery; trimmed after a week |
| `submission` | id, email_id, thread_id, identity_id, message_id, blob, size, mail_from, recipients, rcpt_count, undo, queue, created_at, send_at, next_try, tries, claimed_by, claimed_until, destroyed, submit_key, modseq | outbound queue: one row per message; `recipients` is JSON with the state of each; `blob` is the message as it goes out; a node works on a row while its claim lasts |
| `identity` | id, name, email, reply_to, bcc, text_signature, html_signature, modseq | JMAP identities; id 1 is made with the account |
| `upload` | blob, size, type, created_at | blobs a client uploaded; trimmed after a day |
| `mta_sts` | domain, policy_id, mode, mx, fetched_at, expires_at | MTA-STS policies of other domains, cached for all nodes; not per account |
| `tls_result` | day, domain, policy, result, mx, ip, count | TLS reporting (RFC 8460): outbound sessions counted per UTC day, recipient domain, policy and outcome; kept 7 days; not per account |
| `tls_report_sent` | day, domain, at, outcome | the TLS reports that were made: the row is the claim, so one node makes each; kept 90 days |
| `tls_report_in` | id, received_at, organization, report_id, domain, start_at, end_at, successful, failed, json | TLS reports other servers sent us; unique per organization and report id; kept 90 days |
| `acme` | key, value | ACME account and certificates, shared by nodes (not built yet) |
| `node` | id, address, cert, started_at, seen_at | the robinmail nodes: where the others reach each, the SHA-256 of its certificate, its heartbeat |
| `login_failure` | key, started, count | failures in a 15 minute window, for all nodes together: `a:` wrong passwords at the web login per address, `m:` wrong secrets from mail clients per address, `t:` wrong TOTP and recovery codes per address, `i:` all of them per IP |
| `guard` | ok | always empty; how a batch checks a precondition (below) |
| `schema_version` | version | checked at start; an older database is migrated in one guarded batch per version (so far 2 to 3 up to 7 to 8) |

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
  `modseq` doubles as `CONDSTORE`'s: a message's is its Email's, a
  mailbox's highest is the account's. A mailbox's `uidvalidity` is the
  second it was made, or one more than the last one the account gave out
  if that is not later, so a name never gets a value it had before.
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
  The poll that finds changes made on other nodes (`internal/notify`)
  uses `none` too, for `modseq` and for `auth_seq`.
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
internal/users       Argon2id hash and verify; app passwords and recovery codes   port the hashing; storage moves to rqlite
internal/delivery    recipient to mailbox; submission   port; talks to a store interface, mailbox by role
internal/rqlite      rqlite HTTP client for a cluster   new
internal/blob        encrypted blob files on disk, replicated between nodes   new
internal/cluster     node table, mutual TLS, blob transfer between nodes      new
internal/clustertest tests against the Docker cluster   new; "make cluster-test"
internal/store       schema and all mail operations     new, replaces owl's store
internal/message     MIME to JMAP body structure, HTML view   new
internal/search      words of a text, search terms, snippets  new
internal/srs         sender rewriting for forwarded mail (SRS0, SRS1)   new
internal/sieve       Sieve scripts: parser and interpreter (RFC 5228 and extensions)   new
internal/tlsrpt      TLS reports (RFC 8460): the JSON, the record, the mail   new
internal/jmap        JMAP core and mail, protocol only  new
internal/web         HTTPS, login, sessions, settings, OIDC login, static app   new
internal/totp        HOTP and TOTP (RFC 4226, RFC 6238), otpauth URI    new
internal/qr          QR codes, as far as an otpauth URI needs them      new
internal/jose        JWS compact, JWK, RS256 and ES256: verification    new
internal/oidc        OpenID Connect relying party; oidctest is a provider for tests   new
internal/queue       outbound delivery: MX, MTA-STS, retries, bounces   new
internal/notify      who hears of a change: a hub per node, a poll for other nodes   new
internal/imap        IMAP server, protocol only         new
web/                 Vue webmail (Vite, Nuxt UI); src/jmap is the client, e2e the browser test
Makefile             web build, binary with the webmail, tests, dev
Dockerfile           the binary with the webmail in an image
contrib/rspamd       Rspamd config, DKIM signing        port as is; dkim_signing.conf new
contrib/compose      rqlite, Rspamd for dev             new; with dev-only Rspamd overrides
                     cluster.yaml, cluster.sh: three rqlite and three robinmail nodes
                     keycloak.yaml: a Keycloak with a realm, for "make keycloak-test"
contrib/dev          seed and demo scripts, sample mail new; a local instance in ./dev;
                     e2e.sh and oidc-provider for the browser tests
contrib/lab          the interoperability lab: Postfix, Exim, OpenSMTPD, IMAP clients, Thunderbird, DNS, CA   new; "make lab-test"
internal/labtest     tests against that lab             new
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

### Phase 5: outbound, submission, compose — done

- Submission on 465 and 587, outbound through Rspamd for DKIM, queue in
  `submission` (a node claims a row for a limited time), MX delivery with
  STARTTLS, MTA-STS, retries, bounces.
- JMAP `Email/set` create, blob upload, `EmailSubmission/set`, `Identity/get`.
- Webmail: compose, reply, forward, attachments, drafts.
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
  implemented: it needs a resolver that validates DNSSEC. TLS-RPT and
  serving our own policy came in phase 8.
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
  routes every domain that is not its own to Mailpit, a mail catcher in
  `compose.yaml` (SMTP on 127.0.0.1:1025, its page on
  http://127.0.0.1:8025) that keeps what it gets and has no relay, so the
  instance cannot send real mail. The rule for development and tests
  stays: nothing leaves loopback or the Docker network. `compose.yaml` has a second rqlite
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
  - A text attachment had its line ends made CRLF like a text body, so a
    file uploaded with LF came back changed (the webmail's end-to-end
    test). A part made from a blob is now written as base64 when its
    line ends are not all CRLF (`BuildPart.Exact`).
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
  - No DANE, no REQUIRETLS, no DSN extension (RFC 3461: no
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

Built as follows (the webmail in `web/`):

- **Where things are.** `src/compose` is the logic without requests or
  DOM: addresses, `mailto:`, HTML to text and back as strings, subjects,
  threading headers, reply recipients, quoting, the `Email/set` create
  object. `src/store/compose.ts` holds the open composers, their drafts
  on the server, uploads, send and undo, identities and submissions.
  Screens: `ComposerDock`, `ComposerPanel`, `AddressInput`, `RichEditor`,
  `pages/SettingsPage.vue`. No new dependency.
- **Composer.** A panel at the bottom right over the mail view; several
  can be open or minimised, as many next to each other as fit, and the
  ones opened longest ago are minimised when one more opens. Under 640
  pixels an open composer covers the screen. From shows when there is
  more than one identity. To, Cc and Bcc are chips; typed or pasted text
  is read as a list (commas, semicolons, line ends, `Name <address>`,
  quoted names); what is not an address stays in the field, marked, and
  stops a send. A message without a subject asks once.
- **Body.** Plain text is the default; the choice of the last composer
  is kept in `localStorage`. Rich text is an editor of our own on
  `contenteditable`, not a dependency: bold, italic, lists, quote, link
  and inline images are what `document.execCommand` does in every
  browser, the editor around it is about 200 lines, and a library would
  be several packages and more script than the rest of the composer for
  the same six buttons. What matters for safety is the same either way
  and is ours: HTML crosses into and out of the page only through
  `compose/richdom.ts`, which parses it in a document of its own and
  makes the allowed elements anew (`p`, `div`, `br`, `b`, `i`, `u`, lists,
  `blockquote`, `pre`, `a` with an `http`, `https` or `mailto` address,
  `img` only for a `cid:` that is an attachment of the draft). Pasted and
  dropped content comes in as plain text. A rich message is sent as
  `text/plain` (a rendering of the HTML: quotes with `> `, list marks,
  link addresses) plus `text/html`.
- **Attachments.** File picker, drag and drop, paste. Uploads go through
  `XMLHttpRequest`, the only way to see progress; each can be cancelled
  or tried again; a file over the session's `maxSizeUpload` is refused
  before any byte is sent. A pasted image in rich text is inline: shown
  from the download URL, sent as `cid:`. Every attachment gets a
  Content-ID, also the ordinary ones: after each save the composer reads
  the new Email's attachments back and follows them by that id to their
  new blob ids (`<blob of the draft>-<part>`), because what they pointed
  at goes away (the replaced draft at once, an upload after a day).
- **Drafts.** A change is saved 1.5 seconds after the last one, as an
  `Email/set` with a create in Drafts (`$draft`, `$seen`) and the destroy
  of the draft before. A save that fails keeps the composer and says so;
  it is tried again after 10 seconds, or after the next login when the
  answer was 401 (the composers are in memory and outlive the session;
  another user's login drops them). When the create is refused but the
  destroy went through, the composer knows it has no draft on the
  server. Closing saves first and stays open if that fails; a composer
  nobody typed in leaves nothing. The tab asks before closing with
  something unsaved and saves when it is hidden. A draft opens from the
  Drafts mailbox (a row that is one draft opens the composer, not the
  thread) and from a thread. A saved draft says what it answers in
  `X-Robinmail-Draft: reply|forward <email id>`, so a draft opened later
  still marks the original; the message that is sent does not have it.
- **Reply and forward.** `In-Reply-To` and `References` by RFC 5322
  3.6.4. One `Re:` or `Fwd:`, whatever was there (`RE: Re[2]:`). A reply
  goes to `Reply-To` or else `From`; reply all adds the other recipients
  as Cc; our own addresses (every identity) are left out unless nobody
  else is left; a reply to our own message goes to its recipients. The
  identity is the one the message was sent to. The original is quoted
  from its text body; for mail that only has HTML from a text rendering
  of it, made on strings. In a rich reply the quote is escaped text in a
  `blockquote`. Forward carries the attachments by the blob ids of the
  original's parts; "forward as attachment" attaches the whole message
  as `message/rfc822`.
- **Send.** One request: `Email/set` create (and destroy of the saved
  draft), `EmailSubmission/set` with `onSuccessUpdateEmail` (Drafts to
  Sent, `$draft` off), and for a reply or forward a third call that sets
  `$answered` or `$forwarded` on the original. `onSuccessUpdateEmail`
  can only change the message that was sent (RFC 8621 7.5), and the
  third call must not run when the send failed, so it takes its
  `destroy` argument from `/created/send/dsnBlobIds` of the submission:
  an empty list after a send, a reference that does not resolve
  otherwise, which makes the server refuse the call. A refused send
  leaves the message as the draft and shows why (`forbiddenFrom`,
  `tooManyRecipients`, `invalidRecipients`, `forbiddenToSend` with the
  server's reason, `tooLarge`, `noRecipients`). When no answer comes the
  composer says it is not known whether the message went.
- **Undo.** After a send a notice offers "Undo" until `sendAt`. It sets
  `undoStatus` to `canceled`, moves the message back to Drafts with
  `$draft`, takes the keyword off the original if the send put it there,
  and opens the composer again. Too late is said as such.
- **Delivery status.** A message in Sent shows a line per recipient:
  sending (still in the undo time), queued with the last reply, sent
  (`unknown` in JMAP: another server took it), delivered (a mailbox on
  this server), failed with the SMTP reply. The submissions of a thread
  are asked for once (`EmailSubmission/query` by email id) and then
  follow `EmailSubmission/changes`, every 3 seconds for two minutes
  after a send and with every refresh after that.
- **Identities** (`/settings`): the account's address, and `+tag`
  addresses with name, reply-to and a text signature, through
  `Identity/set`. The signature goes under a new message after `-- `
  and above a quote, and is swapped when From changes, if it is still as
  it was put. An identity's `bcc` is filled in; `htmlSignature` is not
  used.
- **Keys and links.** `c` new message, `r` reply, `a` reply all, `f`
  forward, Ctrl or Cmd with Enter sends. Any `mailto:` link in the app
  opens the composer with its addresses, subject and body (RFC 6068);
  mail addresses and web addresses in a text body are links.
- **JMAP client.** `using` has `urn:ietf:params:jmap:submission`. A
  `/set` that created something no longer moves the held state on: what
  it made reaches the store through `/changes`.
- **Tests.** Vitest (109 new, 171 in all): address parsing, `mailto:`,
  HTML to text, subjects, threading headers, reply recipients, quoting,
  the create object, and the store against the in-memory server (the
  save sequence, a failed save, a 401, uploads, the send request with
  its result reference, a refused send, undo, delivery status,
  identities). Playwright (`e2e/compose.spec.ts`, 8 tests) against the
  real binary, between `dev@` and `other@robinmail.test` of `seed.sh`:
  send with an attachment and see it arrive byte for byte, reply and see
  the thread with both messages and `$answered`, a draft over a reload,
  undo and nothing arrives, a message to an unknown local user with
  `550 5.1.1` as failed status and the bounce in the thread, rich text
  with a pasted image arriving as HTML with the image inline, reply all,
  forward both ways, `mailto:`, an identity with its signature, an
  expired session with a composer open, phone width. One domain is
  enough for all of it; two servers stay with `TestSendBetweenTwoServers`
  and `send-demo.sh`. A ninth test, skipped when Mailpit does not answer
  or the instance does not route to it, sends a rich message with an
  inline image, a text file, a hidden recipient and a subject and name
  outside ASCII to two outside domains, and reads it back through
  Mailpit's HTTP API: someone else's parser finds the same headers, both
  bodies, the image by its Content-ID and the file byte for byte; the
  bytes on the wire are 7-bit, CRLF, at most 998 a line, shaped
  mixed[alternative[text, related[html, image]], file]; `Bcc` is in the
  envelope and in no header; our `Received` line names no client. `e2e/compose-shots.spec.ts` takes the screenshots.
- **The tests found and fixed**: the text attachment that came back with
  other line ends (Go, above); the notice with "Undo" never showed,
  because the composer that should announce the send was gone by then;
  a stored single-part text gains a line end, which a reopened draft now
  drops; the editor wrote a list inside a paragraph (`<p><ul>`), as the
  browser builds it, which HTML does not allow (the Mailpit test).
- **Not built.** No address book or completion of addresses. No
  formatting beyond the six buttons (no fonts, colours, tables). An
  HTML signature. Scheduling a send. A read receipt. A draft is not
  kept in the browser: text typed in the last 1.5 seconds before a
  crash is lost (the tab asks before a normal close). On a phone there
  is no button for a new message while a thread is open. A `mailto:`
  link inside an HTML message is in the sandboxed frame and opens the
  system's mail program, not the composer. Only Chromium was run.

### Phase 5c: interop lab — done

robinmail had only ever exchanged mail with itself, Go test doubles, Rspamd
and Mailpit. There is no public host yet, so real providers stay out of
reach; this phase puts three other mail servers next to it instead.

- A lab in Docker: robinmail, Postfix, Exim and OpenSMTPD, each with its
  own domain, real MX records, STARTTLS with a private CA, MTA-STS.
- A harness that sends both ways and reads the results back.
- Every fault it finds in robinmail fixed, with a test that needs no
  Docker. Fastmail's JMAP-TestSuite run against the same robinmail.
- Exit: `make lab-test` passes.

Built as follows:

- **The lab** (`contrib/lab`, `lab.sh up|down|versions|suite`). One
  internal Docker network (10.77.25.0/24) that has no route out. CoreDNS
  1.15.0 is mounted as the only resolver of every container: it serves
  `.test` (MX, A, TXT for SPF and MTA-STS, PTR) from `dns/test.zone` and
  answers NXDOMAIN to everything else, so no name outside the lab
  resolves and no mail can leave. `ca.sh` makes a CA and a certificate
  per host (`secrets/`, not in git); robinmail trusts only that CA
  (`SSL_CERT_FILE`), the peers have it in their system store. nginx 1.30.6
  serves `https://mta-sts.<domain>/` and is the gate: it is also on a
  second network and passes a few ports on 127.0.0.1 into the lab for the
  harness. rqlite 10.7.1.
- **The peers**, from Debian 13 packages (`debian:trixie-20261104-slim`):
  Postfix 3.10.13 with postgrey 1.37, Exim 4.98.2, OpenSMTPD 7.6.0p1.
  Each has `<peer>.test` with the local users alice and bob (Maildir),
  `late.<peer>.test` that takes any recipient and bounces afterwards, and
  is set to deliver to robin.test only over TLS with a certificate the
  lab CA signed. Two more Postfix containers: `badtls.test` (self-signed
  certificate) and `notls.test` (no STARTTLS), each also under a second
  name with an MTA-STS policy in mode `enforce`. Policies: postfix.test
  and exim.test `enforce`, opensmtpd.test `testing`.
- **Two development settings** in robinmail's config, section `dev`, for
  labs only: `retry_seconds` replaces the retry schedule by a fixed wait
  (the queue's clock was only reachable from Go tests), `smtp_trace` logs
  every SMTP command and reply of the listeners and of the outbound
  client, without message content and without credentials, and marks a
  command that arrived together with the next one (a pipelining client).
- **The harness** (`internal/labtest`, `make lab-test`, skipped without
  `ROBINMAIL_TEST_LAB`). Peers send through their own `sendmail`; their
  Maildirs are read with `docker compose exec`. robinmail is driven over
  JMAP through the gate (`Email/set` as the webmail does it, or
  `Email/import` of a raw message, then `EmailSubmission/set`), with TLS
  checked against the lab CA. Raw SMTP for what no server sends. The
  trace tells what each peer put on the wire. Attachments are unpacked on
  the peer by ripmime 1.4. Submission is done by swaks. It prints the
  matrix at the end; the last run is in `contrib/lab/lab-test-output.txt`
  (4.5 minutes).
- **The matrix.** All cells pass except where it says otherwise.

  | Case | Postfix | Exim | OpenSMTPD |
  |---|---|---|---|
  | to robinmail: ASCII; 8-bit body in UTF-8 and Latin-1; encoded words; raw UTF-8 headers | pass | pass | pass |
  | to robinmail: SMTPUTF8 sender address | pass | pass | n/a: no SMTPUTF8 |
  | to robinmail: just under our size limit; over it (refused, the peer bounces) | pass | pass | pass |
  | to robinmail: three recipients, one unknown (two delivered, the peer's bounce parsed) | pass | pass | pass |
  | to robinmail: a line of 998; lines starting with a dot | pass | pass | pass |
  | to robinmail: a line of 3000 | peer folds it | peer bounces it | peer re-encodes it |
  | to robinmail: STARTTLS, our certificate verified by the peer | pass | pass | pass |
  | from robinmail: ASCII; 8-bit (`BODY=8BITMIME`); UTF-8 from `Email/set`; attachments | pass | pass | pass |
  | from robinmail: raw UTF-8 headers; SMTPUTF8 recipient | pass | pass | pass: bounced with 5.6.7 |
  | from robinmail: just under the peer's SIZE; over it (not offered, 5.3.4) | pass | pass | under only: one limit for listener and local users |
  | from robinmail: three recipients, one unknown (550, bounce, `deliveryStatus`) | pass | pass | pass |
  | from robinmail: a line of 998; of 3000 (kept as it is); dot lines | pass | pass | pass |
  | from robinmail: STARTTLS, peer's certificate verified (MTA-STS) | pass | pass | pass |
  | peer accepts, then bounces: its DSN sets `deliveryStatus` | pass | pass | pass |
  | peer down: queued, delivered when it is back | pass | pass | pass |
  | greylisting (postgrey): 450, delivered on the third try | pass | | |
  | sender domain with an underscore, to and from | pass | peer refuses its own sender | pass |

  Without a column: a self-signed certificate delivers without a policy
  and does not under `enforce` (4.7.0); no STARTTLS delivers in the clear
  without a policy and does not under `enforce`; a null reverse-path and
  source routes by hand; swaks on 587 and 465 with AUTH PLAIN and LOGIN, a
  wrong password (535), and 587 without STARTTLS (no AUTH offered, 530).
- **What the peers do**, as the trace shows it.

  | | Postfix | Exim | OpenSMTPD |
  |---|---|---|---|
  | pipelines MAIL, RCPT, DATA | yes | yes | no |
  | declares `SIZE=` | yes | yes | no |
  | declares `BODY=8BITMIME` for 8-bit content | yes | no | no |
  | asks `SMTPUTF8` for UTF-8 headers | yes | no (only for an address) | no |
  | offers a message over our `SIZE` | no, bounces at once | yes, takes the 552 at MAIL | no |
  | its bounce is a `multipart/report` | yes | yes | no: `multipart/mixed` |
  | status in its bounce for our `550 5.1.1` | 5.1.1 | 5.0.0 | 5.0.0, no Diagnostic-Code |
  | accepts `MAIL FROM` with an underscore in the domain | 250 | 501 | 250 |
  | accepts a source route | 250 | 250 | 250 |

  All three keep our `Return-Path` once, put their `Received` above ours
  and say there which TLS they negotiated.
- **Fixed in robinmail**, each with a test in the normal Go suite (the
  transactions and bounces are replayed from what the lab recorded:
  `internal/smtp/interop*_test.go`, `internal/message/testdata/dsn-*.eml`):
  - A sender whose domain has an underscore got 501. Postfix and
    OpenSMTPD send from and accept such senders; Exim is as strict as RFC
    5321 4.1.2. Accepted now in the reverse-path and in a recipient we
    send to, so such mail can be answered; RCPT TO for our own domains and
    source routes stay strict. This settles the open question of phase 2:
    tolerant, because the name is only ever looked up and Postfix-run
    sites accept it.
  - The server closed the socket under TLS without `close_notify`. Exim
    logged every delivery as a TLS error.
  - OpenSMTPD's bounce was not read as a DSN, so a message that bounced
    there stayed "sent". A multipart of another kind with a
    `message/delivery-status` part directly in it is read now.
  - `smtpReply` and the Diagnostic-Code of our own bounces had no reply
    code for reasons robinmail makes up itself ("connecting to … failed"),
    and were empty or bare text for reports without a usable
    Diagnostic-Code (Exim, OpenSMTPD, Postfix's `X-Postfix`). Always a
    reply now: `451 4.4.1 connecting to …`, `550 5.0.0 reported by
    mx.exim.test without a reason`.
- **Not changed, kept with a test that says so**: 8-bit content without
  `BODY=8BITMIME` and UTF-8 headers without `SMTPUTF8` are accepted (Exim
  and OpenSMTPD send nothing else; RFC 6152 and 6531 want the
  declaration, a server cannot check it before DATA). A message over the
  limit that was sent anyway is read to its end and answered 552.
- **Fastmail's JMAP-TestSuite** (github.com/fastmail/JMAP-TestSuite at
  2fa1934, Perl) runs in a container of its own inside the lab: `lab.sh
  suite`, 73 seconds. The adapter
  (`contrib/lab/jmapsuite/Robinmail.pm`) makes a new user for every test
  with `robinmail user add`. First run: 24 of 173 files failed. Now: 110
  pass, 49 skip themselves for capabilities robinmail does not offer
  (calendars, contacts, blob, quota, MDN, principals, vacation response),
  4 skip for want of two accounts that share (the `/copy` tests), 10 fail
  (`contrib/lab/jmapsuite/suite-output.txt`).
  - Fixed in `internal/jmap` and `internal/message`
    (`internal/jmap/testsuite_test.go`): `subParts` on parts that are not
    multiparts only when asked for, and then also in `textBody`,
    `htmlBody`, `attachments`; `maxBodyValueBytes: null`; a part from
    `bodyValues` may have any type; `size` next to a `blobId` is ignored;
    a header on the Email and on its `bodyStructure` is refused; a text
    body without a final line end comes back without one (written as
    quoted-printable with a soft line break at the end); a Raw header
    value gets its leading space; `Email/import` answers
    `invalidProperties` for a blob that is not there and names every
    missing property; `Mailbox/set` takes server-set properties that have
    their current value (RFC 8620 5.3) and names each right of `myRights`
    it refuses.
  - Fail because robinmail does not have it: the `text` filter of
    `Email/query` and `Email/queryChanges` (2 files; phase 8),
    `SearchSnippet/get` (1; phase 8), `updatedProperties` of
    `Mailbox/changes` (1; the RFC allows null).
  - Fail because the suite asks for more than the RFC, as we read it:
    `isSubscribed` false on a mailbox the user made (2 files; RFC 8621 2
    says it SHOULD default to true there), mailbox names sorted by octet
    where we ignore case (1; the default collation is the server's), the
    `position` 0 in the answer to a query past the end (1; we return the
    position asked for), the order of names in `invalidProperties` (1),
    a display name written as a quoted string where we write atoms (1).
- **Not covered.** Exchange, Gmail, Outlook.com, Yahoo and every other
  real provider are still untested: Exchange does not run in a container,
  the others need a public host. So are: IPv6 (the lab is IPv4), DKIM,
  SPF and DMARC verification by a receiver (no Rspamd in the lab; mail
  goes out unsigned), DANE, the DSN extension, CHUNKING (Exim would use
  it if we offered it), a peer that drops the connection mid-transaction,
  IDN domains, rate limits. msmtp was not used; swaks was.

### Phase 6: push and IMAP — done

- JMAP push over EventSource. A node polls `modseq` for the accounts that have
  a connection open on it.
- IMAP4rev1 on 993 with `IDLE`, `UIDPLUS`, `MOVE`, `SPECIAL-USE`, `CONDSTORE`.
- Exit: Thunderbird and iOS Mail sync, move, flag and see new mail arrive.

Built as follows:

- **Change notifications** (`internal/notify`). Every node has a hub that
  JMAP event streams and IMAP sessions subscribe to, per account. It hears
  of a change in two ways. A write through the node's own store calls the
  hub before it returns (`store.OnChange`, set for every batch that bumps
  the modseq). A write on another node is found by polling: while an
  account has listeners, the hub reads its `modseq` once a second, all
  such accounts in one query at level `none`, and wakes the listeners of
  accounts whose modseq moved. A wake-up carries no data; the listener
  reads the state. No node tells another about a write: the writer cannot
  know which nodes listen for the account, so every write would call
  every node, and the poll would still be needed for calls that get lost.
  A node without listeners asks nothing. Measured with two nodes on one
  rqlite: a listener on the other node is woken 6 to 50 ms after the
  write with a 50 ms poll, and within a second with the default.
- **JMAP push** (RFC 8620 7.3): `GET /jmap/eventsource/?types=&closeafter=&ping=`
  answers `text/event-stream` with `state` events that hold a
  StateChange. Every type's state string is the account modseq, so the
  handler reports the types whose objects changed since the state it sent
  last, from the change log: `Mailbox`, `Thread`, `Email`,
  `EmailDelivery` (only when an Email was created), `EmailSubmission`,
  `Identity`. The event id is the state, so a client that reconnects with
  `Last-Event-ID` is told at once what it missed. `ping` is kept between
  5 seconds and an hour; 16 streams per account per node; streams end
  when the server stops. `PushSubscription` (RFC 8620 7.2) is not
  offered: it needs an outbound HTTPS client with verification, expiry
  and retries, and message encryption (RFC 8291).
- **API requests wait for a slot.** With push, every tab and client of an
  account asks for the changes in the same moment, and the fifth request
  at once was refused with the `limit` error (the browser test lost an
  undo to that). A request over `maxConcurrentRequests` now waits up to 5
  seconds for a slot, 32 at most per account, and is refused only then.
- **Webmail.** `web/src/jmap/push.ts` keeps one EventSource (`types=*`,
  `ping=30`). A `state` event, and every time the connection comes up,
  makes the page fetch changes, also while the tab is hidden. An error
  closes the connection and a new one is made after 1, 2, 4 ... 60
  seconds (75 to 100% of each), from 1 second again once one opened; 75
  seconds without an event or ping counts as an error. While push is not
  connected the 30 second timer asks as before. The JMAP client keeps at
  most `maxConcurrentRequests` requests in flight and queues the rest;
  the mail page runs one round of refresh requests at a time.
- **IMAP** (`internal/imap`, protocol only; `cmd/robinmail/imap.go` puts
  the store behind its `Backend` and `User` interfaces). Config section
  `imap {listen_tls, listen, max_connections, max_connections_per_ip,
  max_connections_per_user}`: 993 with TLS from the first byte, and
  optionally 143, which announces `LOGINDISABLED` and refuses `LOGIN` and
  `AUTHENTICATE` until `STARTTLS` (a command sent in the same packet as
  `STARTTLS` ends the connection). Logins go through
  `web.Server.Login`, so IMAP shares the failure counters with the web
  login and SMTP AUTH on all nodes; three failed logins end a
  connection.
  - Capabilities before login: `IMAP4rev1 IMAP4rev2 LITERAL+ SASL-IR ID
    ENABLE`, plus `AUTH=PLAIN` under TLS or `STARTTLS LOGINDISABLED`
    without. After login also `IDLE NAMESPACE UNSELECT UIDPLUS MOVE
    SPECIAL-USE CHILDREN LIST-EXTENDED LIST-STATUS CONDSTORE ESEARCH
    SEARCHRES BINARY STATUS=SIZE UTF8=ACCEPT`.
  - Commands: `CAPABILITY NOOP LOGOUT ID STARTTLS LOGIN AUTHENTICATE
    ENABLE SELECT EXAMINE CREATE DELETE RENAME SUBSCRIBE UNSUBSCRIBE LIST
    LSUB NAMESPACE STATUS APPEND IDLE CHECK CLOSE UNSELECT EXPUNGE SEARCH
    FETCH STORE COPY MOVE` and `UID FETCH|STORE|SEARCH|COPY|MOVE|EXPUNGE`.
  - IMAP4rev2 (RFC 9051) after `ENABLE IMAP4rev2`: mailbox names in UTF-8,
    `SEARCH` answers with `ESEARCH`, no `RECENT` and `UNSEEN` in `SELECT`
    but a `LIST` line, `[CLOSED]` when another mailbox is selected,
    `STATUS` with `SIZE` and `DELETED`. The rev1 commands keep working.
  - `QRESYNC` is not offered. It needs to say which UIDs left a mailbox
    since a modseq (`VANISHED`); the change log knows which Emails
    changed, not which UID each had in which mailbox, so it would take a
    table of expunged UIDs written by every removal.
- **How the store's model shows in IMAP.**
  - A mailbox is a label. An Email with three labels is a message in
    three folders, with a UID of its own in each. Its flags are the same
    in all of them, because keywords belong to the Email.
  - Mailbox names are paths from `parent_id` with `/` between the levels;
    the mailbox with the role `inbox` is `INBOX`. On the wire they are
    modified UTF-7, or UTF-8 after `ENABLE IMAP4rev2` or `UTF8=ACCEPT`.
    Roles are the special-use attributes (`\Drafts \Sent \Junk \Trash`).
    A mailbox whose own name contains `/`, and one at the top called
    "inbox" that is not the inbox, cannot be named and are not shown.
  - Flags: `\Seen \Flagged \Answered \Draft` are `$seen $flagged
    $answered $draft`; other keywords are flags as they are. `\Deleted`
    is the keyword `$deleted`, which JMAP has no meaning for and shows
    like any keyword. There is no `\Recent`: `RECENT` is always 0.
  - `COPY` adds the target's label: the same Email, the target's next
    UID. A message that is in the target already stays as it is there and
    `COPYUID` names the UID it has (Gmail does the same). `MOVE` adds the
    target and takes the source away, so the Email keeps its id and JMAP
    sees an update; a `MOVE` into the mailbox the message is in gives it
    a new UID.
  - `EXPUNGE` (and `CLOSE`, `UID EXPUNGE`) takes the messages with
    `$deleted` out of the mailbox. An Email that has another label only
    loses this one, and the `$deleted` keyword with it, so the copies
    elsewhere are not swept up by the next expunge there. An Email
    without another label is destroyed. Nothing is moved to Trash: mail
    clients do that themselves before they expunge, and expunge in Trash
    has to mean gone.
  - `DELETE` of a mailbox takes its messages out of it by the same rule.
    A mailbox with children is refused; one with a role too.
  - `RENAME` keeps the mailbox, its UIDs and its UIDVALIDITY; renaming
    `INBOX` moves its messages to a new mailbox, as RFC 3501 6.3.5 says.
  - `HIGHESTMODSEQ` is the account modseq and a message's `MODSEQ` its
    Email's: a mailbox's value also moves when something else in the
    account changes, which costs a client an empty `CHANGEDSINCE` fetch.
  - `CREATE` over IMAP makes an unsubscribed mailbox (a client subscribes
    itself); over JMAP the default stays subscribed.
- **A session's view.** A session that selected a mailbox holds the list
  of its messages (UID, Email id, flags, modseq, size, date); sequence
  numbers are positions in it. It changes only when the client is told:
  after a command, and during `IDLE`, the session reads the mailbox again
  if the hub said something changed (all UIDs, and the messages with a
  higher modseq, in one query) and sends `FETCH` for changed flags,
  `EXPUNGE` for UIDs that went, `EXISTS` for new ones. `EXPUNGE` is held
  back while answering `FETCH`, `STORE` and `SEARCH` (RFC 3501 7.4.1); a
  message that went meanwhile keeps its number until then.
- **Store.** `ViewMailbox`, `SetFlags` (add, remove, replace, with
  `UNCHANGEDSINCE`), `CopyMessages` and `Expunge` work on UID ranges and
  are one batch each. A batch stamps the emails it is about with the new
  modseq and names the set by that stamp in its later statements; one
  with nothing to do fails on a guard and bumps nothing. Schema version
  6: `account.uidvalidity`, the last UIDVALIDITY the account gave out. A
  new mailbox gets the later of that plus one and the clock.
- **What it costs.** `FETCH` of flags, UID, size, date and modseq is
  answered from the session. `ENVELOPE`, `BODYSTRUCTURE` and any body
  section read and decrypt the whole message, also for one header field:
  a client's first sync of a mailbox reads every message once.
  `SEARCH` by flags, UID, size, internal date and modseq costs nothing;
  `FROM TO CC BCC SUBJECT` and the `SENT` dates are answered from the
  cached header columns in one query (a message is opened only when a
  field was cut at 250 characters, has comments in it, or its `Date` is
  within a day of the date asked for); `HEADER`, `BODY` and `TEXT` open
  every message the cheaper keys left. There is no index until phase 8.
- **Limits.** 30 minutes without a command, also during `IDLE`, ends a
  session with `BYE` (one minute before login). 1000 connections, 20 per
  address, 20 per user by default. A command line may have 64 KiB, a
  literal that is not a message 64 KiB, a message the SMTP limit; a
  non-synchronizing literal over the limit ends the connection. 20
  commands in a row that are not IMAP end it too.
- **Tests.**
  - `internal/imap` against an in-memory backend with the store's label
    rules: the sample session of RFC 3501 section 8 line for line, the
    examples of 6.1 to 6.4.8 and 7.4.2 (every search key, the part
    numbering example as a real message, the `BODYSTRUCTURE` examples),
    and each extension from its RFC: 2177, 4315, 6851, 6154, 2342, 7888,
    2971, 5161, 7162, 3691, 5258, 5819, 4959 with 4616, 4731, 5182,
    3516, 6855, modified UTF-7 (3501 5.1.3) and IMAP4rev2 after `ENABLE`
    (9051). Autologout, connection limits, shutdown, a selected mailbox
    that is deleted. Fuzz tests: a logged-in session fed arbitrary
    commands, the MIME tree, modified UTF-7, the `LIST` pattern matcher
    against its definition.
  - `TestIMAPAndJMAPOnTwoNodes` (`cmd/robinmail`): two `serve` on one
    rqlite. Mail over SMTP on node B is `EXISTS` in an `IDLE` on node A
    (0.7 s) and a StateChange on A's event source; a flag over JMAP is an
    untagged `FETCH` (1 s); a `MOVE` in IMAP is `EXPUNGE` for the other
    session and an updated Email in `Email/changes`; `COPY` is one Email
    in two mailboxes; `EXPUNGE` drops a label, then destroys the Email;
    ten failed IMAP logins close the web login on both nodes.
  - `internal/notify`, `internal/store` (two stores on one rqlite, the
    batches above), `internal/jmap` (RFC 8620 7.1 and 7.3), Vitest for the
    webmail's push, Playwright (`e2e/push.spec.ts`): mail delivered by the
    command line shows within seconds without a reload, a lost connection
    is made again with growing pauses, and with push blocked the timer
    finds the mail.
- **The lab** (`make lab-test`). A container `imapclients` with clients
  that are not ours, on the lab network, checking robinmail's certificate
  against the lab CA.
  - Dovecot's ImapTest (the project's binary for Debian 13 from its
    `latest` release, built from commit bc23019, checksum pinned; its
    source does not build against Debian's Dovecot 2.4.1). Scripted
    tests: 72 groups, 36 pass, 30 skip themselves for capabilities
    robinmail does not offer (`SORT`, `THREAD`, `CATENATE`, `MULTIAPPEND`,
    `NOTIFY`, `URLAUTH`, `CONTEXT`, `PARTIAL`, `WITHIN`), 6 fail: 13 of
    597 commands. The first run failed 14 groups. Stress: 20 clients for
    30 seconds, about 11,000 commands, no complaint except the one
    explained below.
  - Fixed after ImapTest: UIDVALIDITY was the second of creation, so a
    mailbox deleted and made again within a second got the same one; a
    UID of 4294967295 was read as `*`; the `SENT` search keys went by
    the UTC date of the `Date` field instead of the date written there;
    `MOVE` into the same mailbox did nothing; `CREATE` made subscribed
    mailboxes; a name that is not modified UTF-7 was `BAD` instead of
    `NO`; `HIGHESTMODSEQ` was not sent when a command enabled CONDSTORE
    in a selected mailbox; `ESEARCH` with `MIN` or `MAX` reported the
    highest modseq of all matches; an empty `Sender` or `Reply-To` was
    not replaced by `From` in `ENVELOPE`; `CHILDINFO` was given for
    children the patterns listed themselves; an address written with
    comments inside it was not found by `FROM`.
  - Left failing, with the reason in `internal/labtest/imap_test.go`:
    `\Recent` (10 commands in `select`, `copy`, `search-flags`: there is
    no such flag here); a deleted mailbox that was subscribed does not
    stay listed as `\NonExistent` (a subscription is a property of the
    mailbox); the source route of `<@route:user@domain>` is not in the
    `ENVELOPE`; a message appended with `Content-Transfer-Encoding:
    binary` and NUL octets is returned as it is.
  - In the stress run about 20 of 430 searches by header or body text
    miss a message that another client expunged a moment before. The
    session still holds its number, UID, flags, size and date and finds
    it by those; its text went with the Email. ImapTest cannot know and
    counts it; the test checks that every such search had a text key.
  - mbsync (isync 1.5.1) and OfflineIMAP 8.0.0 each keep a Maildir in
    step with an account: a first sync of 12 messages in 4 mailboxes
    (one nested, with flags, UTF-8, attachments, long bodies), then a
    flag, a destroy, a new message, a move and a new mailbox over JMAP
    and a seen mark, a delete and a new message in the Maildir, a second
    sync, and a third that must change nothing. After each, every
    mailbox is compared over JMAP by SHA-256 of each message and its
    flags. Both pass.
  - Dovecot as a server was not needed: no behaviour was in dispute.
- **Thunderbird and iOS Mail.** Thunderbird is driven in the lab since
  phase 7b: sync, new mail by IDLE, flags, move and copy, delete and
  empty Trash, folders, send, reply, drafts, both ways against JMAP.
  iOS Mail does not run without a person and a device. To try by hand on
  the VPS, with a certificate the device trusts (ACME) and
  `imap.listen_tls: ":993"`, `submission.listen_tls: ":465"`:
  1. iOS Mail, all of it: Mail, Add Account, Other, IMAP, `host:993`
     with SSL, user name the full address, SMTP `host:465`. Expect
     Inbox, Drafts, Sent, Junk and Trash recognised as such; counts and
     unread counts equal to the webmail; a message with attachments and
     one with a non-ASCII subject open correctly; flag, read, move,
     delete and send show in the webmail and the reverse. Note whether
     new mail comes by IDLE or on its fetch schedule, and how long it
     takes.
  2. Thunderbird's account dialog: manual config, IMAP `host:993`
     SSL/TLS, "Normal password", SMTP `host:465` SSL/TLS. The lab starts
     from a finished profile. There is no autoconfig yet (phase 8), so
     the dialog asks for the server.
  3. Thunderbird's setting "When I delete a message: Just mark it as
     deleted": the `$deleted` keyword shows in JMAP and the webmail does
     not hide such mail. The lab uses the default, move to Trash.
  4. Subscribe and unsubscribe in Thunderbird's dialog (it only shows
     subscribed folders: one made in the webmail is subscribed, one made
     over IMAP by another client is not until that client subscribes).
  5. Search in the client, in subjects and in bodies, and the time on a
     large mailbox (body search reads every message).
  6. Leave both clients connected for a day: no repeated logins in the
     log (the 30 minute timer against the client's keep-alive), no
     duplicate messages, and the login failure counter not tripped by a
     client that retries a stale password.
- **Not handled.**
  - No `\Recent`, `QRESYNC`, `SORT`, `THREAD`, `MULTIAPPEND`, `CATENATE`,
    `NOTIFY`, `COMPRESS`, quotas or ACLs. `BINARY` is `FETCH` only.
  - Subscriptions to mailboxes that do not exist, and names left behind
    as `\Noselect` when a mailbox with children is deleted.
  - A message with two labels counts twice in a client's totals, and a
    client that deletes it in one folder leaves the other copy.
  - A session cannot read the text of a message another session expunged
    before this one was told (RFC 2180 4.1.2 allows the empty answer).
  - A deleted mailbox ends the connections that have it selected.
  - IMAP logs in with the account password until phase 7 has app
    passwords. A wrong password stored in a mail client runs into the
    shared limit of ten failures per address in 15 minutes, which also
    closes the web login for that time.
  - The limits on connections are per node.

### Phase 7: auth — done

- App passwords: one per device, for IMAP and SMTP submission, revocable.
  The account password then only opens the webmail.
- TOTP (RFC 6238) for web login, with recovery codes.
- Optional OIDC login for the webmail against any provider: authorization
  code flow with PKCE, ID token checked with our own JWT and JWKS code.
- Exit: Thunderbird logs in with an app password and not with the account
  password; web login asks for TOTP; a Keycloak container logs a user in
  through OIDC.

Built as follows:

- **The flaky shared login limit was a test, not the server.**
  `TestIMAPAndJMAPOnTwoNodes` now and then timed out "waiting for node B
  to refuse the login too". `TestLoginFailures` in `internal/store` runs
  with a clock in 2027 and ended with `TrimLoginFailures` at that clock,
  which deletes every counter that started before 2027: all live ones of
  the database the test packages share. When that fell between the ten
  wrong passwords on node A and the login on node B, node B found no
  counter and let the login in, while node A still refused from its
  memory. Reproduced by looping the store test next to the two-node
  test. The server never trims a live window (`serve` trims at now minus
  the window) and nothing else deletes a counter but a success. The
  store test now trims at a time long past. The wrong trim had also
  hidden that the two-node test leaves ten failures for 127.0.0.1 per
  run, so the sixth run within fifteen minutes met the limit per IP; the
  test harness now forgets the loopback counter when it starts a server.
  The two-node test no longer waits for node B: the count is written
  before node A answers the tenth failure.
- **Which secret opens which door.**

  | | web login | IMAP, SMTP submission, JMAP with Basic auth |
  |---|---|---|
  | account password | yes | only while the account has no app password and no TOTP, and `auth.require_app_passwords` is off |
  | app password | no | yes, within its scope |
  | TOTP or recovery code | second step | no |
  | OIDC provider | yes | no |

  The account password stops working for mail clients with the first app
  password, so that it is typed into one place only, and with TOTP,
  because a second factor that IMAP walks around is none. When the last
  app password is revoked (and there is no TOTP) it works there again.
- **App passwords** (`internal/users/secrets.go`, `app_password`). The
  server makes them: 160 random bits, shown once as `abcd-efgh-...` (32
  base32 characters in groups of four; dashes, spaces and capitals do not
  matter). Stored as SHA-256, not Argon2id. Argon2id makes each guess at
  a password a person chose expensive, because such a password is one of
  perhaps a billion likely ones; against 2^160 equally likely values no
  price per guess matters. And the cost would be ours: a mail client
  logs in on several connections every few minutes, and Argon2id as
  configured (64 MiB, about 0.1 s of four cores) on each of them would
  be the largest cost of serving mail and an easy way to exhaust a node.
  A SHA-256 and one indexed read cost microseconds. Each has a name, a
  creation time, an optional scope (`imap`, `smtp`, `jmap`) and the time
  and address of its last use, which is written at most once in ten
  minutes per address (and logged as an auth event with it). At most 50
  per account. Making one asks for the account password again.
- **One door for mail clients** (`web.Server.MailLogin`), for IMAP, SMTP
  AUTH and Basic auth. A secret that has the form of an app password is
  only ever looked up as one, without Argon2id; any other is only ever
  checked as the account password, with Argon2id, also when the account
  does not exist or may not use its password there. So how long a
  refusal takes depends on what was sent, not on whether the account
  exists, has app passwords or has TOTP, and the answer is the same
  "wrong name or password" in each case. (A password that happens to
  have the form of an app password cannot be set.)
- **Counters** (`login_failure`, shared by the nodes, with the node's
  own in memory as fallback): wrong passwords at the web login per
  address (`a:`, 10 in 15 minutes), wrong secrets from mail clients per
  address (`m:`, 10), wrong codes per address (`t:`, 5), and all of them
  per IP (`i:`, 50). A phone with a stale password closes IMAP for the
  address, not the webmail. A success forgets the address's failures of
  its own kind.
- **TOTP** (`internal/totp`: HOTP of RFC 4226, TOTP of RFC 6238, SHA-1,
  six digits, 30 seconds, which is what authenticator apps assume). The
  secret is 160 random bits, sealed in the `totp` row with the account's
  blob key, so a copy of the database without the master key does not
  give it up. Enrolment answers with the secret as text, as `otpauth://`
  URI and as a QR code, and counts only once a code was shown. A code is
  accepted for the current step and one to either side (RFC 6238 5.2),
  compared in constant time. Replay: accepting a code claims its time
  step with `UPDATE totp SET last_step = step WHERE last_step < step`,
  one statement that only one node can win, so a code, and every older
  one, works once in the cluster. Ten recovery codes (80 random bits,
  SHA-256, shown once); using one deletes its row, which also only one
  request can do. Turning TOTP off and making new recovery codes take
  the password and a code.
- **The QR code is drawn by our own code** (`internal/qr`: byte mode,
  level L, versions 1 to 10, Reed-Solomon, masks by penalty, format and
  version information; about 300 lines). The alternative was to show the
  secret as text only. An encoder for the one case we need is small, and
  the secret then never passes through somebody else's library or a web
  service. The server sends the modules as rows of 0 and 1 and the
  webmail draws an SVG path, dark on white in either theme. It is
  checked against jsQR, a decoder that shares no code with it: every
  version in a Go test (through node, skipped without
  `web/node_modules`) and the real enrolment page in the browser test.
- **The login in two steps.** `POST /auth/login` with a right password
  answers `{"step": "totp"}` for an account with TOTP and sets a second
  cookie for a login that waits (a `session` row of stage `totp`, five
  minutes); `POST /auth/login/totp` trades it, with a code or a recovery
  code, for the session. The waiting login opens nothing, under either
  cookie name. A wrong password gets the same answer with or without
  TOTP, so nothing is learned about the account before the password was
  right.
- **Sessions.** Every login gets a new token and deletes the session the
  browser came with, in the same batch (rotation): a token planted before
  the login is worth nothing after it. Two lifetimes: 12 hours idle and
  7 days at most, with a cookie that goes when the browser closes; with
  "remember this device" 14 days idle, 30 days at most and a cookie that
  stays. "Last seen" is written every five minutes and when the address
  changes. The user's list shows device (from the User-Agent), addresses,
  times and how the login was made; one session, or all others, can be
  ended, and changing the password (which takes the current one) ends
  all others.
- **Cookies and cross-site requests, looked at again.** All three
  cookies are `HttpOnly`, and on HTTPS `Secure` and `__Host-`. The
  session cookie and the waiting login's are `SameSite=Strict`. Every
  endpoint under `/auth` passes one guard (`web.Server.guard`): the
  `Origin` and `Sec-Fetch-Site` check, and for everything that is not a
  GET the content type `application/json`, body or not, which a page on
  another site cannot send without a preflight this server never
  answers. No endpoint changes state on GET. With that there is no
  request left for a CSRF token to stop, so there is none; a test walks
  every state-changing endpoint from another origin, with a form's
  content types, and without a session. The exceptions are the two
  endpoints of an OIDC login, below. Changes that would let a visitor
  with a stolen session stay (a new app password, TOTP on or off, new
  recovery codes, a new password) ask for the account password, with the
  login limit on wrong ones; an account without a password needs a login
  no older than 15 minutes instead.
- **Revocation reaches open connections.** `account.auth_seq` moves when
  a session or app password is revoked, a password changes or the first
  app password is made. A second `notify.Hub` follows it the way the
  first follows `modseq`: at once on the node that wrote, by a poll once
  a second on the others, and only for accounts with listeners. An IMAP
  session (`imap.Revocable`) then checks what it logged in with and, if
  that is gone, says `* BYE this login was revoked` and closes, in IDLE,
  between commands or after the command it is in. An event stream ends
  when its session or app password is gone. Measured on two nodes on one
  rqlite: BYE in IDLE on the other node 0.55 to 0.8 s after the revoke;
  on the Docker cluster (three rqlite nodes) the event stream of another
  node ended 0.35 and 0.6 s after. The memory of verified Basic
  credentials is checked against the account on every use, so a changed
  password ends it on every node at once (an open question since phase
  3).
- **OIDC** (`internal/oidc`, `internal/jose`, `internal/web/oidc.go`;
  config `auth.oidc`, a list). Authorization code flow with PKCE (S256),
  `state` and `nonce`, each 256 random bits. Discovery: the document's
  `issuer` must be the configured one, endpoints must be https (http only
  when the issuer is on loopback, for development), redirects are not
  followed. The discovery document and the key set are kept for an hour;
  a token that names an unknown key makes the set be fetched again, at
  most once a minute, which is how a provider's key rotation is followed
  without letting a stream of bad tokens become a stream of requests.
  The ID token is validated by OpenID Connect Core 3.1.3.7 with our own
  code: signature (RS256 or ES256) by a key of the provider, also though
  the token came over TLS; `iss`; `aud` holds the client; `azp` required
  with several audiences and equal to the client whenever present; `exp`
  and `iat` with a minute of leeway, at most ten minutes old; `nbf`;
  `nonce`; `sub`. `internal/jose` refuses `none`, every HMAC algorithm
  (there is no HMAC in it, so no public key can be used as a secret),
  every other algorithm, keys that do not fit the algorithm or say they
  are for something else, a `crit` header, RSA keys under 2048 bits,
  points off the curve.
  - The flow's state lives in a ten-minute cookie, not on the server, so
    any node takes the callback. It is the one `SameSite=Lax` cookie: it
    has to come back with a navigation the provider started. The
    callback is bound to the browser by it (`state` must be the
    cookie's; the code is useless without the verifier), and answers
    with a page that goes on by itself instead of a redirect, because a
    browser does not send a Strict cookie along a redirect that another
    site started.
  - The configured claim (default `email`, and for that claim only with
    `email_verified` true, unless `allow_unverified_email`) names an
    existing account. No account is made unless the address's domain is
    in the provider's `auto_provision_domains` and robinmail accepts
    mail for it; such an account has no password. The provider's `sub`
    is recorded at the first login and a later login for the account
    under another `sub` is refused (`user oidc-unlink` forgets it): an
    address that the provider gave to somebody else does not come with
    the mailbox.
  - After a provider's login robinmail's TOTP is not asked for: the
    second factor is the provider's business. `require_totp` on the
    provider turns that around. Mail clients cannot use a provider and
    need an app password; the settings page says so.
  - Failed callbacks count against the IP like failed logins.
- **Webmail.** The login page has "remember this device", the second
  step (code or recovery code, kept across a reload) and a button per
  provider. `/settings/security`: password, app passwords (the secret
  once, with the server settings beside it; last use; revoke), two-step
  login (QR code, key as text, recovery codes once, new codes, off),
  and the browsers that are logged in.
- **Administration** (`cmd/robinmail/auth.go`): `user password` (reset;
  ends the sessions), `user totp-disable` for somebody locked out, `user
  app-passwords`, `user app-password-revoke`, `user sessions`, `user
  session-revoke <id|all>`, `user oidc-unlink`, `audit [address|all]
  [count]`. They write to the database, so they hold for every node, and
  a running server acts on them through `auth_seq`.
- **The log of auth events** (`auth_event`): logins at the web, failed
  logins at every door with the reason, wrong codes, a recovery code
  used, changes to passwords, app passwords, TOTP and sessions, refused
  provider logins, accounts made by a first provider login, and what an
  administrator did. No secret is in it. Successful logins of mail
  clients are written with the app password's "last used", every ten
  minutes at most; failed ones are bounded by the login limits. Kept for
  `auth.audit_retention_days` (90) and trimmed with the rest.
- **Tests.**
  - From the RFCs: RFC 4226 appendix D and the truncated values; RFC
    6238 appendix B for SHA-1, SHA-256 and SHA-512; RFC 7636 appendix B;
    RFC 7515 A.2 (RS256) and A.3 (ES256) verify, A.1 (HMAC), A.4 (ES512),
    A.5 (`none`) and appendix E (`crit`) are refused; RFC 7517 A.1 to
    A.3; RFC 7520 4.1 verifies, 4.2 to 4.4 are refused. The vectors are
    cut from the RFC texts (`internal/jose/testdata`). Key confusion
    with the public key as DER, PEM and modulus; a fuzz test of the
    token parser. ISO 18004 for the QR code: capacities, generator
    polynomials, the Reed-Solomon example of annex I.
  - `internal/oidc/oidctest` is a provider that shares no code with the
    relying party and can sign with `none`, with HMAC under its public
    key, with another key, and change any header or claim. 32 wrong ID
    tokens are each refused; discovery, caching and rotation are checked
    by counting its requests.
  - `internal/web` runs against a store in memory, two servers on it as
    two nodes: app passwords and their timing rule (no Argon2id), the
    two-step login, replay and recovery codes, limits, sessions and
    their lifetimes, the guard on every endpoint, the OIDC login and its
    sixteen mappings, event streams ending with their login.
  - `TestAuthEndToEnd` (`cmd/robinmail`): two `serve` on one database
    with real IMAP, SMTP and HTTP, through to the admin commands and the
    audit log. `TestOIDCLoginEndToEnd`: the server as configured against
    the fake provider.
  - `make cluster-test`, part g: three robinmail nodes on three rqlite
    nodes. An app password made on node 1 works on nodes 2 and 3 at once;
    revoking it, and a session, ends their event streams on other nodes;
    a login's first step on node 2 and second on node 3; a code refused
    the second time on node 1; five wrong codes, and ten wrong
    passwords, on nodes 1 and 2 close node 3, which read the limit from its own rqlite follower at
    the first try, 2 ms after the last failure was answered.
  - `make lab-test`: mbsync and OfflineIMAP log in with the account
    password until the account has an app password, are refused with it
    afterwards, sync with the app password, and are refused once it is
    revoked.
  - `make e2e` (`web/e2e/auth.spec.ts`): app passwords, password change
    and sessions with two browsers, two-step login set up by reading the
    QR code off the page, and the login through a provider. For that
    `contrib/dev/e2e.sh` starts `contrib/dev/oidc-provider`, the fake
    provider on a fixed port. `ROBINMAIL_E2E_PORT` moves the tested
    server off 8080 when a development server is running there.
  - `make keycloak-test`: Keycloak 26.8.2 from
    `contrib/compose/keycloak.yaml` with a realm that has the client and
    two users; a browser logs in through it into the webmail, and a user
    without a mailbox is turned away. Optional: the test skips itself
    without the container. Both pass (2026-11-14). The realm's users
    have fixed ids: Keycloak otherwise makes new ones at every import,
    and robinmail, which holds an account to the `sub` of its first
    login, rightly refused the second container's user.
- **The development instance takes a mail client.** `seed.sh` makes a
  self-signed certificate once (`contrib/dev/cert.sh`: localhost,
  127.0.0.1, mail.robinmail.test) and the dev config has IMAP on
  127.0.0.1:1993 and :1143 and submission on :1465 and :1587 next to the
  plain webmail on 8080. The dev account logs in there with its account
  password until somebody makes it an app password. `cert_file` and
  `key_file` are now resolved against the config file like the other
  paths. A dev config from before is moved aside and replaced.
- **Threats this covers.** A password guessed or phished: TOTP. A
  password stored in a mail client that leaks: it is an app password,
  for one device, with a scope, revocable without touching the others,
  and not a way into the webmail or the settings. Guessing: counters for
  the cluster as a whole, per address and per IP, per kind of secret. A
  stolen copy of the database: no password, token, app password or
  recovery code is in it, only hashes, and the TOTP secrets are sealed
  under the master key. A stolen session cookie: it cannot be read by
  script, ends by itself, can be ended from the list, and cannot mint an
  app password or turn TOTP off without the password. Cross-site
  requests and session fixation: as above. A forged or replayed ID
  token, an attacker's callback opened in the victim's browser, a code
  that leaks, an address reassigned at the provider: as above. A
  withdrawn login that stays open: ended within about a second on every
  node.
- **What it does not cover.**
  - WebAuthn and passkeys. TOTP can be phished by a page that passes the
    code on at once; a passkey cannot. Passkeys need the WebAuthn
    ceremonies, attestation formats and CBOR/COSE, all on the standard
    library: a phase of its own.
  - OAUTHBEARER and XOAUTH2 (RFC 7628) for mail clients. A client that
    could log in to IMAP with a token from the provider would need no app
    password. It needs robinmail to validate access tokens of each
    provider (introspection or JWT profiles differ) and clients that are
    configured for the provider; Thunderbird only knows a fixed list.
  - A lockout policy. Limits are per window and end by themselves:
    nobody is locked out for good, and nobody is told. There is no rule
    for password strength beyond eight characters, no check against
    known leaked passwords, no forced change, no notice to the user
    about a new login or a run of failures.
  - A compromised node: it holds the master key and sees passwords and
    codes as they come in.
  - Somebody who knows a user's address can close that address's web
    login, or IMAP, for fifteen minutes at a time with ten wrong
    passwords. The counters per kind keep one door open when the other
    is closed; the user's sessions and app password sessions that are
    open stay open.
  - Logging out at the provider: robinmail ends its own session, not the
    provider's, and does not hear when the provider ends its session
    (no front- or back-channel logout).
  - The first exit line as written, Thunderbird with an app password,
    was first checked in phase 7b.
- **By hand, with a real client** (iOS Mail, Apple Mail; Thunderbird does
  this in the lab now): add the account with the account password while
  it has no app password (works); make an app password in the webmail;
  the client asks for a password at the next check (the old one is
  refused, the open connections were closed); enter the app password for
  IMAP and SMTP (works); revoke it in the webmail and see the client ask
  again.

### Phase 7b: Thunderbird in the lab — done

Phases 6 and 7 both ended on "Thunderbird syncs" and "Thunderbird logs in
with an app password", and both were checked with other clients and a
list of things to try by hand. This phase runs the real program.

- Thunderbird in a container of the lab, driven by the harness.
- Every case looks at both sides: what Thunderbird shows, what robinmail
  has over JMAP, and the IMAP and SMTP traces for what went over the wire.
- Exit: `make lab-test` passes with the Thunderbird cases in it.

Built as follows:

- **Thunderbird 153.7.0 ESR**, Debian 13's package
  (`1:153.7.0esr-1~deb13u1`, pinned in `contrib/lab/thunderbird/Dockerfile`;
  the build fails on the version when Debian replaces it), started with
  `--headless`: no Xvfb, no display. The container `thunderbird`
  (10.77.25.7) only waits; a test writes a profile and starts the program
  on it (`/lab/thunderbird.sh`). `policies.json` makes the lab CA a
  trusted authority and turns updates and telemetry off; the lab has no
  way out for the rest.
- **The profile** is a `user.js` the harness writes: one IMAP account
  (`mx.robin.test:993`, TLS, normal password) and one SMTP server (465,
  TLS) for a user the test just made. Nothing happens until the test
  asks: no login at startup, no timer that checks for mail, no download
  of bodies for offline use, no junk filter, no index, no first-run pages
  or questions. The password is not in the profile: the harness stores it
  in Thunderbird's login manager once it runs, which is what the password
  dialog does with "remember", and replaces it there when a case needs
  another one.
- **Driven over Marionette** (`--marionette --remote-allow-system-access`),
  with a client in Go on the standard library
  (`internal/labtest/marionette_test.go`): the protocol is
  `length:JSON` frames and the harness needs four commands, so Mozilla's
  Python driver would have cost a Python in the image and a second
  language in the harness for nothing. The connection is the standard
  input and output of a `socat` that `docker compose exec` runs in the
  container: Marionette's port never leaves it. Scripts run with the
  privileges of Thunderbird's own code after a library
  (`internal/labtest/thunderbird.js`) that calls its mail interfaces, not
  a WebExtension: `nsIImapService` to list and select folders, the folder
  and message database for the lists, the message service to open a
  message, the copy service to move and copy, the folder's own
  `deleteMessages`, `emptyTrash`, `createSubfolder`, `rename` and
  `deleteSelf`, and a real compose window (`OpenComposeWindowWithParams`,
  `cmd_sendNow`, `SaveAsDraft`, `OpenComposeWindow` for a reply), so
  sending, filing in Sent and saving drafts run the code a person's click
  runs.
- **What it took.** `updateFolderWithListener` forgets its listener when
  the folder is busy; `nsIImapService.selectFolder` does not.
  `Components` does not exist in Marionette's sandbox. Marionette's own
  handling of dialogs misses the ones a compose window opens, so every
  script races a watcher for `commonDialog` windows and the harness hears
  what Thunderbird asks ("Login to server mx.robin.test with username …
  failed. [Enter New Password] [Cancel] [Retry]"). `MOZ_LOG=IMAP:4`
  writes Thunderbird's own protocol log next to the profile; the tests
  print its end when they fail, and the replay tests below were written
  from it. `TestThunderbirdEval` runs a script file in the Thunderbird a
  test left running (`ROBINMAIL_TB_KEEP=1`).
- **The cases** (`internal/labtest/thunderbird*_test.go`; `make
  thunderbird-test` runs only these, about 70 seconds). All pass.

  | Case | Result |
  |---|---|
  | First sync of 300 messages in 9 folders (one nested, names with umlauts, `&` and Japanese), flags and tags, ten Emails in two mailboxes: lists, counts, unread counts and flags equal to JMAP, an Email with two labels in both folders, five messages opened and compared byte for byte, the five roles recognised | pass; the sync takes 0.4 s over five connections |
  | Mark read, flag, tag (`$label1`), and off again, one and several: seen over JMAP | pass |
  | A flag, a read mark and a tag set over JMAP show in the Inbox | pass, without a refresh, 2 to 3 s |
  | Move and copy, one and several; the Email keeps its id; a copy into a folder it is in doubles nothing | pass |
  | A move and a second label made over JMAP show (out of the Inbox without a refresh) | pass |
  | Delete to Trash, empty Trash (the Email is destroyed); a destroy over JMAP shows | pass |
  | Create, nest, rename and delete a folder with a non-ASCII name; its messages keep their other labels | pass |
  | A mailbox made over JMAP shows; one destroyed there | pass; the destroyed one goes at Thunderbird's next start |
  | Send with an attachment to a local user and to Postfix; in Sent exactly once | pass |
  | Reply: `In-Reply-To`, `References`, the quote; the original is `$answered` | pass |
  | Save a draft, change it, save again: one draft, the second version | pass |
  | Mail delivered over SMTP by Postfix shows without a refresh | pass, 0.1 to 1.1 s after delivery |
  | Account password works; refused for IMAP and SMTP once the account has an app password, Thunderbird shows the failed login; app password works; revoked, Thunderbird shows the failed login again, also with a second app password left; account password back when none is left | pass |
  | robinmail restarts under an idle Thunderbird: the next click logs in again, the mail that came meanwhile is there once | pass |

- **What Thunderbird does on the wire.**

  | | |
  |---|---|
  | Login | `authenticate PLAIN` and waits for the challenge, though `SASL-IR` is offered; then `ID ("name" "Thunderbird" "version" "153.7.0")` and `ENABLE UTF8=ACCEPT` on every connection |
  | Connections | up to five, each parked in `IDLE` on the folder it selected last; a first sync used five |
  | Folder list | `list (subscribed) "" "*" return (special-use)`, then `list "" "INBOX"`; it takes Drafts, Sent, Junk and Trash from the special-use attributes |
  | Sync | `select`, `UID fetch 1:* (FLAGS)`, then one `UID fetch` of `UID RFC822.SIZE FLAGS BODY.PEEK[HEADER.FIELDS (…)]` for all new UIDs; later `noop` and `UID fetch <next>:* (FLAGS)` |
  | `CONDSTORE` | not used (no `CHANGEDSINCE`, no `MODSEQ`): changed flags reach it as the untagged `FETCH` of `IDLE` and `NOOP`, for which it needs the UID in them |
  | Opening a message | `UID fetch n (UID RFC822.SIZE BODY[])`, which sets `\Seen` |
  | Flags | `uid store n +Flags (\Seen)`, `+FLAGS ($label1)`; tags are the keywords `$label1` to `$label5`; it reads our lower-case `$forwarded` as forwarded |
  | Move, copy | `uid move`, `uid copy` (no `COPY` plus `EXPUNGE`) |
  | Delete | `uid move n "Trash"`; empty Trash is `store 1:* +FLAGS.SILENT (\Deleted)`, `expunge`, `list "" "Trash/*"` and `delete` of each folder in it |
  | Folders | `create` then `subscribe`; rename is `close`, `rename`, `subscribe` to the new names, `unsubscribe` from the old ones; deleting a folder is `rename` into `Trash/` |
  | Sent | after the SMTP server took the message: `append "Sent" (\Seen) UTF8 (~{n+}` |
  | Drafts | `append "Drafts" (\Draft) UTF8 (~{n+}`; a second save appends the new version, then `uid store 1 +Flags (\Seen \Deleted)` and `uid expunge 1` |
  | Submission | port 465, `EHLO [10.77.25.7]`, `AUTH PLAIN` with the initial response, `MAIL FROM:<…> BODY=8BITMIME SIZE=n`; `QUIT` only after five idle seconds: until then the next message goes out on the same connection without a new `AUTH` (up to ten per connection) |

- **Fixed in robinmail.** A submission session stayed authorized after
  its login was withdrawn. Thunderbird sent mail with the account
  password after the account had got its first app password, on the
  connection it had kept from the message before (it keeps one for five
  seconds; another client may keep it for as long as the server lets
  it), and would have with a revoked app password. Such a session is now told `421 4.7.0 … This
  login was withdrawn` and closed, at once when it waits for a command,
  the way an IMAP session gets its `BYE` (`smtp.RevocableAuthenticator`,
  the same watch on `auth_seq`). 421 and not a refusal at `MAIL`: a
  client takes a closed connection in its stride, connects again and is
  refused at `AUTH`, where it asks its user for a password. Tests without
  Docker: `internal/smtp/interop_thunderbird_test.go` replays the
  session; `TestAuthEndToEnd` holds a submission session open on one node
  over an app password made and revoked on the other.
- **Nothing to fix in IMAP.** `internal/imap/interop_thunderbird_test.go`
  replays Thunderbird's commands as it spells them, from login to empty
  Trash, so that it stays so.
- **Decided.**
  - Sent: robinmail files no copy of a message that comes in over SMTP
    submission. The client does, with `APPEND`, as every mail client
    expects to; a copy of ours next to it would double every sent
    message. A message sent from the
    webmail is filed by the webmail. The lab checks: one Email with that
    Message-ID, in Sent, seen.
  - `UNSUBSCRIBE` of a name that is no longer there answers OK.
    Thunderbird sends it after every `RENAME` and after the `DELETE`s of
    empty Trash. RFC 3501 6.3.7 has NO for a subscription that cannot be
    removed; one that went with its mailbox is removed. Tolerant, or
    every rename would end in an error box.
  - Untagged `FETCH` carries the UID once a session used a `UID`
    command, also in `IDLE` (RFC 3501 asks for it only in the answer to
    a `UID` command). Thunderbird takes flag changes from it.
- **Seen and left.**
  - Thunderbird keeps showing a mailbox that was destroyed over JMAP
    until it starts again: it lists the folders, gets the answer without
    that one, and keeps its own.
  - It counts an Email with two labels in both folders, as phase 6 said
    it would.
  - A Thunderbird with a password that is no longer good costs two
    failed logins for every check of the mail (it tries IMAP twice) and
    two for every message it tries to send (`AUTH PLAIN`, then `AUTH
    LOGIN`). Five clicks reach the limit of ten per address, which then
    refuses the right password too for fifteen minutes; Thunderbird
    shows robinmail's words, "too many failed logins; try again later".
    The harness met the limit of 50 per IP itself and now forgets that
    counter when it is half full.
- **Fixed in the harness.** `dropQueued`, from phase 5c, never deleted
  anything: rqlite's command line ignores a statement given as an
  argument. It goes in on standard input now, and the undeliverable
  messages of the TLS cases no longer stay in the lab's queue.
- **The development instance** got a database of its own on the way
  (`rqlite-dev`, 127.0.0.1:4005, in `contrib/compose/compose.yaml`): it
  shared the one on 4001 with the Go tests, which fill it with throwaway
  domains and whose servers' queue workers took its rows. `seed.sh`
  points an old `dev/robinmail.json` at the new one and says what to do
  once; nothing is migrated.
- **Stays manual.** iOS Mail, Apple Mail and Outlook: none runs in a
  container. Thunderbird's account dialog and autoconfig, which phase 8
  adds; its subscribe dialog, its search, the "just mark it as deleted"
  setting, and a connection left open for a day (the list in phase 6).
  OAuth2 in Thunderbird: robinmail offers none.
### Phase 8: completeness — in progress

- Aliases and catch-all addresses.
- JMAP `VacationResponse`.
- Filters: Sieve (RFC 5228) run at delivery, managed over JMAP (RFC 9661).
- Full-text search behind `Email/query` and IMAP `SEARCH`.
- Quotas (RFC 9425).
- MTA-STS policy serving, TLS-RPT, client autoconfig.
- Admin commands, backup and restore of rqlite plus blobs.
- Exit: each item shown working with a test.

Built in this order, each item committed with its tests: 1 full-text
search (done), 2 aliases (done), 3 Sieve (done), 4 VacationResponse (done), 5 quotas (done), 6 MTA-STS
serving and TLS-RPT (done), 7 autoconfig, 8 operations, 9 README, 10 lab and
cluster cases.

Built as follows:

- **Full-text search: an FTS5 index inside rqlite.** Checked for real on
  rqlite 10.7.1 (SQLite 3.54.1): FTS5 is compiled in, virtual tables and
  triggers replicate like any statement. The table `email_fts` has the
  columns `subject`, `addr` (From, To, Cc, Bcc), `body`, `attach` and
  `hdr`, tokenizer `unicode61 remove_diacritics 2`: words are runs of
  letters and digits, compared without case and diacritics. No stemming
  and no prefixes: a search finds whole words.
  - **What it means for encryption at rest.** The blobs stay encrypted;
    the index is not. It is contentless (`content=''`): SQLite keeps each
    word and the positions it stands at, not the text, and a column
    cannot be read back with a query. That is a smaller file, not a
    secret: from words and positions a message can be put together
    again, without its punctuation and capitals. The email table already
    held the subject, the addresses and a preview in the clear; now
    everything a message says is in rqlite's files, its Raft log and its
    snapshots. Whoever gets those reads the mail without the master key.
    The alternatives were an index of our own in the same database
    (the same exposure, more code) and no index, decrypting every
    message per search (phase 6 did that for IMAP: every search reads
    the mailbox). A searchable index that is itself encrypted with the
    account key cannot live in a database that has to run the search.
    So: rqlite's data directory belongs on an encrypted disk, and a
    backup of it is as sensitive as the mail.
  - **Writing.** The row is inserted by the batch that stores the email
    (delivery, `Email/set` create, `/import`, `/copy`, `APPEND`: all are
    `store.Deliver`), so email and index entry exist together or not at
    all. A trigger on `email` deletes the row with the email, whichever
    way it goes (destroy, expunge, a mailbox destroyed with its mail).
    `robinmail reindex [address]` rebuilds the entries of stored mail and
    is what indexes mail from before schema version 8: the migration
    cannot read blobs.
  - **What is indexed.** The subject and the four address fields as
    decoded text; the text body (RFC 8621's `textBody`; for mail that
    only has HTML the text of the HTML, without markup, scripts and
    styles); of attachments the file name of each, and the content only
    of those with a `text/*` type; at most 256 KiB of body text and as
    much of attachments per message, because the text travels through
    Raft with the delivery. Every header field for the `header`
    condition: each word of a field's value is written behind a marker
    token for the field's name (`X-Foo: bar baz` is `M bar M baz`), so
    "field X has these words" is a phrase that no other field can
    satisfy; 200 fields and 40 words per field at most.
  - **`Email/query`** (RFC 8621 4.4.1): `text` (subject, addresses,
    body, attachments), `body` and `header` (a name, or a name and
    words). Words must all be there, in any place; "quoted words" must
    be there as written. A condition is `e.id IN (SELECT rowid ... MATCH
    ?)` within the rowid range of the account, so it combines with every
    other condition and with `AND`, `OR`, `NOT`, and
    `Email/queryChanges` has it for free. The query text given to FTS5 is
    built from the tokens (`internal/search`), never from the user's
    string: nothing a client sends can write FTS5 syntax (a fuzz test
    holds that). `from`, `to`, `cc`, `bcc` and `subject` stay substring
    matches on the cached columns.
  - **`SearchSnippet/get`** (RFC 8621 5). A contentless index has no
    text to cut a snippet from, so the handler reads and decrypts the
    messages whose ids are asked for (the page a client shows), finds
    the words itself and answers with the subject and about 200
    characters of the body around the first match, matches in `<mark>`,
    everything else escaped. The RFC's example marks "Foo" in
    "Foosball" for the search "foo"; here that needs the whole word
    (deviation, in the test).
  - **IMAP `SEARCH BODY` and `TEXT`** ask the index when the backend has
    one (`imap.TextSearcher`): one query per key instead of reading every
    message. The string is a phrase of whole words, not a substring as
    RFC 3501 6.4.4 has it: `BODY "harb"` no longer finds "harbour"
    (deviation, with a test; Dovecot with an index and Gmail do the
    same). `TEXT` looks in subject, addresses, body and attachments, and
    for a single word in every header field. A string without a letter
    or digit in it, and `HEADER`, still read the messages.
    Dovecot's ImapTest checks substrings ("sdfghjk" in "asdfghjkl"):
    its group `search-body` now fails 6 commands and is listed as known
    in the lab test, so the scripted run is 35 groups passing, 7 failing
    (all known), 30 skipped.
  - **Webmail.** A search box over the list (`/` puts the cursor in it)
    searches the open mailbox; the text is in the address (`?q=`). Rows
    show what matched: the subject with marks and the snippet in place
    of the preview. The snippet's HTML is not put into the page: it is
    cut at the `<mark>` tags into pieces of text.
  - **Tests.** `internal/search` (tokens, query parsing, snippets, the
    header scheme, a fuzz test), `internal/store` (`TestTextSearch`: 35
    queries against rqlite, the index following destroy, mailbox destroy
    and reindex, a refused delivery leaving nothing, the size limit),
    `internal/jmap` (RFC 8621 4.4.1 and 5 from the text of the RFC),
    `cmd/robinmail` (IMAP `SEARCH` through the index on two nodes),
    Vitest and Playwright (`e2e/search.spec.ts`).
  - **JMAP-TestSuite** after this item: 113 files pass, 7 fail (was 110
    and 10): `Email/query/filtering`, `Email/queryChanges/simple` and
    `SearchSnippet/get/basic` pass now. The 7 left are the ones phase 5c
    explains.
  - **Not handled.** No stemming, no prefix or substring search in
    bodies, no ranking (results keep the sort of the query). The search
    box searches one mailbox, not all mail: the list is a mailbox's and
    move and delete act within it. Attachments that are not text (PDF,
    office files) are found by name only. An account's index rows are
    one rowid range of one table for all accounts; a very common word is
    cheap to look up but the table is shared. `HEADER` in IMAP reads the
    messages. Snippets cost a read and a decrypt per message shown.
- **Aliases, catch-all and forwarding.** An alias is an address of one
  of our domains without a mailbox: `info@d`, a pattern with one `*`
  (`sales-*@d`), or the catch-all `*@d` (written `@d`). Its targets are
  addresses here (accounts, or other aliases) or anywhere else. `alias
  add <alias> <target>...`, `alias list [domain]`, `alias remove <alias>
  [target]`; schema version 9. A domain with aliases cannot be removed;
  an address that is an account cannot be an alias.
  - **Where mail for an address goes** (`delivery.route`, for port 25
    and for mail between users here): the account with that address;
    the account without the `+tag`; the alias with that address; the
    alias without the tag; the pattern of the domain with the most
    characters outside its `*`, so the catch-all comes last. An account
    always wins. Targets at our domains are looked up the same way, up
    to five aliases deep and never the same alias twice; a target that
    leads nowhere is skipped and logged, an alias whose targets all lead
    nowhere is no address (`550 5.1.1`). Each mailbox and each outside
    address gets one copy however many aliases lead to it.
  - **Forwarding keeps the envelope sender honest: SRS.** A message that
    leaves again does so with the sender rewritten under the alias's
    domain, `SRS0=hash=day=original.domain=local@our.domain`
    (`internal/srs`; the format of libsrs2, which Postfix and Exim sites
    use; there is no RFC). The next server checks SPF against our
    domain, which really sends the message, and a bounce comes back
    here. Decided against the two simpler ways: keeping the original
    sender (classic `.forward`) makes us send in the name of a domain
    whose SPF does not list us, and the null sender for everything loses
    every bounce and is scored as suspicious. The null sender is the
    fallback where rewriting cannot work: a notification stays one, and
    a sender that does not fit a 64-octet local part rewritten goes out
    as `<>`. An address that another forwarder already rewrote becomes
    `SRS1` and does not grow. The hash is HMAC-SHA-256 under a key
    derived from the master key (HKDF, `blob.MasterKey.Derive`), so every
    node makes and accepts the same addresses and there is no new secret
    to hand out; an address works for 21 days.
  - **The way back.** Mail for an `SRS0=`/`SRS1=` address of ours is
    accepted only with the null reverse-path (`550 5.7.1` otherwise: it
    would be a relay for anyone who saw such an address), only when the
    hash is ours and the day is recent, and is sent on to the original
    sender with the null sender. Our own queue's bounce of a forward
    that failed goes the same way, through `Deliverer.Local`.
  - **Who owns a forward in the queue.** The queue is per account, and a
    forward has none. A system account per domain owns them: id
    `system-<hash of the domain>`, address `!system@domain`, a blob key
    and nothing else, made at the first forward. `store.Forward` takes a
    key (the delivery key plus the recipient), so a sender that retries
    after a partial failure causes no second forward.
  - **Loops.** A forwarded message gets `Delivered-To: <the address it
    was sent to>` on top; a message that arrives for an alias with that
    line already in it is refused (`554 5.4.6`), as is any message with
    100 `Received` fields (RFC 5321 6.3). Bounces cannot loop: they
    travel with the null sender and nothing answers those.
  - **Spam is not forwarded.** What Rspamd marks as spam is filed under
    Junk for the local targets and not sent on; if forwarding was all
    the message was for, the sender gets `550 5.7.1`.
  - **What a forwarded message looks like.** Our `Received` line and the
    `Delivered-To` on top, no `Return-Path` (that is final delivery's),
    the rest untouched, so a DKIM signature survives. The trace line
    names no recipient when the message also goes elsewhere: a forwarded
    copy must not tell where else the alias leads.
  - **Sending as an alias.** An account may use an alias as envelope
    sender, in `From` and `Sender`, and as an identity (also with a
    `+tag`) when the alias is one address and names the account as a
    target. A pattern or a catch-all gives no such right: the reader of
    a catch-all could otherwise write as anybody in the domain. The
    identities page offers these addresses (`GET /auth/addresses`).
  - **Tests.** `internal/srs` from the SRS paper's formats (SRS0, SRS1
    across three forwarders, hash, expiry, the wrap of the timestamp, a
    fuzz test that found a domain with `=` in it), `internal/store`
    (patterns, lookup order, targets, the system account and its key),
    `internal/delivery` (order, depth, loops, spam, the forwarded
    message, the way back, sending as an alias), `internal/jmap`,
    `internal/web`, and `TestAliasesAndForwarding` (`cmd/robinmail`):
    two servers with a database each. Mail for an alias on A reaches
    the account on A and, forwarded with an SRS sender, the account on
    B; an alias that leads to nobody on B bounces there, A's queue makes
    the notification and it finds its way back to the sender on B; a
    rewritten address refuses mail that has a sender; sending as an
    alias over SMTP and JMAP. Playwright (`e2e/aliases.spec.ts`): the
    alias as an identity, sent from, and answered.
  - **Not handled.** No alias a user makes for themselves: aliases are
    the administrator's, from the command line. Forwarding breaks SPF
    alignment for DMARC by its nature; the message passes at the next
    hop only when its DKIM signature does (robinmail adds no ARC seal).
    A forward that fails after the SRS address expired (21 days in the
    queue's five) cannot happen; a bounce that arrives later than that
    is refused. Master key rotation changes the SRS key: bounces for
    mail forwarded before it are refused. Mail between users here that
    goes to an alias with outside targets is forwarded without a spam
    scan of its own (it was scanned when it was submitted). The system
    accounts stay when their domain is removed.
- **Filters: Sieve.** Each account can have Sieve scripts, one of them
  active. The active one runs on every message for the account as it
  arrives, after Rspamd.
  - **The language** (`internal/sieve`, our own: about 1700 lines). RFC
    5228 whole, and the extensions `fileinto`, `envelope`, `body` (RFC
    5173), `variables` (RFC 5229), `vacation` (RFC 5230), `relational`
    (RFC 5231), `imap4flags` (RFC 5232), `subaddress` (RFC 5233), `copy`
    (RFC 3894), `mailbox` (RFC 5490), `reject` and `ereject` (RFC 5429),
    `date` and `index` (RFC 5260), the comparators `i;octet`,
    `i;ascii-casemap` and `i;ascii-numeric`. `Parse` checks a script
    completely (syntax, `require` first and only for what exists, every
    command and test known, required and with the arguments it takes),
    so a script that is stored will run. `Run` touches nothing: it
    returns the actions, and delivery does them.
  - **Limits.** A script: 64 KiB, 32 levels of nesting, 5000 commands
    and tests, 1000 strings in a list, 16 scripts per account. One
    message: 20,000 steps, 4 redirects, 16 mailboxes, 256 variables of
    4000 octets, a bound on the work of one `:matches` (a pattern of
    thirty `*a` against five thousand `a` is given up, not waited for),
    1 MiB of a body part looked at. A script that fails at run time, or
    whose actions do not go together (`reject` with `keep`), keeps the
    message (RFC 5228 2.10.6). A script that cannot be read defers the
    mail (`451`): a filter may be what keeps a message from being
    forwarded.
  - **Where it runs** (`delivery.finish`, for port 25 and for mail
    between users here, the same code). The script sees the message as
    it will be stored, with our `Received` line and Rspamd's headers,
    and as envelope "to" the address that led to the account, so an
    alias or a `+tag` can be tested for. Not run: `APPEND`, `Email/set`
    create, `/import`, and the bounces our own queue stores.
  - **What the actions do.** `keep` is the default mailbox: the Inbox,
    or Junk for what Rspamd marked. `fileinto` names a mailbox the way
    IMAP shows it (`Work/Invoices`, `INBOX`); one that is not there
    keeps the message in the default mailbox, unless `:create` makes
    it, with the levels above it. **A message filed into several
    mailboxes is one Email with those labels**, and its flags (`\Seen`
    becomes `$seen`, and so on; keywords stay) are the union of what
    each action gave it, since an Email has one set of keywords.
    `discard` stores nothing. `redirect` sends the message on like an
    alias does: the sender rewritten (SRS) under the account's domain,
    `Delivered-To` the account; a message that comes back with that
    line is kept instead of sent round again, and spam is kept, not
    redirected. `reject` and `ereject`: when the message was for this
    account alone the refusal is the reply to the sender's server (`550
    5.7.1` and the reason), which is what RFC 5429 prefers; when it also
    went to others, they get it and the sender gets a DSN from the
    system account.
  - **Vacation** (RFC 5230, RFC 3834). No reply to: the null
    reverse-path; `MAILER-DAEMON`, `owner-*`, `*-request`, `LISTSERV`,
    `majordomo` and no-reply senders; the user's own address; mail with
    `Auto-Submitted` other than `no`, any `List-*` field, `Precedence`
    bulk, list or junk, or `X-Auto-Response-Suppress`; spam; mail that
    does not name one of the user's addresses (the account's, its
    aliases, the envelope recipient, `:addresses`) in `To`, `Cc`, `Bcc`
    or their `Resent-` forms. One reply per sender per period: the key
    is the `:handle`, or without one the reply's own text, and the
    sender; `vacation_sent` takes it with one statement that gives one
    of several nodes the yes. `:days` is 7 by default, 30 at most. The
    reply: `From` the account (or `:from` when that is one of its
    addresses), `To` the envelope sender, `Subject` from `:subject` or
    `Auto: ` and the original, `In-Reply-To`, `References`,
    `Auto-Submitted: auto-replied`; `:mime` gives the body as an entity
    of its own. It goes out through the Submitter like the account's own
    mail (scanned, signed, counted against its hourly limit) with the
    null reverse-path, so nothing bounces to it or answers it.
  - **Storage.** The script is a blob of the account: encrypted on disk
    like a message, replicated like one, downloadable by its blobId,
    known to the sweeper and the repair pass. Schema version 10. A
    parsed script is cached per node by the hash of its text.
  - **JMAP Sieve** (RFC 9661): capability `urn:ietf:params:jmap:sieve`
    with the limits and the list of extensions, `SieveScript/get`,
    `/set` with `onSuccessActivateScript` (also for a script made in the
    same call) and `onSuccessDeactivateScript`, `/query` and
    `/validate`. `invalidSieve` carries the line of the fault;
    `sieveIsActive` refuses the destroy of the active script;
    `alreadyExists` a second script of a name. Push names `SieveScript`.
  - **Webmail.** `/settings/filters`: a list of rules (From, To or Cc,
    Subject contains; all or any; then move, label, mark read, forward,
    delete), in order, with a switch for on and off, and beside it the
    script as text. The page stores nothing of its own: rules are
    written as a script of a fixed shape (`web/src/sieve/rules.ts`) and
    read back from it. Whether a script is "rules" is decided by a round
    trip: it is, when writing the rules read from it gives the same
    script again. Anything else is shown as text only, and runs all the
    same. One file (`web/src/sieve/testdata/rules.sieve`) is the fixture
    of the page's test and of the interpreter's.
  - **ManageSieve (RFC 5804) is left out.** It is a protocol of its own
    with its own listener, SASL and capability handling, for the few
    mail clients that have a Sieve editor; JMAP and the webmail cover
    the need here.
  - **Tests.** `internal/sieve`: the examples of RFC 5228 (2.4, 2.10,
    3.1, 4, 5, the extended example of 10, 45 scripts that must not
    parse), 5229, 5230, 5231, 5232, 5233, 3894, 5490, 5429, 5260 and
    5173, limits, a fuzz test that parses and runs anything.
    `internal/delivery`: every action, spam, rejection alone and among
    several recipients, loops, 24 vacation cases, the reply, the period.
    `internal/jmap`: RFC 9661 1.3 and 2.1 to 2.4. `TestSieveEndToEnd`
    (`cmd/robinmail`): two servers; a script uploaded and activated over
    JMAP; mail from the other server filed into mailboxes the script
    makes (one Email, two labels, flagged), answered once by vacation
    with the null reverse-path, redirected with the rewritten sender,
    and refused with the reason arriving in the other server's bounce.
    Playwright (`e2e/filters.spec.ts`): a rule made in the page runs on
    mail that comes in over SMTP; a broken script is refused with its
    line; a script written by hand runs and is shown as text.
  - **Deviations**, each in a test: a bare LF ends a line of a script
    (RFC 5228 has CRLF only; a script typed in a browser has LF). The
    RFC texts have two slips the tests write correctly: `elseif` in RFC
    5231 6, a comma after the last argument in RFC 5260 6.
  - **Not handled.** No `include`, `editheader`, `enotify`, `regex`,
    `duplicate`, `spamtest`, `environment`, `mime`/`foreverypart`,
    `encoded-character`, `vacation-seconds`. A script's `:flags` on
    several `fileinto` are not kept apart per mailbox. A rejection after
    DATA for several recipients is a DSN, not a refusal per recipient
    (that would be LMTP's way). Bounces our queue makes for a local
    sender are stored without passing the sender's script. The rules
    page knows three fields and "contains"; everything else is the
    script tab. A vacation reply is made from the script; JMAP's
    VacationResponse (next item) is a second way to the same code.
- **JMAP VacationResponse** (RFC 8621 8). Capability
  `urn:ietf:params:jmap:vacationresponse`; the object with the id
  `singleton` (`isEnabled`, `fromDate`, `toDate`, `subject`, `textBody`,
  `htmlBody`); `VacationResponse/get` and `/set`, where create and
  destroy answer `singleton`. Schema version 11, table `vacation`.
  - **Backed by the vacation machinery of Sieve.** It is not a second
    implementation and not a generated script: at delivery, after the
    account's script has run, an enabled response whose dates fit
    ("on or after" `fromDate`, "before" `toDate`) is put into the plan
    as a vacation action, and from there it is that action: the same
    rules (never to lists, bulk, notifications or automatic mail, only
    to mail that names the user), the same once per sender, the same
    reply through the Submitter with the null reverse-path. The period
    is 7 days; the key is one fixed handle, so changing the text does
    not make everybody hear from it again. A script that has its own
    `vacation`, or that refuses the message, comes first.
  - **The reply.** `subject` or else `Auto: ` and the original subject;
    `textBody`, or text made from `htmlBody`, or a default sentence; with
    an `htmlBody` the reply is `multipart/alternative`.
  - **Webmail.** `/settings/vacation`: on and off, from and until (in the
    browser's time zone, stored as UTC), subject, message. An HTML body
    another client set is kept until the message is changed here.
  - **Tests.** `internal/jmap` from RFC 8621 8 (the singleton, patches,
    every refusal), `internal/store`, `internal/delivery` (dates, the
    defaults, HTML, once per sender across a changed text, a script's
    own vacation first), Playwright (`e2e/vacation.spec.ts`): turned on
    in the page, a message over SMTP gets the reply with
    `Auto-Submitted`, one with `List-Id` gets none.
  - **Not handled.** One more read per delivered message and account
    (the `vacation` row). The period is fixed. The reply has no
    signature of the identity.
- **Quotas.** An account may hold so many octets of mail and so many
  messages. What counts is its Emails, each once whatever number of
  mailboxes it is in, by the size of the message. Uploads, the queue and
  Sieve scripts do not count.
  - **Counting that holds under concurrency.** Nothing in Go counts. The
    account row has `used_bytes` and `used_count`, and two triggers on
    `email` move them with every insert and delete, inside whatever
    batch does it: delivery, destroy, expunge, a mailbox destroyed with
    its mail. The limit is a guard in the batch that stores an Email
    (`store.Deliver`, which is every way in): "what the account holds
    plus this message is within the limit", evaluated by SQLite inside
    the batch. rqlite applies batches one after another, so the guard
    always sees what every earlier batch left, from whatever node.
    Schema version 12; the migration counts the mail that is there.
    `quota recount` sets the counters to what the emails add up to, for
    a database that was changed by hand.
  - **Limits.** `quota {default_bytes, default_messages}` in the config
    (0: none, the default) for accounts without limits of their own;
    `user quota <address> [<storage> <messages>]` shows the use and sets
    an account's own (`5G`, `none`, `default`). A limit below what the
    account holds removes nothing; it stops new mail.
  - **Where it is enforced.**
    - SMTP, at RCPT: a recipient whose mailbox is full gets `452 4.2.2`,
      for that recipient only, so the sender's server still delivers to
      the others. An alias for several accounts is full when all are.
    - SMTP, after DATA (the account filled up meanwhile, or this
      message is what does not fit): `452 4.2.2` when there is no room
      now, `552 5.2.3` when the message alone is larger than the limit
      and the account was its only recipient. With several recipients
      the answer is `452` for the whole message: those who have it
      recognise the retry by the delivery key. RFC 5321 4.2.2 has 452
      for insufficient storage and 552 for an exceeded allocation; a
      full mailbox is treated as temporary because its owner can make
      room, as RFC 3463's X.2.2 allows for.
    - Mail between users here: the queue gets the same replies, retries
      a `452` and bounces a `552`.
    - IMAP `APPEND`: `NO [OVERQUOTA]`. `COPY` and `MOVE` add labels, not
      messages, and are never over quota.
    - JMAP `Email/set` create, `/import`, `/copy`: `overQuota`. Upload:
      HTTP 507, by the octet limit only (an upload is no message, and
      may be a Sieve script).
    - Not refused: a bounce our own queue owes the account
      (`Message.IgnoreQuota`), which would otherwise be lost.
  - **JMAP Quota** (RFC 9425): capability `urn:ietf:params:jmap:quota`;
    up to two objects, `octets` and `count`, each only when that limit
    is set; `Quota/get`, `/changes` (with `updatedProperties: ["used"]`
    when only the use moved, derived from the Email change log, so no
    row is written for it), `/query` (filter name, scope, resourceType,
    type; sort name, used) and `/queryChanges`. Push names `Quota` when
    mail came or went.
  - **IMAP QUOTA** (RFC 9208): `QUOTA QUOTA=RES-STORAGE
    QUOTA=RES-MESSAGE`, `GETQUOTA`, `GETQUOTAROOT`; one quota root `""`
    per account; `STORAGE` in units of 1024 octets (use rounded up, the
    limit down); a resource without a limit is not listed and an account
    without limits has no root. `SETQUOTA` answers `NO [NOPERM]`:
    `QUOTASET` is not offered, limits are the administrator's.
  - **Webmail.** The sidebar shows a bar per limit ("1.4 MB of 5 MB",
    "120 of 1000 messages"), in warning colour from 90%, with a line
    when the account is full. It is read with the account and with
    every refresh, in the same request.
  - **Tests.** `internal/store`: the counters through every way an
    email comes and goes; limits by octets and by count, own and
    default; **40 concurrent deliveries through two stores (two nodes)
    into room for ten: ten stored, thirty refused, counters at ten**.
    `internal/delivery`: the replies at RCPT and after DATA.
    `internal/imap`: RFC 9208 3, 4.1 to 4.3, 5.1 and 5.2 with the
    examples' shape. `internal/jmap`: RFC 9425 3 and 4.1 to 4.4,
    `overQuota`, the upload. `TestQuotaEndToEnd` (`cmd/robinmail`):
    a default of three messages; the fourth refused at RCPT while the
    other recipient of the same transaction gets it; `GETQUOTAROOT`,
    `APPEND`, `Quota/get`, `Email/import`; an own limit in octets; 552
    for a message larger than the quota, 452 for one there is no room
    for; room made by `EXPUNGE`. Playwright (`e2e/quota.spec.ts`).
  - **Not handled.** No soft or warn limit and no notice to the user
    before the account is full. No `DELETED-STORAGE` in IMAP `STATUS`.
    No quota per domain or for the whole server. Sent mail counts: an
    account at its limit cannot save a draft or keep a copy in Sent
    (sending itself still works over SMTP submission). The default
    limits are per node's config and must be the same on all nodes. A
    message larger than the quota among several recipients is answered
    `452` and retried by the sender until it gives up.

- **MTA-STS serving, TLS reports, DNS records.**
  - **Our own policy** (RFC 8461 3.2, 3.3). `mta_sts {mode,
    max_age_seconds, mx}` in the config; without a mode nothing is
    served. The web listener answers
    `https://mta-sts.<domain>/.well-known/mta-sts.txt` for every domain
    in `domain` (read from the store, cached 30 seconds, so `domain add`
    needs no restart): the policy as `text/plain` with CRLF, GET and
    HEAD only, 404 for every other path. Nothing else of the web server
    is reachable under that name; a name that is no domain of ours gets
    421 as before. One policy for all domains: they share the mail
    servers. `mx` defaults to the hostname. With ACME the certificate
    for `mta-sts.<domain>` is asked for on the first handshake (the host
    policy accepts the names of our domains); with certificate files
    the certificate has to cover them, which `robinmail dns` says.
  - **The id** of the `_mta-sts` TXT record is the first 20 hex digits of
    the SHA-256 of the policy text: the same on every node, new when the
    policy changes, and nothing to keep.
  - **TLS reports out** (RFC 8460). The queue worker counts every
    session with a recipient domain's server in `tls_result`: one row
    per UTC day, domain, policy and outcome, `count = count + 1` in one
    statement, from any node. The policy is the MTA-STS policy that
    applied (`sts`, with its lines) or `no-policy-found`. Outcomes:
    success, `starttls-not-supported`, `certificate-host-mismatch`,
    `certificate-expired`, `certificate-not-trusted`,
    `validation-failure`, `sts-policy-fetch-error`,
    `sts-policy-invalid`; failures carry the MX and its address.
    Counted for policies in `testing` too, which is what that mode is
    for. A session over a route of the config is counted under the
    recipient domain, without a policy.
  - Every hour (first after up to ten minutes) each node looks at the
    last three finished days. For a domain with counts and no row in
    `tls_report_sent` it inserts the row (`INSERT OR IGNORE`: one node
    gets it), reads `_smtp._tls.<domain>` (RFC 8460 3; exactly one
    `v=TLSRPTv1` record or none), builds the report and sends it to
    every `rua`: `mailto:` as a message (RFC 8460 5.3: `TLS-Report-Domain`
    and `TLS-Report-Submitter` fields, `multipart/report;
    report-type=tlsrpt`, the report as `application/tlsrpt+gzip`), signed
    by Rspamd with the domain's DKIM key and queued like any mail with
    an idempotency key; `https:` as a POST of the gzip (5.4), no
    redirects. What became of it is kept in the row (`tlsrpt results`).
    The report is claimed before it is sent: a node that dies in between
    leaves that day's report for that domain unsent. A lost report costs
    less than a doubled one that makes the receiver count twice.
  - Reports are sent from `tls_rpt.from` (default `postmaster@` the
    first domain), `organization-name` is `tls_rpt.organization` (the
    hostname), the report id `<yyyymmdd>.<domain>@<hostname>`.
    `tls_rpt.disabled` stops the sending; counting and receiving go on.
  - **TLS reports in.** The `rua` we publish is a mailbox or an alias
    like any other (`postmaster@<domain>` in `robinmail dns`). A message
    that was stored and carries a report (`application/tlsrpt+gzip` or
    `+json`, also as the whole body) is parsed after delivery and kept
    in `tls_report_in`, once per organization and report id; the message
    itself stays in the mailbox. `tlsrpt received` lists them, `tlsrpt
    show <id>` prints the JSON. Nothing is verified or acted on: a
    report is its sender's claim. There is no HTTPS endpoint for
    reports to us.
  - **`robinmail dns`** now also prints: `_mta-sts` TXT and the
    `mta-sts` CNAME with the policy as a comment (when `mta_sts` is
    set), `_smtp._tls` TXT, the `autoconfig` CNAME, and the SRV records
    of RFC 6186 and RFC 8314 (`_imaps`, `_imap`, `_submissions`,
    `_submission` with the ports of the config; `0 0 0 .` for what is
    not offered: POP3, and whichever of the others has no listener).
  - **Tests.** `internal/tlsrpt`: RFC 8460 3 (the record and its
    examples), 4.3 and 4.4 with the report of Appendix B, 5.1 (file
    name), 5.3 (the message and its fields); the parser is fuzzed.
    `internal/store`: counts from two stores at once add up; a report
    is claimed by one of twenty callers; a received report is kept
    once. `internal/queue`: an MTA-STS `enforce` failure and an
    opportunistic session are counted with their result types.
    `TestMTASTSPolicyServed` (`cmd/robinmail`): the policy over HTTPS
    for a domain, 404 for other paths, 421 for foreign names, a domain
    added while the server runs, the `dns` output.
    `TestTLSReports`: counts of yesterday become one report, sent by
    mail to a robinmail (this one, which keeps it) and by POST; a second
    run sends nothing; a repeated report is kept once.
  - **Not handled.** No DANE, so no `tlsa` policies or results. One
    policy for all domains. No report is sent when a domain's record
    could not be read on the hour it was claimed. No summary for the
    administrator of reports received: they are listed, not judged. No
    compression choice: always gzip. The session that delivers a report
    is counted like any other, so two servers that report to each other
    exchange one report a day about the previous day's report; it does
    not grow. A failed MTA-STS check also holds back a report (RFC 8460
    5.3 would have it delivered regardless).

## Open questions

- Passkeys (WebAuthn) are the second factor that cannot be phished, and
  could replace the password. The work is the ceremonies, CBOR and COSE
  on the standard library. Worth a phase?
- OAUTHBEARER for mail clients would let a provider's users do without
  app passwords, for the few clients that can be pointed at a provider
  of one's own. Is there a client in use here that can?
- The login limits end by themselves and tell nobody. Should a run of
  failures, or a login from a new device, send the user a message? It is
  a mail server: the message could simply be delivered to the account.
- Ten wrong passwords close an address's web login for fifteen minutes
  for everybody, also for its owner, and anybody can send them. A limit
  per address and IP together, with a far higher one per address alone,
  would keep the owner in at the price of more guesses for a botnet.
- A node reads the failure counters from its own rqlite node's copy
  (level `none`), which on a follower can be a moment behind. A login
  with the right secret on a node that has not seen the last failures
  yet is let in and clears the counter. With guesses at one node a time
  nothing is lost; spread over nodes at once, the limit is the count
  plus what arrives within that moment.
- The account password works for mail clients again when the last app
  password is revoked and there is no TOTP. Simple to explain, but an
  account that once moved to app passwords could also stay there for
  good; `auth.require_app_passwords` does that for everybody.
- A provider login maps by the email claim and pins the provider's
  subject at first use. An account that should move to another identity
  at the provider needs `user oidc-unlink` by an administrator. Is that
  the right owner of that decision, or should the user be able to do it
  from a password login?
- An account made by a provider login has no password and cannot get
  one from the webmail; an administrator can set one (`user password`).
  Nothing removes such an account when the provider stops knowing the
  user.
- The audit log is one table for all accounts and is only read from the
  command line. Showing a user their own recent events in the settings
  is a query away; an administrator's view over HTTP needs the notion of
  an administrator first, which robinmail does not have.
- robinmail does not log the user out at the provider and does not hear
  when the provider ends a session (RP-initiated, front-channel and
  back-channel logout are not built).
- The second hub polls `auth_seq` once a second per node while accounts
  have listeners: one more small read next to the one for `modseq`. One
  query could read both.
- TOTP uses SHA-1 because authenticator apps silently ignore the
  `algorithm` parameter more often than they honour it. HMAC-SHA-1 is
  not weakened by SHA-1's collisions, but SHA-256 would read better.
- A code is refused once a newer one was used. Two authenticators of one
  user whose clocks differ by a step can lose a login to that now and
  then.
- IMAP's `\Deleted` is the keyword `$deleted`, and the webmail shows mail
  that carries it like any other. Hide it in JMAP, show it struck
  through, or leave it: a client set to "just mark as deleted" leaves
  such mail around for good.
- A message with several labels is one message per folder in IMAP. A
  client that deletes it in one folder leaves it in the others, and its
  totals count it more than once. Gmail lives with the same; an "All
  mail" view and a rule for what delete means would be the alternative.
- `HIGHESTMODSEQ` is the account's modseq, so it moves for a mailbox in
  which nothing changed. Exact would be the highest modseq of the
  mailbox's own rows, which needs every write to an Email to stamp all
  its mailboxes.
- `QRESYNC` needs a record of which UID left which mailbox at which
  modseq. A table written by every removal would give it, and would let
  a session send `VANISHED` instead of comparing UID lists; worth it
  once a client that uses it is slow without.
- Every `FETCH` of a header field or structure reads and decrypts the
  whole message, and a first sync does that for every message. The
  envelope and body structure could be cached in the email row, or the
  header kept apart from the body.
- A session reads the whole UID list of its mailbox after every change
  to the account. Fine for thousands of messages; a mailbox of a million
  wants the change log instead.
- One poll a second per node finds changes from other nodes, so push and
  IDLE are up to a second late across nodes. A nudge between nodes would
  cut that; so far nothing needs it.
- An event source is one connection per tab, and a browser opens at most
  six per host over HTTP/1.1. Go's HTTPS listener offers HTTP/2, where
  that limit does not apply (not checked here); the plain development
  listener is HTTP/1.1, so a seventh tab there waits.
- ImapTest is the Dovecot project's binary from a release tag that moves;
  the lab pins its checksum, and the build fails when the project
  replaces it. Its source does not build against a released Dovecot.

- Where the master key comes from in production: a file is all there is. A
  lost key file loses all mail, so it needs a backup that is not next to the
  blobs.
- Delivery keys derived from content mean a byte-identical message from the
  same sender within a week is dropped as a retry. Intended, but worth
  knowing.
- Where phase 5 gets tested against real providers: which host and domain.
  Still open. Phase 5c exchanged mail with Postfix, Exim and OpenSMTPD in
  a lab, which covers the protocol. What only real providers can show:
  whether Gmail, Outlook and the like accept the mail (PTR, SPF, DKIM
  alignment, their rate limits), how their greylisting and 4yz replies
  meet our backoff, what their MTA-STS policies and certificates do under
  `enforce`, and whether their bounces parse. Exchange is the one big
  server nobody can put in a container.
- A line longer than 998 characters goes out as it came in (phase 5c:
  Postfix and OpenSMTPD took one of 3000, Exim took it and would not pass
  it on). RFC 5322 2.1.1 says MUST NOT. Refuse it at submission, or
  re-encode the part as quoted-printable, which changes a signed message?
- Exim and OpenSMTPD report 5.0.0 for every failure and give no
  Diagnostic-Code for their own. `smtpReply` then says who reported it and
  nothing else; the text part of the bounce has the reason, for a person
  to read. Worth parsing?
- A bounce is recognised by its `message/delivery-status` part even when
  it is not a `multipart/report` (OpenSMTPD). That widens, a little, what
  can mark a sent message as failed; the null reverse-path and the
  Message-ID are still required.
- The `dev` section (`retry_seconds`, `smtp_trace`) is in every binary. A
  production config with it set gets a warning at start for the first and
  a very large log for the second. A build tag would keep them out.
- The JMAP-TestSuite is pinned to one commit and changes daily. Ten files
  fail, six of them over readings of the RFC that could be raised with its
  authors.
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
  the standard way. The end-to-end tests wait it out on every send.
- `deliveryStatus` says `unknown` for mail another server accepted, as RFC
  8621 defines it. The webmail words it as "Sent".
- Marking the original `$answered` only when the send worked needs a
  third call guarded by a result reference into the created submission
  (phase 5, webmail). It works on any server that follows RFC 8620 3.7,
  but it is a trick. The plain way is a second request after the send,
  which a closed tab can lose; a server extension would be cleaner.
- Every autosave of a draft is a new Email: a new id, a new Message-ID,
  two state changes, and a row in Drafts that is replaced while it is
  looked at. Immutable Emails leave no other way in JMAP; a longer
  pause between saves is the only knob.
- When the answer to a send is lost the webmail cannot tell whether the
  message went, and sending again may send it twice. A creation id the
  server remembers for a while would make a repeat safe.
- The rich text editor stands on `document.execCommand`, which browsers
  mark as obsolete and still all implement. If one drops it, the editor
  needs its own handling of selections, or a library after all.
- A `mailto:` link in an HTML message opens the system's mail program.
  The view could rewrite such links to a page of the webmail, which the
  frame may open as a popup.
- The queue asks rqlite for due rows every 5 seconds on every node, also
  when it is empty. A submission could wake the local worker at once; the
  5 seconds are what a send waits today at most.
- The MTA-STS TXT record is looked up for every delivery to a domain, and
  the resolver's cache is the only one in front of it.
- A bounce is stored in the Inbox without going through Rspamd or any
  filter; with Sieve (phase 8) it should pass through the user's rules.
- The tests of all packages share one rqlite database. Queue rows are now
  purged by the tests that make them, but a `serve` on the development
  database still works whatever is queued there. Seen in phase 7: a
  development server started during a full test run took the rows of
  the queue tests ("2 of 12 arrived") in two runs of five. A database
  for the tests apart from the development one, a third rqlite in the
  compose file, would end that.
