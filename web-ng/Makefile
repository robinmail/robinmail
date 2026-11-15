# The Angular webmail: one workspace with the libraries @robinmail/jmap and
# @robinmail/webmail (built with @angular/build:ng-packagr into dist/) and
# the application "shell" that only starts the router with the library's
# routes (dist/shell). The Vue webmail in ../web is the one "make" in the
# repository root builds; this one stands next to it.
#
#   make            both libraries, then the shell from the built libraries
#   make test       the unit tests of both libraries (Vitest, the runner of ng test)
#   make dev        ng serve on http://localhost:4200 from the library sources,
#                   with live reload, passing /jmap, /.well-known/jmap and /auth
#                   on to the Go server on 127.0.0.1:8080 (ROBINMAIL_DEV_URL
#                   for another): start that one with "make dev" or
#                   "dev/robinmail -config dev/robinmail.json serve" in the root
#   make go         ../robinmail-ng: the Go server with this webmail in it
#                   ("make build-ng" in the root; the build tag webmail_ng)
#   make e2e        the browser tests of ../web/e2e against that binary on
#                   127.0.0.1:8085, with the development instance in ../dev;
#                   "make e2e ARGS=mail" runs the files with "mail" in the name
#   make clean

NPM ?= npm

.PHONY: all build test dev go e2e clean

all: build

node_modules: package.json package-lock.json
	$(NPM) ci
	@touch node_modules

build: node_modules
	$(NPM) run build

test: node_modules
	$(NPM) test

dev: node_modules
	$(NPM) run dev

go:
	$(MAKE) -C .. build-ng

e2e: go
	scripts/e2e.sh $(ARGS)

clean:
	rm -rf dist out-tsc .angular
