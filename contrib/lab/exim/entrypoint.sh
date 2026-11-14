#!/bin/sh
# Starts Exim as the lab peer exim.test: a daemon on port 25 that also runs
# the queue every five seconds. Its logs go to the container's output.
set -eu
cp /lab/secrets/ca.crt /usr/local/share/ca-certificates/lab.crt
update-ca-certificates >/dev/null 2>&1
install -o Debian-exim -g Debian-exim -m 600 /lab/secrets/mx.exim.test.crt /etc/exim4/tls.crt
install -o Debian-exim -g Debian-exim -m 600 /lab/secrets/mx.exim.test.key /etc/exim4/tls.key
mkdir -p /var/log/exim4 && chown Debian-exim:Debian-exim /var/log/exim4
touch /var/log/exim4/mainlog /var/log/exim4/rejectlog /var/log/exim4/paniclog
chown Debian-exim:Debian-exim /var/log/exim4/*
tail -q -F /var/log/exim4/mainlog /var/log/exim4/paniclog &
exec exim4 -bdf -q5s
