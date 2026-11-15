#!/bin/sh
# Sends mail between two robinmail servers on this machine, with curl and
# JMAP: server A has the domain a.robinmail.test and the user anna, server B
# has b.robinmail.test and bob. Each has its own database and its own blob
# directory. Neither asks DNS about the other: each has a route for the
# other's domain to a port on 127.0.0.1 (outbound.routes), so no mail
# leaves this machine.
#
#   contrib/dev/send-demo.sh
#
# What it shows: an upload, an Email made from properties with an
# attachment, an EmailSubmission, the delivery status, the message on the
# other server, a reply, and a bounce for an address that does not exist.
#
# The two instances live in ./dev/send-a and ./dev/send-b (ignored by git),
# each with its database in its data directory, and are stopped when the
# script ends. The users' password is for these local instances only; set
# ROBINMAIL_DEV_PASSWORD to use another.
set -eu
cd "$(dirname "$0")/../.."
PASSWORD=${ROBINMAIL_DEV_PASSWORD:-robinmail-dev}
A=dev/send-a
B=dev/send-b
A_URL=http://127.0.0.1:8085
B_URL=http://127.0.0.1:8086

mkdir -p dev "$A" "$B"
go build -o dev/robinmail ./cmd/robinmail

config() { # dir hostname smtp http other-domain other-smtp
	[ -f "$1/robinmail.json" ] || cat > "$1/robinmail.json" <<JSON
{
  "hostname": "$2",
  "data_dir": "data",
  "master_key_file": "master.key",
  "smtp": {"listen": "127.0.0.1:$3"},
  "web": {"http_listen": "127.0.0.1:$4"},
  "submission": {"undo_seconds": 0},
  "outbound": {"routes": {"$5": "127.0.0.1:$6"}}
}
JSON
}
# An instance from before phase 10 names an rqlite: it starts again.
for d in "$A" "$B"; do
	if [ -f "$d/robinmail.json" ] && grep -q '"rqlite"' "$d/robinmail.json"; then rm -rf "$d"; mkdir -p "$d"; fi
done
config "$A" mail.a.robinmail.test 2535 8085 b.robinmail.test 2536
config "$B" mail.b.robinmail.test 2536 8086 a.robinmail.test 2535
a() { dev/robinmail -config "$A/robinmail.json" "$@"; }
b() { dev/robinmail -config "$B/robinmail.json" "$@"; }
setup() { # command domain user
	[ -f "$(dirname "$4")/master.key" ] || $1 key generate >/dev/null
	$1 domain list | grep -qx "$2" || $1 domain add "$2"
	$1 user list | grep -q "^$3" || printf '%s\n' "$PASSWORD" | $1 user add "$3"
}
setup a a.robinmail.test anna@a.robinmail.test "$A/robinmail.json"
setup b b.robinmail.test bob@b.robinmail.test "$B/robinmail.json"

# The binary itself in the background, not the shell function: the pid is
# then the server's, and the trap below stops it.
dev/robinmail -config "$A/robinmail.json" serve 2> "$A/serve.log" &
PID_A=$!
dev/robinmail -config "$B/robinmail.json" serve 2> "$B/serve.log" &
PID_B=$!
JAR_A=$(mktemp)
JAR_B=$(mktemp)
trap '{ kill $PID_A $PID_B; wait $PID_A $PID_B; } 2>/dev/null || true; rm -f "$JAR_A" "$JAR_B"' EXIT
for url in $A_URL $B_URL; do
	i=0
	until curl -s -o /dev/null -m 2 "$url/auth/session"; do
		i=$((i + 1))
		[ $i -lt 50 ] || { echo "$url does not answer; see $A/serve.log and $B/serve.log" >&2; exit 1; }
		sleep 0.2
	done
done

pretty() { python3 -m json.tool 2>/dev/null || cat; }
login() { # jar url user -> account id
	curl -sS -c "$1" -H 'Content-Type: application/json' -d "{\"username\":\"$3\",\"password\":\"$PASSWORD\"}" "$2/auth/login" |
		sed 's/.*"accountId":"\([^"]*\)".*/\1/'
}
api() { # jar url body
	curl -sS -b "$1" -H 'Content-Type: application/json' "$2/jmap/api" -d "{
  \"using\": [\"urn:ietf:params:jmap:core\", \"urn:ietf:params:jmap:mail\", \"urn:ietf:params:jmap:submission\"],
  \"methodCalls\": $3}"
}
# wait_status jar url account submission recipient: until we no longer hold the message.
wait_status() {
	i=0
	while :; do
		out=$(api "$1" "$2" "[[\"EmailSubmission/get\", {\"accountId\": \"$3\", \"ids\": [\"$4\"], \"properties\": [\"undoStatus\", \"deliveryStatus\"]}, \"0\"]]")
		case "$out" in *'"delivered":"queued"'*) ;; *) break ;; esac
		i=$((i + 1))
		[ $i -lt 100 ] || break
		sleep 0.2
	done
	printf '%s\n' "$out" | pretty
}

ANNA=$(login "$JAR_A" $A_URL anna@a.robinmail.test)
BOB=$(login "$JAR_B" $B_URL bob@b.robinmail.test)

echo "== A: POST /jmap/upload/$ANNA/  (an attachment)"
UPLOAD=$(printf 'date,item\n2026-11-17,lunch\n' | curl -sS -b "$JAR_A" -H 'Content-Type: text/csv' --data-binary @- "$A_URL/jmap/upload/$ANNA/")
printf '%s\n' "$UPLOAD" | pretty
BLOB=$(printf '%s' "$UPLOAD" | sed 's/.*"blobId":"\([^"]*\)".*/\1/')

echo "== A: Identity/get, then Email/set create and EmailSubmission/set in one request"
OUT=$(api "$JAR_A" $A_URL '[
  ["Identity/get", {"accountId": "'"$ANNA"'", "ids": null, "properties": ["email", "name"]}, "i"],
  ["Email/set", {"accountId": "'"$ANNA"'", "create": {"draft": {
      "mailboxIds": {"m2": true}, "keywords": {"$draft": true, "$seen": true},
      "from": [{"name": "Anna", "email": "anna@a.robinmail.test"}],
      "to": [{"name": "Bob", "email": "bob@b.robinmail.test"}],
      "subject": "Lunch op dinsdag? — met een bijlage",
      "textBody": [{"partId": "text", "type": "text/plain"}],
      "htmlBody": [{"partId": "html", "type": "text/html"}],
      "attachments": [{"blobId": "'"$BLOB"'", "type": "text/csv", "name": "planning.csv"}],
      "bodyValues": {
        "text": {"value": "Hoi Bob,\n\nheb je dinsdag tijd om te lunchen? Zie de planning.\n\nAnna\n"},
        "html": {"value": "<p>Hoi Bob,</p><p>heb je <b>dinsdag</b> tijd om te lunchen? Zie de planning.</p><p>Anna</p>"}}}}}, "e"],
  ["EmailSubmission/set", {"accountId": "'"$ANNA"'",
      "create": {"send": {"identityId": "i1", "emailId": "#draft"}},
      "onSuccessUpdateEmail": {"#send": {"mailboxIds/m2": null, "mailboxIds/m3": true, "keywords/$draft": null}}}, "s"]]')
printf '%s\n' "$OUT" | pretty
SUB=$(printf '%s' "$OUT" | sed 's/.*"created":{"send":{[^}]*}[^}]*},[^}]*"id":"\(s[0-9a-z]*\)".*/\1/')
case "$SUB" in s[0-9a-z]*) ;; *) SUB=$(printf '%s' "$OUT" | grep -o '"id":"s[0-9a-z]*"' | head -1 | sed 's/.*"\(s[0-9a-z]*\)"/\1/') ;; esac

echo "== A: EmailSubmission/get once the queue has handed the message over"
wait_status "$JAR_A" $A_URL "$ANNA" "$SUB"

echo "== B: bob's Inbox, and the message as B stored it (header and the start of the body)"
b list bob@b.robinmail.test
FIRST=$(b list bob@b.robinmail.test | sed -n 's/^\(e[0-9a-z]*\) .*Lunch op dinsdag.*/\1/p' | tail -1)
b cat bob@b.robinmail.test "$FIRST" | sed -e 's/\r$//' | sed -n '1,40p'
MSGID=$(b cat bob@b.robinmail.test "$FIRST" | sed -n 's/^Message-ID: <\(.*\)>.*/\1/p' | head -1)

echo "== B: bob replies over JMAP"
OUT=$(api "$JAR_B" $B_URL '[
  ["Email/set", {"accountId": "'"$BOB"'", "create": {"reply": {
      "mailboxIds": {"m3": true}, "keywords": {"$seen": true},
      "from": [{"name": "Bob", "email": "bob@b.robinmail.test"}],
      "to": [{"name": "Anna", "email": "anna@a.robinmail.test"}],
      "subject": "Re: Lunch op dinsdag? — met een bijlage",
      "inReplyTo": ["'"$MSGID"'"], "references": ["'"$MSGID"'"],
      "textBody": [{"partId": "t", "type": "text/plain"}],
      "bodyValues": {"t": {"value": "Dinsdag is prima. Twaalf uur?\n"}}}}}, "e"],
  ["EmailSubmission/set", {"accountId": "'"$BOB"'", "create": {"send": {"identityId": "i1", "emailId": "#reply"}}}, "s"]]')
SUB=$(printf '%s' "$OUT" | grep -o '"id":"s[0-9a-z]*"' | head -1 | sed 's/.*"\(s[0-9a-z]*\)"/\1/')
wait_status "$JAR_B" $B_URL "$BOB" "$SUB"

echo "== A: anna sends to an address B does not have"
OUT=$(api "$JAR_A" $A_URL '[
  ["Email/set", {"accountId": "'"$ANNA"'", "create": {"lost": {
      "mailboxIds": {"m3": true},
      "from": [{"name": "Anna", "email": "anna@a.robinmail.test"}],
      "to": [{"name": null, "email": "nobody@b.robinmail.test"}],
      "subject": "Is daar iemand?",
      "textBody": [{"partId": "t", "type": "text/plain"}], "bodyValues": {"t": {"value": "Hallo?\n"}}}}}, "e"],
  ["EmailSubmission/set", {"accountId": "'"$ANNA"'", "create": {"send": {"identityId": "i1", "emailId": "#lost"}}}, "s"]]')
SUB=$(printf '%s' "$OUT" | grep -o '"id":"s[0-9a-z]*"' | head -1 | sed 's/.*"\(s[0-9a-z]*\)"/\1/')
wait_status "$JAR_A" $A_URL "$ANNA" "$SUB"

echo "== A: anna's Inbox: bob's reply and the bounce"
sleep 0.5
a list anna@a.robinmail.test
BOUNCE=$(a list anna@a.robinmail.test | sed -n 's/^\(e[0-9a-z]*\) .*Undelivered Mail.*/\1/p' | tail -1)
echo "== A: the delivery status part of the bounce"
a cat anna@a.robinmail.test "$BOUNCE" | sed -e 's/\r$//' | sed -n '/^Reporting-MTA/,/^--/p'
