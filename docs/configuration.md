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
| `tls` | Certificate and key files, or `acme` for automatic certificates (ACME is untested against a real CA). |
| `smtp` | The inbound listener and its limits: message size, recipients, connections, connections per address. |
| `submission` | Ports 587 and 465, recipients per hour, seconds of undo send. |
| `imap` | The TLS and STARTTLS listeners and connection limits per address and per user. |
| `web` | The HTTPS listener for JMAP, login and the webmail. `dev_origins` is for development only. |
| `rspamd` | Where Rspamd is. `fail_open` decides what happens to mail when it does not answer. |
| `outbound` | Queue concurrency, fixed `routes` (the development instance routes everything to Mailpit), and `allow_private_addresses`, which is off so the queue never connects to internal addresses. |
| `auth` | `require_app_passwords` for mail programs, audit log retention, OIDC providers for the web login. |
| `quota` | Default bytes and messages per account; `0` is no limit. |
| `mta_sts` | The policy robinmail serves for its domains. |
| `tls_rpt` | The TLS reports it sends. |
| `metrics` | The metrics listener; keep it on loopback. |

## DNS

```bash
./robinmail -config robinmail.json dns example.org
```

prints the MX, SPF, DKIM, DMARC, MTA-STS, TLS-RPT and autoconfig records the domain needs.
