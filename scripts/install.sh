#!/bin/sh
# This HTTPS-fetched script is the initial trust root. Before execution, verify
# the known v0.7.9 native manager against a checksum pinned in this script.
set -eu

# The native manager selects gateway setup under sudo, worker setup otherwise.
# Explicit administration commands continue to be forwarded unchanged.
if [ "$#" -eq 0 ]; then
  set -- setup
fi

fail() { printf '%s\n' "Installation failed: $*" >&2; exit 1; }
stable_tag() {
  case "$1" in v*) tag_numbers=${1#v} ;; *) return 1 ;; esac
  tag_major=${tag_numbers%%.*}; tag_rest=${tag_numbers#*.}
  [ "$tag_rest" != "$tag_numbers" ] || return 1
  tag_minor=${tag_rest%%.*}; tag_patch=${tag_rest#*.}
  [ "$tag_patch" != "$tag_rest" ] || return 1
  for tag_part in "$tag_major" "$tag_minor" "$tag_patch"; do
    case "$tag_part" in ''|*[!0-9]*|0[0-9]*) return 1 ;; esac
  done
}
select_version() {
  stable_tag "$1" || fail '--version must select a stable vMAJOR.MINOR.PATCH release'
  [ -z "$release_tag" ] || [ "$release_tag" = "$1" ] || fail 'conflicting --version arguments'
  release_tag=$1
}
select_repo() {
  case "$1" in ''|*[!A-Za-z0-9_./-]*|/*|*/|*/*/*) fail '--repo must be OWNER/REPOSITORY' ;; esac
  case "$1" in */*) ;; *) fail '--repo must be OWNER/REPOSITORY' ;; esac
  for repo_part in "${1%%/*}" "${1#*/}"; do
    case "$repo_part" in .|..) fail '--repo must be OWNER/REPOSITORY' ;; esac
  done
  [ -z "$requested_repo" ] || [ "$requested_repo" = "$1" ] || fail 'conflicting --repo arguments'
  requested_repo=$1
  repository=$1
}

repository=arizzi74/MACT
requested_repo=
release_tag=
expect=
for argument do
  if [ -n "$expect" ]; then
    case "$expect" in version) select_version "$argument" ;; repo) select_repo "$argument" ;; esac
    expect=
    continue
  fi
  case "$argument" in
    --version) expect=version ;;
    --version=*) select_version "${argument#--version=}" ;;
    --repo) expect=repo ;;
    --repo=*) select_repo "${argument#--repo=}" ;;
  esac
done
[ -z "$expect" ] || fail "--$expect requires a value"
for utility in curl uname mktemp chmod rm; do
  command -v "$utility" >/dev/null 2>&1 || fail "$utility is required"
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  fail 'sha256sum or shasum is required'
fi
case "$(uname -s)" in Linux) platform=linux ;; Darwin) platform=darwin ;; *) fail 'unsupported operating system' ;; esac
case "$(uname -m)" in x86_64|amd64) architecture=amd64 ;; aarch64|arm64) architecture=arm64 ;; *) fail 'unsupported CPU architecture' ;; esac
asset="codex-telegramgw-$platform-$architecture"
# These pins come from the authenticated canonical v0.7.9 SHA256SUMS manifest
# (SHA256 7ed8bdf540fb43db1f67d6feb4d9aff202c8aa69a290cc0b55d29984d8eea309).
# The bootstrap version is independent of the release/repository being installed.
# This manager verifies signed provenance and checksums for the target release.
bootstrap_version=v0.7.9
case "$platform-$architecture" in
  linux-amd64) expected=c994d10109a0626d64a5209ca594534f2fa88ddd640650062f34df63c1739dd9 ;;
  linux-arm64) expected=e9c8aaf0fa6e95f004d6921b4b6a39415416b17265f8363f621d5e222731fe95 ;;
  darwin-amd64) expected=cdf9d3a74e3f534b237daba6b93417f344e1f11539fdda8fead20acd204ccecb ;;
  darwin-arm64) expected=700a5839c8a6f6d5750f75e38182662439f308114496f7f19611e0788127eecd ;;
esac
printf 'Preparing the %s installer for %s/%s...\n' "$1" "$platform" "$architecture"
download() {
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --tlsv1.2 --connect-timeout 15 --max-time 180 --retry 2 "$@"
}
selection=latest
if [ -n "$release_tag" ]; then selection="tag/$release_tag"; fi
release_url=$(download --max-filesize 1048576 --output /dev/null --write-out '%{url_effective}' \
  "https://github.com/$repository/releases/$selection") || fail 'could not resolve the selected release'
# Only the deliberate official rename can change repository scope. Resolve tag
# pages too: historical release assets acquire the renamed repository's URLs.
case "$repository:$release_url" in
  arizzi74/Codex-Telegram-Gateway:https://github.com/arizzi74/MACT/releases/tag/*) repository=arizzi74/MACT ;;
esac
case "$release_url" in
  "https://github.com/$repository/releases/tag/"*) resolved_tag=${release_url##*/} ;;
  *) fail 'GitHub returned an unexpected release URL' ;;
esac
stable_tag "$resolved_tag" || fail 'GitHub did not return a stable release tag'
[ "$release_url" = "https://github.com/$repository/releases/tag/$resolved_tag" ] || fail 'GitHub returned an unexpected release URL'
[ -z "$release_tag" ] || [ "$release_tag" = "$resolved_tag" ] || fail 'GitHub returned the wrong release version'
release_tag=$resolved_tag
if [ "$repository" = arizzi74/MACT ]; then
  case "$release_tag" in
    v0.0.*|v0.1.*|v0.2.*|v0.3.*|v0.4.*|v0.5.*|v0.6.[0-9]|v0.6.1[0-9]|v0.6.20)
      fail 'releases before v0.6.21 contain an updater that cannot follow the MACT repository rename; install v0.6.21 or newer' ;;
  esac
fi

temporary=$(mktemp -d "${TMPDIR:-/tmp}/codex-telegramgw-install.XXXXXXXX")
trap 'rm -rf -- "$temporary"' EXIT
trap 'exit 1' HUP INT TERM
base="https://github.com/arizzi74/MACT/releases/download/$bootstrap_version"
printf 'Downloading and verifying installer %s for release %s...\n' "$bootstrap_version" "$release_tag"
download --max-filesize 67108864 --output "$temporary/$asset" "$base/$asset" || fail 'could not download the release manager'
printf '%s  %s\n' "$expected" "$asset" > "$temporary/manager.sha256"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$temporary" && sha256sum -c manager.sha256) >/dev/null 2>&1 || fail 'installer checksum verification failed; no installer code was executed'
else
  (cd "$temporary" && shasum -a 256 -c manager.sha256) >/dev/null 2>&1 || fail 'installer checksum verification failed; no installer code was executed'
fi
chmod 0700 "$temporary/$asset"
CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE=$release_tag
export CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE
printf 'Pinned installer checksum verified. Follow the terminal prompts; press Enter to accept defaults.\n'
"$temporary/$asset" "$@"
