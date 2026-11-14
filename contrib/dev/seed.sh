#!/bin/sh
# Sets up a development instance of robinmail in ./dev (ignored by git): a
# config, a master key, the domain robinmail.test, one user and some mail.
# It needs rqlite from contrib/compose:
#
#   docker compose -f contrib/compose/compose.yaml up -d
#   contrib/dev/seed.sh
#   dev/robinmail -config dev/robinmail.json serve
#
# Then JMAP is on http://127.0.0.1:8080 and SMTP on 127.0.0.1:2525, without
# TLS and without Rspamd. Mail sent from it to robinmail.test is delivered
# in the instance itself; mail to any other domain goes nowhere: the config
# routes it to a port nothing listens on ("outbound": {"routes": {"*":
# "127.0.0.1:1"}}), so a development instance never sends real mail. It
# stays in the queue and bounces after five days. The binary built here has no webmail in it: for
# that use "make dev" (Vite on http://localhost:5173, live reload) or
# "make" and run ./robinmail with the same config (see the Makefile). Run it again any time: it rebuilds the binary and
# leaves an existing user and their mail alone.
#
# The user is dev@robinmail.test. A second one, other@robinmail.test, has
# no mail: it is there to send mail to and to answer from (the end-to-end
# test does). The password below, the same for both, is for this local
# instance only; set ROBINMAIL_DEV_PASSWORD to use another.
set -eu
cd "$(dirname "$0")/../.."

DIR=dev
USER_ADDRESS=dev@robinmail.test
OTHER_ADDRESS=other@robinmail.test
PASSWORD=${ROBINMAIL_DEV_PASSWORD:-robinmail-dev}

mkdir -p "$DIR"
[ -f "$DIR/robinmail.json" ] || cp contrib/dev/robinmail.dev.json "$DIR/robinmail.json"
go build -o "$DIR/robinmail" ./cmd/robinmail
robinmail() { "$DIR/robinmail" -config "$DIR/robinmail.json" "$@"; }

[ -f "$DIR/master.key" ] || robinmail key generate
robinmail domain list | grep -qx robinmail.test || robinmail domain add robinmail.test

robinmail user list | grep -q "^$OTHER_ADDRESS" || printf '%s\n' "$PASSWORD" | robinmail user add "$OTHER_ADDRESS"

if robinmail user list | grep -q "^$USER_ADDRESS"; then
	echo "$USER_ADDRESS exists, leaving the mail as it is"
	exit 0
fi
printf '%s\n' "$PASSWORD" | robinmail user add "$USER_ADDRESS"

# Mail on the wire has CRLF line ends; the files here have LF.
deliver() { awk '{ printf "%s\r\n", $0 }' "$1" | robinmail deliver "$USER_ADDRESS" "$2"; }
for f in contrib/dev/mail/*.eml; do
	case "$f" in
	*junk*) deliver "$f" junk ;;
	*) deliver "$f" inbox ;;
	esac
done
robinmail flag "$USER_ADDRESS" e1 '$seen' >/dev/null
robinmail mailbox list "$USER_ADDRESS"
