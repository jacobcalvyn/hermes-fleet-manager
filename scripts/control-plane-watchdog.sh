#!/usr/bin/env bash
set -euo pipefail

project_directory="${FLEET_WATCHDOG_PROJECT_DIRECTORY:-}"
state_directory="${FLEET_WATCHDOG_STATE_DIRECTORY:-/var/lib/hermes-fleet-watchdog}"
control_plane_url="${FLEET_WATCHDOG_CONTROL_PLANE_URL:-http://127.0.0.1:9180}"
failure_threshold="${FLEET_WATCHDOG_FAILURE_THRESHOLD:-3}"
recovery_window_seconds="${FLEET_WATCHDOG_RECOVERY_WINDOW_SECONDS:-900}"
max_recoveries="${FLEET_WATCHDOG_MAX_RECOVERIES:-3}"

for value_name in failure_threshold recovery_window_seconds max_recoveries; do
  value="${!value_name}"
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "Fleet watchdog $value_name must be a positive integer." >&2
    exit 2
  fi
done
if [[ -z "$project_directory" || ! -f "$project_directory/docker-compose.yml" ]]; then
  echo "Fleet watchdog project directory is invalid." >&2
  exit 2
fi

install -d -o root -g root -m 0700 "$state_directory"
failure_file="$state_directory/consecutive-liveness-failures"
recovery_file="$state_directory/recovery-attempts"
status_file="$state_directory/status"
now="$(date +%s)"

write_status() {
  local state="$1"
  local detail="$2"
  local temporary
  temporary="$(mktemp "$state_directory/status.XXXXXX")"
  printf '%s\t%s\t%s\n' "$now" "$state" "$detail" > "$temporary"
  chmod 0600 "$temporary"
  mv "$temporary" "$status_file"
}

read_failure_count() {
  local value=0
  if [[ -r "$failure_file" ]]; then
    read -r value < "$failure_file" || value=0
  fi
  if [[ ! "$value" =~ ^[0-9]+$ ]]; then
    value=0
  fi
  printf '%s' "$value"
}

set_failure_count() {
  local value="$1"
  local temporary
  temporary="$(mktemp "$state_directory/failures.XXXXXX")"
  printf '%s\n' "$value" > "$temporary"
  chmod 0600 "$temporary"
  mv "$temporary" "$failure_file"
}

if curl -fsS --max-time 5 "${control_plane_url%/}/livez" >/dev/null 2>&1; then
  set_failure_count 0
  if curl -fsS --max-time 5 "${control_plane_url%/}/readyz" >/dev/null 2>&1; then
    write_status healthy "control plane is live and ready"
  else
    write_status degraded "control plane is live but not ready; no automatic restart was attempted"
  fi
  exit 0
fi

failure_count="$(( $(read_failure_count) + 1 ))"
set_failure_count "$failure_count"
if (( failure_count < failure_threshold )); then
  write_status observing "liveness failed ${failure_count}/${failure_threshold}; waiting for confirmation"
  exit 0
fi

recent_recoveries="$(mktemp "$state_directory/recoveries.XXXXXX")"
if [[ -r "$recovery_file" ]]; then
  awk -v minimum="$((now - recovery_window_seconds))" '$1 ~ /^[0-9]+$/ && $1 >= minimum { print $1 }' "$recovery_file" > "$recent_recoveries"
else
  : > "$recent_recoveries"
fi
recovery_count="$(wc -l < "$recent_recoveries" | tr -d '[:space:]')"
if (( recovery_count >= max_recoveries )); then
  mv "$recent_recoveries" "$recovery_file"
  chmod 0600 "$recovery_file"
  write_status exhausted "liveness recovery stopped after ${recovery_count} attempts in ${recovery_window_seconds}s"
  echo "Fleet control-plane watchdog exhausted its bounded recovery budget." >&2
  exit 1
fi

printf '%s\n' "$now" >> "$recent_recoveries"
mv "$recent_recoveries" "$recovery_file"
chmod 0600 "$recovery_file"
container_id="$(docker compose --project-directory "$project_directory" ps -q control-plane)"
if [[ -n "$container_id" ]]; then
  write_status recovering "confirmed liveness failure; restarting existing control plane"
  docker compose --project-directory "$project_directory" restart control-plane
else
  write_status recovering "confirmed liveness failure; starting missing control plane"
  docker compose --project-directory "$project_directory" up -d --no-deps control-plane
fi
set_failure_count 0
sleep 5
if ! curl -fsS --max-time 5 "${control_plane_url%/}/livez" >/dev/null 2>&1; then
  write_status failed "control plane remained unavailable after bounded recovery"
  echo "Fleet control-plane recovery did not restore liveness." >&2
  exit 1
fi
write_status recovered "control-plane liveness restored; readiness will converge independently"
