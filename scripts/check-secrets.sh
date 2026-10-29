#!/bin/sh
# Requires an independently installed, version-pinned Gitleaks binary.
set -eu
exec python3 "$(dirname "$0")/check-secrets.py" "$@"
