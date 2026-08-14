#!/usr/bin/env bash

control_plane_container_name="hermes-fleet-control-plane"
control_plane_stable_image="local/hermes-fleet-manager:0.12.1"
control_plane_upgrade_headroom_bytes=$((2 * 1024 * 1024 * 1024))

report_control_plane_readiness_failure() {
  local control_plane_url="$1"
  local response=""

  if ! response="$(curl -sS --max-time 10 "${control_plane_url%/}/readyz" 2>/dev/null)"; then
    echo "Fleet Manager readiness check failed: the readiness endpoint is unreachable." >&2
    return 1
  fi
  printf '%s' "$response" | python3 -c '
import json
import sys

try:
    readiness = json.load(sys.stdin)
except (json.JSONDecodeError, TypeError):
    print("Fleet Manager readiness check failed: the endpoint returned an invalid response.", file=sys.stderr)
    raise SystemExit(1)

reasons = []
capacity = readiness.get("capacity")
if isinstance(capacity, dict) and capacity.get("operations_safe") is False:
    reasons.append(capacity.get("blocking_reason") or "the storage safety reserve is not satisfied")
for field, label in (
    ("database", "database"),
    ("storage", "storage"),
    ("release_catalog", "release catalog"),
):
    status = readiness.get(field)
    if status not in (None, "ready") and not (field == "storage" and reasons):
        reasons.append(f"{label} is {status}")

if not reasons:
    reasons.append("the service did not report a ready state")
print("Fleet Manager readiness check failed: " + "; ".join(reasons) + ".", file=sys.stderr)
raise SystemExit(1)
'
}

recover_stopped_control_plane() {
  local control_plane_url="$1"
  local current_image=""
  local running=""
  local attempt=0

  current_image="$(docker inspect --format '{{.Config.Image}}' "$control_plane_container_name" 2>/dev/null || true)"
  if [[ -z "$current_image" ]]; then
    return 0
  fi
  running="$(docker inspect --format '{{.State.Running}}' "$control_plane_container_name" 2>/dev/null || true)"
  if [[ "$running" == "true" ]]; then
    return 0
  fi

  echo "The existing Fleet Manager stack is stopped; starting the same control-plane image before evaluating an upgrade."
  if ! FLEET_MANAGER_IMAGE="$current_image" docker compose up \
    -d --no-build --no-recreate control-plane cloudflare-admin cloudflare-instances; then
    echo "The existing Fleet Manager stack could not be started; candidate deployment remains blocked." >&2
    return 1
  fi

  for ((attempt = 1; attempt <= 15; attempt++)); do
    # A non-2xx readiness response still proves that the old control plane is
    # listening. Its specific gate is reported by the normal preflight below.
    if curl -sS --max-time 2 "${control_plane_url%/}/readyz" >/dev/null 2>&1; then
      echo "The existing Fleet Manager is responding; readiness evaluation will continue before any candidate build."
      return 0
    fi
    sleep 2
  done

  echo "The existing Fleet Manager did not open its readiness endpoint after restart; candidate deployment remains blocked." >&2
  return 1
}

control_plane_upgrade_capacity_preflight() {
  local control_plane_url="$1"
  local response=""

  if ! response="$(curl -fsS --max-time 10 "${control_plane_url%/}/readyz")"; then
    echo "Fleet Manager upgrade preflight could not read the current capacity status." >&2
    return 1
  fi
  printf '%s' "$response" | python3 -c '
import json
import math
import sys

headroom = int(sys.argv[1])
try:
    readiness = json.load(sys.stdin)
    capacity = readiness["capacity"]
    free_bytes = int(capacity["free_bytes"])
    total_bytes = int(capacity["total_bytes"])
    minimum_bytes = int(capacity["minimum_free_bytes"])
    minimum_percent = float(capacity["minimum_free_percent"])
except (json.JSONDecodeError, KeyError, TypeError, ValueError):
    print("Fleet Manager upgrade preflight received an invalid capacity status.", file=sys.stderr)
    raise SystemExit(1)

if capacity.get("operations_safe") is not True:
    reason = capacity.get("blocking_reason") or "the current storage safety reserve is not satisfied"
    print(f"Fleet Manager upgrade preflight refused: {reason}.", file=sys.stderr)
    raise SystemExit(1)

reserve_bytes = max(minimum_bytes, math.ceil(total_bytes * minimum_percent / 100))
required_bytes = reserve_bytes + headroom
if free_bytes < required_bytes:
    deficit = required_bytes - free_bytes
    gib = 1024 ** 3
    print(
        "Fleet Manager upgrade preflight refused: "
        f"{free_bytes / gib:.2f} GiB is free, but {required_bytes / gib:.2f} GiB is required "
        f"to preserve the safety reserve plus build headroom ({deficit / gib:.2f} GiB more needed).",
        file=sys.stderr,
    )
    raise SystemExit(1)
' "$control_plane_upgrade_headroom_bytes"
}

release_catalog_is_only_readiness_failure() {
  local control_plane_url="$1"
  local response=""

  if ! response="$(curl -sS --max-time 10 "${control_plane_url%/}/readyz")"; then
    return 1
  fi
  printf '%s' "$response" | python3 -c '
import json
import sys

try:
    readiness = json.load(sys.stdin)
except (json.JSONDecodeError, TypeError):
    raise SystemExit(1)

capacity = readiness.get("capacity")
if not isinstance(capacity, dict):
    raise SystemExit(1)
expected = (
    readiness.get("ready") is False
    and readiness.get("database") == "ready"
    and readiness.get("storage") == "ready"
    and readiness.get("release_catalog") == "unavailable"
    and capacity.get("operations_safe") is True
)
raise SystemExit(0 if expected else 1)
'
}

recover_orphaned_release_catalog_mount() {
  local root_dir="$1"
  local control_plane_url="$2"
  local catalog_path="$root_dir/.state/hermes-releases.json"
  local current_image=""

  if curl -fsS "${control_plane_url%/}/readyz" >/dev/null 2>&1; then
    return 0
  fi
  current_image="$(docker inspect --format '{{.Config.Image}}' "$control_plane_container_name" 2>/dev/null || true)"
  if [[ -z "$current_image" ]] || [[ ! -s "$catalog_path" ]]; then
    return 0
  fi
  if ! release_catalog_is_only_readiness_failure "$control_plane_url"; then
    return 0
  fi
  if docker exec --user fleet "$control_plane_container_name" \
    sh -c 'head -c 1 "$1" >/dev/null' _ /etc/hermes-fleet/hermes-releases.json \
    >/dev/null 2>&1; then
    return 0
  fi

  echo "The running Fleet Manager lost its release-catalog bind mount; recreating the same image to restore the mount."
  if ! FLEET_MANAGER_IMAGE="$current_image" docker compose up \
    -d --no-deps --force-recreate --wait control-plane; then
    echo "The existing Fleet Manager image could not be recreated with its release catalog." >&2
    return 1
  fi
  if ! curl -fsS "${control_plane_url%/}/readyz" >/dev/null; then
    echo "The recreated Fleet Manager is still not ready; candidate deployment remains blocked." >&2
    return 1
  fi
  echo "The release-catalog mount was restored without changing the Fleet Manager image."
}

database_interrupt_is_only_readiness_failure() {
  local control_plane_url="$1"
  local response=""

  if ! response="$(curl -sS --max-time 10 "${control_plane_url%/}/readyz")"; then
    return 1
  fi
  printf '%s' "$response" | python3 -c '
import json
import sys

try:
    readiness = json.load(sys.stdin)
except (json.JSONDecodeError, TypeError):
    raise SystemExit(1)

capacity = readiness.get("capacity")
expected = (
    readiness.get("ready") is False
    and readiness.get("database") == "unavailable"
    and readiness.get("storage") == "ready"
    and readiness.get("release_catalog") == "ready"
    and isinstance(capacity, dict)
    and capacity.get("operations_safe") is True
)
raise SystemExit(0 if expected else 1)
'
}

recover_interrupted_database_connection() {
  local control_plane_url="$1"
  local current_image=""
  local attempt=0

  if curl -fsS "${control_plane_url%/}/readyz" >/dev/null 2>&1; then
    return 0
  fi
  current_image="$(docker inspect --format '{{.Config.Image}}' "$control_plane_container_name" 2>/dev/null || true)"
  if [[ -z "$current_image" ]] || ! database_interrupt_is_only_readiness_failure "$control_plane_url"; then
    return 0
  fi
  if ! docker logs --tail 200 "$control_plane_container_name" 2>&1 \
    | grep -E 'readiness database check.*interrupted \(9\)' >/dev/null; then
    return 0
  fi

  echo "The running Fleet Manager has an interrupted SQLite connection; restarting the same image to recover it before evaluating an upgrade."
  if ! docker compose restart control-plane; then
    echo "The existing Fleet Manager process could not be restarted; candidate deployment remains blocked." >&2
    return 1
  fi
  for ((attempt = 1; attempt <= 15; attempt++)); do
    if curl -fsS --max-time 2 "${control_plane_url%/}/readyz" >/dev/null 2>&1; then
      echo "The existing Fleet Manager database connection recovered without changing its image or data volume."
      return 0
    fi
    sleep 2
  done
  echo "The existing Fleet Manager database remained unavailable after a process restart; candidate deployment remains blocked." >&2
  return 1
}

rollback_control_plane_upgrade() {
  local rollback_image="$1"
  local previous_image_ref="$2"
  local previous_image_id="$3"
  local candidate_image="$4"
  local control_plane_url="$5"
  local restore_image="$rollback_image"
  local referenced_image_id=""
  local active_image_id=""
  local rollback_ready=0

  if [[ -z "$rollback_image" ]]; then
    echo "No previous Fleet Manager image is available for automatic rollback." >&2
    docker image rm "$candidate_image" >/dev/null 2>&1 || true
    return 1
  fi

  if [[ -n "$previous_image_ref" ]]; then
    referenced_image_id="$(docker image inspect --format '{{.Id}}' "$previous_image_ref" 2>/dev/null || true)"
    if [[ "$referenced_image_id" == "$previous_image_id" ]]; then
      restore_image="$previous_image_ref"
    fi
  fi

  echo "Candidate Fleet Manager verification failed; restoring the previous image." >&2
  # Persist the known previous reference before Compose runs. Even when a
  # global readiness gate such as storage capacity remains unhealthy, a later
  # restart must never select the failed candidate.
  set_env_value FLEET_MANAGER_IMAGE "$restore_image"
  if FLEET_MANAGER_IMAGE="$restore_image" docker compose up \
    -d --no-deps --force-recreate --wait control-plane \
    && curl -fsS "${control_plane_url%/}/readyz" >/dev/null; then
    rollback_ready=1
  fi

  active_image_id="$(docker inspect --format '{{.Image}}' "$control_plane_container_name" 2>/dev/null || true)"
  docker image rm "$candidate_image" >/dev/null 2>&1 || true
  if [[ "$restore_image" != "$rollback_image" ]]; then
    docker image rm "$rollback_image" >/dev/null 2>&1 || true
  fi

  if [[ "$rollback_ready" == "1" ]]; then
    echo "The previous Fleet Manager image was restored and passed readiness checks." >&2
    return 0
  fi
  if [[ "$active_image_id" == "$previous_image_id" ]]; then
    echo "The previous Fleet Manager image was restored, but a global readiness check is still failing; inspect /readyz before retrying setup." >&2
  else
    echo "Automatic rollback could not restore the previous Fleet Manager image." >&2
  fi
  return 1
}

upgrade_control_plane() {
  local root_dir="$1"
  local control_plane_url="$2"
  local admin_token="$3"
  local fleet_build_id="$4"
  local upgrade_guard="$5"
  local candidate_image="local/hermes-fleet-manager:0.12.1-${fleet_build_id}"
  local rollback_image=""
  local previous_image_ref=""
  local previous_image_id=""
  local snapshot_root="$root_dir/.state/control-plane-upgrades"
  local snapshot_dir=""
  local snapshot_manifest=""

  if ! recover_stopped_control_plane "$control_plane_url"; then
    return 1
  fi
  if ! recover_orphaned_release_catalog_mount "$root_dir" "$control_plane_url"; then
    return 1
  fi
  if ! recover_interrupted_database_connection "$control_plane_url"; then
    return 1
  fi

  if curl -fsS "${control_plane_url%/}/readyz" >/dev/null 2>&1 \
    && "$upgrade_guard" verify --url "$control_plane_url" --token "$admin_token" --build-id "$fleet_build_id" >/dev/null 2>&1; then
    echo "Fleet Manager build $fleet_build_id is already active; deployment was skipped."
    return 0
  fi

  previous_image_ref="$(docker inspect --format '{{.Config.Image}}' "$control_plane_container_name" 2>/dev/null || true)"
  previous_image_id="$(docker inspect --format '{{.Image}}' "$control_plane_container_name" 2>/dev/null || true)"

  if curl -fsS "${control_plane_url%/}/readyz" >/dev/null 2>&1; then
    if ! control_plane_upgrade_capacity_preflight "$control_plane_url"; then
      return 1
    fi
    mkdir -p "$snapshot_root"
    snapshot_dir="$(mktemp -d "$snapshot_root/${fleet_build_id}.XXXXXX")"
    "$upgrade_guard" snapshot \
      --url "$control_plane_url" \
      --token "$admin_token" \
      --output-dir "$snapshot_dir"
    snapshot_manifest="$snapshot_dir/snapshot.json"
  elif [[ -n "$previous_image_id" ]]; then
    report_control_plane_readiness_failure "$control_plane_url" || true
    echo "The running Fleet Manager is not ready; refusing to deploy a candidate over an unhealthy control plane." >&2
    return 1
  fi

  if [[ -n "$previous_image_id" ]]; then
    rollback_image="local/hermes-fleet-manager:rollback-${fleet_build_id}"
    docker image tag "$previous_image_id" "$rollback_image"
  fi

  # Both connector services consume the same image. Building it once avoids a
  # parallel exporter race when Compose targets share an identical image tag.
  FLEET_MANAGER_IMAGE="$candidate_image" docker compose build control-plane cloudflare-admin
  if ! FLEET_MANAGER_IMAGE="$candidate_image" docker compose up -d --no-build --wait; then
    rollback_control_plane_upgrade \
      "$rollback_image" "$previous_image_ref" "$previous_image_id" "$candidate_image" "$control_plane_url" || true
    return 1
  fi
  local verify_arguments=(
    verify
    --url "$control_plane_url"
    --token "$admin_token"
    --build-id "$fleet_build_id"
  )
  if [[ -n "$snapshot_manifest" ]]; then
    verify_arguments+=(--snapshot "$snapshot_manifest")
  fi
  if ! "$upgrade_guard" "${verify_arguments[@]}"; then
    rollback_control_plane_upgrade \
      "$rollback_image" "$previous_image_ref" "$previous_image_id" "$candidate_image" "$control_plane_url" || true
    return 1
  fi
  docker image tag "$candidate_image" "$control_plane_stable_image"
  set_env_value FLEET_MANAGER_IMAGE "$control_plane_stable_image"
  if [[ -n "$rollback_image" ]]; then
    docker image rm "$rollback_image" >/dev/null 2>&1 || true
  fi
  echo "Fleet Manager candidate $fleet_build_id passed readiness, identity, and instance-state verification."
}
