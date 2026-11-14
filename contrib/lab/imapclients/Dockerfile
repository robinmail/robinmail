# IMAP clients for the lab, to drive robinmail's IMAP server with code that
# is not ours:
#
#   imaptest      Dovecot's ImapTest (https://github.com/dovecot/imaptest):
#                 its scripted compliance tests and its stress test
#   mbsync        isync 1.5.1, a Maildir synchronizer in C
#   offlineimap   OfflineIMAP 3, another one, in Python
#
# Everything is installed in this image and nothing on the host. The build
# fetches Debian packages and ImapTest from the network; the container
# then runs inside the lab, which has no way out.
#
# ImapTest is the binary the Dovecot project builds for Debian 13 and
# publishes with its "latest" release, checked against the SHA-256 it had
# on 2026-11-14 (built from commit bc23019). It is not built here: its
# source follows Dovecot's main branch and does not compile against the
# Dovecot 2.4.1 headers of Debian 13 at any commit since March 2025. The
# release is a moving tag: when the project replaces the binary the build
# fails on the checksum, and the three ARGs below are what to update. The
# test scripts come from the source at the same commit.
FROM debian:trixie-20261104-slim
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl git isync offlineimap3 openssl procps \
    && rm -rf /var/lib/apt/lists/*
ARG IMAPTEST_COMMIT=bc2301917a0d7d0acd2e6557dc191988a05e81ba
ARG IMAPTEST_SHA256_aarch64=b5da4baedbe8205679ce71e8ccd1e2b16b92905ed14cbcd2d7c8e9429a76e2da
ARG IMAPTEST_SHA256_x86_64=f8bc02a5b3b74541e5cee8c17fb97e34ecec2306a24aaa3cc73f09ceb4f2b4e3
RUN arch=$(uname -m) \
    && curl -fsSL -o /usr/local/bin/imaptest "https://github.com/dovecot/imaptest/releases/download/latest/imaptest-$arch-debian-13" \
    && eval "sum=\$IMAPTEST_SHA256_$arch" \
    && echo "$sum  /usr/local/bin/imaptest" | sha256sum -c - \
    && chmod 755 /usr/local/bin/imaptest \
    && git clone -q https://github.com/dovecot/imaptest.git /src/imaptest \
    && git -C /src/imaptest checkout -q $IMAPTEST_COMMIT \
    && mkdir -p /lab/imaptest && cp -R /src/imaptest/src/tests /lab/imaptest/tests && rm -rf /src/imaptest
RUN useradd -m -s /bin/sh mail1
COPY entrypoint.sh /entrypoint.sh
COPY messages.mbox /lab/imaptest/messages.mbox
ENTRYPOINT ["/entrypoint.sh"]
