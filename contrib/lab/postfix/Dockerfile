# Postfix from Debian 13, with postgrey (greylisting), swaks (the SMTP
# client the submission tests use) and ripmime (the MIME tool that takes
# attachments apart). The same image runs as the peer postfix.test and, with
# other settings (entrypoint.sh), as the hosts with a broken certificate
# and without STARTTLS.
FROM debian:trixie-20261104-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        postfix postgrey swaks libnet-ssleay-perl libio-socket-ssl-perl ripmime ca-certificates openssl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -s /bin/sh alice && useradd -m -s /bin/sh bob
COPY main.cf master.cf transport virtual tls_policy grey /etc/postfix/
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
