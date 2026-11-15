# Fastmail's JMAP-TestSuite (https://github.com/fastmail/JMAP-TestSuite), a
# compliance suite in Perl, with an adapter for robinmail. Everything it
# needs is installed in this image and nothing on the host. The build
# fetches the suite and its Perl modules from the network; the container
# then runs inside the lab, which has no way out.
#
#   contrib/lab/lab.sh suite                 the whole suite
#   contrib/lab/lab.sh suite t/Mailbox       a part of it
#
# The suite is pinned to the commit it was run at.
FROM robinmail-lab AS robinmail

FROM perl:5.42.3-slim-trixie
RUN apt-get update && apt-get install -y --no-install-recommends \
        git gcc g++ make libc6-dev libssl-dev zlib1g-dev libexpat1-dev ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ARG SUITE_COMMIT=2fa1934688d0e26249cf9f88b4f6cfa050dbdef4
RUN git clone https://github.com/fastmail/JMAP-TestSuite.git /suite \
    && cd /suite && git checkout -q $SUITE_COMMIT
WORKDIR /suite
RUN cpanm --notest --quiet --installdeps . && cpanm --notest --quiet LWP::Protocol::https && rm -rf /root/.cpanm
# The adapter makes a new user for every test that wants an empty account,
# with robinmail's own command, run as on robinmail's node: the container
# shares robinmail's data directory and loopback, and the command reaches
# the running server through its control listener.
COPY --from=robinmail /usr/local/bin/robinmail /usr/local/bin/robinmail
COPY Robinmail.pm /suite/lib/JMAP/TestSuite/ServerAdapter/Robinmail.pm
COPY robinmail.json /suite/robinmail.json
RUN mkdir -p /suite/account-locks
ENV JMAP_SERVER_ADAPTER_FILE=/suite/robinmail.json \
    PERL_LWP_SSL_CA_FILE=/lab/secrets/ca.crt
ENTRYPOINT ["prove", "-Ilib"]
CMD ["-r", "t"]
