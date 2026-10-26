#!/usr/bin/env bash
# Independent checks used by the reusable workflow and the full local CI command.
set -euo pipefail
cd "$(dirname "$0")/.."

usage() {
  echo "usage: $0 [all|format|vet|vulnerabilities|python]" >&2
  exit 2
}

check_format() {
  local unformatted
  unformatted=$(gofmt -l cmd internal migrations native)
  if [[ -n "$unformatted" ]]; then
    printf 'Go files need gofmt:\n%s\n' "$unformatted" >&2
    return 1
  fi
}

check_python() {
  # Python tests the source-only PostgreSQL migration utilities and CI planner.
  # Installation, updates and release packaging use native Go executables.
  PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_sqlite_*.py'
  PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_ci_*.py'
}

[[ $# -le 1 ]] || usage
case "${1:-all}" in
  format) check_format ;;
  vet) go vet ./... ;;
  vulnerabilities) go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./... ;;
  python) check_python ;;
  all)
    check_format
    go vet ./...
    go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
    check_python
    ;;
  *) usage ;;
esac
