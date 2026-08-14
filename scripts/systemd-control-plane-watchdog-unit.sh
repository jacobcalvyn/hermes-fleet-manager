#!/usr/bin/env bash

render_systemd_control_plane_watchdog_service() {
  local watchdog_binary="$1"
  local project_directory="$2"

  cat <<EOF
[Unit]
Description=Hermes Fleet control-plane liveness watchdog
After=docker.service network-online.target
Requires=docker.service

[Service]
Type=oneshot
Environment="FLEET_WATCHDOG_PROJECT_DIRECTORY=${project_directory}"
ExecStart=${watchdog_binary}
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=read-only
ProtectSystem=strict
ReadWritePaths=/var/lib/hermes-fleet-watchdog
EOF
}

render_systemd_control_plane_watchdog_timer() {
  cat <<'EOF'
[Unit]
Description=Run the Hermes Fleet control-plane liveness watchdog

[Timer]
OnBootSec=2min
OnUnitActiveSec=30s
AccuracySec=5s
Persistent=true
Unit=hermes-fleet-control-plane-watchdog.service

[Install]
WantedBy=timers.target
EOF
}
