# The Angular webmail: one workspace with the libraries @robinmail/jmap and
# @robinmail/webmail (built with @angular/build:ng-packagr into dist/) and
# the application "shell" that only starts the router with the library's
# routes (dist/shell).
#
#   make            both libraries, then the shell from the built libraries
#   make test       the unit tests of both libraries (Vitest, the runner of ng test)
#   make dev        ng serve on http://localhost:4200 from the library sources,
#                   passing /jmap, /.well-known/jmap and /auth on to the Go
#                   server on 127.0.0.1:8080 (ROBINMAIL_DEV_URL for another);
#                   that server must have http://localhost:4200 in web.dev_origins
#   make clean

NPM ?= npm

.PHONY: all build test dev clean

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

clean:
	rm -rf dist out-tsc .angular
