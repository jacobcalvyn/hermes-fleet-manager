#!/usr/bin/env bash

ensure_host_agent_config() {
  local agent_binary="$1"
  local agent_config="$2"
  local control_plane_url="$3"
  local enrollment_token="$4"
  local admin_token="$5"
  local host_name="$6"
  local managed_root="$7"
  local enrollment_output=""
  local enrollment_status=0
  local local_config_valid=0
  local probe_output=""
  local probe_status=0

  if [[ -f "$agent_config" ]] &&
    "$agent_binary" validate --config "$agent_config" >/dev/null 2>&1; then
    local_config_valid=1
    if probe_output="$(
      "$agent_binary" probe \
        --config "$agent_config" \
        --url "$control_plane_url" \
        --name "$host_name" \
        --managed-root "$managed_root" 2>&1
    )"; then
      return 0
    else
      probe_status=$?
    fi
    if [[ "$probe_status" -ne 10 ]]; then
      echo "Host Agent probe failed without an authentication rejection; the existing credential was preserved." >&2
      if [[ -n "$probe_output" ]]; then
        printf '%s\n' "$probe_output" >&2
      fi
      return 1
    fi
  fi

  if [[ "$local_config_valid" == "0" ]]; then
    if enrollment_output="$(
      printf '%s\n' "$enrollment_token" | "$agent_binary" enroll \
          --token-stdin \
          --url "$control_plane_url" \
          --name "$host_name" \
          --managed-root "$managed_root" \
          --config "$agent_config" 2>&1
    )"; then
      if [[ -n "$enrollment_output" ]]; then
        printf '%s\n' "$enrollment_output"
      fi
      return 0
    else
      enrollment_status=$?
    fi
    if [[ "$enrollment_status" -ne 11 ]]; then
      echo "Standard Host Agent enrollment failed without a duplicate-host response; credential recovery was not attempted." >&2
      if [[ -n "$enrollment_output" ]]; then
        printf '%s\n' "$enrollment_output" >&2
      fi
      return 1
    fi
  fi

  if printf '%s\n' "$admin_token" | "$agent_binary" recover \
    --admin-token-stdin \
    --url "$control_plane_url" \
    --name "$host_name" \
    --managed-root "$managed_root" \
    --config "$agent_config"; then
    return 0
  fi

  if [[ "$local_config_valid" == "1" ]]; then
    echo "Host Agent authentication was rejected; safe credential recovery also failed and the existing config was preserved." >&2
  else
    echo "The Host Agent name is already enrolled and safe credential recovery failed:" >&2
    printf '%s\n' "$enrollment_output" >&2
  fi
  return 1
}
