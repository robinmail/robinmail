#!/bin/sh
# Gives a fresh development Garage a layout, a bucket and a fixed access key.
# The key is public (it is in this file): development only.
set -eu

KEY_ID=GK0123456789abcdef01234567
KEY_SECRET=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
BUCKET=robinmail

cd "$(dirname "$0")"
garage() { docker compose exec -T garage /garage "$@"; }

node=$(garage status | awk '/^[0-9a-f]{16} / { print $1; exit }')
garage layout assign -z dev -c 1G "$node"
garage layout apply --version 1
garage key import --yes -n robinmail "$KEY_ID" "$KEY_SECRET"
garage bucket create "$BUCKET"
garage bucket allow --read --write --owner "$BUCKET" --key robinmail
echo "bucket $BUCKET ready at http://127.0.0.1:3900 (region garage)"
