#!/usr/bin/env bash

setup_lock_dir=""
setup_lock_pid_file=""
setup_lock_acquired=0

configure_setup_lock() {
  setup_lock_dir="$1"
  setup_lock_pid_file="$setup_lock_dir/pid"
  setup_lock_acquired=0
}

release_setup_lock() {
  local lock_pid=""

  if [[ "$setup_lock_acquired" != "1" ]]; then
    return
  fi
  if [[ -f "$setup_lock_pid_file" ]]; then
    lock_pid="$(<"$setup_lock_pid_file")"
  fi
  if [[ "$lock_pid" == "$$" ]]; then
    rm -f "$setup_lock_pid_file"
    rmdir "$setup_lock_dir" 2>/dev/null || true
  fi
  setup_lock_acquired=0
}

acquire_setup_lock() {
  local existing_pid=""

  if [[ -z "$setup_lock_dir" || -z "$setup_lock_pid_file" ]]; then
    echo "The Fleet setup lock was not configured." >&2
    return 1
  fi
  if mkdir "$setup_lock_dir" 2>/dev/null; then
    printf '%s\n' "$$" > "$setup_lock_pid_file"
    setup_lock_acquired=1
    return
  fi

  if [[ -f "$setup_lock_pid_file" ]]; then
    existing_pid="$(<"$setup_lock_pid_file")"
  fi
  if [[ "$existing_pid" =~ ^[0-9]+$ ]] && kill -0 "$existing_pid" 2>/dev/null; then
    cat >&2 <<EOF
Another Fleet bootstrap or upgrade process is already running for the shared Fleet volume
(PID $existing_pid).
Wait for it to finish before retrying. Concurrent setup runs can make Docker
Compose conflict while it replaces the control-plane container.
EOF
    return 1
  fi

  if [[ -n "$existing_pid" ]]; then
    rm -f "$setup_lock_pid_file"
    rmdir "$setup_lock_dir" 2>/dev/null || true
    if mkdir "$setup_lock_dir" 2>/dev/null; then
      printf '%s\n' "$$" > "$setup_lock_pid_file"
      setup_lock_acquired=1
      return
    fi
  fi

  cat >&2 <<EOF
The host-wide Fleet setup lock exists but its owner cannot be verified:
  $setup_lock_dir

Verify that no Fleet bootstrap or upgrade process is running, then remove this lock
directory and retry.
EOF
  return 1
}
