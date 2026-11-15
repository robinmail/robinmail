#!/bin/sh
# Lets a phone or another computer on the same network use the development
# instance: calendars and contacts (CalDAV, CardDAV), mail (IMAP and
# submission) and the webmail, on this machine's address in the network,
# over TLS with a self-signed certificate for that address.
#
#   contrib/dev/phone.sh [address]     writes dev/robinmail.phone.json and dev/phone/tls.{crt,key}
#   ./robinmail -config dev/robinmail.phone.json serve
#
# The address is this machine's in the local network; without one the
# script takes the first private IPv4 address it finds. The instance is
# the same as "make dev" (same database, same users): stop a running
# development server first, or keep both, the ports below differ from it.
#
#   https://<address>:8443/          the webmail; CalDAV and CardDAV under /dav/
#   IMAP <address>:2993 (TLS)        submission <address>:2465 (TLS)
#
# The phone shows the certificate once ("cannot verify server identity")
# and asks to trust it. Everything else of the instance stays as it is:
# mail to other domains goes to Mailpit, nothing leaves this machine.
set -eu
cd "$(dirname "$0")/../.."
ADDR=${1:-$(ifconfig 2>/dev/null | awk '/inet (10|192\.168|172\.(1[6-9]|2[0-9]|3[01]))\./ {print $2; exit}')}
[ -n "$ADDR" ] || { echo "no address found; give one: contrib/dev/phone.sh 192.168.1.20" >&2; exit 1; }
[ -f dev/robinmail.json ] || contrib/dev/seed.sh
mkdir -p dev/phone
if ! openssl x509 -in dev/phone/tls.crt -noout -ext subjectAltName 2>/dev/null | grep -q "IP Address:$ADDR"; then
	umask 077
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
		-subj "/CN=robinmail development on $ADDR" -addext "subjectAltName=IP:$ADDR" \
		-keyout dev/phone/tls.key -out dev/phone/tls.crt 2>/dev/null
	chmod 644 dev/phone/tls.crt
	echo "made a self-signed certificate for $ADDR in dev/phone"
fi
ADDR=$ADDR node -e '
const fs = require("fs")
const cfg = JSON.parse(fs.readFileSync("dev/robinmail.json", "utf8"))
const a = process.env.ADDR
cfg.hostname = a
cfg.tls = { cert_file: "phone/tls.crt", key_file: "phone/tls.key" }
cfg.web = { listen: a + ":8443" }
cfg.smtp = { ...cfg.smtp, listen: "127.0.0.1:2526" }
cfg.imap = { ...cfg.imap, listen_tls: a + ":2993", listen: "" }
cfg.submission = { ...cfg.submission, listen_tls: a + ":2465", listen: "" }
fs.writeFileSync("dev/robinmail.phone.json", JSON.stringify(cfg, null, 2) + "\n", { mode: 0o600 })
'
echo "dev/robinmail.phone.json: https://$ADDR:8443/ (CalDAV and CardDAV: https://$ADDR:8443/dav/)"
echo "start it with: ./robinmail -config dev/robinmail.phone.json serve"
