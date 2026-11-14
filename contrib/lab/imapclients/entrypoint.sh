#!/bin/sh
# Trust the lab's CA, then wait: the tests run commands in this container.
set -eu
cp /lab/secrets/ca.crt /usr/local/share/ca-certificates/lab.crt
update-ca-certificates >/dev/null 2>&1
exec sleep infinity
