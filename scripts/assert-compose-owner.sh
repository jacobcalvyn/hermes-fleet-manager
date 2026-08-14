#!/usr/bin/env bash

assert_compose_owner_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$assert_compose_owner_root/scripts/setup-lib.sh"

fleet_owner_marker_path="/var/lib/hermes-fleet/.fleet-owner"

fleet_owner_hash() {
  local root_dir
  root_dir="$(cd "$1" && pwd -P)"
  fleet_sha256_text "$root_dir"
}

fleet_marker_value() {
  local marker="$1"
  local key="$2"
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' <<< "$marker"
}

fleet_marker_is_valid() {
  local marker="$1"
  [[ "$(fleet_marker_value "$marker" format)" == "1" ]] \
    && [[ "$(fleet_marker_value "$marker" owner_sha256)" =~ ^[0-9a-f]{64}$ ]] \
    && [[ "$(fleet_marker_value "$marker" secret_key_sha256)" =~ ^[0-9a-f]{64}$ ]] \
    && [[ "$(fleet_marker_value "$marker" recovery_key_sha256)" =~ ^[0-9a-f]{64}$ ]]
}

fleet_container_workdir() {
  docker inspect --format '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$1" 2>/dev/null || true
}

fleet_container_uses_data_volume() {
  local mount_identity=""

  mount_identity="$(docker inspect --format \
    '{{range .Mounts}}{{if eq .Destination "/var/lib/hermes-fleet"}}{{printf "%s\t%s" .Type .Name}}{{end}}{{end}}' \
    "$1" 2>/dev/null || true)"
  [[ "$mount_identity" == $'volume\t'"$fleet_data_volume_name" ]]
}

fleet_container_key_fingerprints() {
  local container_name="$1"
  local environment=""
  local secret_key=""
  local recovery_key=""
  local secret_count=0
  local recovery_count=0

  environment="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container_name" 2>/dev/null)" || return 1
  secret_key="$(awk -F= '$1 == "FLEET_SECRET_ENCRYPTION_KEY" { sub(/^[^=]*=/, ""); print; exit }' <<< "$environment")"
  recovery_key="$(awk -F= '$1 == "FLEET_RECOVERY_ENCRYPTION_KEY" { sub(/^[^=]*=/, ""); print; exit }' <<< "$environment")"
  secret_count="$(awk -F= '$1 == "FLEET_SECRET_ENCRYPTION_KEY" { count++ } END { print count + 0 }' <<< "$environment")"
  recovery_count="$(awk -F= '$1 == "FLEET_RECOVERY_ENCRYPTION_KEY" { count++ } END { print count + 0 }' <<< "$environment")"
  if [[ "$secret_count" != "1" ]] \
    || [[ "$recovery_count" != "1" ]] \
    || ! fleet_valid_encryption_key "$secret_key" \
    || ! fleet_valid_encryption_key "$recovery_key"; then
    return 1
  fi
  printf '%s\t%s\n' "$(fleet_key_fingerprint "$secret_key")" "$(fleet_key_fingerprint "$recovery_key")"
}

fleet_legacy_key_container() {
  local root_dir="$1"
  local container_name=""
  local existing_workdir=""
  local relocation_candidate=""

  root_dir="$(cd "$root_dir" && pwd -P)"
  for container_name in hermes-fleet-control-plane hermes-fleet-manager-control-plane-1; do
    existing_workdir="$(fleet_container_workdir "$container_name")"
    if [[ -z "$existing_workdir" ]] || ! fleet_container_uses_data_volume "$container_name"; then
      continue
    fi
    existing_workdir="$(cd "$existing_workdir" 2>/dev/null && pwd -P || printf '%s' "$existing_workdir")"
    if [[ "$existing_workdir" == "$root_dir" ]]; then
      printf '%s\n' "$container_name"
      return 0
    fi
    if [[ "${FLEET_ALLOW_PROJECT_RELOCATION:-0}" == "1" && -z "$relocation_candidate" ]]; then
      relocation_candidate="$container_name"
    fi
  done
  if [[ -n "$relocation_candidate" ]]; then
    printf '%s\n' "$relocation_candidate"
    return 0
  fi
  return 1
}

fleet_marker_reader_image() {
  local container_name image=""

  for container_name in hermes-fleet-control-plane hermes-fleet-manager-control-plane-1; do
    image="$(docker inspect --format '{{.Image}}' "$container_name" 2>/dev/null || true)"
    if [[ -n "$image" ]]; then
      printf '%s\n' "$image"
      return 0
    fi
  done
  image="$(docker image ls --filter reference='local/hermes-fleet-manager:*' --format '{{.ID}}' 2>/dev/null | head -n 1)"
  if [[ -n "$image" ]]; then
    printf '%s\n' "$image"
    return 0
  fi
  image="$(docker image ls --filter reference='local/hermes-fleet-runtime:*' --format '{{.ID}}' 2>/dev/null | head -n 1)"
  if [[ -n "$image" ]]; then
    printf '%s\n' "$image"
    return 0
  fi
  return 1
}

read_fleet_owner_marker() {
  local image
  local container_name

  for container_name in hermes-fleet-control-plane hermes-fleet-manager-control-plane-1; do
    # Docker reports State.Running=true while a restart-policy container is in
    # its restarting transition. docker exec rejects that state, so only use
    # exec for a stable running container and otherwise inspect the volume with
    # an isolated one-shot reader.
    if [[ "$(docker inspect --format '{{.State.Status}}' "$container_name" 2>/dev/null || true)" == "running" ]] \
      && fleet_container_uses_data_volume "$container_name"; then
      docker exec "$container_name" sh -c \
        "if [ -f '$fleet_owner_marker_path' ]; then exec cat '$fleet_owner_marker_path'; else exit 3; fi"
      return
    fi
  done
  image="$(fleet_marker_reader_image)" || return 5
  docker run --rm \
    --network none \
    --read-only \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --entrypoint /bin/sh \
    --mount "type=volume,src=$fleet_data_volume_name,dst=/var/lib/hermes-fleet,readonly" \
    "$image" -c "if [ -f '$fleet_owner_marker_path' ]; then exec cat '$fleet_owner_marker_path'; else exit 3; fi"
}

assert_compose_owner() {
  local root_dir="$1"
  local container_name
  local existing_workdir
  local matching_active_owner=0
  local legacy_container_available=0
  local mounted_control_planes=0
  local uses_data_volume=0
  local marker=""
  local marker_status=0
  local expected_owner

  root_dir="$(cd "$root_dir" && pwd -P)"
  expected_owner="$(fleet_owner_hash "$root_dir")"

  # Check the current explicit name and the pre-0.6.9 Compose-generated name so
  # a stale deployment from another checkout cannot bypass the ownership fence.
  for container_name in hermes-fleet-control-plane hermes-fleet-manager-control-plane-1; do
    uses_data_volume=0
    existing_workdir="$(fleet_container_workdir "$container_name")"
    if [[ -n "$existing_workdir" ]]; then
      existing_workdir="$(cd "$existing_workdir" 2>/dev/null && pwd -P || printf '%s' "$existing_workdir")"
    fi
    if fleet_container_uses_data_volume "$container_name"; then
      uses_data_volume=1
      mounted_control_planes=$((mounted_control_planes + 1))
    fi
    if [[ "$uses_data_volume" == "1" && -z "$existing_workdir" ]]; then
      cat >&2 <<EOF
Fleet control-plane container $container_name mounts the protected data volume,
but its Compose checkout ownership label is missing. Refusing to infer ownership.
Restore the verified Compose metadata or remove the obsolete container explicitly.
EOF
      return 1
    fi
    if [[ -n "$existing_workdir" && "$existing_workdir" == "$root_dir" ]]; then
      if [[ "$uses_data_volume" != "1" ]]; then
        echo "The matching Fleet container does not mount the protected Fleet data volume." >&2
        return 1
      fi
      matching_active_owner=1
      legacy_container_available=1
    elif [[ -n "$existing_workdir" && "${FLEET_ALLOW_PROJECT_RELOCATION:-0}" != "1" ]]; then
      cat >&2 <<EOF
Refusing to manage the hermes-fleet-manager Compose project from this checkout.
Active owner: $existing_workdir
Current path: $root_dir

Run the command from the active owner checkout. For a reviewed relocation,
set FLEET_ALLOW_PROJECT_RELOCATION=1 for that single command.
EOF
      return 1
    elif [[ -n "$existing_workdir" && "$uses_data_volume" == "1" ]]; then
      legacy_container_available=1
    fi
  done
  if [[ "$mounted_control_planes" -gt 1 ]]; then
    cat >&2 <<EOF
Multiple Fleet control-plane containers mount the protected data volume.
Refusing to continue while ownership and exclusive SQLite access are ambiguous.
Stop and remove the obsolete control-plane container explicitly, then retry.
EOF
    return 1
  fi

  if fleet_volume_exists "$fleet_data_volume_name"; then
    marker_status=0
  else
    marker_status=$?
    if [[ "$marker_status" == "2" ]]; then
      return 1
    fi
    return 0
  fi
  if marker="$(read_fleet_owner_marker)"; then
    marker_status=0
  else
    marker_status=$?
  fi
  if [[ "$marker_status" == "0" ]]; then
    if ! fleet_marker_is_valid "$marker"; then
      echo "The persistent Fleet volume owner marker is malformed; refusing to overwrite it." >&2
      return 1
    fi
    if [[ "$(fleet_marker_value "$marker" owner_sha256)" != "$expected_owner" ]] \
      && [[ "${FLEET_ALLOW_PROJECT_RELOCATION:-0}" != "1" ]]; then
      cat >&2 <<EOF
Refusing to manage the persistent Fleet volume from this checkout.
The volume is owned by another checkout.
Current path: $root_dir

For a reviewed relocation that preserves the original .env, set
FLEET_ALLOW_PROJECT_RELOCATION=1 for that single command.
EOF
      return 1
    fi
    return 0
  fi
  if [[ "$marker_status" == "3" && "$matching_active_owner" == "1" ]]; then
    # Legacy deployment: the active Compose working-dir label is sufficient
    # evidence for a one-time migration. Fleet maintenance writes the durable marker.
    return 0
  fi
  if [[ "$marker_status" == "3" \
    && "${FLEET_ALLOW_PROJECT_RELOCATION:-0}" == "1" \
    && "$legacy_container_available" == "1" ]]; then
    return 0
  fi
  cat >&2 <<EOF
The persistent Fleet volume exists, but its checkout owner cannot be verified.
No active matching Compose deployment or durable owner marker was found.

Start from the last owner checkout, or perform a reviewed relocation with
FLEET_ALLOW_PROJECT_RELOCATION=1 while preserving the original .env.

For a pre-marker installation, the original owning control-plane container
must still exist for one setup run so Fleet can compare its encryption keys
and write the durable marker. Relocation never bypasses that key check. If both
the marker and owning container are gone, restore that verified legacy
container metadata before retrying; Fleet will not guess ownership or keys.
EOF
  return 1
}

assert_fleet_marker_keys() {
  local root_dir="$1"
  local secret_key="$2"
  local recovery_key="$3"
  local marker=""
  local marker_status=0
  local legacy_container=""
  local fingerprints=""
  local expected_fingerprints=""

  if fleet_volume_exists "$fleet_data_volume_name"; then
    marker_status=0
  else
    marker_status=$?
    [[ "$marker_status" != "2" ]]
    return
  fi
  if marker="$(read_fleet_owner_marker)"; then
    marker_status=0
  else
    marker_status=$?
  fi
  if [[ "$marker_status" == "3" ]]; then
    legacy_container="$(fleet_legacy_key_container "$root_dir")" || {
      cat >&2 <<EOF
Legacy Fleet encryption keys cannot be verified without the original owning
control-plane container. A pre-marker installation must keep that container
for one setup run so Fleet can compare its environment with .env and write the
durable marker. FLEET_ALLOW_PROJECT_RELOCATION does not bypass this check.
EOF
      return 1
    }
    fingerprints="$(fleet_container_key_fingerprints "$legacy_container")" || {
      echo "Legacy Fleet encryption keys cannot be verified from the owning container." >&2
      return 1
    }
    expected_fingerprints="$(printf '%s\t%s' \
      "$(fleet_key_fingerprint "$secret_key")" \
      "$(fleet_key_fingerprint "$recovery_key")")"
    if [[ "$fingerprints" != "$expected_fingerprints" ]]; then
      echo "The encryption keys in .env do not match the legacy Fleet container." >&2
      return 1
    fi
    return 0
  fi
  if [[ "$marker_status" != "0" ]] || ! fleet_marker_is_valid "$marker"; then
    echo "The persistent Fleet volume key marker cannot be verified." >&2
    return 1
  fi
  if [[ "$(fleet_marker_value "$marker" secret_key_sha256)" != "$(fleet_key_fingerprint "$secret_key")" ]] \
    || [[ "$(fleet_marker_value "$marker" recovery_key_sha256)" != "$(fleet_key_fingerprint "$recovery_key")" ]]; then
    cat >&2 <<EOF
The encryption keys in .env do not match the persistent Fleet volume.
Restore the original .env before running setup. No key was changed.
EOF
    return 1
  fi
}

write_fleet_owner_marker() {
  local root_dir="$1"
  local secret_key="$2"
  local recovery_key="$3"
  local marker

  if ! fleet_valid_encryption_key "$secret_key" || ! fleet_valid_encryption_key "$recovery_key"; then
    echo "Refusing to write a Fleet owner marker with invalid encryption keys." >&2
    return 1
  fi
  marker="$(
    printf 'format=1\n'
    printf 'owner_sha256=%s\n' "$(fleet_owner_hash "$root_dir")"
    printf 'secret_key_sha256=%s\n' "$(fleet_key_fingerprint "$secret_key")"
    printf 'recovery_key_sha256=%s\n' "$(fleet_key_fingerprint "$recovery_key")"
  )"
  printf '%s\n' "$marker" | docker exec -i hermes-fleet-control-plane sh -c \
    "set -eu; umask 077; temporary='$fleet_owner_marker_path.tmp'; cat > \"\$temporary\"; mv \"\$temporary\" '$fleet_owner_marker_path'"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  if [[ $# -ne 1 ]]; then
    echo "Usage: assert-compose-owner.sh <checkout-root>" >&2
    exit 2
  fi
  assert_compose_owner "$1"
fi
