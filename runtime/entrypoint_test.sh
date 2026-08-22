#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
mkdir -p "$temporary/bin" "$temporary/data" "$temporary/python/hermes_cli"

mkdir -p "$temporary/playwright/chromium-1234/chrome-linux"
cat > "$temporary/playwright/chromium-1234/chrome-linux/chrome" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$temporary/playwright/chromium-1234/chrome-linux/chrome"

cat > "$temporary/bin/assert-browser-runtime" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ -x "${AGENT_BROWSER_EXECUTABLE_PATH:?}" ]]
[[ "$AGENT_BROWSER_EXECUTABLE_PATH" == "${EXPECTED_BROWSER_EXECUTABLE:?}" ]]
EOF
chmod +x "$temporary/bin/assert-browser-runtime"

cat > "$temporary/bin/hermes-fleet-runtime-configure" <<'EOF'
#!/usr/bin/env bash
exec python3 "$FLEET_CONFIGURE_SCRIPT" "$@"
EOF
chmod +x "$temporary/bin/hermes-fleet-runtime-configure"

cat > "$temporary/python/hermes_cli/__init__.py" <<'EOF'
EOF
cat > "$temporary/python/hermes_cli/config.py" <<'EOF'
import json
import os
from pathlib import Path


def _path():
    return Path(os.environ["HERMES_HOME"]) / "config.json"


def read_raw_config():
    try:
        return json.loads(_path().read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}


def load_config():
    return read_raw_config()


def save_config(config, **_kwargs):
    _path().write_text(json.dumps(config), encoding="utf-8")
EOF

common_env=(
  PATH="$temporary/bin:$PATH"
  PYTHONPATH="$temporary/python"
  HERMES_HOME="$temporary/data"
  HERMES_WORKSPACE="$temporary/workspace"
  HERMES_INFERENCE_MODEL="gpt-5.6-sol"
  HERMES_INFERENCE_PROVIDER="openai-codex"
  HERMES_REASONING_EFFORT="medium"
  HERMES_SERVICE_TIER="normal"
  HERMES_FLEET_RUNTIME_BUILD_ID="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  FLEET_CONFIGURE_SCRIPT="$root/runtime/configure.py"
)

env -i \
  PATH="$temporary/bin:$PATH" \
  HERMES_HOME="$temporary/browser-data" \
  HERMES_WORKSPACE="$temporary/browser-workspace" \
  PLAYWRIGHT_BROWSERS_PATH="$temporary/playwright" \
  HERMES_FLEET_BROWSER_REQUIRED=true \
  EXPECTED_BROWSER_EXECUTABLE="$temporary/playwright/chromium-1234/chrome-linux/chrome" \
  "$root/runtime/entrypoint.sh" assert-browser-runtime

env -i \
  PATH="$temporary/bin:$PATH" \
  HERMES_HOME="$temporary/explicit-browser-data" \
  HERMES_WORKSPACE="$temporary/explicit-browser-workspace" \
  AGENT_BROWSER_EXECUTABLE_PATH="$temporary/playwright/chromium-1234/chrome-linux/chrome" \
  HERMES_FLEET_BROWSER_REQUIRED=true \
  EXPECTED_BROWSER_EXECUTABLE="$temporary/playwright/chromium-1234/chrome-linux/chrome" \
  "$root/runtime/entrypoint.sh" assert-browser-runtime

if env -i \
  PATH="$temporary/bin:$PATH" \
  HERMES_HOME="$temporary/missing-browser-data" \
  HERMES_WORKSPACE="$temporary/missing-browser-workspace" \
  PLAYWRIGHT_BROWSERS_PATH="$temporary/missing-playwright" \
  HERMES_FLEET_BROWSER_REQUIRED=true \
  "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Required Hermes browser runtime accepted a missing executable." >&2
  exit 1
fi

if env -i \
  PATH="$temporary/bin:$PATH" \
  HERMES_HOME="$temporary/invalid-browser-data" \
  HERMES_WORKSPACE="$temporary/invalid-browser-workspace" \
  AGENT_BROWSER_EXECUTABLE_PATH="$temporary/missing-chrome" \
  "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Hermes browser runtime accepted an invalid explicit executable." >&2
  exit 1
fi

env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=true \
  "$root/runtime/entrypoint.sh" true
env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=false HERMES_FLEET_READY_TIMEOUT=0 \
  "$root/runtime/entrypoint.sh" true

env -i PATH="$PATH" HERMES_HOME="$temporary/unconfigured" HERMES_WORKSPACE="$temporary/unconfigured-workspace" \
  "$root/runtime/entrypoint.sh" true

pending_env=(
  PATH="$temporary/bin:$PATH"
  PYTHONPATH="$temporary/python"
  HERMES_HOME="$temporary/pending"
  HERMES_WORKSPACE="$temporary/pending-workspace"
  HERMES_INFERENCE_PROVIDER="openai-codex"
  HERMES_INFERENCE_MODEL=""
  HERMES_REASONING_EFFORT=""
  HERMES_SERVICE_TIER=""
  HERMES_FLEET_RUNTIME_BUILD_ID="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  FLEET_CONFIGURE_SCRIPT="$root/runtime/configure.py"
)
env "${pending_env[@]}" HERMES_FLEET_CONFIG_OWNER=true \
  "$root/runtime/entrypoint.sh" true
env "${pending_env[@]}" HERMES_FLEET_CONFIG_OWNER=false HERMES_FLEET_READY_TIMEOUT=0 \
  "$root/runtime/entrypoint.sh" true
if [[ -e "$temporary/pending/config.json" || -e "$temporary/pending/.fleet-runtime-ready.json" ]]; then
  echo "Pending Codex setup unexpectedly persisted a runtime configuration." >&2
  exit 1
fi

python3 - "$temporary/data" <<'PY'
import hashlib
import json
from pathlib import Path
import sys

home = Path(sys.argv[1])
config = json.loads((home / "config.json").read_text(encoding="utf-8"))
state = json.loads((home / ".fleet-runtime-ready.json").read_text(encoding="utf-8"))
expected_revision = hashlib.sha256(b"openai-codex\0gpt-5.6-sol\0medium\0normal").hexdigest()
assert config["model"] == {"default": "gpt-5.6-sol", "provider": "openai-codex"}
assert config["agent"] == {"reasoning_effort": "medium", "service_tier": "normal"}
assert state["schema_version"] == 2
assert state["configuration_revision"] == expected_revision
assert state["reasoning"] == "medium"
assert state["service_tier"] == "normal"
assert state["runtime_build_id"] == "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
PY

python3 - "$temporary/data" <<'PY'
import json
from pathlib import Path
import sys

path = Path(sys.argv[1]) / "config.json"
config = json.loads(path.read_text(encoding="utf-8"))
config["agent"]["service_tier"] = "priority"
path.write_text(json.dumps(config), encoding="utf-8")
PY
if env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=false HERMES_FLEET_READY_TIMEOUT=0 \
  "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Runtime readiness accepted stale effective agent settings." >&2
  exit 1
fi
env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=true \
  "$root/runtime/entrypoint.sh" true

if env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=false HERMES_SERVICE_TIER=priority \
  HERMES_FLEET_READY_TIMEOUT=0 "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Runtime readiness accepted a different service tier." >&2
  exit 1
fi

if env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=false \
  HERMES_FLEET_RUNTIME_BUILD_ID=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
  HERMES_FLEET_READY_TIMEOUT=0 "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Runtime readiness accepted a different wrapper build identity." >&2
  exit 1
fi

if env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=true HERMES_SERVICE_TIER="" \
  "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Runtime configuration accepted a partial reasoning and service-tier tuple." >&2
  exit 1
fi
if env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=true \
  HERMES_REASONING_EFFORT="" HERMES_SERVICE_TIER="" \
  "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Runtime configuration accepted missing reasoning and service-tier settings." >&2
  exit 1
fi

python3 - "$temporary/data" <<'PY'
import json
from pathlib import Path
import sys

path = Path(sys.argv[1]) / ".fleet-runtime-ready.json"
state = json.loads(path.read_text(encoding="utf-8"))
state["schema_version"] = 1
path.write_text(json.dumps(state), encoding="utf-8")
PY
if env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=false HERMES_FLEET_READY_TIMEOUT=0 \
  "$root/runtime/entrypoint.sh" true 2>/dev/null; then
  echo "Runtime readiness accepted the legacy two-field schema." >&2
  exit 1
fi

env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=true \
  "$root/runtime/entrypoint.sh" true
env "${common_env[@]}" HERMES_FLEET_CONFIG_OWNER=false HERMES_FLEET_READY_TIMEOUT=0 \
  "$root/runtime/entrypoint.sh" true
