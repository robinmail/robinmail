# robinmail: the server in Go and the webmail in web/ (Vue, built with Node).
#
#   make            the webmail, then ./robinmail with the webmail inside
#   make web        only the webmail: web/dist, copied to internal/web/dist
#   make test       go vet, the Go tests, type check and unit tests of web/
#   make e2e        the browser test against ./robinmail and the dev instance
#   make dev        a development instance with live reload on :5173
#   make cluster-test   the cluster tests on three rqlite and three robinmail nodes in Docker
#   make clean
#
# A plain "go build ./cmd/robinmail" needs no Node and no make: that binary
# answers with a placeholder page instead of the webmail. The webmail gets
# in through the build tag "webmail", which embeds internal/web/dist; that
# directory and web/dist are build output and not in git.
#
# Tests that need rqlite are skipped unless ROBINMAIL_TEST_RQLITE is set:
#
#   docker compose -f contrib/compose/compose.yaml up -d
#   ROBINMAIL_TEST_RQLITE=http://127.0.0.1:4001 make test
#
# The test that sends mail between two servers (cmd/robinmail) also wants
# ROBINMAIL_TEST_RQLITE_B=http://127.0.0.1:4003, the second database of that
# compose file.
#
# Development (make dev): contrib/dev/seed.sh makes an instance in ./dev
# with the user dev@robinmail.test (password in that script) and some mail.
# The Go server listens on http://127.0.0.1:8080 and Vite on
# http://localhost:5173, which is the one to open: it serves web/src with
# live reload and passes /jmap, /.well-known/jmap and /auth on to Go. It
# needs rqlite from contrib/compose, as above.
#
# make e2e uses the same instance, but the webmail inside ./robinmail on
# http://127.0.0.1:8080. The first time: cd web && npx playwright install chromium

GO ?= go
NPM ?= npm
WEB_SRC := $(shell find web/src web/public web/index.html web/package.json web/package-lock.json web/vite.config.ts web/tsconfig.json -type f 2>/dev/null)

.PHONY: all build web test test-go test-web e2e dev clean cluster-test

all: build

build: internal/web/dist/index.html
	$(GO) build -tags webmail -o robinmail ./cmd/robinmail

web: internal/web/dist/index.html

web/node_modules: web/package.json web/package-lock.json
	cd web && $(NPM) ci
	@touch web/node_modules

# "npm run build" type-checks and writes web/dist. The copy is what go:embed
# reads: a pattern cannot reach outside its own package directory.
internal/web/dist/index.html: web/node_modules $(WEB_SRC)
	cd web && $(NPM) run build
	rm -rf internal/web/dist
	cp -R web/dist internal/web/dist

test: test-go test-web

# With and without the webmail in the binary.
test-go: internal/web/dist/index.html
	$(GO) vet ./...
	$(GO) vet -tags webmail ./...
	$(GO) test -race ./...
	$(GO) test -race -tags webmail ./internal/web ./cmd/robinmail

test-web: web/node_modules
	cd web && $(NPM) run typecheck && $(NPM) test

e2e: build
	contrib/dev/seed.sh
	@./robinmail -config dev/robinmail.json serve & pid=$$!; trap 'kill $$pid' EXIT; sleep 1; \
		cd web && ROBINMAIL_E2E_URL=http://127.0.0.1:8080 npx playwright test

dev: web/node_modules
	contrib/dev/seed.sh
	@trap 'kill 0' EXIT; dev/robinmail -config dev/robinmail.json serve & \
		cd web && $(NPM) run dev

# Builds the image, starts the cluster of contrib/compose/cluster.yaml, runs
# the tests in internal/clustertest (they kill and restart containers) and
# removes the cluster again, also when a test failed. About six minutes
# after the first build. KEEP=1 leaves the cluster running.
cluster-test:
	contrib/compose/cluster.sh up
	@ROBINMAIL_TEST_CLUSTER=1 $(GO) test -count=1 -v -timeout 30m ./internal/clustertest; s=$$?; \
		[ -n "$(KEEP)" ] || contrib/compose/cluster.sh down; exit $$s

clean:
	rm -rf robinmail web/dist internal/web/dist
