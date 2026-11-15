# Testing

| Command | What it runs | Needs |
|---|---|---|
| `make test` | `go vet`, the Go tests, type check and unit tests of the Vue webmail | compose services for the database tests |
| `make test-ng` | unit tests of the Angular libraries, Go tests with the Angular tag | Node |
| `make e2e`, `make e2e-ng` | a webmail in a real browser (Playwright) | compose services |
| `make cluster-test` | three rqlite and three robinmail nodes; nodes are killed and restarted | Docker |
| `make lab-test` | SMTP with Postfix, Exim, OpenSMTPD; IMAP with ImapTest, mbsync, OfflineIMAP, Thunderbird | Docker |
| `contrib/lab/lab.sh suite` | Fastmail's JMAP-TestSuite against the lab's robinmail; not part of `lab-test` | Docker, the lab up |
| `make thunderbird-test` | only Thunderbird, driven through Marionette | Docker |
| `make keycloak-test` | the OIDC login against a real Keycloak | Docker |

## Go tests with a database

They are skipped unless told where rqlite is:

```bash
ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 ROBINMAIL_TEST_RQLITE_B=http://127.0.0.1:4003 go test -race ./...
```

## Rules the tests follow

- Every protocol package has tests written from the RFC, citing the section.
- The parsers that face the network have fuzz targets; an input that found a bug is kept.
- No test sends mail beyond loopback or a private Docker network. The lab's name server
  knows no name outside the lab.

## What no test covers

- Real providers (Gmail, Outlook and the rest). That needs a host with port 25 and a PTR
  record.
- ACME against a real certificate authority.
- iOS Mail, Apple Mail and Outlook as clients.
