#!/bin/sh
# Runs the browser tests of web/e2e against ./robinmail (built by "make")
# and the development instance of seed.sh. Arguments go to Playwright:
#
#   contrib/dev/e2e.sh               all tests
#   contrib/dev/e2e.sh auth          the tests in files with "auth" in the name
#
# Besides the server it starts the fake OpenID provider of
# contrib/dev/oidc-provider, for the "Log in with" button, and stops both
# when the tests are done. See e2e-config.mjs for the config it runs with.
#
# The server listens on 127.0.0.1:8080 like the development instance. When
# one is running there already, ROBINMAIL_E2E_PORT=8085 puts this one on
# another port (and its SMTP on the next); both then work on the same
# database, as two nodes would.
set -eu
cd "$(dirname "$0")/../.."
PORT=${ROBINMAIL_E2E_PORT:-8080}
export ROBINMAIL_E2E_PORT="$PORT"

contrib/dev/seed.sh
go build -o dev/oidc-provider ./contrib/dev/oidc-provider
node contrib/dev/e2e-config.mjs

dev/oidc-provider -listen 127.0.0.1:8090 &
provider=$!
./robinmail -config dev/robinmail.e2e.json serve &
server=$!
stop() {
	kill "$server" "$provider" 2>/dev/null || true
}
trap stop EXIT
sleep 1

cd web
ROBINMAIL_E2E_URL="http://127.0.0.1:$PORT" npx playwright test "$@"
