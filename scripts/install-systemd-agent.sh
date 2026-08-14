#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/systemd-agent-unit.sh"

SERVICE_NAME="hermes-fleet-host-agent.service"
RECOVERY_SERVICE_NAME="hermes-fleet-host-agent-recovery.service"
RECOVERY_TIMER_NAME="hermes-fleet-host-agent-recovery.timer"
SERVICE_USER="${FLEET_AGENT_SERVICE_USER:-hermes-fleet-agent}"
AGENT_SOURCE="$ROOT_DIR/bin/hermes-fleet-agent"
AGENT_TARGET="${FLEET_AGENT_BINARY_PATH:-/usr/local/bin/hermes-fleet-agent}"
AGENT_CONFIG="${FLEET_AGENT_CONFIG_PATH:-/etc/hermes-fleet/agent.json}"
MANAGED_ROOT="${FLEET_MANAGED_ROOT:-/var/lib/hermes-fleet-agent/instances}"
STATE_DIR="$(dirname "$MANAGED_ROOT")"
LOG_DIR="${FLEET_AGENT_LOG_DIR:-/var/log/hermes-fleet-agent}"
UNIT_PATH="${FLEET_SYSTEMD_UNIT_PATH:-/etc/systemd/system/$SERVICE_NAME}"
RECOVERY_SERVICE_PATH="${FLEET_SYSTEMD_RECOVERY_SERVICE_PATH:-/etc/systemd/system/$RECOVERY_SERVICE_NAME}"
RECOVERY_TIMER_PATH="${FLEET_SYSTEMD_RECOVERY_TIMER_PATH:-/etc/systemd/system/$RECOVERY_TIMER_NAME}"
DOCKER_PATH="$(command -v docker || true)"
agent_backup=""
unit_backup=""
recovery_service_backup=""
recovery_timer_backup=""
unit_stage=""
recovery_service_stage=""
recovery_timer_stage=""
had_agent=0
had_unit=0
had_recovery_service=0
had_recovery_timer=0
install_committed=0
install_verified=0

cleanup_install_files() {
  rm -f \
    "$agent_backup" \
    "$unit_backup" \
    "$recovery_service_backup" \
    "$recovery_timer_backup" \
    "$unit_stage" \
    "$recovery_service_stage" \
    "$recovery_timer_stage"
}

rollback_install() {
  if [[ "$install_committed" != "1" || "$install_verified" == "1" ]]; then
    return
  fi
  echo "Host Agent verification failed; restoring the previous systemd installation." >&2
  if [[ "$had_agent" == "1" ]]; then
    install -o root -g root -m 0755 "$agent_backup" "$AGENT_TARGET" || true
  else
    rm -f "$AGENT_TARGET"
  fi
  if [[ "$had_unit" == "1" ]]; then
    install -o root -g root -m 0644 "$unit_backup" "$UNIT_PATH" || true
  else
    systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
    rm -f "$UNIT_PATH"
  fi
  systemctl stop "$RECOVERY_TIMER_NAME" >/dev/null 2>&1 || true
  if [[ "$had_recovery_service" == "1" ]]; then
    install -o root -g root -m 0644 "$recovery_service_backup" "$RECOVERY_SERVICE_PATH" || true
  else
    rm -f "$RECOVERY_SERVICE_PATH"
  fi
  if [[ "$had_recovery_timer" == "1" ]]; then
    install -o root -g root -m 0644 "$recovery_timer_backup" "$RECOVERY_TIMER_PATH" || true
  else
    rm -f "$RECOVERY_TIMER_PATH"
  fi
  systemctl daemon-reload || true
  if [[ "$had_unit" == "1" ]]; then
    systemctl restart "$SERVICE_NAME" || true
  fi
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

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "The systemd Host Agent installer requires Linux." >&2
  exit 1
fi
if [[ "$(id -u)" != "0" ]]; then
  echo "Run the systemd Host Agent installer as root." >&2
  exit 1
fi
for command_name in systemctl install groupadd useradd usermod getent; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "$command_name is required before installing the systemd Host Agent." >&2
    exit 1
  fi
done
if [[ -z "$DOCKER_PATH" ]]; then
  echo "Docker CLI is required before installing the systemd Host Agent." >&2
  exit 1
fi
if [[ ! -x "$AGENT_SOURCE" ]]; then
  echo "Build bin/hermes-fleet-agent before installing the systemd service." >&2
  exit 1
fi
if [[ ! -f "$AGENT_CONFIG" ]]; then
  echo "Enroll the Host Agent before installing the systemd service." >&2
  exit 1
fi
if ! "$AGENT_SOURCE" validate --config "$AGENT_CONFIG" >/dev/null; then
  echo "The new Host Agent rejected the enrolled configuration." >&2
  exit 1
fi
if ! getent group docker >/dev/null; then
  echo "The Docker group does not exist; install and start Docker first." >&2
  exit 1
fi

if ! getent group "$SERVICE_USER" >/dev/null; then
  groupadd --system "$SERVICE_USER"
fi
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd \
    --system \
    --home-dir "$STATE_DIR" \
    --create-home \
    --gid "$SERVICE_USER" \
    --shell /usr/sbin/nologin \
    --groups docker \
    "$SERVICE_USER"
else
  usermod --gid "$SERVICE_USER" "$SERVICE_USER"
  usermod -aG docker "$SERVICE_USER"
fi

install -d -o root -g root -m 0755 \
  "$(dirname "$AGENT_TARGET")" \
  "$(dirname "$UNIT_PATH")" \
  "$(dirname "$RECOVERY_SERVICE_PATH")" \
  "$(dirname "$RECOVERY_TIMER_PATH")"
install -d -o root -g "$SERVICE_USER" -m 0750 "$(dirname "$AGENT_CONFIG")"
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0700 "$STATE_DIR" "$MANAGED_ROOT" "$LOG_DIR"
chown "$SERVICE_USER:$SERVICE_USER" "$AGENT_CONFIG"
chmod 0600 "$AGENT_CONFIG"

agent_backup="$(mktemp)"
unit_backup="$(mktemp)"
recovery_service_backup="$(mktemp)"
recovery_timer_backup="$(mktemp)"
if [[ -f "$AGENT_TARGET" ]]; then
  had_agent=1
  cp -p "$AGENT_TARGET" "$agent_backup"
fi
if [[ -f "$UNIT_PATH" ]]; then
  had_unit=1
  cp -p "$UNIT_PATH" "$unit_backup"
fi
if [[ -f "$RECOVERY_SERVICE_PATH" ]]; then
  had_recovery_service=1
  cp -p "$RECOVERY_SERVICE_PATH" "$recovery_service_backup"
fi
if [[ -f "$RECOVERY_TIMER_PATH" ]]; then
  had_recovery_timer=1
  cp -p "$RECOVERY_TIMER_PATH" "$recovery_timer_backup"
fi

unit_stage="$(mktemp)"
recovery_service_stage="$(mktemp)"
recovery_timer_stage="$(mktemp)"
render_systemd_agent_unit \
  "$SERVICE_USER" \
  "$AGENT_TARGET" \
  "$AGENT_CONFIG" \
  "$DOCKER_PATH" \
  "$STATE_DIR" \
  "$LOG_DIR" > "$unit_stage"
render_systemd_agent_recovery_service > "$recovery_service_stage"
render_systemd_agent_recovery_timer > "$recovery_timer_stage"

install_committed=1
install -o root -g root -m 0755 "$AGENT_SOURCE" "$AGENT_TARGET"
install -o root -g root -m 0644 "$unit_stage" "$UNIT_PATH"
install -o root -g root -m 0644 "$recovery_service_stage" "$RECOVERY_SERVICE_PATH"
install -o root -g root -m 0644 "$recovery_timer_stage" "$RECOVERY_TIMER_PATH"
rm -f "$unit_stage" "$recovery_service_stage" "$recovery_timer_stage"
unit_stage=""
recovery_service_stage=""
recovery_timer_stage=""
systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

for _ in $(seq 1 30); do
  if systemctl is-active --quiet "$SERVICE_NAME" \
    && "$AGENT_TARGET" probe --config "$AGENT_CONFIG" >/dev/null 2>&1; then
    install_verified=1
    break
  fi
  sleep 1
done
if [[ "$install_verified" != "1" ]]; then
  systemctl status "$SERVICE_NAME" --no-pager >&2 || true
  journalctl -u "$SERVICE_NAME" -n 40 --no-pager >&2 || true
  exit 1
fi

echo "systemd service $SERVICE_NAME is enabled, running, and protected by bounded delayed recovery."
