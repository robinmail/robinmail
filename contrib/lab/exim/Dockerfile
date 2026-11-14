# Exim from Debian 13 (the "heavy" daemon, which has SMTPUTF8), with its
# whole configuration in exim4.conf instead of Debian's generated one.
FROM debian:trixie-20261104-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        exim4-daemon-heavy ripmime ca-certificates openssl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/sh alice && useradd -m -s /bin/sh bob
COPY exim4.conf aliases /etc/exim4/
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
