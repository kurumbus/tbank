#!/bin/sh
set -eu
# Install directory is explicit so cron reads the intended .env and configuration.
cd "${TBANK_INSTALL_DIR:-/opt/tbank}"
mkdir -p var
chmod 700 var
# One lock covers all scans; failure of one family does not starve the others.
# Each process also has its own API timeout and a five-minute process bound.
exec 9>var/hourly.lock
flock -n 9 || exit 0
result=0
timeout 5m ./bin/tbank scan -config config/scanner.json -notify || result=1
timeout 5m ./bin/tbank curve-scan -config config/curves.json -notify || result=1
timeout 5m ./bin/tbank relative-scan -config config/relative.json -notify || result=1
exit "$result"
