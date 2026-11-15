# Security

## Status

A review was done on 2026-11-15 by an AI session that did not write the code. It covered
the Go server; the webmail client code was left out on purpose. No human has audited
robinmail.

| Severity | Found | Fixed | Open |
|---|---|---|---|
| Critical | 0 | 0 | 0 |
| High | 1 | 1 | 0 |
| Medium | 7 | 5 | 2 |
| Low | 8 | 1 | 7 |

Every finding of medium severity or higher is a public issue. The tracking issue is
[mikepage/robinmail#44](https://github.com/mikepage/robinmail/issues/44).

**Verdict of the review:** fit to face the internet with throwaway accounts. Not yet with
real mail.

## What the review found no way to do

- Read another account's mail, over JMAP, IMAP, downloads or the HTML view.
- Take over an account, or relay mail as a stranger.
- Run script in the webmail through an HTML message.
- Smuggle a message through SMTP line endings or inject commands before STARTTLS.

## What was fixed

- Login rate limits could be outrun by many attempts at once (high).
- An IPv6 client could avoid limits by changing address within its /64.
- JMAP result references could grow a request past the size limit.
- A Sieve script could make one run compare an unbounded amount of text.
- A submitted message could carry more than one From or Sender field.
- The outbound queue and TLS reports could be pointed at internal addresses.

## What is open

See the issues for the options and the recommendation for each.

- The process can be driven out of memory through the SMTP listener on a small machine.
  Lower `smtp.max_connections` and `smtp.max_message_bytes` to fit the memory you have.
- Anybody can lock an account out of the web login by sending wrong passwords.
- A panic in one SMTP or IMAP session ends the whole process.

## The model, in short

- **Passwords:** Argon2id. **Sessions:** a cookie that is `Secure`, `HttpOnly` and
  `SameSite=Strict`, plus an Origin check on every request that changes state.
- **Mail programs:** app passwords, random, stored as a hash, revocable one by one.
- **Two steps:** TOTP with recovery codes; optional OIDC login for the webmail.
- **At rest:** message files are encrypted per account under a master key. Metadata in
  rqlite is not; it holds addresses, subjects, previews and the search index.
- **rqlite:** on loopback, with a login, TLS between its nodes. Whoever can write to it
  owns every account.
- **Between nodes:** mutual TLS, pinned to the node table.
- **Outbound:** no connection to an address that is not public, unless the config allows it.

## Before it faces the internet

1. Lock down rqlite as PLAN.md describes.
2. Put the data and the rqlite directory on an encrypted disk.
3. Keep the master key file apart from the backups of the data.
4. Set the SMTP limits to fit the machine's memory.
5. Start with throwaway accounts.

## Reporting a problem

Open an issue on GitHub. This is an experiment built in public; there is no private
channel.
