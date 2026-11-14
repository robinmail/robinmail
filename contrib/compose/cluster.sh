#!/bin/sh
# Starts and stops the robinmail cluster of contrib/compose/cluster.yaml.
#
#   cluster.sh up [rspamd]   make the secrets, build the image, start three
#                            rqlite and three robinmail nodes, wait for them,
#                            add the domain cluster.test and the users
#                            user1@cluster.test .. user4@cluster.test, and
#                            start "the other side": robinmail-sink with
#                            sink@sink.test
#   cluster.sh down          stop and remove the containers and their volumes
#   cluster.sh ps|logs ...   passed on to docker compose
#
# The secrets (a master key all nodes share and self-signed certificates
# for mail.robinmail.test and mail.sink.test) are made once in contrib/compose/cluster/secrets,
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
$COMPOSE $PROFILE up -d --build

robinmail() { $COMPOSE exec -T robinmail-1 robinmail -config /etc/robinmail/n1.json "$@"; }

# rqlite needs a moment to elect a leader, and robinmail waits for it.
i=0
until robinmail node list 2>/dev/null | grep -c '^n[123] ' | grep -qx 3; do
	i=$((i + 1))
	if [ $i -gt 60 ]; then
		echo "the cluster did not come up:" >&2
		$COMPOSE ps >&2
		$COMPOSE logs --tail 20 >&2
		exit 1
	fi
	sleep 1
done

robinmail domain list | grep -qx cluster.test || robinmail domain add cluster.test
for u in user1 user2 user3 user4; do
	robinmail user list | grep -q "^$u@cluster.test" || printf '%s\n' "$PASSWORD" | robinmail user add "$u@cluster.test"
done
robinmail node list

# The other side: one robinmail with its own database and the domain
# sink.test, which the cluster sends to and gets mail from.
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
