#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/assert-compose-owner.sh"

test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
mkdir -p "$test_root/current" "$test_root/other"
tests_run=0
fake_active_workdir=""
fake_legacy_workdir=""
fake_marker_status=0
fake_marker=""
fake_mount_identity=""
fake_legacy_mount_identity=""
fake_secret_key="$(printf '%064d' 1)"
fake_recovery_key="$(printf '%064d' 2)"

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
  if [[ "$1" == "inspect" && "$2" == "--format" ]]; then
    local container_name="${4:-}"
    local workdir="$fake_active_workdir"
    local mount_identity="$fake_mount_identity"
    if [[ "$container_name" == "hermes-fleet-manager-control-plane-1" ]]; then
      workdir="$fake_legacy_workdir"
      mount_identity="$fake_legacy_mount_identity"
    fi
    case "$3" in
      *working_dir*)
        printf '%s\n' "$workdir"
        ;;
      *State.Status*)
        printf 'exited\n'
        ;;
      *.Mounts*)
        printf '%s\n' "$mount_identity"
        ;;
      *.Config.Env*)
        printf 'FLEET_SECRET_ENCRYPTION_KEY=%s\n' "$fake_secret_key"
        printf 'FLEET_RECOVERY_ENCRYPTION_KEY=%s\n' "$fake_recovery_key"
        ;;
      *)
        printf '\n'
        ;;
    esac
    return 0
  fi
  echo "unexpected Docker command in ownership test: $*" >&2
  return 97
}

fleet_volume_exists() {
  return 0
}

read_fleet_owner_marker() {
  if [[ "$fake_marker_status" == "0" ]]; then
    printf '%s\n' "$fake_marker"
    return 0
  fi
  return "$fake_marker_status"
}

marker_for() {
  local root_dir="$1"
  local secret="${2:-secret}"
  local recovery="${3:-recovery}"
  printf 'format=1\n'
  printf 'owner_sha256=%s\n' "$(fleet_owner_hash "$root_dir")"
  printf 'secret_key_sha256=%s\n' "$(fleet_key_fingerprint "$secret")"
  printf 'recovery_key_sha256=%s\n' "$(fleet_key_fingerprint "$recovery")"
}

reset_state() {
  unset FLEET_ALLOW_PROJECT_RELOCATION || true
  fake_active_workdir=""
  fake_legacy_workdir=""
  fake_mount_identity=""
  fake_legacy_mount_identity=""
  fake_secret_key="$(printf '%064d' 1)"
  fake_recovery_key="$(printf '%064d' 2)"
  fake_marker_status=0
  fake_marker="$(marker_for "$test_root/current")"
}

test_matching_marker_passes_without_container() {
  reset_state
  assert_compose_owner "$test_root/current"
}

test_mismatched_marker_fails_without_container() {
  reset_state
  fake_marker="$(marker_for "$test_root/other")"
  if assert_compose_owner "$test_root/current" >/dev/null 2>&1; then
    return 1
  fi
}

test_unmarked_volume_requires_active_owner_or_override() {
  reset_state
  fake_marker_status=3
  if assert_compose_owner "$test_root/current" >/dev/null 2>&1; then
    return 1
  fi
  fake_active_workdir="$test_root/current"
  fake_mount_identity=$'volume\thermes-fleet-manager-data'
  assert_compose_owner "$test_root/current"
}

test_active_foreign_checkout_fails() {
  reset_state
  fake_active_workdir="$test_root/other"
  fake_mount_identity=$'volume\thermes-fleet-manager-data'
  if assert_compose_owner "$test_root/current" >/dev/null 2>&1; then
    return 1
  fi
}

test_scoped_relocation_override() {
  reset_state
  fake_marker="$(marker_for "$test_root/other")"
  FLEET_ALLOW_PROJECT_RELOCATION=1 assert_compose_owner "$test_root/current"
}

test_key_fingerprint_mismatch_fails() {
  reset_state
  fake_marker="$(marker_for "$test_root/current" original-secret original-recovery)"
  if assert_fleet_marker_keys "$test_root/current" wrong-secret original-recovery >/dev/null 2>&1; then
    return 1
  fi
  assert_fleet_marker_keys "$test_root/current" original-secret original-recovery
}

test_legacy_marker_migration_requires_matching_container_keys() {
  reset_state
  fake_marker_status=3
  fake_active_workdir="$test_root/current"
  fake_mount_identity=$'volume\thermes-fleet-manager-data'
  assert_compose_owner "$test_root/current"
  assert_fleet_marker_keys "$test_root/current" "$fake_secret_key" "$fake_recovery_key"
  if assert_fleet_marker_keys "$test_root/current" "$(printf '%064d' 3)" "$fake_recovery_key" >/dev/null 2>&1; then
    return 1
  fi
}

test_legacy_relocation_still_requires_matching_container_keys() {
  reset_state
  fake_marker_status=3
  fake_active_workdir="$test_root/other"
  fake_mount_identity=$'volume\thermes-fleet-manager-data'
  FLEET_ALLOW_PROJECT_RELOCATION=1 assert_compose_owner "$test_root/current"
  FLEET_ALLOW_PROJECT_RELOCATION=1 assert_fleet_marker_keys \
    "$test_root/current" "$fake_secret_key" "$fake_recovery_key"
  if FLEET_ALLOW_PROJECT_RELOCATION=1 assert_fleet_marker_keys \
    "$test_root/current" "$fake_secret_key" "$(printf '%064d' 4)" >/dev/null 2>&1; then
    return 1
  fi
}

test_matching_container_with_wrong_volume_fails_closed() {
  reset_state
  fake_marker_status=3
  fake_active_workdir="$test_root/current"
  fake_mount_identity=$'volume\tanother-volume'
  if assert_compose_owner "$test_root/current" >/dev/null 2>&1; then
    return 1
  fi
}

test_multiple_control_planes_on_data_volume_fail_closed() {
  reset_state
  fake_marker_status=3
  fake_active_workdir="$test_root/current"
  fake_legacy_workdir="$test_root/current"
  fake_legacy_mount_identity=$'volume\thermes-fleet-manager-data'
  if assert_compose_owner "$test_root/current" >/dev/null 2>&1; then
    return 1
  fi
}

test_unlabeled_container_on_data_volume_fails_closed() {
  reset_state
  fake_mount_identity=$'volume\thermes-fleet-manager-data'
  if assert_compose_owner "$test_root/current" >/dev/null 2>&1; then
    return 1
  fi
}

test_invalid_keys_cannot_write_owner_marker() (
  source "$ROOT_DIR/scripts/assert-compose-owner.sh"
  docker() {
    echo "owner marker write must not reach Docker" >&2
    return 99
  }
  if write_fleet_owner_marker "$test_root/current" invalid "$fake_recovery_key" >/dev/null 2>&1; then
    return 1
  fi
)

test_restarting_container_uses_isolated_marker_reader() (
  source "$ROOT_DIR/scripts/assert-compose-owner.sh"
  docker() {
    if [[ "$1" == "inspect" && "$2" == "--format" ]]; then
      case "$3" in
        *State.Status*)
          printf 'restarting\n'
          ;;
        *.Mounts*)
          printf 'volume\thermes-fleet-manager-data\n'
          ;;
        *.Image*)
          printf 'fleet-image-id\n'
          ;;
        *)
          printf '\n'
          ;;
      esac
      return 0
    fi
    if [[ "$1" == "run" ]]; then
      return 3
    fi
    if [[ "$1" == "exec" ]]; then
      echo "marker read attempted docker exec against a restarting container" >&2
      return 99
    fi
    return 97
  }

  local marker_status=0
  if read_fleet_owner_marker >/dev/null; then
    return 1
  else
    marker_status=$?
  fi
  [[ "$marker_status" == "3" ]]
)

run_test matching_marker_passes_without_container test_matching_marker_passes_without_container
run_test mismatched_marker_fails_without_container test_mismatched_marker_fails_without_container
run_test unmarked_volume_requires_active_owner_or_override test_unmarked_volume_requires_active_owner_or_override
run_test active_foreign_checkout_fails test_active_foreign_checkout_fails
run_test scoped_relocation_override test_scoped_relocation_override
run_test key_fingerprint_mismatch_fails test_key_fingerprint_mismatch_fails
run_test legacy_marker_migration_requires_matching_container_keys test_legacy_marker_migration_requires_matching_container_keys
run_test legacy_relocation_still_requires_matching_container_keys test_legacy_relocation_still_requires_matching_container_keys
run_test matching_container_with_wrong_volume_fails_closed test_matching_container_with_wrong_volume_fails_closed
run_test multiple_control_planes_on_data_volume_fail_closed test_multiple_control_planes_on_data_volume_fail_closed
run_test unlabeled_container_on_data_volume_fails_closed test_unlabeled_container_on_data_volume_fails_closed
run_test invalid_keys_cannot_write_owner_marker test_invalid_keys_cannot_write_owner_marker
run_test restarting_container_uses_isolated_marker_reader test_restarting_container_uses_isolated_marker_reader

echo "compose owner regression tests passed ($tests_run tests)."
