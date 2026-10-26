#!/usr/bin/env bash
# Fresh race tests; CI runs the registry shards and remaining packages in parallel.
set -euo pipefail
cd "$(dirname "$0")/.."

usage() {
  echo "usage: $0 [all|packages|registry SHARD COUNT]" >&2
  exit 2
}

mode=${1:-all}
args=()
manifest_args=()
case "$mode" in
  all|packages) [[ $# -le 1 ]] || usage ;;
  registry)
    [[ $# -eq 3 && $2 =~ ^[0-9]+$ && $3 =~ ^[0-9]+$ ]] || usage
    ;;
  *) usage ;;
esac

log_dir=${CI_LOG_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/telegramgw-go-tests.XXXXXX")}
mkdir -p "$log_dir"
label=$mode
if [[ "$mode" == registry ]]; then
  label="registry-$2-of-$3"
fi
log="$log_dir/$label.jsonl"
manifest="$log_dir/$label-plan.json"
started_at=$SECONDS

case "$mode" in
  all) args=(./...) ;;
  packages)
    packages=$(python3 scripts/ci_go_shards.py packages --manifest "$manifest")
    while IFS= read -r package; do
      args+=("$package")
    done <<< "$packages"
    manifest_args=(--manifest "$manifest")
    ;;
  registry)
    pattern=$(python3 scripts/ci_go_shards.py registry --shard "$2" --shards "$3" --manifest "$manifest")
    args=(-run "$pattern" ./internal/registry)
    manifest_args=(--manifest "$manifest")
    ;;
esac

printf 'Race test JSON and timings: %s\n' "$log"
# Keep the complete isolated SQLite suite enabled on slower hosted runners.
# -count=1 always executes tests even when source-keyed compilation is cached.
set +e
go test -json -race -count=1 -timeout=1800s "${args[@]}" | tee "$log"
pipeline_status=("${PIPESTATUS[@]}")
set -e
elapsed_seconds=$((SECONDS - started_at))
summary_args=(--log "$log" --label "$label" --elapsed-seconds "$elapsed_seconds" "${manifest_args[@]}")
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  summary_args+=(--github-summary "$GITHUB_STEP_SUMMARY")
fi
summary_status=0
python3 scripts/ci_go_shards.py summary "${summary_args[@]}" || summary_status=$?
[[ "${pipeline_status[0]}" -eq 0 && "${pipeline_status[1]}" -eq 0 && "$summary_status" -eq 0 ]]
