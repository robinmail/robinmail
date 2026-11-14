# OpenSMTPD from Debian 13.
FROM debian:trixie-20261104-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        opensmtpd ripmime ca-certificates openssl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/sh alice && useradd -m -s /bin/sh bob
COPY smtpd.conf /etc/smtpd.conf
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
