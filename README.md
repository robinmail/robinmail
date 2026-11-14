# robinmail

A mail server in Go that is built around JMAP. It receives mail over SMTP, stores it,
sends it, and serves it over JMAP (RFC 8620, RFC 8621) and IMAP. A webmail in Vue ships
inside the binary.

## What it is, and what it is not

robinmail is a learning project. Read this before you run it anywhere that matters.

- **Not production-hardened.** Nobody has run it under real load, attacked it on purpose,
  or audited it.
- **Never tested against real providers.** It has exchanged mail with Postfix, Exim and
  OpenSMTPD, and been read by Thunderbird, mbsync, OfflineIMAP and Dovecot's ImapTest,
  all in a closed lab on one machine. It has never delivered a message to Gmail,
  Outlook, or any other server on the internet. How they treat it is unknown.
- **No stable formats.** The config, the database schema and the commands change without
  notice. There are migrations between schema versions, and no promise.
- **No licence.** The repository has no LICENSE file, so the default applies: you may
  read the code, and nothing grants you the right to use, change or distribute it.

What works is listed, phase by phase, in [PLAN.md](PLAN.md), with what was left out and
why. That file is the design document and the record of what was built and tested.

## Architecture

- **One binary**, `cmd/robinmail`: inbound SMTP, submission, the outbound queue, IMAP,
  JMAP, the web login and the webmail. Standard library plus `golang.org/x`, nothing else.
- **Metadata in rqlite** (SQLite replicated with Raft), reached over HTTP. Every write is
  one atomic batch; there are no interactive transactions.
- **Messages as files** on each node's own disk, encrypted with a key per account. Nodes
  copy them to each other over mutual TLS. No object store.
- **Any node serves any account.** A cluster is several robinmail nodes on one rqlite
  cluster.
- **Spam filtering, SPF, DKIM and DMARC are Rspamd's**, asked over HTTP.
- **The webmail** (`web/`, Vue with Nuxt UI) only speaks JMAP and is embedded in the
  binary.

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

Open http://localhost:5173 and log in as `dev@robinmail.test`. The password is in
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
setting; `robinmail dns <domain>` prints the DNS records a domain needs.

## Tests

| Command | What it runs | Needs |
|---|---|---|
| `make test` | `go vet`, the Go tests, type check and unit tests of the webmail | see below |
| `make e2e` | the webmail in a browser (Playwright) against the dev instance | the compose services |
| `make cluster-test` | three rqlite and three robinmail nodes; nodes are killed and restarted | Docker |
| `make lab-test` | SMTP with Postfix, Exim and OpenSMTPD; IMAP with ImapTest, mbsync, OfflineIMAP and Thunderbird | Docker |
| `make thunderbird-test` | only the Thunderbird part of the lab | Docker |

The Go tests that need a database are skipped unless they are told where one is:

```bash
ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RQLITE_B=http://127.0.0.1:4003 go test -race ./...
```

Both databases come from the compose file above. A few tests (backup and restore, master
key rotation) start an rqlite container of their own and are skipped without Docker.

`make e2e` wants port 8080. With a development server running there, use another port:

```bash
ROBINMAIL_E2E_PORT=8085 contrib/dev/e2e.sh
```

The lab and the cluster have no route to the internet: their name server knows no name
outside them.

## Where to read on

- [PLAN.md](PLAN.md): decisions, the store, every phase with what was built, how it was
  tested and what is not handled, and the open questions.
- `Makefile`: the targets, with what each needs.
- `contrib/`: the compose files, the development instance, the lab, the cluster.
