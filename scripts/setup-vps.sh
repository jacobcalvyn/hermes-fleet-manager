#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "The VPS setup requires Linux." >&2
  exit 1
fi
if [[ "$(id -u)" != "0" ]]; then
  echo "Run scripts/setup-vps.sh as root." >&2
  exit 1
fi
if [[ ! -r /etc/os-release ]]; then
  echo "The VPS operating system could not be identified." >&2
  exit 1
fi

source /etc/os-release
case "${ID:-}" in
  ubuntu|debian)
    ;;
  *)
    echo "The VPS setup currently supports Ubuntu and Debian only." >&2
    exit 1
    ;;
esac

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y \
  ca-certificates \
  curl \
  docker.io \
  docker-compose-v2 \
  libdigest-sha-perl \
  openssl \
  python3

systemctl enable --now docker
docker info >/dev/null
docker compose version >/dev/null

fleet_installation_exists=0
if docker volume inspect hermes-fleet-manager-data >/dev/null 2>&1; then
  fleet_installation_exists=1
fi
if [[ "$fleet_installation_exists" == "1" && ! -f .env ]]; then
  echo "The existing Fleet data volume requires the original .env; restore it before upgrading." >&2
  exit 1
fi
if [[ ! -f .env ]]; then
  install -m 0600 .env.example .env
fi

set_env_value() {
  local key="$1"
  local value="$2"
  local temporary
  temporary="$(mktemp)"
  awk -v key="$key" -v value="$value" \
    'BEGIN { found=0 } $0 ~ "^" key "=" { if (!found) print key "=" value; found=1; next } { print } END { if (!found) print key "=" value }' \
    .env > "$temporary"
  mv "$temporary" .env
  chmod 0600 .env
}

host_name="${FLEET_VPS_HOST_NAME:-$(hostname -s)}"
if [[ -z "$host_name" ]]; then
  echo "The VPS host name must not be empty." >&2
  exit 1
fi
set_env_value FLEET_HOST_NAME "$host_name"
set_env_value FLEET_CONTROL_PLANE_URL "http://127.0.0.1:9180"

fleet_maintenance_command="fleet-bootstrap.sh"
if [[ "$fleet_installation_exists" == "1" ]]; then
  fleet_maintenance_command="fleet-upgrade.sh"
fi
FLEET_AGENT_CONFIG_PATH=/etc/hermes-fleet/agent.json \
FLEET_MANAGED_ROOT=/var/lib/hermes-fleet-agent/instances \
FLEET_AGENT_LOG_DIR=/var/log/hermes-fleet-agent \
FLEET_HOST_AGENT_SUPERVISOR=systemd \
  "$ROOT_DIR/scripts/$fleet_maintenance_command"

"$ROOT_DIR/scripts/install-systemd-watchdog.sh"
"$ROOT_DIR/scripts/vps-qualification.sh"

echo "Hermes Fleet Manager VPS deployment passed its soak gate on loopback port 9180."
echo "Use an SSH tunnel until reviewed remote access is configured:"
echo "ssh -L 9180:127.0.0.1:9180 <user>@<vps-ip>"
