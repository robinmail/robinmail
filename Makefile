# robinmail: the server in Go and the webmail in web-ng/ (Angular, built
# with Node).
#
#   make            the webmail, then ./robinmail with the webmail inside
#   make web        only the webmail: web-ng/dist/shell, copied to internal/web/dist
#   make test       go vet, the Go tests, and the unit tests of web-ng/
#   make e2e        the browser tests of e2e/ against ./robinmail and the dev instance
#   make keycloak-test  the browser test of the OpenID Connect login against a Keycloak container
#   make dev        a development instance with live reload (ng serve on http://localhost:4200)
#   make cluster-test   the cluster tests on three robinmail nodes in Docker, killed and restarted
#   make lab-test       the interoperability tests in Docker: Postfix, Exim and OpenSMTPD over SMTP,
#                       Dovecot's ImapTest, mbsync, OfflineIMAP and Thunderbird over IMAP,
#                       CalDAV and CardDAV clients
#   make thunderbird-test   only the Thunderbird tests of the lab
#   make clean
#
# A plain "go build ./cmd/robinmail" needs no Node and no make: that binary
# answers with a placeholder page instead of the webmail. The build tag
# "webmail" embeds internal/web/dist, the Angular build. That directory and
# web-ng/dist are build output and not in git. (A first webmail in Vue,
# web/, was removed on 2026-11-15; git history has it.)
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
# without asking. The first time: cd e2e && npm ci && npx playwright install chromium
# It takes about three minutes: the tests that send mail wait for the
# server's undo time and its queue. Mail goes between the two users of the
# instance (dev@ and other@robinmail.test); one test sends to an outside
# address, which the instance routes to Mailpit from contrib/compose
# (http://127.0.0.1:8025), and is skipped when Mailpit is not running.
# Nothing leaves loopback or the Docker network.

GO ?= go
NPM ?= npm

WEB_NG_SRC := $(shell find web-ng/projects web-ng/scripts web-ng/angular.json web-ng/package.json web-ng/package-lock.json web-ng/tsconfig.json web-ng/.postcssrc.json -type f 2>/dev/null)

.PHONY: all build web test test-go test-web e2e dev clean cluster-test lab-test thunderbird-test keycloak-test

all: build

# The Angular webmail. "npm run build" builds the two libraries with
# ng-packagr and the application from them, and moves the files named by
# their content to assets/, where the server caches them for a year.
build: internal/web/dist/index.html
	$(GO) build -tags webmail -o robinmail ./cmd/robinmail

web: internal/web/dist/index.html

web-ng/node_modules: web-ng/package.json web-ng/package-lock.json
	cd web-ng && $(NPM) ci
	@touch web-ng/node_modules

# The copy is what go:embed reads: a pattern cannot reach outside its own
# package directory.
internal/web/dist/index.html: web-ng/node_modules $(WEB_NG_SRC)
	cd web-ng && $(NPM) run build
	rm -rf internal/web/dist
	cp -R web-ng/dist/shell internal/web/dist

test: test-go test-web

# With and without the webmail in the binary.
test-go: internal/web/dist/index.html
	$(GO) vet ./...
	$(GO) vet -tags webmail ./...
	$(GO) test -race ./...
	$(GO) test -race -tags webmail ./internal/web ./cmd/robinmail

test-web: web-ng/node_modules
	cd web-ng && $(NPM) test

# On 127.0.0.1:8085, so that a development server on 8080 can stay up.
e2e/node_modules: e2e/package.json e2e/package-lock.json
	cd e2e && $(NPM) ci
	@touch e2e/node_modules

e2e: build e2e/node_modules
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
	rm -rf robinmail internal/web/dist web-ng/dist e2e/test-results
