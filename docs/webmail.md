# Webmail

There are two webmails with the same screens. Both speak only JMAP and the `/auth` API.

| | Vue | Angular |
|---|---|---|
| Directory | `web/` | `web-ng/` |
| Stack | Vue 3, Vite, Nuxt UI | Angular 22.2, Tailwind 4, Angular CDK, own components |
| Build | `make` | `make build-ng` |
| Binary | `./robinmail` | `./robinmail-ng` (build tag `webmail_ng`) |
| Unit tests | `make test-web` | `make test-ng` |
| Browser tests | `make e2e` | `make e2e-ng` |
| Live reload | `make dev` | `make dev-ng` |

The default binary carries the Vue webmail. A plain `go build` needs no Node.

## The Angular workspace

- `projects/jmap` is the library `@robinmail/jmap`: JMAP client, sync, push, mail and
  compose stores, the `/auth` API, Sieve rules.
- `projects/webmail` is the library `@robinmail/webmail`: every screen. It exports
  `webmailRoutes` and `provideWebmail()`.
- `projects/shell` is a thin app that starts the router with those routes.

The libraries are built with `@angular/build:ng-packagr`. A builder named
`@angular/build:library` does not exist in Angular 22.2.

The browser tests in `web/e2e` run against both webmails. Against the Angular build: 27
pass, 14 are skipped, the same as against Vue.

## HTML mail

A message's HTML is cleaned by an allowlist on the server and shown in a sandboxed frame
from its own URL, with a content security policy that forbids script. Remote images are
off until the reader turns them on.
