#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/systemd-agent-unit.sh"

unit="$(
  render_systemd_agent_unit \
    hermes-fleet-agent \
    /usr/local/bin/hermes-fleet-agent \
    /etc/hermes-fleet/agent.json \
    /usr/bin/docker \
    /var/lib/hermes-fleet-agent \
    /var/log/hermes-fleet-agent
)"

for expected in \
  'User=hermes-fleet-agent' \
  'SupplementaryGroups=docker' \
  'OnFailure=hermes-fleet-host-agent-recovery.timer' \
  'StartLimitIntervalSec=300' \
  'StartLimitBurst=3' \
  'Restart=on-failure' \
  'RestartSec=30' \
  'TimeoutStopSec=660' \
  'NoNewPrivileges=true' \
  'ProtectSystem=strict' \
  'ProtectHome=true' \
  'ReadWritePaths=/var/lib/hermes-fleet-agent /var/log/hermes-fleet-agent' \
  'WantedBy=multi-user.target'; do
  if ! grep -Fqx -- "$expected" <<< "$unit"; then
    echo "systemd unit is missing: $expected" >&2
    exit 1
  fi
done

if ! grep -Fq -- '--shutdown-grace-period 10m' <<< "$unit"; then
  echo "systemd unit does not pass the Host Agent shutdown grace period." >&2
  exit 1
fi

recovery_service="$(render_systemd_agent_recovery_service)"
for expected in \
  'Type=oneshot' \
  'ExecStart=/bin/systemctl reset-failed hermes-fleet-host-agent.service' \
  'ExecStart=/bin/systemctl start hermes-fleet-host-agent.service'; do
  if ! grep -Fqx -- "$expected" <<< "$recovery_service"; then
    echo "systemd recovery service is missing: $expected" >&2
    exit 1
  fi
done

recovery_timer="$(render_systemd_agent_recovery_timer)"
for expected in \
  'OnActiveSec=5min' \
  'AccuracySec=10s' \
  'Unit=hermes-fleet-host-agent-recovery.service'; do
  if ! grep -Fqx -- "$expected" <<< "$recovery_timer"; then
    echo "systemd recovery timer is missing: $expected" >&2
    exit 1
  fi
done

if grep -Fq '/home/' <<< "$unit"; then
  echo "systemd unit unexpectedly depends on an interactive user home." >&2
  exit 1
fi

installer="$ROOT_DIR/scripts/install-systemd-agent.sh"
if ! grep -Fq 'systemctl restart "$SERVICE_NAME"' "$installer"; then
  echo "systemd installer must restart an already-running Host Agent after replacing its binary." >&2
  exit 1
fi
if grep -Fq 'systemctl enable --now "$SERVICE_NAME"' "$installer"; then
  echo "systemd installer must not rely on enable --now to activate a replaced Host Agent binary." >&2
  exit 1
fi

echo "systemd Host Agent unit tests passed."
