#!/bin/sh
# A JMAP session with curl against the development instance of seed.sh:
# log in, read the session resource, then one request of three method calls
# chained by result references. Needs "serve" running:
#
#   dev/robinmail -config dev/robinmail.json serve
#   contrib/dev/jmap-demo.sh
set -eu
BASE=${ROBINMAIL_DEV_URL:-http://127.0.0.1:8080}
PASSWORD=${ROBINMAIL_DEV_PASSWORD:-robinmail-dev}
JAR=$(mktemp)
trap 'rm -f "$JAR"' EXIT
pretty() { python3 -m json.tool 2>/dev/null || cat; }

echo "== POST /auth/login"
curl -sS -i -c "$JAR" -H 'Content-Type: application/json' \
	-d "{\"username\":\"dev@robinmail.test\",\"password\":\"$PASSWORD\"}" "$BASE/auth/login" |
	sed -e 's/\(robinmail=\)[A-Za-z0-9_-]*/\1<token>/' -e 's/\r$//'
echo
ACCOUNT=$(curl -sS -b "$JAR" "$BASE/auth/session" | sed 's/.*"accountId":"\([^"]*\)".*/\1/')

echo "== GET /.well-known/jmap"
curl -sS -b "$JAR" "$BASE/.well-known/jmap" | pretty

echo "== POST /jmap/api: the inbox, one row per thread, with result references"
curl -sS -b "$JAR" -H 'Content-Type: application/json' "$BASE/jmap/api" -d '{
  "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"],
  "methodCalls": [
    ["Mailbox/query", {"accountId": "'"$ACCOUNT"'", "filter": {"role": "inbox"}}, "inbox"],
    ["Email/query", {"accountId": "'"$ACCOUNT"'",
      "filter": {"operator": "AND", "conditions": [{"hasAttachment": false}, {"notKeyword": "$seen"}]},
      "sort": [{"property": "receivedAt", "isAscending": false}],
      "collapseThreads": true, "limit": 10, "calculateTotal": true}, "q"],
    ["Email/get", {"accountId": "'"$ACCOUNT"'",
      "#ids": {"resultOf": "q", "name": "Email/query", "path": "/ids"},
      "properties": ["threadId", "mailboxIds", "from", "subject", "receivedAt", "preview", "keywords"]}, "g"],
    ["Thread/get", {"accountId": "'"$ACCOUNT"'",
      "#ids": {"resultOf": "g", "name": "Email/get", "path": "/list/*/threadId"}}, "t"]
  ]}' | pretty

echo "== POST /auth/logout, then the API again"
curl -sS -o /dev/null -w '%{http_code}\n' -b "$JAR" -c "$JAR" -X POST "$BASE/auth/logout"
curl -sS -w '\n%{http_code}\n' -b "$JAR" -H 'Content-Type: application/json' -d '{"using":[],"methodCalls":[]}' "$BASE/jmap/api"
