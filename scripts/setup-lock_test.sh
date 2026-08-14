#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/setup-lock.sh"

test_root="$(mktemp -d)"
trap 'release_setup_lock; rm -rf "$test_root"' EXIT
shared_lock="$test_root/hermes-fleet-manager-data.lock"

configure_setup_lock "$shared_lock"
acquire_setup_lock
if bash -euo pipefail -c '
  source "$1/scripts/setup-lock.sh"
  configure_setup_lock "$2"
  acquire_setup_lock
' _ "$ROOT_DIR" "$shared_lock" >/dev/null 2>&1; then
  echo "FAIL: a second checkout acquired the shared setup lock" >&2
  exit 1
fi

release_setup_lock
bash -euo pipefail -c '
  source "$1/scripts/setup-lock.sh"
  configure_setup_lock "$2"
  trap release_setup_lock EXIT
  acquire_setup_lock
' _ "$ROOT_DIR" "$shared_lock"

echo "setup lock regression tests passed."
