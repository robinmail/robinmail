#!/bin/sh
# Sets up a development instance of robinmail in ./dev (ignored by git): a
# config, a master key, the domain robinmail.test, one user and some mail.
# It needs rqlite-dev from contrib/compose (127.0.0.1:4005), a database
# that only this instance and "make e2e" use:
#
#   docker compose -f contrib/compose/compose.yaml up -d
#   contrib/dev/seed.sh
#   dev/robinmail -config dev/robinmail.json serve
#
# Then the webmail and JMAP are on http://127.0.0.1:8080 and SMTP on
# 127.0.0.1:2525, without Rspamd. Mail sent from it to robinmail.test is delivered
# in the instance itself. Mail to any other domain goes to Mailpit, a mail
# catcher from the same compose file: the config routes every domain that
# is not ours to it ("outbound": {"routes": {"*": "127.0.0.1:1025"}}), and
# Mailpit shows what it got on http://127.0.0.1:8025 and passes nothing on.
# So a development instance never sends real mail. Without Mailpit running
# such mail stays in the queue and bounces after five days.
#
# The binary built here has no webmail in it: for that use "make dev" (Vite
# on http://localhost:5173, live reload) or "make" and run ./robinmail with
# the same config (see the Makefile). Run this script again any time: it
# rebuilds the binary and leaves an existing user and their mail alone. An
# existing dev/robinmail.json is left alone too, with one exception: one
# from before the IMAP and submission listeners is moved aside and
# replaced (the script says so). One from before Mailpit still routes to
# the dead port 127.0.0.1:1 until that line is changed.
#
# Until November 2026 the instance used the rqlite on 127.0.0.1:4001, the
# one the Go tests fill with throwaway domains and accounts. An existing
# dev/robinmail.json that still names it is pointed at 4005 by this
# script (it says so). To move over, once:
#
#   docker compose -f contrib/compose/compose.yaml up -d    # starts rqlite-dev
#   contrib/dev/seed.sh                                     # new database: the users and the sample mail again
#   stop the running dev server and start it again          # until then it stays on 4001
#
# Nothing is carried over: the instance starts fresh, with the same
# master key. Mail, app passwords and settings of the old one stay in the
# database on 4001 until that container is recreated, and its message
# files stay in dev/data, where nothing refers to them any more ("rm -rf
# dev/data" before seeding, with no server running, clears them).
#
# A mail client (Thunderbird, Apple Mail, mbsync) can use the instance
# too. IMAP and submission only speak TLS, so this script makes a
# self-signed certificate once (dev/tls.crt, by contrib/dev/cert.sh; it
# names localhost, 127.0.0.1 and mail.robinmail.test). Settings:
#
#   incoming   IMAP   127.0.0.1   port 1993   SSL/TLS    (or 1143, STARTTLS)
#   outgoing   SMTP   127.0.0.1   port 1465   SSL/TLS    (or 1587, STARTTLS)
#   user name  dev@robinmail.test             authentication: normal password
#
# The client shows the certificate once and asks whether to trust it:
# make the exception, for incoming and for outgoing. Thunderbird's
# automatic set-up finds nothing for robinmail.test; choose "configure
# manually". Mail the client sends to an outside address lands in Mailpit
# on http://127.0.0.1:8025, like the webmail's.
#
# Which password: the account password below, as long as the account has
# no app password and no two-step login. That is how seed.sh leaves the
# dev account, so a client can be set up without opening the webmail. To
# try app passwords, make one in the webmail under Security (the shield):
# from then on mail clients are refused the account password ("wrong name
# or password") and want the app password, and the account password only
# opens the webmail. Revoking the last app password brings the account
# password back for mail clients.
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
contrib/dev/cert.sh "$DIR"
# A config from before the instance had IMAP and submission listeners is
# set aside and replaced: it has no "imap" section. Changes made to it by
# hand are in the copy.
if [ -f "$DIR/robinmail.json" ] && ! grep -q '"imap"' "$DIR/robinmail.json"; then
	mv "$DIR/robinmail.json" "$DIR/robinmail.json.before-mail-clients"
	echo "$DIR/robinmail.json had no IMAP and submission listeners: replaced by contrib/dev/robinmail.dev.json,"
	echo "the old one is $DIR/robinmail.json.before-mail-clients; restart a running dev server to use the new one"
fi
# A config that still uses the database of the Go tests gets the
# instance's own.
if [ -f "$DIR/robinmail.json" ] && grep -q '"url": "http://127.0.0.1:4001"' "$DIR/robinmail.json"; then
	sed -i.before-rqlite-dev 's|"url": "http://127.0.0.1:4001"|"url": "http://127.0.0.1:4005"|' "$DIR/robinmail.json"
	echo "$DIR/robinmail.json used the database of the Go tests (127.0.0.1:4001): it now names 127.0.0.1:4005,"
	echo "which starts empty; restart a running dev server to use it (the old file is $DIR/robinmail.json.before-rqlite-dev)"
fi
[ -f "$DIR/robinmail.json" ] || cp contrib/dev/robinmail.dev.json "$DIR/robinmail.json"
go build -o "$DIR/robinmail" ./cmd/robinmail
RQLITE=$(sed -n 's|.*"url": "\(http://[^"]*\)".*|\1|p' "$DIR/robinmail.json" | head -1)
if ! curl -fsS -o /dev/null --max-time 3 "$RQLITE/readyz"; then
	echo "no rqlite on $RQLITE: docker compose -f contrib/compose/compose.yaml up -d" >&2
	exit 1
fi
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
