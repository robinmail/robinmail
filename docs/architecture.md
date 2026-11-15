# Architecture

```
            25            465/587        993/143          443
             |               |              |              |
   +---------+---------------+--------------+--------------+---------+
   |  inbound SMTP    submission          IMAP      JMAP, login,     |
   |                                                webmail          |
   |                         robinmail (one Go binary)               |
   |  delivery, Sieve, outbound queue, push, search, blob copies     |
   +------+-----------------------+-----------------------+----------+
          |                       |                       |
       rqlite                 blob files               Rspamd
   (all metadata)        (encrypted, per node)    (spam, SPF, DKIM, DMARC)
```

## One binary

`cmd/robinmail` holds every listener and every worker. It uses the Go standard library
and `golang.org/x`, nothing else. The packages are under `internal/`, one per concern:
`smtp`, `submission`, `queue`, `imap`, `jmap`, `sieve`, `store`, `blob`, `web`, `jose`,
`oidc`, `message`, `rqlite`.

## Metadata: rqlite

rqlite is SQLite replicated with Raft, reached over HTTP. robinmail has its own small
client for it. There are no interactive transactions, so every write is one atomic batch
that computes ids, counters, the thread and the IMAP UID in SQL.

- Each account has a `modseq` and a change log. JMAP state strings, `/changes` and IMAP
  CONDSTORE all come from it.
- A delivery is idempotent: a second attempt with the same id changes nothing.
- A message can be in several mailboxes (labels). Each placement has its own IMAP UID.

## Messages: files

A message is a file at `<data_dir>/blobs/<account>/<2 hex>/<sha256>`, encrypted with
XChaCha20-Poly1305 under a key per account. The account key is wrapped by the master key
file. There is no object store.

## A cluster

Several robinmail nodes share one rqlite cluster. Any node serves any account.

- Nodes find each other through a node table in rqlite and talk over mutual TLS pinned
  to that table.
- A delivery is acknowledged when the message file is on two nodes.
- A repair pass copies what a node misses. A sweeper removes files nothing refers to.

rqlite is the crown jewel: whoever reaches its HTTP port without a login owns every
account. A deployment keeps it on loopback with a login and with TLS between rqlite
nodes. See "rqlite, locked down" in [PLAN.md](../PLAN.md).

## What is encrypted at rest

| Data | Encrypted |
|---|---|
| Message files | yes |
| Addresses, subjects, previews, full-text index (rqlite) | no; use an encrypted disk |

## Protocols

| Protocol | Notes |
|---|---|
| SMTP in (25) | Rspamd decides spam; Sieve filters; aliases with SRS |
| Submission (465, 587) | app passwords, undo send, per-hour limit |
| Outbound | MX, MTA-STS, retries for five days, bounces as RFC 3464 |
| JMAP | RFC 8620, RFC 8621, EventSource push, Sieve (RFC 9661), vacation, quotas |
| IMAP | IMAP4rev1 and rev2, IDLE, CONDSTORE |
| Login | Argon2id passwords, TOTP with recovery codes, app passwords, optional OIDC |
