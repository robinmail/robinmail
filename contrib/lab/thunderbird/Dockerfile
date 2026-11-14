# Thunderbird for the lab: a real mail client, driven by the tests in
# internal/labtest over Marionette, Mozilla's remote control protocol.
#
# Debian 13's package of the ESR, at the version named below. Debian
# replaces it with every security release; the build then fails on the
# version, which is the one line to update (apt-cache policy thunderbird).
# The build fetches packages from the network; the container then runs
# inside the lab, which has no way out, so Thunderbird's own calls home
# (updates, add-on lists, telemetry) go nowhere. policies.json turns most
# of them off anyway and makes the lab CA a trusted authority.
#
# Nothing runs when the container starts: the tests write a profile, start
# Thunderbird on it with /lab/thunderbird.sh and stop it again.
FROM debian:trixie-20261104-slim
ARG THUNDERBIRD_VERSION=1:153.7.0esr-1~deb13u1
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates procps socat sqlite3 thunderbird=$THUNDERBIRD_VERSION \
    && rm -rf /var/lib/apt/lists/*
RUN useradd -m -s /bin/sh tb
COPY policies.json /usr/lib/thunderbird/distribution/policies.json
COPY thunderbird.sh /lab/thunderbird.sh
USER tb
WORKDIR /home/tb
ENTRYPOINT ["sleep", "infinity"]
