#!/bin/sh
# Starts and stops the robinmail cluster of contrib/compose/cluster.yaml.
#
#   cluster.sh up [rspamd]   make the secrets, build the image, start node 1,
#                            admit and start nodes 2 and 3 (node add on 1,
#                            node join on the new node), add the domain
#                            cluster.test and the users
#                            user1@cluster.test .. user4@cluster.test, and
#                            start "the other side": robinmail-sink with
#                            sink@sink.test
#   cluster.sh down          stop and remove the containers and their volumes
#   cluster.sh ps|logs ...   passed on to docker compose
#
# The secrets (a master key all nodes share, self-signed certificates
# for mail.robinmail.test and mail.sink.test) are made once in
# contrib/compose/cluster/secrets,
# which git ignores. The users' password is for this local cluster only; set
# ROBINMAIL_CLUSTER_PASSWORD to use another.
set -eu
cd "$(dirname "$0")"
COMPOSE="docker compose -f cluster.yaml"
SECRETS=cluster/secrets
PASSWORD=${ROBINMAIL_CLUSTER_PASSWORD:-robinmail-cluster}

case "${1:-}" in
up) ;;
down) exec $COMPOSE --profile rspamd down --volumes --remove-orphans ;;
"") echo "usage: cluster.sh up [rspamd] | down | <docker compose arguments>" >&2; exit 2 ;;
*) exec $COMPOSE "$@" ;;
esac

mkdir -p "$SECRETS"
if [ ! -f "$SECRETS/master.key" ]; then
	# The format of "robinmail key generate": 32 random bytes in base64
	# without padding.
	(umask 077; head -c 32 /dev/urandom | base64 | tr -d '=\n' > "$SECRETS/master.key"; echo >> "$SECRETS/master.key")
fi
if [ ! -f "$SECRETS/tls.crt" ]; then
	(umask 077; openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
		-subj "/CN=mail.robinmail.test" -addext "subjectAltName=DNS:mail.robinmail.test" \
		-keyout "$SECRETS/tls.key" -out "$SECRETS/tls.crt" 2>/dev/null)
fi

if [ ! -f "$SECRETS/sink.crt" ]; then
	(umask 077; openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
		-subj "/CN=mail.sink.test" -addext "subjectAltName=DNS:mail.sink.test" \
		-keyout "$SECRETS/sink.key" -out "$SECRETS/sink.crt" 2>/dev/null)
fi

PROFILE=
if [ "${2:-}" = rspamd ]; then
	PROFILE="--profile rspamd"
	for n in 1 2 3; do
		grep -q '"rspamd"' cluster/n$n.json || {
			echo "add \"rspamd\": {\"url\": \"http://rspamd:11333\"} to cluster/n$n.json to scan mail" >&2
		}
	done
fi
$COMPOSE $PROFILE build
[ -n "$PROFILE" ] && $COMPOSE $PROFILE up -d rspamd

robinmail() { $COMPOSE exec -T robinmail-1 robinmail -config /etc/robinmail/n1.json "$@"; }
on() { n=$1; shift; $COMPOSE run --rm --no-deps -T robinmail-$n -config /etc/robinmail/n$n.json "$@"; }
wait_for() { # what, command...
	what=$1; shift
	i=0
	until "$@" >/dev/null 2>&1; do
		i=$((i + 1))
		if [ $i -gt 90 ]; then
			echo "$what did not happen:" >&2
			$COMPOSE ps >&2
			$COMPOSE logs --tail 30 >&2
			exit 1
		fi
		sleep 1
	done
}
has_leader() { robinmail node list | grep -q leader; }
member() { robinmail node list | grep -q "^n$1 "; }

# Node 1 starts the cluster. Nodes 2 and 3 are admitted one after the
# other: with two members both must be up before a third can be added.
$COMPOSE up -d robinmail-1
wait_for "node 1 to lead" has_leader
for n in 2 3; do
	if ! member $n; then
		cert=$(on $n node id | awk '{print $3}')
		join=$(robinmail node add n$n robinmail-$n:7443 "$cert" | sed -n 's/.*robinmail node join //p')
		on $n node join $join
	fi
	$COMPOSE up -d robinmail-$n
	wait_for "node $n to be up" $COMPOSE exec -T robinmail-$n robinmail -config /etc/robinmail/n$n.json node list
done
wait_for "the cluster to have a leader" has_leader

robinmail domain list | grep -qx cluster.test || robinmail domain add cluster.test
for u in user1 user2 user3 user4; do
	robinmail user list | grep -q "^$u@cluster.test" || printf '%s\n' "$PASSWORD" | robinmail user add "$u@cluster.test"
done
robinmail node list

# The other side: one robinmail with its own database and the domain
# sink.test, which the cluster sends to and gets mail from.
$COMPOSE up -d robinmail-sink
sink() { $COMPOSE exec -T robinmail-sink robinmail -config /etc/robinmail/sink.json "$@"; }
i=0
until sink domain list >/dev/null 2>&1; do
	i=$((i + 1))
	if [ $i -gt 60 ]; then
		echo "robinmail-sink did not come up:" >&2
		$COMPOSE logs --tail 20 robinmail-sink >&2
		exit 1
	fi
	sleep 1
done
sink domain list | grep -qx sink.test || sink domain add sink.test
sink user list | grep -q "^sink@sink.test" || printf '%s\n' "$PASSWORD" | sink user add sink@sink.test
