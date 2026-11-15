# Configuration

One JSON file, given with `-config`. [`robinmail.example.json`](../robinmail.example.json)
shows every setting; this page says what each block is for.

| Block | What it sets |
|---|---|
| `hostname` | The name the server uses in SMTP and in certificates. |
| `data_dir` | Where message files and local state live. |
| `master_key_file` | The key that wraps every account key. Losing it loses all mail. Back it up apart from the data. |
| `rqlite` | `url` or `urls` of the database, `auth_file` with its login, `ca_file` for its certificate. |
| `node` | For a cluster: this node's `id`, the `listen` and `address` of the node-to-node API, `blob_copies`, the repair interval. |
| `tls` | Certificate and key files, or `acme` (with `acme_email`) for automatic certificates. They are kept on each node's disk under `<data_dir>/acme`. Untested against a real CA. |
| `smtp` | The inbound listener and its limits: message size, recipients, connections, connections per address. |
| `submission` | Ports 587 and 465, recipients per hour, seconds of undo send. |
| `imap` | The TLS and STARTTLS listeners and connection limits per address and per user. |
| `web` | `listen` is the HTTPS listener for JMAP, login and the webmail. `http_listen` is a plain HTTP listener, for development only, like `dev_origins`. |
| `rspamd` | Where Rspamd is, and `timeout_seconds` for its answer. `fail_open` decides what happens to mail when it does not answer. Without a `url`, mail is not scanned and not DKIM-signed. |
| `outbound` | Queue concurrency, fixed `routes` (the development instance routes everything to Mailpit), and `allow_private_addresses`, which is off so the queue never connects to internal addresses. |
| `auth` | `require_app_passwords` for mail programs, audit log retention, OIDC providers for the web login. |
| `quota` | Default bytes and messages per account; `0` is no limit. |
| `mta_sts` | The policy robinmail serves for its domains: `mode` (`testing`, `enforce` or `none`; empty, the default, serves no policy), `max_age_seconds`, and `mx`, the names of our mail servers (default: the hostname). The example file sets `testing`. |
| `tls_rpt` | The TLS reports it sends. |
| `metrics` | The metrics listener; keep it on loopback. |
| `scrub` | How often each node reads all its message files back to find damage (`interval_hours`, default a week), or `disabled`. |
| `dev` | For development and the lab only: `retry_seconds` replaces the queue's retry schedule, `smtp_trace` and `imap_trace` log every command. Never in a real installation. |

## DNS

```bash
./robinmail -config robinmail.json dns example.org
```

prints the MX, SPF, DKIM, DMARC, MTA-STS, TLS-RPT and autoconfig records the domain needs.
