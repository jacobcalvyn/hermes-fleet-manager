#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/setup-lib.sh"

duration="${FLEET_VPS_SOAK_SECONDS:-120}"
interval="${FLEET_VPS_SOAK_INTERVAL_SECONDS:-10}"
required_successes="${FLEET_VPS_SOAK_REQUIRED_SUCCESSES:-3}"
control_plane_url="${FLEET_CONTROL_PLANE_URL:-http://127.0.0.1:9180}"
env_file="${FLEET_ENV_FILE:-$ROOT_DIR/.env}"

for value_name in duration interval required_successes; do
  value="${!value_name}"
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "VPS qualification $value_name must be a positive integer." >&2
    exit 2
  fi
done
for command_name in curl docker systemctl python3; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    echo "$command_name is required for VPS qualification." >&2
    exit 1
  fi
done
admin_token="$(fleet_env_value "$env_file" FLEET_ADMIN_TOKEN)"
if [[ -z "$admin_token" ]]; then
  echo "FLEET_ADMIN_TOKEN is required for VPS qualification." >&2
  exit 1
fi

agent_restarts_start="$(systemctl show hermes-fleet-host-agent.service --property=NRestarts --value)"
container_restarts_start="$(docker inspect --format '{{.RestartCount}}' hermes-fleet-control-plane)"
deadline="$(( $(date +%s) + duration ))"
consecutive_successes=0
sample_count=0
last_error="qualification did not run"
sample_file="$(mktemp)"
instances_file="$(mktemp)"
error_file="$(mktemp)"
trap 'rm -f "$sample_file" "$instances_file" "$error_file"' EXIT

validate_sample() {
  systemctl is-active --quiet hermes-fleet-host-agent.service || return 1
  curl -fsS --max-time 5 "${control_plane_url%/}/livez" >/dev/null || return 1
  curl -fsS --max-time 5 "${control_plane_url%/}/readyz" >/dev/null || return 1
  curl -fsS --max-time 10 -H "Authorization: Bearer $admin_token" \
    "${control_plane_url%/}/api/v1/system/runtime-health" > "$sample_file" || return 1
  curl -fsS --max-time 10 -H "Authorization: Bearer $admin_token" \
    "${control_plane_url%/}/api/v1/instances" > "$instances_file" || return 1
  python3 - "$sample_file" "$instances_file" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    health = json.load(handle)
health_status = health.get("status")
if health_status not in {"healthy", "degraded"}:
    raise SystemExit(f"control-plane runtime health is {health_status or 'unknown'}")

# Remote access is optional. A partially configured tunnel reports `pending`,
# but it must not block an otherwise healthy Fleet Manager deployment. Any
# other degraded component remains a hard failure for the soak gate.
for component in health.get("components") or []:
    component_name = str(component.get("component") or "unknown")
    component_status = str(component.get("status") or "unknown")
    component_detail = str(component.get("detail") or "")
    remote_access_pending = (
        component_name == "remote_access"
        and component_status == "degraded"
        and component_detail == "pending"
    )
    if component_status != "healthy" and not remote_access_pending:
        suffix = f": {component_detail}" if component_detail else ""
        raise SystemExit(
            f"control-plane component {component_name} is {component_status}{suffix}"
        )
queue = health.get("queue") or {}
if queue.get("expired_leases", 0) or queue.get("admission_rejected", False):
    raise SystemExit("Host Agent queue is not stable")

with open(sys.argv[2], encoding="utf-8") as handle:
    instances = json.load(handle) or []
for instance in instances:
    status = instance.get("status")
    if status in {"FAILED", "PROVISIONING", "UPDATING", "DELETING"}:
        raise SystemExit(f"instance {instance.get('name', instance.get('id'))} is {status}")
    if status != "RUNNING":
        continue
    checks = ((instance.get("observation") or {}).get("checks") or [])
    runtime = next(
        (
            check
            for check in checks
            if str(check.get("name") or "").lower() == "runtime"
        ),
        None,
    )
    if runtime is not None and runtime.get("status") != "OK":
        raise SystemExit(f"instance {instance.get('name', instance.get('id'))} runtime is not healthy")
PY
}

while (( $(date +%s) <= deadline )); do
  sample_count="$((sample_count + 1))"
  if validate_sample 2> "$error_file"; then
    consecutive_successes="$((consecutive_successes + 1))"
    last_error=""
    echo "VPS qualification sample ${sample_count}: healthy (${consecutive_successes} consecutive)."
  else
    consecutive_successes=0
    last_error="$(tail -n 1 "$error_file" 2>/dev/null || true)"
    echo "VPS qualification sample ${sample_count}: unhealthy (${last_error:-unknown failure})." >&2
  fi

  agent_restarts_now="$(systemctl show hermes-fleet-host-agent.service --property=NRestarts --value)"
  container_restarts_now="$(docker inspect --format '{{.RestartCount}}' hermes-fleet-control-plane)"
  if [[ "$agent_restarts_now" != "$agent_restarts_start" || "$container_restarts_now" != "$container_restarts_start" ]]; then
    echo "VPS soak detected an unexpected process restart." >&2
    exit 1
  fi
  if (( $(date +%s) >= deadline )); then
    break
  fi
  sleep "$interval"
done

if (( consecutive_successes >= required_successes )); then
  echo "VPS qualification passed after ${duration}s with ${consecutive_successes} final consecutive healthy samples."
  exit 0
fi

echo "VPS qualification did not reach ${required_successes} consecutive healthy samples: ${last_error:-unknown failure}" >&2
exit 1
