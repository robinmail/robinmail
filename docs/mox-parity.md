# robinmail compared with mox

- Run: 2026-11-15, 01:00 to 01:15 CET, scheduled task `robinmail-mox-parity-check`.
- mox: commit `b32f0def9763b2644ad7bd1e8b3894786e80858b` (main, 2026-10-27). Latest tag: v0.0.17. Read: README, `website/` (features, protocols), `config/config.go`, `doc.go` (commands), `imapserver/server.go`, `smtpserver/server.go`, `queue/`, `webadmin/`, `webaccount/`, `webmail/`. Cloned shallow into the scratchpad; nothing built or run.
- robinmail: `main` at `cfe6cc6`, working tree read only. Read: `PLAN.md` (decisions, not-handled lists, open questions), `README.md`, `robinmail.example.json`, and the code where named below.
- Issues: https://github.com/mikepage/robinmail/issues/35 (tracking), #1 to #34.

Legend for "On purpose": yes = PLAN.md says it was left out; no = not built, no stated reason; n/a = both have it or neither.

## SMTP receiving and policy

| Feature | mox | robinmail | On purpose | Issue |
|---|---|---|---|---|
| SPF, DKIM verify, DMARC | Own packages `spf/`, `dkim/`, `dmarc/` | Rspamd over HTTP (PLAN Decisions) | yes | #29 |
| DKIM signing | In the queue, keys in config | Rspamd signs at submission; keys are files on each Rspamd | yes | #29 |
| ARC | Roadmap | No | n/a | - |
| DNSBL for incoming | `DNSBLs` per listener, only when reputation is unclear | Rspamd's | yes | #30 |
| Reputation and Bayes per account | `junk/`, learned from flags | Rspamd's score; no learning from users (`internal/rspamd` only scans) | yes / no | #30, #10 |
| Greylisting | No; 15 s delay for first-time senders | Rspamd's greylist action gives a temporary error (`delivery.verdict`); needs Redis in Rspamd | yes | #30 |
| Rejects mailbox | `RejectsMailbox`, `KeepRejects` | Rejected mail is gone | no | #4 |
| Introbox, subjectpass | Yes | No | yes (part of the Rspamd choice) | #30 |
| Rate limits, incoming | Per IP and IP class (`ratelimit/`) | `max_connections`, `max_connections_per_ip` | n/a | - |
| Subaddressing | Separators configurable per domain | `+` only (`internal/delivery/alias.go`) | no | #9 |
| Catch-all | Yes | Yes (phase 8) | n/a | - |
| Case-sensitive local parts | Option per domain | Lowercase only | no | #9 |
| REQUIRETLS | Yes | No (phase 5 not handled) | no | #1 |
| SMTPUTF8 | Yes, incl. UTF-8 local accounts | Announced and passed on; local parts ASCII only | yes | #34 |
| IDNA domains | Yes | Outbound via `x/net/idna`; hosted IDN domains not determined | - | #34 |
| DSN extension (RFC 3461) | No (todo in source) | No | n/a | - |
| CHUNKING, BINARYMIME | No (roadmap) | No | n/a | - |
| PIPELINING, 8BITMIME, SIZE, ENHANCEDSTATUSCODES | Yes | Yes (`internal/smtp/server.go`) | n/a | - |
| LIMITS (RFC 9422) | `LIMITS RCPTMAX` | No | no | #2 |
| FUTURERELEASE | Yes | No; `undo_seconds` and JMAP `sendAt` | no | #2 |
| Size limits | `SMTPMaxMessageSize` (100 MB default) | `smtp.max_message_bytes` | n/a | - |
| Alias as list (members only, member list) | `PostPublic`, `ListMembers`, `AllowMsgFrom` | Open aliases; send-as-alias yes | no | #9 |
| Forwarding to outside addresses | Roadmap | Yes, with SRS | n/a | - |

## Sending

| Feature | mox | robinmail | On purpose | Issue |
|---|---|---|---|---|
| Queue with retries, bounces | Yes | Yes (`internal/queue`) | n/a | - |
| "Delayed" DSN | Yes (`queue/dsn.go`) | No | no | #3 |
| Hold, hold rules, fail with bounce, retired list | Yes | `queue list/show/retry/delete` | no | #3 |
| MTA-STS outbound | Yes, cached policies | Yes (`internal/queue/sts.go`) | n/a | - |
| DANE | Yes, AD bit from local resolver | No | yes (phase 8) | #31 |
| TLS-RPT send and receive | Yes, with DANE results | Yes, without `tlsa` | n/a | #31 |
| DMARC reports receive | Parsed, stored, shown | Arrive as mail to postmaster, not parsed | no | #8 |
| DMARC reports send | Yes | No (Rspamd could) | no | #8 |
| Transports: smarthost with auth, SOCKS, per account/domain, min attempts | Yes | `outbound.routes`: domain or `*` to host:port, no auth | no | #5 |
| IPv4/IPv6 choice, source address | `DisableIPv4/6`, listener IPs | Resolver and kernel decide | no | #5 |
| Suppression list | Per account, automatic | No | no | #6 |
| Per-account daily limits | Messages and first-time recipients per day | One hourly recipient limit for all | no | #6 |
| Webhooks | Outgoing and incoming, queued | No; JMAP event source only, no PushSubscription | no | #7 |
| HTTP API for sending | webapi | JMAP `EmailSubmission` | n/a | #7 |
| FBL / ARF | Roadmap | No | n/a | - |

## IMAP

Both: IMAP4rev1, IMAP4rev2, ENABLE, LITERAL+, IDLE, SASL-IR, BINARY (robinmail: fetch only), UNSELECT, UIDPLUS, ESEARCH, SEARCHRES, MOVE, UTF8=ACCEPT, LIST-EXTENDED, SPECIAL-USE, LIST-STATUS, ID, CONDSTORE, STATUS=SIZE, QUOTA, QUOTA=RES-STORAGE, NAMESPACE.

| Extension | mox | robinmail | On purpose | Issue |
|---|---|---|---|---|
| QRESYNC | Yes | No (open question) | no | #11 |
| NOTIFY | Yes | No | no | #12 |
| METADATA, LIST-METADATA | Yes | No | no | #13 |
| MULTIAPPEND, REPLACE, WITHIN, SAVEDATE, PREVIEW, CREATE-SPECIAL-USE, APPENDLIMIT, UIDONLY, INPROGRESS, MULTISEARCH | Yes | No | no | #14 |
| AUTH=SCRAM-*, CRAM-MD5, EXTERNAL | Yes | PLAIN, LOGIN | no | #16, #17 |
| CHILDREN, QUOTA=RES-MESSAGE | No | Yes | n/a | - |
| SORT, THREAD, CATENATE, URLAUTH, ACL | No (roadmap) | No | n/a | - |
| COMPRESS=DEFLATE | Written, switched off | No | n/a | - |
| IMAP on 443 via ALPN | `EnabledOnHTTPS` | No | no | #23 |

## Webmail

| Feature | mox | robinmail (`web/`) | On purpose | Issue |
|---|---|---|---|---|
| Threads, search, keyboard, remote images on request | Yes | Yes | n/a | - |
| HTML compose | No | Yes (rich text) | n/a | - |
| Address completion | Yes | No (phase 5 not built) | no | #15 |
| Drag and drop to move | Yes | Not found in `web/src` | no | #15 |
| Mute thread, collapse | Yes | Not found | no | #15 |
| Layout choice | Yes | No | no | #15 |
| Send later | Yes | Undo only | no | #15, #2 |
| Recipient TLS info, REQUIRETLS switch | Yes | No | no | #15, #1 |
| Rule from a message | Yes (rulesets) | Filters page (Sieve), not from a message | no | #15 |
| Filters, vacation, quota bar, security page | Rulesets only | Yes | n/a | - |
| Import/export in account page | Yes | No | yes | #32 |

`web-ng/` (Angular) was being written by another session during this run and was not compared.

## JMAP

mox: none (roadmap: "JMAP, IMAP OBJECTID extension, IMAP JMAPACCESS extension"). robinmail: core, mail, submission, quota, sieve, vacationresponse, event-source push. Not in robinmail: `PushSubscription`, WebSocket, blob management, MDN, contacts, calendars. No parity issue; `PushSubscription` is mentioned in #7.

## Accounts and auth

| Feature | mox | robinmail | On purpose | Issue |
|---|---|---|---|---|
| SCRAM-SHA-256(-PLUS), SCRAM-SHA-1(-PLUS) | Yes | No | no | #16 |
| CRAM-MD5 | Yes | No | not worth adding | #16 |
| TLS client certificates | Yes (`store/tlspubkey.go`) | No | no | #17 |
| App passwords | Not found | Yes (phase 7) | n/a | - |
| TOTP | Not found | Yes | n/a | - |
| OIDC / OAuth | Roadmap | OIDC login for the webmail | n/a | - |
| Login rate limits | Per IP | Per address, and per client address for app passwords | n/a | - |
| Disable account or domain login with a message | Yes | No | no | #18 |
| User sees own login attempts | Yes | CLI `audit` only | no | #18 |
| Generated-only passwords | `NoCustomPassword` | No | no | #18 |

## Administration

| Feature | mox | robinmail | On purpose | Issue |
|---|---|---|---|---|
| Web admin interface | Yes, about 90 methods | CLI only; no administrator role | no | #19 |
| Quickstart | Yes | Manual setup | no | #20 |
| Print DNS records | Yes, incl. CAA, TLSA | `robinmail dns`: MX, SPF, DKIM, DMARC, MTA-STS, TLS-RPT, SRV | partly | #21 |
| Check live DNS | `config dnscheck`, admin page | No | no | #21 |
| Monitor own IPs on DNSBLs | `MonitorDNSBLs` | No | no | #21 |
| Config reload | Domains file reloaded | Domains, users, aliases live in rqlite; JSON needs restart | n/a | - |
| Import/export mbox, Maildir | Yes | No ("Not in scope: migration tooling") | yes | #32 |
| Backup, verify | `backup`, `verifydata` | `backup`, `restore`, `verify`, `scrub`, `health` | n/a | - |
| Metrics | Prometheus, example rules | Prometheus (`internal/metrics`) | n/a | - |
| Logging | Levels per package, runtime change, trace levels, cid | slog; traces under `dev` | no | #22 |
| Autoconfig | Thunderbird, Autodiscover, Apple profile with QR, SRV | Thunderbird, Outlook POX, Apple profile, SRV printed | partly yes | - |
| ACME | Any directory, EAB, CAA hint, tls-alpn-01 and http-01 | `autocert` defaults | no | #23 |
| MTA-STS hosting | Yes | Yes | n/a | - |
| Mailing-list friendliness | `ListAllowDomain`, `IsForward` in rulesets | Rspamd; Sieve for filing | yes | #30 |
| Retention / auto-delete | Rejects mailbox only | Trims of change log, uploads, audit; no mail retention | n/a | - |
| Rulesets in place of Sieve | Rulesets | Sieve | n/a | - |
| Built-in web server | Static, proxy, redirects | No | yes | #33 |
| sendmail command, localserve | Yes | No; `make dev` | no | - |

## Operations and safety

| Feature | mox | robinmail | On purpose | Issue |
|---|---|---|---|---|
| Privilege drop | `User` in config, re-exec | No | no | #24 |
| systemd unit | Printed by `config printservice` | No | no | #24 |
| Sandboxing | Roadmap | No | n/a | - |
| Fuzzing | 12 files with fuzz tests | Fuzz tests in imap, message, sieve-adjacent packages (10 files) | n/a | - |
| Single binary | Yes, nothing else needed | One binary plus rqlite and Rspamd | yes | #29 |
| Upgrades | `test-upgrade.sh` over all releases | Migrations, untested across versions | no | #25 |
| Releases, version, reproducible build | Yes | No tags, no `version` | no | #25 |
| Licence | MIT | None | open question | #28 |
| Real provider and client tests | Claimed in README | Lab only | open question | #26 |
| Documentation | Generated config and command reference, RFC table | README, PLAN.md, example JSON | no | #27 |

## What robinmail has that mox does not

JMAP; Sieve and vacation; a cluster on rqlite with replicated blobs; message files encrypted per account with key rotation; labels; TOTP, app passwords, OIDC; forwarding with SRS; a full-text index; quota on message count; an automated lab with Exim, OpenSMTPD and a driven Thunderbird; scrub; an audit log.

## Issues

| # | Title | Area | Size | Kind |
|---|---|---|---|---|
| 1 | SMTP: REQUIRETLS | smtp | medium | gap |
| 2 | SMTP: FUTURERELEASE and LIMITS | smtp | small | gap |
| 3 | Outbound queue: delayed notice, hold, fail, history | smtp | medium | gap |
| 4 | Rejects mailbox | smtp | medium | gap |
| 5 | Outbound transports | smtp | medium | gap |
| 6 | Suppression list and daily limits | smtp | medium | gap |
| 7 | Webhooks | smtp | medium | gap |
| 8 | DMARC aggregate reports | smtp | medium | gap |
| 9 | Address options | smtp | small | gap |
| 10 | Train Rspamd from Junk moves | smtp | small | gap |
| 11 | IMAP QRESYNC | imap | medium | gap |
| 12 | IMAP NOTIFY | imap | medium | gap |
| 13 | IMAP METADATA | imap | medium | gap |
| 14 | IMAP smaller extensions | imap | medium | gap |
| 15 | Webmail features | webmail | medium | gap |
| 16 | SCRAM | auth | medium | gap |
| 17 | TLS client certificates | auth | medium | gap |
| 18 | Account controls | auth | small | gap |
| 19 | Web admin interface | admin | large | gap |
| 20 | Quickstart | admin | medium | gap |
| 21 | DNS check, CAA/TLSA, DNSBL watch | admin | medium | gap |
| 22 | Logging | admin | small | gap |
| 23 | ACME options, ALPN on 443 | admin | small | gap |
| 24 | Privilege drop, systemd unit | ops | small | gap |
| 25 | Releases and upgrades | ops | medium | gap |
| 26 | Real providers and clients | ops | medium | gap |
| 27 | Docs | docs | medium | gap |
| 28 | Licence | docs | small | gap |
| 29 | Mail auth in Rspamd | smtp | - | decision |
| 30 | Junk filtering in Rspamd | smtp | - | decision |
| 31 | No DANE | smtp | - | decision |
| 32 | No mbox/Maildir import or export | admin | - | decision |
| 33 | No general web server | admin | - | decision |
| 34 | ASCII-only local addresses | smtp | - | decision |
| 35 | Parity with mox: tracking | - | - | tracking |

## Not determined

- Whether a hosted domain can be an IDN in robinmail (not tested; noted in #34).
- What `web-ng/` covers; it was in flux.
- Drag and drop and thread muting in `web/` were judged by a text search of `web/src`, not by running the webmail.
- mox's app passwords, TOTP and OIDC were judged absent by a source search for those words.
- mox's behaviour was read from docs and source, not run.

## Security

No exploitable weakness in robinmail came out of this comparison, so no private notes file was written. Hardening features (SCRAM, client certificates, privilege drop) were filed as features: #16, #17, #24.

## Choices made without asking

- Internationalised local parts and DANE were filed as `decision`, not as gaps, because PLAN.md states them as deliberate.
- CRAM-MD5 and SCRAM-SHA-1 are named in #16 as not worth adding.
- Licence (#28) and real-provider tests (#26) are PLAN.md open questions; filed as gaps so they show in the tracking table.
- No `jmap` issue: mox has no JMAP. The label exists and is unused.
- `./robinmail` was run once without arguments (prints usage, touches nothing). No server, test or write in the checkout.
