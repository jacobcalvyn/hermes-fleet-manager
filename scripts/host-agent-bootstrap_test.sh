#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/host-agent-bootstrap.sh"

test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
fake_agent="$test_root/hermes-fleet-agent"
log_file="$test_root/calls.log"
config_file="$test_root/agent.json"

cat > "$fake_agent" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

command="$1"
shift
printf '%s-args:%s\n' "$command" "$*" >> "$FAKE_AGENT_LOG"
case "$command" in
  validate)
    exit "${FAKE_VALIDATE_STATUS:-1}"
    ;;
  probe)
    exit "${FAKE_PROBE_STATUS:-1}"
    ;;
  enroll)
    enrollment_status="${FAKE_ENROLL_STATUS:-1}"
    if [[ " $* " == *" --token-stdin "* ]]; then
      IFS= read -r enrollment_token
      printf 'enroll-stdin:%s\n' "$enrollment_token" >> "$FAKE_AGENT_LOG"
    fi
    if [[ "$enrollment_status" == "0" ]]; then
      printf '{}\n' > "$FAKE_CONFIG_FILE"
      chmod 600 "$FAKE_CONFIG_FILE"
      printf 'enrolled\n'
      exit 0
    fi
    printf 'host-agent: enrollment failed (409): duplicate host\n' >&2
    exit "$enrollment_status"
    ;;
  recover)
    IFS= read -r admin_token
    printf 'recover-stdin:%s\n' "$admin_token" >> "$FAKE_AGENT_LOG"
    if [[ "${FAKE_RECOVER_STATUS:-1}" == "0" ]]; then
      printf '{}\n' > "$FAKE_CONFIG_FILE"
      chmod 600 "$FAKE_CONFIG_FILE"
      printf 'recovered\n'
      exit 0
    fi
    printf 'host-agent: recovery failed\n' >&2
    exit 1
    ;;
esac
exit 2
EOF
chmod 700 "$fake_agent"

export FAKE_AGENT_LOG="$log_file"
export FAKE_CONFIG_FILE="$config_file"
export FAKE_VALIDATE_STATUS=1
export FAKE_ENROLL_STATUS=11
export FAKE_RECOVER_STATUS=0

ensure_host_agent_config \
  "$fake_agent" \
  "$config_file" \
  "http://127.0.0.1:9180" \
  "enrollment-secret" \
  "admin-secret" \
  "local-mac" \
  "$test_root/instances" >/dev/null

grep -F "recover-args:--admin-token-stdin" "$log_file" >/dev/null
grep -F "recover-stdin:admin-secret" "$log_file" >/dev/null
grep -F "enroll-args:--token-stdin" "$log_file" >/dev/null
grep -F "enroll-stdin:enrollment-secret" "$log_file" >/dev/null
if grep -F "enroll-args:" "$log_file" | grep -F "enrollment-secret" >/dev/null; then
  echo "Enrollment token leaked into Host Agent enrollment arguments." >&2
  exit 1
fi
if grep -F "recover-args:" "$log_file" | grep -F "admin-secret" >/dev/null; then
  echo "Admin token leaked into Host Agent recovery arguments." >&2
  exit 1
fi

: > "$log_file"
export FAKE_VALIDATE_STATUS=0
export FAKE_PROBE_STATUS=0
ensure_host_agent_config \
  "$fake_agent" \
  "$config_file" \
  "http://127.0.0.1:9180" \
  "enrollment-secret" \
  "admin-secret" \
  "local-mac" \
  "$test_root/instances"
grep -F "validate-args:" "$log_file" >/dev/null
grep -F "probe-args:" "$log_file" >/dev/null
if grep -F "enroll-args:" "$log_file" >/dev/null || grep -F "recover-args:" "$log_file" >/dev/null; then
  echo "A valid Host Agent config triggered credential replacement." >&2
  exit 1
fi

printf 'stale-config\n' > "$config_file"
: > "$log_file"
export FAKE_VALIDATE_STATUS=0
export FAKE_PROBE_STATUS=10
export FAKE_RECOVER_STATUS=0
ensure_host_agent_config \
  "$fake_agent" \
  "$config_file" \
  "http://127.0.0.1:9180" \
  "enrollment-secret" \
  "admin-secret" \
  "local-mac" \
  "$test_root/instances" >/dev/null
grep -F "probe-args:" "$log_file" >/dev/null
grep -F "recover-args:--admin-token-stdin" "$log_file" >/dev/null
grep -F "recover-stdin:admin-secret" "$log_file" >/dev/null
if grep -F "enroll-args:" "$log_file" >/dev/null; then
  echo "A locally valid stale config attempted enrollment before recovery." >&2
  exit 1
fi
if grep -F "recover-args:" "$log_file" | grep -F "admin-secret" >/dev/null; then
  echo "Admin token leaked into stale-config recovery arguments." >&2
  exit 1
fi

printf 'preserve-me\n' > "$config_file"
: > "$log_file"
export FAKE_VALIDATE_STATUS=0
export FAKE_PROBE_STATUS=10
export FAKE_ENROLL_STATUS=11
export FAKE_RECOVER_STATUS=1
if ensure_host_agent_config \
  "$fake_agent" \
  "$config_file" \
  "http://127.0.0.1:9180" \
  "enrollment-secret" \
  "admin-secret" \
  "local-mac" \
  "$test_root/instances" >/dev/null 2>&1; then
  echo "Host Agent bootstrap accepted failed enrollment and recovery." >&2
  exit 1
fi
if [[ "$(cat "$config_file")" != "preserve-me" ]]; then
  echo "Failed Host Agent recovery changed the existing config." >&2
  exit 1
fi
if grep -F "enroll-args:" "$log_file" >/dev/null; then
  echo "A locally valid stale config attempted enrollment after probe failure." >&2
  exit 1
fi

printf 'preserve-transient\n' > "$config_file"
: > "$log_file"
export FAKE_VALIDATE_STATUS=0
export FAKE_PROBE_STATUS=1
export FAKE_RECOVER_STATUS=0
if ensure_host_agent_config \
  "$fake_agent" \
  "$config_file" \
  "http://127.0.0.1:9180" \
  "enrollment-secret" \
  "admin-secret" \
  "local-mac" \
  "$test_root/instances" >/dev/null 2>&1; then
  echo "Host Agent bootstrap accepted a non-authentication probe failure." >&2
  exit 1
fi
if [[ "$(cat "$config_file")" != "preserve-transient" ]]; then
  echo "A transient probe failure changed the existing Host Agent config." >&2
  exit 1
fi
if grep -F "recover-args:" "$log_file" >/dev/null || grep -F "enroll-args:" "$log_file" >/dev/null; then
  echo "A transient probe failure triggered credential replacement." >&2
  exit 1
fi

printf 'preserve-invalid\n' > "$config_file"
: > "$log_file"
export FAKE_VALIDATE_STATUS=1
export FAKE_ENROLL_STATUS=1
export FAKE_RECOVER_STATUS=0
if ensure_host_agent_config \
  "$fake_agent" \
  "$config_file" \
  "http://127.0.0.1:9180" \
  "enrollment-secret" \
  "admin-secret" \
  "local-mac" \
  "$test_root/instances" >/dev/null 2>&1; then
  echo "Host Agent bootstrap accepted a non-conflict enrollment failure." >&2
  exit 1
fi
if [[ "$(cat "$config_file")" != "preserve-invalid" ]]; then
  echo "A non-conflict enrollment failure changed the existing Host Agent config." >&2
  exit 1
fi
if grep -F "recover-args:" "$log_file" >/dev/null; then
  echo "A non-conflict enrollment failure triggered credential recovery." >&2
  exit 1
fi
grep -F "enroll-args:--token-stdin" "$log_file" >/dev/null
grep -F "enroll-stdin:enrollment-secret" "$log_file" >/dev/null
if grep -F "enroll-args:" "$log_file" | grep -F "enrollment-secret" >/dev/null; then
  echo "Enrollment token leaked into failed enrollment arguments." >&2
  exit 1
fi

echo "Host Agent bootstrap tests passed."
