# Getting started

You need Go, Node and Docker.

## 1. Start the services

```bash
docker compose -f contrib/compose/compose.yaml up -d
```

This starts rqlite (three databases: `4001` and `4003` for tests, `4005` for the
development instance), Mailpit (`1025`, web on `8025`) and Rspamd (`11333`).

## 2. Build and seed

```bash
make
```

```bash
contrib/dev/seed.sh
```

`make` builds `./robinmail` with the Vue webmail inside. The seed script creates the
instance in `./dev` with two accounts, `dev@robinmail.test` and `other@robinmail.test`.
The passwords are set in `contrib/dev/seed.sh` (`ROBINMAIL_DEV_PASSWORD` overrides the
first). The dev config names no Rspamd, so the instance does not scan or sign mail, even
with Rspamd running.

## 3. Run

```bash
./robinmail -config dev/robinmail.json serve
```

| What | Where |
|---|---|
| Webmail and JMAP | http://127.0.0.1:8080 |
| IMAP | `1993` (TLS), `1143` (STARTTLS) |
| Submission | `1465` (TLS), `1587` (STARTTLS) |
| Inbound SMTP | `127.0.0.1:2525` |
| Mail for other domains | goes to Mailpit, http://127.0.0.1:8025 |

The development instance never sends mail to the internet.

## A mail program

Add the account in Thunderbird with the ports above. The account password works until
the account has an app password or two-step login; from then on a mail program needs an
app password (webmail, Security). `auth.require_app_passwords` makes that the rule from
the start. IMAP and submission use a self-signed certificate, so
the program asks once to accept it.

## With live reload

```bash
make dev
```

Vite serves the Vue webmail on http://localhost:5173 next to the Go server.

## Commands

`robinmail` without arguments lists the commands: accounts, domains, aliases, backup and
restore, `robinmail dns <domain>` for the DNS records a domain needs.
