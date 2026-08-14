#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
tests_run=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

prepare_fixture() {
  local fixture="$1"
  mkdir -p "$fixture/root/scripts" "$fixture/root/bin" "$fixture/home/.config/hermes-fleet" "$fixture/fake-bin"
  cp "$ROOT_DIR/scripts/install-launchd-agent.sh" "$fixture/root/scripts/"
  cp "$ROOT_DIR/scripts/macos-launchagent-status.swift" "$fixture/root/scripts/"
  printf '{}\n' > "$fixture/home/.config/hermes-fleet/agent.json"

  cat > "$fixture/root/bin/hermes-fleet-agent" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  validate|probe) exit 0 ;;
  *) exit 0 ;;
esac
EOF
  chmod +x "$fixture/root/bin/hermes-fleet-agent"

  cat > "$fixture/fake-bin/docker" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  cat > "$fixture/fake-bin/launchctl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  cat > "$fixture/fake-bin/plutil" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "-extract" ]]; then
  if [[ -n "${FAKE_DOCKER_CREDENTIAL_STORE:-}" ]]; then
    printf '%s\n' "$FAKE_DOCKER_CREDENTIAL_STORE"
    exit 0
  fi
  exit 1
fi
exit 0
EOF
  cat > "$fixture/fake-bin/swift" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${FAKE_LAUNCH_AGENT_STATUS:?}"
EOF
  chmod +x "$fixture/fake-bin/"*
}

test_enabled_background_item_is_verified() {
  local fixture="$test_root/enabled"
  local output="$fixture/output"
  prepare_fixture "$fixture"

  HOME="$fixture/home" \
    PATH="$fixture/fake-bin:$PATH" \
    FAKE_LAUNCH_AGENT_STATUS=enabled \
    "$fixture/root/scripts/install-launchd-agent.sh" >"$output" 2>&1

  grep -q 'approved, running, and verified' "$output"
  test -x "$fixture/home/.local/bin/hermes-fleet-agent"
  test -f "$fixture/home/Library/LaunchAgents/io.hermes-fleet.host-agent.plist"
}

test_denied_background_item_fails_without_removing_installation() {
  local fixture="$test_root/requires-approval"
  local output="$fixture/output"
  prepare_fixture "$fixture"

  if HOME="$fixture/home" \
    PATH="$fixture/fake-bin:$PATH" \
    FAKE_LAUNCH_AGENT_STATUS=requires-approval \
    "$fixture/root/scripts/install-launchd-agent.sh" >"$output" 2>&1; then
    return 1
  fi

  grep -q 'requires approval before macOS can start it after login or reboot' "$output"
  grep -q 'Login Items & Extensions' "$output"
  test -x "$fixture/home/.local/bin/hermes-fleet-agent"
  test -f "$fixture/home/Library/LaunchAgents/io.hermes-fleet.host-agent.plist"
  ! grep -q 'running and verified' "$output"
}

test_credential_helper_directory_is_added_to_launch_agent_path() {
  local fixture="$test_root/credential-helper"
  local output="$fixture/output"
  local plist=""
  prepare_fixture "$fixture"
  mkdir -p "$fixture/home/.docker" "$fixture/helper-bin"
  printf '{"credsStore":"desktop"}\n' > "$fixture/home/.docker/config.json"
  cat > "$fixture/helper-bin/docker-credential-desktop" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$fixture/helper-bin/docker-credential-desktop"

  HOME="$fixture/home" \
    PATH="$fixture/fake-bin:$fixture/helper-bin:$PATH" \
    FAKE_DOCKER_CREDENTIAL_STORE=desktop \
    FAKE_LAUNCH_AGENT_STATUS=enabled \
    "$fixture/root/scripts/install-launchd-agent.sh" >"$output" 2>&1

  plist="$fixture/home/Library/LaunchAgents/io.hermes-fleet.host-agent.plist"
  grep -q '<key>EnvironmentVariables</key>' "$plist"
  grep -q '<key>PATH</key>' "$plist"
  grep -Fq "$fixture/helper-bin" "$plist"
}

test_missing_configured_credential_helper_stops_installation() {
  local fixture="$test_root/missing-credential-helper"
  local output="$fixture/output"
  prepare_fixture "$fixture"
  mkdir -p "$fixture/home/.docker"
  printf '{"credsStore":"not-installed"}\n' > "$fixture/home/.docker/config.json"

  if HOME="$fixture/home" \
    PATH="$fixture/fake-bin:$PATH" \
    FAKE_DOCKER_CREDENTIAL_STORE=not-installed \
    FAKE_LAUNCH_AGENT_STATUS=enabled \
    "$fixture/root/scripts/install-launchd-agent.sh" >"$output" 2>&1; then
    return 1
  fi

  grep -q 'docker-credential-not-installed is configured but is not available' "$output"
  test ! -e "$fixture/home/Library/LaunchAgents/io.hermes-fleet.host-agent.plist"
}

test_enabled_background_item_is_verified || fail "enabled background item"
tests_run=$((tests_run + 1))
test_denied_background_item_fails_without_removing_installation || fail "denied background item"
tests_run=$((tests_run + 1))
test_credential_helper_directory_is_added_to_launch_agent_path || fail "credential helper path"
tests_run=$((tests_run + 1))
test_missing_configured_credential_helper_stops_installation || fail "missing credential helper"
tests_run=$((tests_run + 1))

echo "LaunchAgent installer regression tests passed ($tests_run tests)."
