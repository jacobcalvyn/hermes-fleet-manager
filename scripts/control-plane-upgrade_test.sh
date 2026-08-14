#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/control-plane-upgrade.sh"

test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
mkdir -p "$test_root/.state"
command_log="$test_root/commands.log"
env_log="$test_root/env.log"
guard_mode="success"
runtime_state="healthy"
container_running=1
compose_failure_image=""

docker() {
  printf 'docker %s\n' "$*" >> "$command_log"
  if [[ "$runtime_state" == "interrupted-database" ]] && [[ "${1:-}" == "logs" ]]; then
    printf '%s\n' '{"time":"2026-08-06T05:20:58Z","level":"ERROR","msg":"readiness database check","error":"acquire sqlite write reservation: interrupted (9)"}'
    return 0
  fi
  if [[ "$runtime_state" == "orphaned-catalog" ]] && [[ "$1" == "exec" ]]; then
    return 1
  fi
  if [[ "${1:-}" == "inspect" ]]; then
    if [[ "$*" == *'.State.Running'* ]]; then
      if [[ "$container_running" == "1" ]]; then
        printf 'true\n'
      else
        printf 'false\n'
      fi
    elif [[ "$*" == *'.Config.Image'* ]]; then
      printf 'local/hermes-fleet-manager:existing\n'
    else
      printf 'sha256:previous\n'
    fi
  fi
  if [[ "${1:-} ${2:-}" == "image inspect" ]]; then
    printf 'sha256:previous\n'
  fi
  if [[ -n "$compose_failure_image" ]] \
    && [[ "${1:-} ${2:-} ${3:-}" == "compose up -d" ]] \
    && [[ "${FLEET_MANAGER_IMAGE:-}" == "$compose_failure_image" ]]; then
    return 1
  fi
  if [[ "$runtime_state" == "orphaned-catalog" ]] \
    && [[ "${1:-} ${2:-} ${3:-}" == "compose up -d" ]]; then
    runtime_state="healthy"
  fi
  if [[ "${1:-} ${2:-} ${3:-} ${4:-}" == "compose up -d --no-build" ]] \
    && [[ "$*" == *'--no-recreate'* ]]; then
    container_running=1
    if [[ "$runtime_state" == "stopped-healthy" ]]; then
      runtime_state="healthy"
    fi
  fi
  if [[ "$runtime_state" == "interrupted-database" ]] \
    && [[ "${1:-} ${2:-} ${3:-}" == "compose restart control-plane" ]]; then
    runtime_state="healthy"
  fi
  return 0
}

sleep() {
  return 0
}

curl() {
  printf 'curl %s\n' "$*" >> "$command_log"
  if [[ "$runtime_state" == "healthy" ]]; then
    printf '%s\n' '{"ready":true,"database":"ready","storage":"ready","release_catalog":"ready","capacity":{"operations_safe":true,"free_bytes":10737418240,"total_bytes":107374182400,"minimum_free_bytes":1073741824,"minimum_free_percent":5}}'
    return 0
  fi
  if [[ "$runtime_state" == "healthy-low-headroom" ]]; then
    printf '%s\n' '{"ready":true,"database":"ready","storage":"ready","release_catalog":"ready","capacity":{"operations_safe":true,"free_bytes":6442450944,"total_bytes":107374182400,"minimum_free_bytes":1073741824,"minimum_free_percent":5}}'
    return 0
  fi
  if [[ "$runtime_state" == "orphaned-catalog" ]]; then
    if [[ "$*" == *'-fsS'* ]]; then
      return 22
    fi
    printf '%s\n' '{"ready":false,"database":"ready","storage":"ready","release_catalog":"unavailable","capacity":{"operations_safe":true}}'
    return 0
  fi
  if [[ "$runtime_state" == "capacity-blocked" ]]; then
    if [[ "$*" == *'-fsS'* ]]; then
      return 22
    fi
    printf '%s\n' '{"ready":false,"database":"ready","storage":"below_safety_reserve","release_catalog":"ready","capacity":{"operations_safe":false,"blocking_reason":"only 4.6% storage remains; at least 5.0% is required"}}'
    return 0
  fi
  if [[ "$runtime_state" == "interrupted-database" || "$runtime_state" == "database-unavailable" ]]; then
    if [[ "$*" == *'-fsS'* ]]; then
      return 22
    fi
    printf '%s\n' '{"ready":false,"database":"unavailable","storage":"ready","release_catalog":"ready","capacity":{"operations_safe":true}}'
    return 0
  fi
  if [[ "$runtime_state" == "stopped-unreachable" ]]; then
    return 7
  fi
  if [[ "$*" == *'-fsS'* ]]; then
    return 22
  fi
  printf '%s\n' '{"ready":false,"database":"ready","storage":"unavailable","release_catalog":"unavailable","capacity":{"operations_safe":true}}'
}

set_env_value() {
  printf '%s=%s\n' "$1" "$2" >> "$env_log"
}

guard="$test_root/fleet-upgrade-guard"
cat > "$guard" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'guard %s\n' "$*" >> "$UPGRADE_GUARD_LOG"
if [[ "$1" == "snapshot" ]]; then
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == "--output-dir" ]]; then
      mkdir -p "$2"
      printf '{}\n' > "$2/snapshot.json"
      break
    fi
    shift
  done
  exit 0
fi
verify_count=0
if [[ -f "$UPGRADE_GUARD_COUNTER" ]]; then
  verify_count="$(cat "$UPGRADE_GUARD_COUNTER")"
fi
verify_count=$((verify_count + 1))
printf '%s\n' "$verify_count" > "$UPGRADE_GUARD_COUNTER"
if [[ "${UPGRADE_GUARD_MODE:-success}" == "initial-mismatch" && "$verify_count" == "1" ]]; then
  exit 1
fi
if [[ "${UPGRADE_GUARD_MODE:-success}" == "always-fail" ]]; then
  exit 1
fi
EOF
chmod +x "$guard"

export UPGRADE_GUARD_LOG="$command_log"
export UPGRADE_GUARD_COUNTER="$test_root/guard-counter"

printf '{}\n' > "$test_root/.state/hermes-releases.json"

runtime_state="interrupted-database"
recover_interrupted_database_connection "http://127.0.0.1:9180"
grep -q 'docker compose restart control-plane' "$command_log"
[[ "$runtime_state" == "healthy" ]]

: > "$command_log"
runtime_state="database-unavailable"
recover_interrupted_database_connection "http://127.0.0.1:9180"
if grep -q 'docker compose restart control-plane' "$command_log"; then
  echo "database failure without an SQLITE_INTERRUPT signature triggered automatic recovery" >&2
  exit 1
fi

: > "$command_log"
runtime_state="orphaned-catalog"
recover_orphaned_release_catalog_mount "$test_root" "http://127.0.0.1:9180"
grep -q 'docker compose up -d --no-deps --force-recreate --wait control-plane' "$command_log"
[[ "$runtime_state" == "healthy" ]]

: > "$command_log"
runtime_state="other-failure"
recover_orphaned_release_catalog_mount "$test_root" "http://127.0.0.1:9180"
if grep -q 'docker compose up' "$command_log"; then
  echo "unrelated readiness failure triggered catalog mount recovery" >&2
  exit 1
fi

: > "$command_log"
: > "$env_log"
: > "$UPGRADE_GUARD_COUNTER"
runtime_state="stopped-healthy"
container_running=0
export UPGRADE_GUARD_MODE="success"
upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "alreadyactive123" "$guard"
grep -q 'docker compose up -d --no-build --no-recreate control-plane cloudflare-admin cloudflare-instances' "$command_log"
if grep -q 'docker compose build' "$command_log"; then
  echo "an already-current stopped control plane triggered a candidate build" >&2
  exit 1
fi

: > "$command_log"
: > "$env_log"
: > "$UPGRADE_GUARD_COUNTER"
runtime_state="stopped-healthy"
container_running=0
export UPGRADE_GUARD_MODE="initial-mismatch"
upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "recovered1234567" "$guard"
grep -q 'docker compose up -d --no-build --no-recreate control-plane cloudflare-admin cloudflare-instances' "$command_log"
grep -q 'docker compose build control-plane cloudflare-admin' "$command_log"

: > "$command_log"
: > "$env_log"
: > "$UPGRADE_GUARD_COUNTER"
runtime_state="stopped-unreachable"
container_running=0
export UPGRADE_GUARD_MODE="initial-mismatch"
if upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "unreachable12345" "$guard"; then
  echo "upgrade_control_plane accepted a stopped control plane that never reopened readiness" >&2
  exit 1
fi
if grep -q 'docker compose build' "$command_log"; then
  echo "candidate build started while the previous readiness endpoint was unreachable" >&2
  exit 1
fi

: > "$command_log"
: > "$env_log"
: > "$UPGRADE_GUARD_COUNTER"
runtime_state="capacity-blocked"
container_running=1
export UPGRADE_GUARD_MODE="initial-mismatch"
readiness_error="$test_root/readiness-error.log"
if upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "blocked123456789" "$guard" 2>"$readiness_error"; then
  echo "upgrade_control_plane accepted an unhealthy running control plane" >&2
  exit 1
fi
grep -q 'only 4.6% storage remains; at least 5.0% is required' "$readiness_error"
if grep -q 'docker compose build' "$command_log"; then
  echo "candidate build started despite a reported storage safety failure" >&2
  exit 1
fi

: > "$command_log"
runtime_state="healthy"
container_running=1
export UPGRADE_GUARD_MODE="initial-mismatch"
upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "candidate12345678" "$guard"
grep -q 'docker compose build control-plane cloudflare-admin' "$command_log"
grep -q 'docker compose up -d --no-build --wait' "$command_log"
grep -q 'docker image tag local/hermes-fleet-manager:0.12.1-candidate12345678 local/hermes-fleet-manager:0.12.1' "$command_log"
grep -q 'FLEET_MANAGER_IMAGE=local/hermes-fleet-manager:0.12.1' "$env_log"
if grep -q 'FLEET_MANAGER_IMAGE=local/hermes-fleet-manager:0.12.1-candidate12345678' "$env_log"; then
  echo "candidate image was persisted before verification" >&2
  exit 1
fi

: > "$command_log"
: > "$env_log"
: > "$UPGRADE_GUARD_COUNTER"
runtime_state="healthy-low-headroom"
export UPGRADE_GUARD_MODE="initial-mismatch"
if upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "capacity1234567" "$guard"; then
  echo "upgrade_control_plane ignored the build headroom gate" >&2
  exit 1
fi
if grep -q 'docker compose build' "$command_log"; then
  echo "capacity preflight ran after the candidate build started" >&2
  exit 1
fi
if [[ -s "$env_log" ]]; then
  echo "capacity preflight mutated the persisted image selection" >&2
  exit 1
fi

: > "$command_log"
: > "$env_log"
: > "$UPGRADE_GUARD_COUNTER"
runtime_state="healthy"
export UPGRADE_GUARD_MODE="initial-mismatch"
compose_failure_image="local/hermes-fleet-manager:0.12.1-deployfail123456"
if upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "deployfail123456" "$guard"; then
  echo "upgrade_control_plane accepted a candidate that failed to start" >&2
  exit 1
fi
compose_failure_image=""
grep -q 'FLEET_MANAGER_IMAGE=local/hermes-fleet-manager:existing' "$env_log"
grep -q 'docker image rm local/hermes-fleet-manager:0.12.1-deployfail123456' "$command_log"
if grep -q 'docker image tag local/hermes-fleet-manager:0.12.1-deployfail123456 local/hermes-fleet-manager:0.12.1' "$command_log"; then
  echo "candidate deployment failure replaced the stable image tag" >&2
  exit 1
fi

: > "$command_log"
: > "$env_log"
: > "$UPGRADE_GUARD_COUNTER"
runtime_state="healthy"
export UPGRADE_GUARD_MODE="always-fail"
if upgrade_control_plane "$test_root" "http://127.0.0.1:9180" "admin-token" "failed123456789" "$guard"; then
  echo "upgrade_control_plane accepted a failed candidate" >&2
  exit 1
fi
grep -q 'docker compose up -d --no-deps --force-recreate --wait control-plane' "$command_log"
grep -q 'FLEET_MANAGER_IMAGE=local/hermes-fleet-manager:existing' "$env_log"
grep -q 'docker image rm local/hermes-fleet-manager:0.12.1-failed123456789' "$command_log"
if grep -q 'docker image tag local/hermes-fleet-manager:0.12.1-failed123456789 local/hermes-fleet-manager:0.12.1' "$command_log"; then
  echo "failed candidate replaced the stable image tag" >&2
  exit 1
fi

: > "$command_log"
: > "$env_log"
runtime_state="other-failure"
if rollback_control_plane_upgrade \
  "local/hermes-fleet-manager:rollback-direct" \
  "local/hermes-fleet-manager:existing" \
  "sha256:previous" \
  "local/hermes-fleet-manager:0.12.1-direct-failure" \
  "http://127.0.0.1:9180"; then
  echo "rollback accepted an unhealthy restored control plane" >&2
  exit 1
fi
grep -q 'FLEET_MANAGER_IMAGE=local/hermes-fleet-manager:existing' "$env_log"
grep -q 'docker image rm local/hermes-fleet-manager:0.12.1-direct-failure' "$command_log"

echo "control-plane upgrade regression tests passed."
