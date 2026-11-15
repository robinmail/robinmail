# robinmail: the server in Go and the webmail in web-ng/ (Angular, built
# with Node).
#
#   make            the webmail, then ./robinmail with the webmail inside
#   make web        only the webmail: web-ng/dist/shell, copied to internal/web/dist-ng
#   make test       go vet, the Go tests, and the unit tests of web-ng/
#   make e2e        the browser tests of web/e2e against ./robinmail and the dev instance
#   make keycloak-test  the browser test of the OpenID Connect login against a Keycloak container
#   make dev        a development instance with live reload (ng serve on http://localhost:4200)
#   make cluster-test   the cluster tests on three robinmail nodes in Docker, killed and restarted
#   make lab-test       the interoperability tests in Docker: Postfix, Exim and OpenSMTPD over SMTP,
#                       Dovecot's ImapTest, mbsync, OfflineIMAP and Thunderbird over IMAP,
#                       CalDAV and CardDAV clients
#   make thunderbird-test   only the Thunderbird tests of the lab
#   make clean
#
# The Vue webmail in web/ was the first one. Since phase 9 the Angular one
# is the main webmail (it has the calendar and the contacts); the Vue one
# stays buildable:
#
#   make build-vue  the Vue webmail, then ./robinmail-vue with it inside
#   make web-vue    only that webmail: web/dist, copied to internal/web/dist
#   make test-vue   its type check and unit tests, and the Go tests of the binary that has it
#   make e2e-vue    the browser tests against ./robinmail-vue (the calendar and contacts specs skip)
#   make dev-vue    a development instance with Vite on http://localhost:5173
#
# build-ng, web-ng, test-ng, e2e-ng and dev-ng are the names the Angular
# targets had before phase 9; they still work.
#
# A plain "go build ./cmd/robinmail" needs no Node and no make: that binary
# answers with a placeholder page instead of the webmail. The build tag
# "webmail_ng" embeds internal/web/dist-ng, the Angular build; the tag
# "webmail" embeds internal/web/dist, the Vue build. Those directories and
# the dist directories of web/ and web-ng/ are build output and not in git.
#
# The Go tests make their databases themselves: one node, or a cluster of
# several in the test's own process, in temporary directories
# (internal/raftdb/raftdbtest). Nothing needs to run for "make test"; the
# end-to-end test of "serve" with Rspamd wants ROBINMAIL_TEST_RSPAMD (see
# contrib/compose/compose.yaml) and is skipped without it.
#
# Development (make dev): contrib/dev/seed.sh makes an instance in ./dev
# with the users dev@ and other@robinmail.test (password in that script)
# and some mail. The Go server listens on http://127.0.0.1:8080 and ng
# serve on http://localhost:4200, which is the one to open: it serves the
# webmail with live reload and passes the API on to Go. Its database is in
# dev/data/db. The instance also takes a mail client (IMAP on
# 127.0.0.1:1993 and :1143, submission on :1465 and :1587, with a
# self-signed certificate; contrib/dev/seed.sh has the settings) and a
# calendar and contacts client (CalDAV and CardDAV on
# http://127.0.0.1:8080/dav/ with an app password).
#
# make e2e uses an instance like it with the webmail inside ./robinmail, on
# http://127.0.0.1:8085 so that a development server on 8080 can stay up,
# with a config (dev/robinmail.e2e.json) and data directory (dev/e2e-data)
# of its own, seeded the same way; the config adds an OpenID
# provider: a fake one, started with the tests, that logs the dev user in
# without asking. The first time: cd web && npx playwright install chromium
# It takes about three minutes: the tests that send mail wait for the
# server's undo time and its queue. Mail goes between the two users of the
# instance (dev@ and other@robinmail.test); one test sends to an outside
# address, which the instance routes to Mailpit from contrib/compose
# (http://127.0.0.1:8025), and is skipped when Mailpit is not running.
# Nothing leaves loopback or the Docker network.

GO ?= go
NPM ?= npm
WEB_SRC := $(shell find web/src web/public web/index.html web/package.json web/package-lock.json web/vite.config.ts web/tsconfig.json -type f 2>/dev/null)

WEB_NG_SRC := $(shell find web-ng/projects web-ng/scripts web-ng/angular.json web-ng/package.json web-ng/package-lock.json web-ng/tsconfig.json web-ng/.postcssrc.json -type f 2>/dev/null)

.PHONY: all build web test test-go test-web e2e dev clean cluster-test lab-test thunderbird-test keycloak-test \
	build-vue web-vue test-vue e2e-vue dev-vue build-ng web-ng test-ng e2e-ng dev-ng

all: build

# The Angular webmail. "npm run build" builds the two libraries with
# ng-packagr and the application from them, and moves the files named by
# their content to assets/, where the server caches them for a year.
build: internal/web/dist-ng/index.html
	$(GO) build -tags webmail_ng -o robinmail ./cmd/robinmail

web: internal/web/dist-ng/index.html

web-ng/node_modules: web-ng/package.json web-ng/package-lock.json
	cd web-ng && $(NPM) ci
	@touch web-ng/node_modules

# The copy is what go:embed reads: a pattern cannot reach outside its own
# package directory.
internal/web/dist-ng/index.html: web-ng/node_modules $(WEB_NG_SRC)
	cd web-ng && $(NPM) run build
	rm -rf internal/web/dist-ng
	cp -R web-ng/dist/shell internal/web/dist-ng

test: test-go test-web

# With and without the webmail in the binary.
test-go: internal/web/dist-ng/index.html
	$(GO) vet ./...
	$(GO) vet -tags webmail_ng ./...
	$(GO) test -race ./...
	$(GO) test -race -tags webmail_ng ./internal/web ./cmd/robinmail

test-web: web-ng/node_modules
	cd web-ng && $(NPM) test

# On 127.0.0.1:8085, so that a development server on 8080 can stay up.
e2e: build
	ROBINMAIL_E2E_PORT=$${ROBINMAIL_E2E_PORT:-8085} contrib/dev/e2e.sh

# Logs a user in through the webmail with a real OpenID provider: starts
# the Keycloak of contrib/compose/keycloak.yaml (a 500 MB image the first
# time), waits until its realm answers, runs the browser test that uses it
# and removes the container again, also when the test failed. KEEP=1
# leaves it running. Nothing else needs Keycloak; "make e2e" runs that
# test too when the container happens to be up, and skips it when not.
keycloak-test: build
	docker compose -f contrib/compose/keycloak.yaml up -d
	@contrib/compose/keycloak-wait.sh || { docker compose -f contrib/compose/keycloak.yaml down; exit 1; }
	@contrib/dev/e2e.sh keycloak; s=$$?; \
		[ -n "$(KEEP)" ] || docker compose -f contrib/compose/keycloak.yaml down; exit $$s

dev: web-ng/node_modules
	contrib/dev/seed.sh
	@trap 'kill 0' EXIT; dev/robinmail -config dev/robinmail.json serve & \
		cd web-ng && $(NPM) run dev

# The Vue webmail, the first one.
build-vue: internal/web/dist/index.html
	$(GO) build -tags webmail -o robinmail-vue ./cmd/robinmail

web-vue: internal/web/dist/index.html

web/node_modules: web/package.json web/package-lock.json
	cd web && $(NPM) ci
	@touch web/node_modules

# "npm run build" type-checks and writes web/dist.
internal/web/dist/index.html: web/node_modules $(WEB_SRC)
	cd web && $(NPM) run build
	rm -rf internal/web/dist
	cp -R web/dist internal/web/dist

test-vue: internal/web/dist/index.html
	cd web && $(NPM) run typecheck && $(NPM) test
	$(GO) vet -tags webmail ./...
	$(GO) test -race -tags webmail ./internal/web ./cmd/robinmail

e2e-vue: build-vue
	ROBINMAIL_BINARY=./robinmail-vue ROBINMAIL_E2E_WEBMAIL=vue ROBINMAIL_E2E_PORT=$${ROBINMAIL_E2E_PORT:-8085} contrib/dev/e2e.sh

dev-vue: web/node_modules
	contrib/dev/seed.sh
	@trap 'kill 0' EXIT; dev/robinmail -config dev/robinmail.json serve & \
		cd web && $(NPM) run dev

# The names of before phase 9.
build-ng: build
web-ng: web
test-ng: test-web
	$(GO) vet -tags webmail_ng ./...
	$(GO) test -race -tags webmail_ng ./internal/web ./cmd/robinmail
e2e-ng: e2e
dev-ng: dev

# Builds the image, starts the cluster of contrib/compose/cluster.yaml, runs
# the tests in internal/clustertest (they kill and restart containers) and
# removes the cluster again, also when a test failed. About six minutes
# after the first build. KEEP=1 leaves the cluster running.
cluster-test:
	contrib/compose/cluster.sh up
	@ROBINMAIL_TEST_CLUSTER=1 $(GO) test -count=1 -v -timeout 30m ./internal/clustertest; s=$$?; \
		[ -n "$(KEEP)" ] || contrib/compose/cluster.sh down; exit $$s

# Builds the images, starts the lab of contrib/lab/compose.yaml (robinmail
# next to Postfix, Exim and OpenSMTPD, a container of IMAP clients and one
# with Thunderbird, with a DNS and a certificate authority of its own),
# runs the tests in internal/labtest and removes the lab again, also when
# a test failed. About seven minutes after the first build. KEEP=1 leaves the lab running. Nothing leaves the lab's
# network: its name server knows no name outside it.
lab-test:
	contrib/lab/lab.sh up
	@ROBINMAIL_TEST_LAB=1 $(GO) test -count=1 -v -timeout 30m ./internal/labtest; s=$$?; \
		[ -n "$(KEEP)" ] || contrib/lab/lab.sh down; exit $$s

# The same lab, and of its tests only those that drive Thunderbird: about
# a minute and a half once the images are built.
thunderbird-test:
	contrib/lab/lab.sh up
	@ROBINMAIL_TEST_LAB=1 $(GO) test -count=1 -v -timeout 30m -run 'TestThunderbird' ./internal/labtest; s=$$?; \
		[ -n "$(KEEP)" ] || contrib/lab/lab.sh down; exit $$s

clean:
	rm -rf robinmail robinmail-vue robinmail-ng web/dist internal/web/dist web-ng/dist internal/web/dist-ng
