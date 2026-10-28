#!/usr/bin/env bash
# Install the dedicated Claude worker using the same per-user service installer.
set -euo pipefail
exec "$(dirname "$0")/install-worker.sh" --agent claude "$@"
