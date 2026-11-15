#!/bin/sh
# Starts and stops the interoperability lab of contrib/lab/compose.yaml.
#
#   lab.sh up              make the CA and the certificates (ca.sh), build
#                          the images, start everything, add the domain
#                          robin.test with the users alice and bob
#   lab.sh down            stop and remove the containers and their volumes
#   lab.sh versions        what runs: robinmail's peers and their versions
#   lab.sh suite [files]   Fastmail's JMAP-TestSuite against the lab's
#                          robinmail (arguments go to prove; default "-r t")
#   lab.sh ps|logs|exec .. passed on to docker compose
#
# The users' password is for this lab only; ROBINMAIL_LAB_PASSWORD sets
# another.
set -eu
cd "$(dirname "$0")"
COMPOSE="docker compose -f compose.yaml"
PASSWORD=${ROBINMAIL_LAB_PASSWORD:-robinmail-lab}

versions() {
	echo "Postfix   $($COMPOSE exec -T postfix postconf -h mail_version)"
	echo "Exim      $($COMPOSE exec -T exim exim4 -bV 2>/dev/null | sed -n 's/^Exim version \([^ ]*\).*/\1/p')"
	echo "OpenSMTPD $($COMPOSE exec -T opensmtpd dpkg-query -W -f '${Version}' opensmtpd)"
	echo "postgrey  $($COMPOSE exec -T postfix dpkg-query -W -f '${Version}' postgrey)"
	echo "swaks     $($COMPOSE exec -T postfix dpkg-query -W -f '${Version}' swaks)"
	echo "ripmime   $($COMPOSE exec -T postfix dpkg-query -W -f '${Version}' ripmime)"
	echo "ImapTest  $($COMPOSE exec -T imapclients sh -c 'echo "release latest, commit $(cat /lab/imaptest/commit 2>/dev/null || echo bc23019)"')"
	echo "mbsync    $($COMPOSE exec -T imapclients mbsync --version)"
	echo "OfflineIMAP $($COMPOSE exec -T imapclients offlineimap --version)"
	echo "Thunderbird $($COMPOSE exec -T thunderbird dpkg-query -W -f '${Version}' thunderbird)"
	echo "caldav    $($COMPOSE exec -T davclients pip show caldav | sed -n 's/^Version: //p')"
	echo "vdirsyncer $($COMPOSE exec -T davclients vdirsyncer --version 2>&1 | sed 's/.*version //')"
	echo "CoreDNS   $($COMPOSE exec -T dns /coredns -version 2>/dev/null | sed -n 's/^CoreDNS-//p')"
	echo "nginx     $($COMPOSE exec -T web nginx -v 2>&1 | sed 's/.*\///')"
	echo "rqlite    $($COMPOSE exec -T rqlite rqlited -version 2>&1 | awk '{print $2}')"
}

case "${1:-}" in
up) ;;
down) exec $COMPOSE --profile suite down --volumes --remove-orphans ;;
suite)
	# Fastmail's JMAP-TestSuite, in a container of its own inside the lab.
	shift
	$COMPOSE --profile suite build jmapsuite
	exec $COMPOSE --profile suite run --rm -T jmapsuite "$@"
	;;
versions) versions; exit ;;
"") echo "usage: lab.sh up | down | versions | suite [prove arguments] | <docker compose arguments>" >&2; exit 2 ;;
*) exec $COMPOSE "$@" ;;
esac

./ca.sh
# The peers that share the Postfix image need it built first.
$COMPOSE build
$COMPOSE up -d

robinmail() { $COMPOSE exec -T robinmail robinmail -config /lab/robinmail.json "$@"; }
i=0
until robinmail domain list >/dev/null 2>&1; do
	i=$((i + 1))
	if [ $i -gt 60 ]; then
		echo "robinmail did not come up:" >&2
		$COMPOSE ps >&2
		$COMPOSE logs --tail 30 robinmail >&2
		exit 1
	fi
	sleep 1
done
robinmail domain list | grep -qx robin.test || robinmail domain add robin.test
# alice and bob exchange mail with the peers; the others are the accounts of
# the IMAP clients (imaptest, mbsync, offlineimap), one each, and carol is
# the user of the calendar and contact clients. The
# Thunderbird tests make their own users.
for u in alice bob imaptest mbsync offlineimap carol; do
	robinmail user list | grep -q "^$u@robin.test" || printf '%s\n' "$PASSWORD" | robinmail user add "$u@robin.test"
done

# Every peer answers on port 25.
for p in postfix exim opensmtpd badtls notls; do
	i=0
	until $COMPOSE exec -T "$p" bash -c 'exec 3<>/dev/tcp/127.0.0.1/25' 2>/dev/null; do
		i=$((i + 1))
		if [ $i -gt 60 ]; then
			echo "$p did not come up:" >&2
			$COMPOSE logs --tail 30 "$p" >&2
			exit 1
		fi
		sleep 1
	done
done
versions
