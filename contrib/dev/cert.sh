#!/bin/sh
# Makes a self-signed certificate for the development instance, so that a
# mail client can reach its IMAP and submission listeners, which only
# speak TLS:
#
#   contrib/dev/cert.sh <directory>     writes tls.crt and tls.key there
#
# The certificate names localhost, mail.robinmail.test, 127.0.0.1 and ::1,
# is its own issuer and lasts ten years. Nothing trusts it: a mail client
# shows it once and asks whether to make an exception. An existing pair is
# left alone, so that exception stays good. seed.sh calls this.
set -eu
DIR=${1:?usage: cert.sh <directory>}
if [ -s "$DIR/tls.crt" ] && [ -s "$DIR/tls.key" ]; then
	exit 0
fi
mkdir -p "$DIR"
umask 077
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
	-subj "/CN=robinmail development" \
	-addext "subjectAltName=DNS:localhost,DNS:mail.robinmail.test,IP:127.0.0.1,IP:::1" \
	-keyout "$DIR/tls.key" -out "$DIR/tls.crt" 2>/dev/null
chmod 600 "$DIR/tls.key"
chmod 644 "$DIR/tls.crt"
echo "made a self-signed certificate in $DIR (tls.crt, tls.key)"
