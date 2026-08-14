#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FLEET_SETUP_ACTION=bootstrap exec "$ROOT_DIR/scripts/fleet-maintenance.sh" "$@"
