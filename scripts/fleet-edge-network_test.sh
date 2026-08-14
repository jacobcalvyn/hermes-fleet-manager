#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/fleet-edge-network.sh"

tests_run=0
network_exists=0
network_label=""
network_internal="false"
create_arguments=""

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

run_test() {
  local name="$1"
  shift
  "$@" || fail "$name"
  tests_run=$((tests_run + 1))
}

docker() {
  if [[ "$1 $2" == "network inspect" && "${3:-}" == "$fleet_edge_network_name" ]]; then
    [[ "$network_exists" == "1" ]]
    return
  fi
  if [[ "$1 $2 $3" == "network inspect --format" ]]; then
    case "$4" in
      *Labels*) printf '%s\n' "$network_label" ;;
      *Internal*) printf '%s\n' "$network_internal" ;;
      *) return 97 ;;
    esac
    return 0
  fi
  if [[ "$1 $2" == "network create" ]]; then
    create_arguments="$*"
    return 0
  fi
  echo "unexpected Docker command: $*" >&2
  return 98
}

reset_state() {
  network_exists=0
  network_label=""
  network_internal="false"
  create_arguments=""
}

test_creates_owned_internal_network() {
  reset_state
  ensure_fleet_edge_network
  [[ "$create_arguments" == *"--internal"* ]]
  [[ "$create_arguments" == *"--label io.hermes-fleet.edge-network=true"* ]]
  [[ "$create_arguments" == *"hermes-fleet-edge"* ]]
}

test_accepts_existing_owned_internal_network() {
  reset_state
  network_exists=1
  network_label="true"
  network_internal="true"
  ensure_fleet_edge_network
  [[ -z "$create_arguments" ]]
}

test_rejects_unowned_network() {
  reset_state
  network_exists=1
  network_internal="true"
  ! ensure_fleet_edge_network >/dev/null 2>&1
}

test_rejects_non_internal_network() {
  reset_state
  network_exists=1
  network_label="true"
  ! ensure_fleet_edge_network >/dev/null 2>&1
}

run_test creates_owned_internal_network test_creates_owned_internal_network
run_test accepts_existing_owned_internal_network test_accepts_existing_owned_internal_network
run_test rejects_unowned_network test_rejects_unowned_network
run_test rejects_non_internal_network test_rejects_non_internal_network

echo "fleet edge network tests passed: $tests_run"
