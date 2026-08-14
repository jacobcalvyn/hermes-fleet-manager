#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/systemd-control-plane-watchdog-unit.sh"

SERVICE_NAME="hermes-fleet-control-plane-watchdog.service"
TIMER_NAME="hermes-fleet-control-plane-watchdog.timer"
WATCHDOG_TARGET="${FLEET_WATCHDOG_BINARY_PATH:-/usr/local/libexec/hermes-fleet-control-plane-watchdog}"
SERVICE_PATH="${FLEET_WATCHDOG_SERVICE_PATH:-/etc/systemd/system/$SERVICE_NAME}"
TIMER_PATH="${FLEET_WATCHDOG_TIMER_PATH:-/etc/systemd/system/$TIMER_NAME}"

if [[ "$(uname -s)" != "Linux" || "$(id -u)" != "0" ]]; then
  echo "Install the Fleet watchdog as root on Linux." >&2
  exit 1
fi
for command_name in docker curl systemctl install; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "$command_name is required before installing the Fleet watchdog." >&2
    exit 1
  fi
done

install -d -o root -g root -m 0755 "$(dirname "$WATCHDOG_TARGET")" "$(dirname "$SERVICE_PATH")" "$(dirname "$TIMER_PATH")"
install -d -o root -g root -m 0700 /var/lib/hermes-fleet-watchdog
install -o root -g root -m 0755 "$ROOT_DIR/scripts/control-plane-watchdog.sh" "$WATCHDOG_TARGET"

service_stage="$(mktemp)"
timer_stage="$(mktemp)"
trap 'rm -f "$service_stage" "$timer_stage"' EXIT
render_systemd_control_plane_watchdog_service "$WATCHDOG_TARGET" "$ROOT_DIR" > "$service_stage"
render_systemd_control_plane_watchdog_timer > "$timer_stage"
install -o root -g root -m 0644 "$service_stage" "$SERVICE_PATH"
install -o root -g root -m 0644 "$timer_stage" "$TIMER_PATH"
systemctl daemon-reload
systemctl enable --now "$TIMER_NAME"

echo "systemd timer $TIMER_NAME is enabled with bounded liveness recovery."
