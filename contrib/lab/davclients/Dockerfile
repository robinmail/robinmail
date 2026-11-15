# Calendar and contact clients that are not ours, for the lab: the Python
# library "caldav" (what many scripts and some servers' own tests use) and
# vdirsyncer (which keeps CalDAV and CardDAV collections in step with local
# directories, as DAVx5 and khal users do). The tests run them in this
# container (internal/labtest/dav_test.go).
#
# The build fetches the packages from PyPI; the container then runs inside
# the lab, which has no way out.
FROM python:3.12-slim-trixie
ARG CALDAV_VERSION=1.4.0
ARG VDIRSYNCER_VERSION=0.19.3
# caldav 1.4 is written against icalendar 5; 6 changed what it imports.
RUN pip install --no-cache-dir "caldav==$CALDAV_VERSION" "icalendar==5.0.13" "vdirsyncer==$VDIRSYNCER_VERSION"
COPY caldav_check.py vdirsyncer.sh /lab/
ENV SSL_CERT_FILE=/lab/secrets/ca.crt REQUESTS_CA_BUNDLE=/lab/secrets/ca.crt
ENTRYPOINT ["sleep", "infinity"]
