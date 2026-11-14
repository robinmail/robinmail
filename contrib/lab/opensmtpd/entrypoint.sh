#!/bin/sh
# Starts OpenSMTPD as the lab peer opensmtpd.test, in the foreground with
# its log on the container's output.
set -eu
cp /lab/secrets/ca.crt /usr/local/share/ca-certificates/lab.crt
update-ca-certificates >/dev/null 2>&1
install -m 600 /lab/secrets/mx.opensmtpd.test.crt /etc/smtpd-tls.crt
install -m 600 /lab/secrets/mx.opensmtpd.test.key /etc/smtpd-tls.key
exec smtpd -d
