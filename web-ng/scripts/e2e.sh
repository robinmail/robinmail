#!/bin/sh
# Runs the browser tests of web/e2e against the Angular webmail: the same
# specs, the same development instance, another binary. Like
# contrib/dev/e2e.sh, with these differences:
#
#   - the server is ./robinmail-ng ("make build-ng" in the repository root:
#     the Go server with web-ng/dist/shell inside), built here when missing;
#   - it listens on 127.0.0.1:8085 unless ROBINMAIL_E2E_PORT says otherwise,
#     so a development server on 8080 can stay up;
#   - the instance is only seeded when ./dev has none yet.
#
#   web-ng/scripts/e2e.sh               all tests ("make e2e-ng" builds first)
#   web-ng/scripts/e2e.sh mail auth     the tests in files with these words in the name
#   SERVE=1 web-ng/scripts/e2e.sh       no tests: the server stays up until Ctrl-C
set -eu
cd "$(dirname "$0")/../.."
PORT=${ROBINMAIL_E2E_PORT:-8085}
export ROBINMAIL_E2E_PORT="$PORT"

[ -x robinmail-ng ] || make build-ng
[ -x dev/robinmail ] && [ -f dev/robinmail.json ] || contrib/dev/seed.sh
[ -x dev/oidc-provider ] || go build -o dev/oidc-provider ./contrib/dev/oidc-provider
node contrib/dev/e2e-config.mjs

dev/oidc-provider -listen 127.0.0.1:8090 &
provider=$!
./robinmail-ng -config dev/robinmail.e2e.json serve &
server=$!
stop() {
	kill "$server" "$provider" 2>/dev/null || true
}
trap stop EXIT INT TERM
sleep 1

if [ -n "${SERVE:-}" ]; then
	echo "the Angular webmail is on http://127.0.0.1:$PORT (Ctrl-C stops it)"
	wait "$server"
	exit 0
fi
cd web
ROBINMAIL_E2E_URL="http://127.0.0.1:$PORT" npx playwright test "$@"
