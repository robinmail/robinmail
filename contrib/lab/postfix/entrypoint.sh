#!/bin/sh
# Starts Postfix as one of the lab's hosts.
#
#   LAB_HOST     the host name, which is also the name of its certificate
#                in /lab/secrets [mx.postfix.test]
#   LAB_DOMAINS  the domains it is the final destination for [postfix.test
#                and under_score.postfix.test]
#   LAB_TLS      "no" turns STARTTLS off on the listener
set -eu
HOST=${LAB_HOST:-mx.postfix.test}
DOMAINS=${LAB_DOMAINS:-postfix.test, under_score.postfix.test}

cp /lab/secrets/ca.crt /usr/local/share/ca-certificates/lab.crt
update-ca-certificates >/dev/null 2>&1

postconf -e "myhostname = $HOST" "myorigin = ${DOMAINS%%,*}" "mydestination = $DOMAINS, localhost"
if [ "${LAB_TLS:-yes}" = no ]; then
	postconf -e "smtpd_tls_security_level = none"
else
	postconf -e "smtpd_tls_cert_file = /lab/secrets/$HOST.crt" "smtpd_tls_key_file = /lab/secrets/$HOST.key"
fi
if [ "$HOST" != mx.postfix.test ]; then
	# The extra hosts have none of the peer's special addresses.
	postconf -e "relay_domains =" "transport_maps =" "virtual_alias_maps =" "smtpd_recipient_restrictions ="
else
	# Greylisting for real, with a delay the tests can wait for. No
	# whitelisting of clients that passed before: every new sender and
	# recipient pair is deferred once.
	mkdir -p /var/lib/postgrey && chown postgrey /var/lib/postgrey
	postgrey --inet=127.0.0.1:10023 --delay=8 --auto-whitelist-clients=0 --daemonize \
		--whitelist-clients=/dev/null --whitelist-recipients=/dev/null
fi
exec postfix start-fg
