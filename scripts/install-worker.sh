#!/usr/bin/env bash
# Install a released worker for the current user. No root is required.
set -euo pipefail

usage() { echo "usage: $0 --worker-binary ABSOLUTE_PATH --config ABSOLUTE_PATH [--agent codex|claude] [--local-binary ABSOLUTE_PATH] [--no-start]" >&2; exit 64; }
worker_bin=""; config=""; local_bin=""; start=1; agent_kind=codex
while (($#)); do
  case "$1" in
    --agent) agent_kind=${2:-}; shift 2 ;;
    --worker-binary) worker_bin=${2:-}; shift 2 ;;
    --config) config=${2:-}; shift 2 ;;
	--local-binary) local_bin=${2:-}; shift 2 ;;
    --no-start) start=0; shift ;;
    *) usage ;;
  esac
done
[[ "$worker_bin" = /* && "$config" = /* && -x "$worker_bin" && -f "$config" ]] || usage
[[ "$agent_kind" == codex || "$agent_kind" == claude ]] || usage
worker_name="${agent_kind}-worker"
if [[ "$agent_kind" == codex && -z "$local_bin" ]]; then
  local_bin="$(dirname -- "$worker_bin")/codex-local"
fi
if [[ "$agent_kind" == codex && ( "$local_bin" != /* || ! -x "$local_bin" ) ]]; then
  echo "codex-local helper is required for attach and was not found at $local_bin." >&2
  echo "Pass --local-binary ABSOLUTE_PATH or install the matching codex-local beside codex-worker." >&2
  exit 64
fi
# Values are consumed by a unit/plist parser. Keep their syntax intentionally
# narrow instead of trying to quote arbitrary shell or XML fragments.
for path in "$worker_bin" "$local_bin" "$config" "$HOME"; do
  case "$path" in *$'\n'*|*$'\r'*|*'"'*|*"'"*|*';'*|*[[:space:]]*) echo "unsafe path (spaces are unsupported by the user-service installer): $path" >&2; exit 64;; esac
done

install_dir="$HOME/.local/bin"
config_dir="$HOME/.config/$worker_name"
install -d -m 0755 "$install_dir"
install -d -m 0700 "$config_dir"
install_binary() {
  local source=$1 destination=$2
  if [[ -e "$destination" && "$source" -ef "$destination" ]]; then
    return
  fi
  install -m 0755 "$source" "$destination"
}
install_binary "$worker_bin" "$install_dir/$worker_name"
if [[ "$agent_kind" == codex ]]; then
  install_binary "$local_bin" "$install_dir/codex-local"
fi

# LoadWorker resolves relative fields against the source JSON and validates the
# result. Write its canonical JSON privately, then replace in one rename.
umask 077
tmp_config=$(mktemp "$config_dir/.config.json.XXXXXX")
trap 'rm -f -- "$tmp_config"' EXIT
"$worker_bin" --config "$config" config export >"$tmp_config"
chmod 0600 "$tmp_config"
mv -f -- "$tmp_config" "$config_dir/config.json"
trap - EXIT

case "$(uname -s)" in
  Linux)
    unit_dir="$HOME/.config/systemd/user"; mkdir -p "$unit_dir"
    install -m 0644 "$(dirname "$0")/../deploy/systemd/$worker_name.service" "$unit_dir/$worker_name.service"
    systemctl --user daemon-reload
    if ((start)); then systemctl --user enable --now "$worker_name.service"; fi
    ;;
  Darwin)
    plist="$HOME/Library/LaunchAgents/com.iaia.$worker_name.plist"
    mkdir -p "$(dirname "$plist")" "$HOME/Library/Logs"
    rm -f -- "$plist"
    pb=/usr/libexec/PlistBuddy
    "$pb" -c "Add :Label string com.iaia.$worker_name" "$plist"
    "$pb" -c 'Add :ProgramArguments array' "$plist"
    "$pb" -c "Add :ProgramArguments:0 string $install_dir/$worker_name" "$plist"
    "$pb" -c 'Add :ProgramArguments:1 string --config' "$plist"
    "$pb" -c "Add :ProgramArguments:2 string $config_dir/config.json" "$plist"
    "$pb" -c 'Add :ProgramArguments:3 string run' "$plist"
    "$pb" -c "Add :WorkingDirectory string $HOME" "$plist"
    "$pb" -c 'Add :EnvironmentVariables dict' "$plist"
    "$pb" -c "Add :EnvironmentVariables:HOME string $HOME" "$plist"
    "$pb" -c "Add :EnvironmentVariables:PATH string $install_dir:/usr/local/bin:/usr/bin:/bin" "$plist"
    "$pb" -c "Add :StandardOutPath string $HOME/Library/Logs/$worker_name.log" "$plist"
    "$pb" -c "Add :StandardErrorPath string $HOME/Library/Logs/$worker_name.log" "$plist"
    "$pb" -c 'Add :RunAtLoad bool true' "$plist"
    "$pb" -c 'Add :KeepAlive bool true' "$plist"
    plutil -lint "$plist"
    if ((start)); then
      uid=$(id -u)
      launchctl bootout "gui/$uid/com.iaia.$worker_name" 2>/dev/null || true
      launchctl bootstrap "gui/$uid" "$plist"
    fi
    ;;
  *) echo "unsupported operating system" >&2; exit 69 ;;
esac
