#!/bin/sh
# The certificate authority of the lab and a certificate for every host,
# in contrib/lab/secrets (not in git). Made once; "ca.sh force" makes them
# again.
#
#   ca.crt, ca.key      the lab CA every container trusts
#   <host>.crt, .key    one per mail host, signed by the CA
#   web.crt, web.key    one for all mta-sts.<domain> names
#   mx.badtls.test.*    self-signed, not by the CA: the broken certificate
#   master.key          robinmail's master key
set -eu
cd "$(dirname "$0")"
S=secrets
[ "${1:-}" = force ] && rm -rf "$S"
mkdir -p "$S"
umask 022
EC="-newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes"

if [ ! -f "$S/ca.crt" ]; then
	openssl req -x509 $EC -days 3650 -subj "/CN=robinmail lab CA" \
		-addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
		-keyout "$S/ca.key" -out "$S/ca.crt" 2>/dev/null
fi

# leaf <file name> <DNS name>...
leaf() {
	name=$1; shift
	# A certificate that is there stays, unless a name was added since.
	if [ -f "$S/$name.crt" ]; then
		have=$(openssl x509 -in "$S/$name.crt" -noout -ext subjectAltName 2>/dev/null || true)
		missing=
		for d in "$@"; do
			case "$have," in *"DNS:$d,"*) ;; *) missing=1 ;; esac
		done
		[ -z "$missing" ] && return
	fi
	san=
	for d in "$@"; do san="$san${san:+,}DNS:$d"; done
	openssl req -new $EC -subj "/CN=$1" -keyout "$S/$name.key" -out "$S/$name.csr" 2>/dev/null
	printf 'subjectAltName=%s\nbasicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n' "$san" > "$S/$name.ext"
	openssl x509 -req -in "$S/$name.csr" -CA "$S/ca.crt" -CAkey "$S/ca.key" -CAcreateserial \
		-days 825 -extfile "$S/$name.ext" -out "$S/$name.crt" 2>/dev/null
	rm -f "$S/$name.csr" "$S/$name.ext"
}

# robinmail also answers as the autoconfig host of its domain.
leaf mx.robin.test mx.robin.test autoconfig.robin.test
leaf mx.postfix.test mx.postfix.test
leaf mx.exim.test mx.exim.test
leaf mx.opensmtpd.test mx.opensmtpd.test
leaf mx.notls.test mx.notls.test
leaf web mta-sts.robin.test mta-sts.postfix.test mta-sts.exim.test mta-sts.opensmtpd.test \
	mta-sts.badtls-sts.test mta-sts.notls-sts.test

if [ ! -f "$S/mx.badtls.test.crt" ]; then
	openssl req -x509 $EC -days 825 -subj "/CN=mx.badtls.test" -addext "subjectAltName=DNS:mx.badtls.test" \
		-keyout "$S/mx.badtls.test.key" -out "$S/mx.badtls.test.crt" 2>/dev/null
fi

if [ ! -f "$S/master.key" ]; then
	# The format of "robinmail key generate".
	(umask 077; head -c 32 /dev/urandom | base64 | tr -d '=\n' > "$S/master.key"; echo >> "$S/master.key")
fi
# The keys are read by the mail servers' own users inside the containers.
chmod 644 "$S"/*.key
chmod 600 "$S/ca.key" "$S/master.key"
