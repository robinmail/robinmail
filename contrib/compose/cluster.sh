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
# The secrets (a master key all nodes share, self-signed certificates
# for mail.robinmail.test and mail.sink.test, and what locks rqlite down: a
# CA, a certificate per rqlite node, rqlite's auth file and robinmail's
# login for it) are made once in contrib/compose/cluster/secrets,
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

# rqlite, locked down (cluster.yaml says how). A CA of the cluster's own,
# and for every rqlite node a certificate for the two names it has: the
# one the other rqlite nodes reach it under (Raft, mutual TLS: so it is a
# server and a client certificate) and the one its robinmail does (HTTPS).
RQ="$SECRETS/rqlite"
mkdir -p "$RQ"
EC="-newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes"
if [ ! -f "$RQ/ca.crt" ]; then
	openssl req -x509 $EC -days 3650 -subj "/CN=robinmail cluster rqlite CA" \
		-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
		-keyout "$SECRETS/rqlite-ca.key" -out "$RQ/ca.crt" 2>/dev/null
	chmod 600 "$SECRETS/rqlite-ca.key"
fi
for n in 1 2 3; do
	[ -f "$RQ/rqlite-$n.crt" ] && continue
	openssl req -new $EC -subj "/CN=rqlite-$n" -keyout "$RQ/rqlite-$n.key" -out "$RQ/rqlite-$n.csr" 2>/dev/null
	printf 'subjectAltName=DNS:rqlite-%s,DNS:rqlite-%s-db\nbasicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth,clientAuth\n' "$n" "$n" > "$RQ/rqlite-$n.ext"
	openssl x509 -req -in "$RQ/rqlite-$n.csr" -CA "$RQ/ca.crt" -CAkey "$SECRETS/rqlite-ca.key" -CAcreateserial \
		-days 825 -extfile "$RQ/rqlite-$n.ext" -out "$RQ/rqlite-$n.crt" 2>/dev/null
	rm -f "$RQ/rqlite-$n.csr" "$RQ/rqlite-$n.ext" "$RQ/ca.srl"
done
# The logins. rqlite's auth file names three users: robinmail with what
# robinmail needs and no more, node for an rqlite node that joins, admin
# for the operator (the tests use it to look at rqlite). Random passwords,
# written to files and never shown or put on a command line.
if [ ! -f "$RQ/auth.json" ]; then
	secret() { head -c 24 /dev/urandom | base64 | tr -d '=+/\n'; }
	robinmail_pw=$(secret); node_pw=$(secret); admin_pw=$(secret)
	(umask 077
	printf 'robinmail:%s\n' "$robinmail_pw" > "$SECRETS/rqlite.auth"
	printf 'admin:%s\n' "$admin_pw" > "$SECRETS/rqlite-admin.auth")
	cat > "$RQ/auth.json" <<AUTH
[
  {"username": "robinmail", "password": "$robinmail_pw", "perms": ["execute", "query", "backup", "load"]},
  {"username": "node", "password": "$node_pw", "perms": ["join"]},
  {"username": "admin", "password": "$admin_pw", "perms": ["all"]}
]
AUTH
fi
# rqlite's container runs as its own user and reads these through a mount.
chmod 644 "$RQ"/*

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
