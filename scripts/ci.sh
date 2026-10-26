#!/usr/bin/env bash
# Every integration test uses an isolated SQLite file; no database service is required.
set -euo pipefail
cd "$(dirname "$0")/.."
./scripts/check.sh
./scripts/test-go.sh
./scripts/release.sh
./scripts/verify-release.sh
