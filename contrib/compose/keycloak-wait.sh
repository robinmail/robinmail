#!/bin/sh
# Waits until the Keycloak of keycloak.yaml serves the discovery document
# of the robinmail realm: up to three minutes. A first start imports the
# realm and builds Keycloak's own caches, which takes about half a minute.
set -eu
url=http://127.0.0.1:8180/realms/robinmail/.well-known/openid-configuration
echo "waiting for Keycloak at $url"
i=0
until curl -sf -o /dev/null "$url"; do
	i=$((i + 1))
	if [ "$i" -ge 90 ]; then
		echo "Keycloak did not come up; its last lines:" >&2
		docker compose -f "$(dirname "$0")/keycloak.yaml" logs --tail 30 >&2
		exit 1
	fi
	sleep 2
done
echo "Keycloak is up"
