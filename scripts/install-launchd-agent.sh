#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENT_SOURCE="$ROOT_DIR/bin/hermes-fleet-agent"
AGENT_TARGET="$HOME/.local/bin/hermes-fleet-agent"
AGENT_CONFIG="$HOME/.config/hermes-fleet/agent.json"
PLIST_PATH="$HOME/Library/LaunchAgents/io.hermes-fleet.host-agent.plist"
LOG_DIR="$HOME/Library/Logs/HermesFleet"
DOMAIN="gui/$(id -u)"
SERVICE="$DOMAIN/io.hermes-fleet.host-agent"
DOCKER_PATH="$(command -v docker || true)"
DOCKER_CONFIG_PATH="$HOME/.docker/config.json"
APPROVAL_CHECKER="$ROOT_DIR/scripts/macos-launchagent-status.swift"
LAUNCH_AGENT_PATH=""
install_committed=0
install_verified=0
install_preserved=0
had_agent=0
had_plist=0
agent_stage=""
plist_stage=""
plist_pending="${PLIST_PATH}.pending"
agent_backup=""
plist_backup=""

append_launch_agent_path() {
  local directory="${1%/}"

  if [[ -z "$directory" || ! -d "$directory" ]]; then
    return
  fi
  case ":$LAUNCH_AGENT_PATH:" in
    *":$directory:"*) return ;;
  esac
  if [[ -n "$LAUNCH_AGENT_PATH" ]]; then
    LAUNCH_AGENT_PATH="$LAUNCH_AGENT_PATH:$directory"
  else
    LAUNCH_AGENT_PATH="$directory"
  fi
}

launch_agent_command_path() {
  local command_name="$1"

  PATH="$LAUNCH_AGENT_PATH" command -v "$command_name" 2>/dev/null || true
}

configure_launch_agent_path() {
  local docker_directory=""
  local configured_store=""
  local credential_helper=""
  local credential_helper_path=""
  local path_directory=""
  local helper_candidate=""
  local -a installer_path=()

  docker_directory="$(dirname "$DOCKER_PATH")"
  append_launch_agent_path "$docker_directory"

  IFS=':' read -r -a installer_path <<< "${PATH:-}"
  for path_directory in "${installer_path[@]}"; do
    if [[ -z "$path_directory" || ! -d "$path_directory" ]]; then
      continue
    fi
    for helper_candidate in "$path_directory"/docker-credential-*; do
      if [[ -x "$helper_candidate" ]]; then
        append_launch_agent_path "$path_directory"
        break
      fi
    done
  done

  append_launch_agent_path "/Applications/Docker.app/Contents/Resources/bin"
  append_launch_agent_path "/opt/homebrew/bin"
  append_launch_agent_path "/usr/local/bin"
  append_launch_agent_path "/usr/bin"
  append_launch_agent_path "/bin"
  append_launch_agent_path "/usr/sbin"
  append_launch_agent_path "/sbin"

  if [[ -f "$DOCKER_CONFIG_PATH" ]]; then
    configured_store="$(plutil -extract credsStore raw -o - "$DOCKER_CONFIG_PATH" 2>/dev/null || true)"
  fi
  if [[ -z "$configured_store" ]]; then
    return
  fi

  credential_helper="docker-credential-$configured_store"
  credential_helper_path="$(launch_agent_command_path "$credential_helper")"
  if [[ -z "$credential_helper_path" || ! -x "$credential_helper_path" ]]; then
    echo "Docker credential helper $credential_helper is configured but is not available to the Host Agent." >&2
    echo "Install or repair the credential helper before installing the LaunchAgent." >&2
    exit 1
  fi
}

cleanup_install_files() {
  local filename=""
  for filename in "$agent_stage" "$plist_stage" "$plist_pending" "$agent_backup" "$plist_backup"; do
    if [[ -n "$filename" ]]; then
      rm -f "$filename"
    fi
  done
}

rollback_install() {
  if [[ "$install_committed" != "1" || "$install_verified" == "1" || "$install_preserved" == "1" ]]; then
    return
  fi
  echo "Host Agent verification failed; restoring the previous installation." >&2
  launchctl bootout "$SERVICE" >/dev/null 2>&1 || true
  if [[ "$had_agent" == "1" ]]; then
    install -m 0755 "$agent_backup" "$AGENT_TARGET" || true
  else
    rm -f "$AGENT_TARGET"
  fi
  if [[ "$had_plist" == "1" ]]; then
    install -m 0644 "$plist_backup" "$PLIST_PATH" || true
    launchctl bootstrap "$DOMAIN" "$PLIST_PATH" >/dev/null 2>&1 || true
    launchctl kickstart -k "$SERVICE" >/dev/null 2>&1 || true
  else
    rm -f "$PLIST_PATH"
  fi
}

launch_agent_approval_status() {
  local status=""

  if [[ ! -f "$APPROVAL_CHECKER" ]] || ! command -v swift >/dev/null 2>&1; then
    printf '%s\n' "unavailable"
    return
  fi
  if ! status="$(swift "$APPROVAL_CHECKER" "$PLIST_PATH" 2>/dev/null)"; then
    printf '%s\n' "unavailable"
    return
  fi
  case "$status" in
    enabled|requires-approval|not-registered|not-found|unsupported)
      printf '%s\n' "$status"
      ;;
    *)
      printf '%s\n' "unavailable"
      ;;
  esac
}

report_launch_agent_approval_failure() {
  local status="$1"

  if [[ "$status" == "requires-approval" ]]; then
    echo "Host Agent requires approval before macOS can start it after login or reboot." >&2
    echo "Open System Settings > General > Login Items & Extensions, then enable hermes-fleet-agent under Allow in Background." >&2
    echo "Rerun this installer after approval." >&2
    return
  fi
  echo "Host Agent restart persistence could not be verified (ServiceManagement status: $status)." >&2
  echo "Do not treat the LaunchAgent installation as complete; verify the background-item approval and rerun this installer." >&2
}

require_launch_agent_approval() {
  local status=""

  status="$(launch_agent_approval_status)"
  case "$status" in
    enabled|unsupported)
      return 0
      ;;
    *)
      report_launch_agent_approval_failure "$status"
      return 1
      ;;
  esac
}

finish_install() {
  local status=$?
  trap - EXIT
  if [[ "$status" != "0" ]]; then
    rollback_install
  fi
  cleanup_install_files
  exit "$status"
}

trap finish_install EXIT

if [[ -z "$DOCKER_PATH" ]]; then
  echo "Docker CLI is required before installing the Host Agent." >&2
  exit 1
fi
configure_launch_agent_path
if [[ ! -f "$APPROVAL_CHECKER" ]]; then
  echo "The macOS LaunchAgent approval checker is missing." >&2
  exit 1
fi
if [[ ! -x "$AGENT_SOURCE" ]]; then
  echo "Build bin/hermes-fleet-agent before installing the LaunchAgent." >&2
  exit 1
fi
if [[ ! -f "$AGENT_CONFIG" ]]; then
  echo "Enroll the Host Agent before installing the LaunchAgent." >&2
  exit 1
fi
if ! "$AGENT_SOURCE" validate --config "$AGENT_CONFIG" >/dev/null; then
  echo "The new Host Agent rejected the existing configuration." >&2
  exit 1
fi

mkdir -p "$HOME/.local/bin" "$HOME/Library/LaunchAgents" "$LOG_DIR"
agent_stage="$(mktemp "${AGENT_TARGET}.new.XXXXXX")"
plist_stage="$(mktemp "${PLIST_PATH}.new.XXXXXX")"
agent_backup="$(mktemp "${AGENT_TARGET}.backup.XXXXXX")"
plist_backup="$(mktemp "${PLIST_PATH}.backup.XXXXXX")"
if [[ -f "$AGENT_TARGET" ]]; then
  had_agent=1
  cp -p "$AGENT_TARGET" "$agent_backup"
fi
if [[ -f "$PLIST_PATH" ]]; then
  had_plist=1
  cp -p "$PLIST_PATH" "$plist_backup"
fi
install -m 0755 "$AGENT_SOURCE" "$agent_stage"

cat > "$plist_stage" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>io.hermes-fleet.host-agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>${AGENT_TARGET}</string>
    <string>run</string>
    <string>--config</string>
    <string>${AGENT_CONFIG}</string>
    <string>--docker</string>
    <string>${DOCKER_PATH}</string>
    <string>--log-path</string>
    <string>${LOG_DIR}/host-agent.log</string>
    <string>--log-max-bytes</string>
    <string>26214400</string>
    <string>--log-max-files</string>
    <string>4</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${LAUNCH_AGENT_PATH}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>/dev/null</string>
  <key>StandardErrorPath</key>
  <string>/dev/null</string>
</dict>
</plist>
EOF

plutil -lint "$plist_stage" >/dev/null
if [[ -f "$AGENT_TARGET" && -f "$PLIST_PATH" ]] \
  && cmp -s "$AGENT_SOURCE" "$AGENT_TARGET" \
  && cmp -s "$plist_stage" "$PLIST_PATH" \
  && launchctl print "$SERVICE" >/dev/null 2>&1 \
  && "$AGENT_TARGET" probe --config "$AGENT_CONFIG" >/dev/null 2>&1; then
  if require_launch_agent_approval; then
    install_verified=1
    echo "LaunchAgent io.hermes-fleet.host-agent is already current, approved, and verified."
    exit 0
  fi
  exit 1
fi
install -m 0644 "$plist_stage" "$plist_pending"
install_committed=1
mv -f "$agent_stage" "$AGENT_TARGET"
agent_stage=""
mv -f "$plist_pending" "$PLIST_PATH"
plist_pending=""
if ! require_launch_agent_approval; then
  # Keep the valid installation visible in Login Items so the operator can
  # approve it. Rolling it back would remove the item that needs approval.
  install_preserved=1
  exit 1
fi
launchctl bootout "$SERVICE" >/dev/null 2>&1 || true
if ! launchctl bootstrap "$DOMAIN" "$PLIST_PATH"; then
  echo "LaunchAgent bootstrap failed; retrying once after a short delay." >&2
  sleep 1
  launchctl bootout "$SERVICE" >/dev/null 2>&1 || true
  launchctl bootstrap "$DOMAIN" "$PLIST_PATH"
fi
launchctl kickstart -k "$SERVICE"

for _ in $(seq 1 20); do
  if launchctl print "$SERVICE" >/dev/null 2>&1 &&
    "$AGENT_TARGET" probe --config "$AGENT_CONFIG" >/dev/null 2>&1; then
    install_verified=1
    break
  fi
  sleep 1
done
if [[ "$install_verified" != "1" ]]; then
  echo "The new LaunchAgent did not pass its post-install probe." >&2
  exit 1
fi

echo "LaunchAgent io.hermes-fleet.host-agent is approved, running, and verified."
