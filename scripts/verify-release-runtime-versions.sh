#!/usr/bin/env bash
# Execute the host gateway package's version commands before publishing it.
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 EXPECTED_VERSION [DIST_DIRECTORY]" >&2
  exit 2
fi
expected_version="${1#v}"
if [[ ! "$expected_version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]*$ ]]; then
  echo 'Expected a nonempty release version or CI revision.' >&2
  exit 2
fi
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
dist_dir="${2:-$repo_root/dist}"
case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) target=linux-amd64 ;;
  Linux/aarch64|Linux/arm64) target=linux-arm64 ;;
  *) echo 'Runtime version verification requires a supported native Linux gateway host.' >&2; exit 1 ;;
esac
archive="$dist_dir/codex-gateway-$target.tar.gz"
umask 077
stage_dir=$(mktemp -d "${TMPDIR:-/tmp}/telegramgw-runtime-versions.XXXXXX")
trap 'rm -rf -- "$stage_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Copy only the four expected regular executables. Do not let archive links,
# duplicate members or traversal paths escape the private extraction directory.
python3 - "$archive" "$stage_dir" <<'PY'
import os, pathlib, shutil, sys, tarfile

required = ('codex-gateway', 'codex-telegramgw', 'scripts/release-manager.py', 'codex-whisper')
with tarfile.open(sys.argv[1], 'r:gz') as archive:
    members = {}
    for member in archive:
        name = member.name.rstrip('/')
        if (not name or '\\' in name or name.startswith('/')
                or any(part in ('', '.', '..') for part in name.split('/'))
                or name in members or not (member.isfile() or member.isdir())):
            raise SystemExit(f'Unsafe or duplicate package member: {member.name!r}')
        members[name] = member
    for name in required:
        member = members.get(name)
        if member is None or not member.isfile() or not member.mode & 0o111:
            raise SystemExit(f'Missing regular executable in gateway package: {name}')
        destination = pathlib.Path(sys.argv[2]) / name
        destination.parent.mkdir(parents=True, exist_ok=True)
        with archive.extractfile(member) as source, destination.open('xb') as output:
            shutil.copyfileobj(source, output)
        os.chmod(destination, 0o700)
PY

for binary in codex-gateway codex-telegramgw scripts/release-manager.py codex-whisper; do
  if ! actual_version=$("$stage_dir/$binary" version); then
    printf 'Cannot read packaged %s runtime version.\n' "$binary" >&2
    exit 1
  fi
  if [[ "$actual_version" != "$expected_version" ]]; then
    printf 'Packaged %s version mismatch: expected %s, got %s.\n' "$binary" "$expected_version" "$actual_version" >&2
    exit 1
  fi
done
printf 'Packaged %s gateway, manager aliases and Whisper all report %s.\n' "$target" "$expected_version"
