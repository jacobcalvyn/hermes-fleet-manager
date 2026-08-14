#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/systemd-control-plane-watchdog-unit.sh"

service="$(render_systemd_control_plane_watchdog_service /usr/local/libexec/hermes-fleet-control-plane-watchdog /opt/hermes-fleet-manager)"
for expected in \
  'Type=oneshot' \
  'Environment="FLEET_WATCHDOG_PROJECT_DIRECTORY=/opt/hermes-fleet-manager"' \
  'ExecStart=/usr/local/libexec/hermes-fleet-control-plane-watchdog' \
  'ProtectHome=read-only' \
  'ProtectSystem=strict' \
  'ReadWritePaths=/var/lib/hermes-fleet-watchdog'; do
  if ! grep -Fqx -- "$expected" <<< "$service"; then
    echo "watchdog service is missing: $expected" >&2
    exit 1
  fi
done

timer="$(render_systemd_control_plane_watchdog_timer)"
for expected in \
  'OnBootSec=2min' \
  'OnUnitActiveSec=30s' \
  'Persistent=true' \
  'WantedBy=timers.target'; do
  if ! grep -Fqx -- "$expected" <<< "$timer"; then
    echo "watchdog timer is missing: $expected" >&2
    exit 1
  fi
done

echo "systemd control-plane watchdog unit tests passed."
