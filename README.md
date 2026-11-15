# robinmail

A mail, calendar and contacts server in Go that is built around JMAP. It receives mail
over SMTP, stores it, sends it, and serves it over JMAP (RFC 8620, RFC 8621) and IMAP;
calendars and address books over JMAP (JMAP for Calendars and Contacts) and over CalDAV
and CardDAV, with invitations by email. The webmail, in Angular, ships inside the binary;
the first webmail, in Vue, can still be built in instead.

Documentation: https://robinmail.github.io/docs/ (source: [robinmail/docs](https://github.com/robinmail/docs)).

## What it is, and what it is not

robinmail is a learning project. Read this before you run it anywhere that matters.

- **Not production-hardened.** Nobody has run it under real load, and no human has
  audited it. An AI session reviewed the server on 2026-11-15; what it found and what is
  still open is on the [security page](https://robinmail.github.io/docs/security/).
- **Never tested against real providers.** It has exchanged mail with Postfix, Exim and
  OpenSMTPD, been read by Thunderbird, mbsync, OfflineIMAP and Dovecot's ImapTest, and
  synced calendars and contacts with Thunderbird, vdirsyncer and the Python caldav
  library, all in a closed lab on one machine. No iPhone, Mac, Outlook or DAVx5 has
  been pointed at it. It has never delivered a message to Gmail,
  Outlook, or any other server on the internet. How they treat it is unknown.
- **No stable formats.** The config, the database schema and the commands change without
  notice. There are migrations between schema versions, and no promise.
- **Licence: AGPL-3.0-or-later** ([LICENSE](LICENSE)). You may use, change and share it.
  Whoever runs a changed robinmail for other people must offer them its source.

What works is listed, phase by phase, in [PLAN.md](PLAN.md), with what was left out and
why. That file is the design document and the record of what was built and tested.

## Architecture

- **One binary**, `cmd/robinmail`: inbound SMTP, submission, the outbound queue, IMAP,
  JMAP, CalDAV and CardDAV, the web login and the webmail. Standard library plus `golang.org/x`,
  and since phase 10 etcd's Raft library and a pure-Go SQLite (no cgo).
- **The database is inside the binary**: SQLite, replicated between the nodes with Raft
  (`internal/raftdb`), in `<data_dir>/db`. Every write is one atomic batch, ordered by the
  Raft leader and applied on every node; reads are linearizable. There is no database
  service and no SQL port: Raft travels over the nodes' mutual TLS, and a command on a
  node reaches its server over loopback with the node's own key.
- **Messages as files** on each node's own disk, encrypted with a key per account. Nodes
  copy them to each other over mutual TLS. No object store.
- **Any node serves any account.** A cluster is three (or five) robinmail nodes and
  nothing else: `robinmail node add` on a member admits one, `robinmail node join` on the
  new node joins.
- **Spam filtering, SPF, DKIM and DMARC are Rspamd's**, asked over HTTP.
- **Calendars and contacts** are stored once, as iCalendar and vCard text in the database;
  JMAP gets them as JSCalendar and JSContact. Invitations go out as email (iMIP) and are
  applied when they come in.
- **The webmail** (`web-ng/`, Angular) only speaks JMAP and is embedded in the binary:
  mail, calendar and contacts. Its browser tests (Playwright) are in `e2e/`.

What is encrypted at rest: the message files. What is not: the database, which
includes addresses, subjects, previews and the full-text index. Those need an encrypted
disk.

## Run a development instance

You need Go and Node; Docker for Mailpit and Rspamd (optional).

```bash
docker compose -f contrib/compose/compose.yaml up -d
```

```bash
make dev
```

Open http://localhost:4200 and log in as `dev@robinmail.test`. The password is in
`contrib/dev/seed.sh`. The instance lives in `./dev`, accepts mail on `127.0.0.1:2525`,
and sends everything for other domains to Mailpit (http://127.0.0.1:8025), so it never
sends real mail.

An instance from before phase 10 used rqlite; `contrib/dev/seed.sh` sets its config aside
and starts a fresh one (nothing is carried over).

A cluster: give each node a `node` section (id, listen, address) and the same master key
file. Start the first; on each new node run `robinmail node id`, on the first run
`robinmail node add <id> <address> <certificate>` with what it printed, then on the new
node the `robinmail node join` line that printed, and start it. Add the nodes one at a
time. `robinmail node list` shows the members and the leader.

For the binary with the webmail inside:

```bash
make
```

```bash
./robinmail -config dev/robinmail.json serve
```

`robinmail` without arguments lists the commands. `robinmail.example.json` shows every
setting; `robinmail dns <domain>` prints the DNS records a domain needs, the SRV records
of CalDAV and CardDAV included.

Calendar and contact programs use `https://<hostname>/dav/` (or find it from
`/.well-known/caldav` and `/.well-known/carddav`) with the address and an app password
that allows "dav". On an iPhone or a Mac, the profile on the webmail's security page
sets up mail, calendars and contacts at once.

## Tests

| Command | What it runs | Needs |
|---|---|---|
| `make test` | `go vet`, the Go tests, and the unit tests of the webmail | see below |
| `make e2e` | the webmail in a browser (Playwright) against an instance like the dev one, on port 8085 | Mailpit for one test |
| `make cluster-test` | three robinmail nodes; the database leader and other nodes are killed and restarted | Docker |
| `make lab-test` | SMTP with Postfix, Exim and OpenSMTPD; IMAP with ImapTest, mbsync, OfflineIMAP and Thunderbird; CalDAV and CardDAV with Thunderbird, vdirsyncer and the Python caldav library; invitations to Postfix and Exim | Docker |
| `make thunderbird-test` | only the Thunderbird part of the lab | Docker |

The Go tests make their own databases, one node or an in-process cluster of several, in
temporary directories:

```bash
go test -race ./...
```

The end-to-end test of `serve` with Rspamd also wants
`ROBINMAIL_TEST_RSPAMD=http://127.0.0.1:11333` and is skipped without it.

`make e2e` runs the server on port 8085, so a development server on 8080 can stay up.
`make e2e-vue` runs the same specs against the Vue webmail; the calendar and contacts
specs skip there.

The lab and the cluster have no route to the internet: their name server knows no name
outside them.

## Inspiration

robinmail is inspired by [mox](https://github.com/mjl-/mox), a mail server in Go by Mechiel
Lukkien. mox showed that a complete, modern mail server can be one Go binary on the standard
library, and it is what robinmail compares itself with.

mox exists thanks to public funding for open internet software: NLnet foundation, the
European Commission's Next Generation Internet (NGI) programme, and the Netherlands
Ministry of the Interior and Kingdom Relations. The funds behind it:

- [NLnet NGI0 Entrust](https://nlnet.nl/entrust/) (2023/2024): [Mox](https://nlnet.nl/project/Mox/)
- [NLnet e-Commons Fund](https://nlnet.nl/commonsfund/) (2024): [Mox API](https://nlnet.nl/project/Mox-API/)
- [NLnet NGI0 Core](https://nlnet.nl/core/) (2024/2025): [Mox Automation](https://nlnet.nl/project/Mox-Automation/)

## Where to read on

- [The documentation site](https://robinmail.github.io/docs/): getting started,
  architecture, configuration, commands, webmail, testing, security, and a comparison
  with mox.
- [PLAN.md](PLAN.md): decisions, the store, every phase with what was built, how it was
  tested and what is not handled, and the open questions.
- `Makefile`: the targets, with what each needs.
- `contrib/`: the compose files, the development instance, the lab, the cluster.
