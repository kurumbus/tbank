#!/bin/sh
set -eu
# Install directory is explicit so cron reads the intended .env and configuration.
cd "${TBANK_INSTALL_DIR:-/opt/tbank}"
mkdir -p var
chmod 700 var
# A shared file lock prevents overlapping manual/cron runs. The process timeout
# is a second bound beyond the API context timeout configured in scanner.json.
exec flock -n var/hourly.lock timeout 5m ./bin/tbank scan -config config/scanner.json -notify
