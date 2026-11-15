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
- **No licence.** The repository has no LICENSE file, so the default applies: you may
  read the code, and nothing grants you the right to use, change or distribute it.

What works is listed, phase by phase, in [PLAN.md](PLAN.md), with what was left out and
why. That file is the design document and the record of what was built and tested.

## Architecture

- **One binary**, `cmd/robinmail`: inbound SMTP, submission, the outbound queue, IMAP,
  JMAP, CalDAV and CardDAV, the web login and the webmail. Standard library plus `golang.org/x`, nothing else.
- **Metadata in rqlite** (SQLite replicated with Raft), reached over HTTP. Every write is
  one atomic batch; there are no interactive transactions.
  Whoever reaches rqlite's HTTP port without a login owns every account, so a deployment
  keeps it on loopback next to each node, with a login and with TLS between rqlite nodes
  (PLAN.md, "rqlite, locked down"). The development services have neither.
- **Messages as files** on each node's own disk, encrypted with a key per account. Nodes
  copy them to each other over mutual TLS. No object store.
- **Any node serves any account.** A cluster is several robinmail nodes on one rqlite
  cluster.
- **Spam filtering, SPF, DKIM and DMARC are Rspamd's**, asked over HTTP.
- **Calendars and contacts** are stored once, as iCalendar and vCard text in rqlite;
  JMAP gets them as JSCalendar and JSContact. Invitations go out as email (iMIP) and are
  applied when they come in.
- **The webmail** (`web-ng/`, Angular) only speaks JMAP and is embedded in the binary:
  mail, calendar and contacts. The Vue webmail in `web/` has mail only and is built with
  `make build-vue`.

What is encrypted at rest: the message files. What is not: everything in rqlite, which
includes addresses, subjects, previews and the full-text index. Those need an encrypted
disk.

## Run a development instance

You need Go, Node and Docker.

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
| `make e2e` | the webmail in a browser (Playwright) against the dev instance, on port 8085 | the compose services |
| `make cluster-test` | three rqlite and three robinmail nodes; nodes are killed and restarted | Docker |
| `make lab-test` | SMTP with Postfix, Exim and OpenSMTPD; IMAP with ImapTest, mbsync, OfflineIMAP and Thunderbird; CalDAV and CardDAV with Thunderbird, vdirsyncer and the Python caldav library; invitations to Postfix and Exim | Docker |
| `make thunderbird-test` | only the Thunderbird part of the lab | Docker |

The Go tests that need a database are skipped unless they are told where one is:

```bash
ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RQLITE_B=http://127.0.0.1:4003 go test -race ./...
```

Both databases come from the compose file above. A few tests (backup and restore, master
key rotation) start an rqlite container of their own and are skipped without Docker.

`make e2e` runs the server on port 8085, so a development server on 8080 can stay up.
`make e2e-vue` runs the same specs against the Vue webmail; the calendar and contacts
specs skip there.

The lab and the cluster have no route to the internet: their name server knows no name
outside them.

## Where to read on

- [The documentation site](https://robinmail.github.io/docs/): getting started,
  architecture, configuration, commands, webmail, testing, security, and a comparison
  with mox.
- [PLAN.md](PLAN.md): decisions, the store, every phase with what was built, how it was
  tested and what is not handled, and the open questions.
- `Makefile`: the targets, with what each needs.
- `contrib/`: the compose files, the development instance, the lab, the cluster.
