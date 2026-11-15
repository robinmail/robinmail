# robinmail documentation

Short pages for running and understanding robinmail. The design record, phase by phase,
is [PLAN.md](../PLAN.md); these pages point into it and do not repeat it.

| Page | What it answers |
|---|---|
| [Getting started](getting-started.md) | How do I run a development instance and log in? |
| [Architecture](architecture.md) | What are the parts and where does the data live? |
| [Configuration](configuration.md) | What does each block of `robinmail.json` do? |
| [Webmail](webmail.md) | The Vue webmail and the Angular one, and how to build each. |
| [Testing](testing.md) | Which test suite covers what, and what each needs. |
| [Security](security.md) | What the review of 2026-11-15 found, and what is still open. |
| [Compared with mox](mox-parity.md) | What mox has that robinmail has not, and where that is on purpose. |

robinmail is a learning project. It has never exchanged mail with a server on the
internet. Read [Security](security.md) before it faces one.
