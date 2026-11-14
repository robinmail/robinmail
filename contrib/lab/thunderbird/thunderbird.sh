#!/bin/sh
# Starts Thunderbird on the profile /home/tb/profiles/<name>, without a
# display, with Marionette listening on 127.0.0.1:2828 in the container.
# Its output goes to <profile>.log, and what its IMAP code sends and gets
# (Mozilla's own protocol log, without passwords) to <profile>.imap.log.
# The tests reach Marionette with
#   docker compose exec -T thunderbird socat STDIO TCP:127.0.0.1:2828
# so no port leaves the container.
#
# --remote-allow-system-access lets Marionette run scripts with the
# privileges of Thunderbird's own code, which is how the tests use its
# mail interfaces.
set -eu
profile=/home/tb/profiles/$1
export MOZ_LOG=IMAP:4,timestamp,append MOZ_LOG_FILE="$profile.imap.log"
exec thunderbird --headless --no-remote --marionette --remote-allow-system-access \
	--profile "$profile" >"$profile.log" 2>&1
