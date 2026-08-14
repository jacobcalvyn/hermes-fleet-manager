#!/usr/bin/env bash

render_systemd_agent_unit() {
  local service_user="$1"
  local agent_binary="$2"
  local agent_config="$3"
  local docker_path="$4"
  local managed_state_dir="$5"
  local log_dir="$6"

  cat <<EOF
[Unit]
Description=Hermes Fleet Host Agent
Documentation=https://github.com/jacobcalvyn/hermes-fleet-manager
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service
OnFailure=hermes-fleet-host-agent-recovery.timer
StartLimitIntervalSec=300
StartLimitBurst=3

[Service]
Type=simple
User=${service_user}
Group=${service_user}
SupplementaryGroups=docker
UMask=0077
ExecStartPre=${agent_binary} validate --config ${agent_config}
ExecStart=${agent_binary} run --config ${agent_config} --docker ${docker_path} --log-path ${log_dir}/host-agent.log --log-max-bytes 26214400 --log-max-files 4 --shutdown-grace-period 10m
Restart=on-failure
RestartSec=30
TimeoutStopSec=660
NoNewPrivileges=true
CapabilityBoundingSet=
AmbientCapabilities=
PrivateDevices=true
PrivateTmp=true
ProtectClock=true
ProtectControlGroups=true
ProtectHome=true
ProtectHostname=true
ProtectKernelLogs=true
ProtectKernelModules=true
ProtectKernelTunables=true
ProtectSystem=strict
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
SystemCallArchitectures=native
ReadWritePaths=${managed_state_dir} ${log_dir}

[Install]
WantedBy=multi-user.target
EOF
}

render_systemd_agent_recovery_service() {
  cat <<'EOF'
[Unit]
Description=Recover Hermes Fleet Host Agent after bounded backoff

[Service]
Type=oneshot
ExecStart=/bin/systemctl reset-failed hermes-fleet-host-agent.service
ExecStart=/bin/systemctl start hermes-fleet-host-agent.service
NoNewPrivileges=true
ProtectHome=true
ProtectSystem=strict
PrivateTmp=true
EOF
}

render_systemd_agent_recovery_timer() {
  cat <<'EOF'
[Unit]
Description=Delayed recovery for Hermes Fleet Host Agent

[Timer]
OnActiveSec=5min
AccuracySec=10s
Unit=hermes-fleet-host-agent-recovery.service
EOF
}
