#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if "$ROOT_DIR/scripts/fleet-maintenance.sh" >/dev/null 2>&1; then
  echo "internal Fleet maintenance unexpectedly ran without an explicit action" >&2
  exit 1
fi

grep -q 'FLEET_SETUP_ACTION=bootstrap' "$ROOT_DIR/scripts/fleet-bootstrap.sh"
grep -q 'FLEET_SETUP_ACTION=upgrade' "$ROOT_DIR/scripts/fleet-upgrade.sh"
test -x "$ROOT_DIR/scripts/fleet-stop.sh"
test ! -e "$ROOT_DIR/scripts/setup-local.sh"
test ! -e "$ROOT_DIR/scripts/stop-local.sh"
test ! -e "$ROOT_DIR/scripts/hermes-runtime-sync.sh"

fleet_version="$(sed -n 's/^[[:space:]]*HostAgentVersion[[:space:]]*=[[:space:]]*"\([^"]*\)"/\1/p' "$ROOT_DIR/internal/compatibility/manifest.go")"
if [[ -z "$fleet_version" ]]; then
  echo "Fleet version could not be read from the compatibility manifest" >&2
  exit 1
fi
grep -Fq "local/hermes-fleet-manager:${fleet_version}" "$ROOT_DIR/docker-compose.yml"
grep -Fq "local/hermes-fleet-cloudflare-connector:${fleet_version}" "$ROOT_DIR/docker-compose.yml"
grep -Fq "local/hermes-fleet-manager:${fleet_version}" "$ROOT_DIR/scripts/control-plane-upgrade.sh"
grep -Fq "\"version\": \"${fleet_version}\"" "$ROOT_DIR/web/package.json"
grep -q 'compatibility.HostAgentVersion' "$ROOT_DIR/internal/mcpdiscovery/client.go"

if grep -q 'runtime/image_smoke_test.sh' "$ROOT_DIR/scripts/fleet-maintenance.sh"; then
  echo "Fleet maintenance still smoke-tests Hermes runtime images" >&2
  exit 1
fi
if grep -q -- '-f runtime/Dockerfile' "$ROOT_DIR/scripts/fleet-maintenance.sh"; then
  echo "Fleet maintenance still builds Hermes runtime images" >&2
  exit 1
fi

echo "Fleet entrypoint separation regression test passed."
