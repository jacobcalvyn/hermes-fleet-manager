#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
source "$ROOT_DIR/scripts/assert-compose-owner.sh"
assert_compose_owner "$ROOT_DIR"

if [[ "$(uname -s)" == "Darwin" ]]; then
  launchctl bootout "gui/$(id -u)/io.hermes-fleet.host-agent" >/dev/null 2>&1 || true
elif [[ -f .state/host-agent.pid ]]; then
  pid="$(cat .state/host-agent.pid)"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
  fi
  rm -f .state/host-agent.pid
fi

docker compose down
