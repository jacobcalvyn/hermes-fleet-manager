#!/usr/bin/env bash
set -euo pipefail

image="${1:?usage: image_smoke_test.sh IMAGE}"
volume="hermes-fleet-runtime-smoke-$(openssl rand -hex 8)"
pending_volume="hermes-fleet-runtime-pending-smoke-$(openssl rand -hex 8)"
cleanup() {
  docker volume rm -f "$volume" >/dev/null 2>&1 || true
  docker volume rm -f "$pending_volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker volume create "$volume" >/dev/null
docker volume create "$pending_volume" >/dev/null
common=(
  --rm
  --network none
  -e HERMES_HOME=/data
  -e HERMES_INFERENCE_PROVIDER=openai-codex
  -e HERMES_INFERENCE_MODEL=gpt-5.6-sol
  -e HERMES_REASONING_EFFORT=medium
  -e HERMES_SERVICE_TIER=normal
  -v "$volume:/data"
)

docker run "${common[@]}" -e HERMES_FLEET_CONFIG_OWNER=true "$image" true
docker run "${common[@]}" -e HERMES_FLEET_CONFIG_OWNER=false -e HERMES_FLEET_READY_TIMEOUT=0 "$image" true
docker run --rm --network none -e HERMES_HOME=/data -v "$volume:/data" --entrypoint python "$image" -c '
import hashlib, json, os
from hermes_cli.config import load_config
config = load_config()
model = config.get("model", {})
agent = config.get("agent", {})
with open(os.path.join(os.environ["HERMES_HOME"], ".fleet-runtime-ready.json"), encoding="utf-8") as handle:
    state = json.load(handle)
assert model.get("provider") == "openai-codex"
assert model.get("default") == "gpt-5.6-sol"
assert agent.get("reasoning_effort") == "medium"
assert agent.get("service_tier") == "normal"
assert state.get("schema_version") == 2
assert state.get("reasoning") == "medium"
assert state.get("service_tier") == "normal"
assert state.get("configuration_revision") == hashlib.sha256(
    b"openai-codex\0gpt-5.6-sol\0medium\0normal"
).hexdigest()
assert state.get("runtime_build_id") == os.environ.get("HERMES_FLEET_RUNTIME_BUILD_ID")
'

pending_common=(
  --rm
  --network none
  -e HERMES_HOME=/data
  -e HERMES_INFERENCE_PROVIDER=openai-codex
  -e HERMES_INFERENCE_MODEL=
  -e HERMES_REASONING_EFFORT=
  -e HERMES_SERVICE_TIER=
  -v "$pending_volume:/data"
)
docker run "${pending_common[@]}" -e HERMES_FLEET_CONFIG_OWNER=true "$image" true
docker run "${pending_common[@]}" -e HERMES_FLEET_CONFIG_OWNER=false -e HERMES_FLEET_READY_TIMEOUT=0 "$image" true
docker run --rm --network none -e HERMES_HOME=/data -v "$pending_volume:/data" --entrypoint python "$image" -c '
import os
home = os.environ["HERMES_HOME"]
assert not os.path.exists(os.path.join(home, "config.yaml"))
assert not os.path.exists(os.path.join(home, ".fleet-runtime-ready.json"))
'

label_version="$(docker image inspect --format '{{ index .Config.Labels "io.hermes-fleet.hermes-version" }}' "$image")"
installed_version="$(docker run --rm --network none --entrypoint python "$image" -c 'import importlib.metadata as m; print(m.version("hermes-agent"))')"
[[ -n "$label_version" && "$installed_version" == "$label_version" ]]

echo "Hermes runtime image readiness smoke test passed."
