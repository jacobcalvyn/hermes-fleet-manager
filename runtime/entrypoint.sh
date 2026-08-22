#!/usr/bin/env bash
set -euo pipefail

mkdir -p "${HERMES_HOME:-/data}" "${HERMES_WORKSPACE:-/workspace}"

configure_browser_runtime() {
  local configured="${AGENT_BROWSER_EXECUTABLE_PATH:-}"
  local required="${HERMES_FLEET_BROWSER_REQUIRED:-false}"

  if [[ -n "$configured" ]]; then
    if [[ ! -x "$configured" ]]; then
      echo "Configured Hermes browser executable is not available: $configured" >&2
      return 1
    fi
    export AGENT_BROWSER_EXECUTABLE_PATH="$configured"
    return 0
  fi

  local browser_root="${PLAYWRIGHT_BROWSERS_PATH:-}"
  local candidate
  if [[ -n "$browser_root" ]]; then
    for candidate in \
      "$browser_root"/chromium-*/chrome-linux/chrome \
      "$browser_root"/chromium_headless_shell-*/chrome-linux/headless_shell; do
      if [[ -x "$candidate" ]]; then
        export AGENT_BROWSER_EXECUTABLE_PATH="$candidate"
        return 0
      fi
    done
  fi

  if [[ "$required" == "true" ]]; then
    echo "Hermes browser runtime is required, but no executable was found." >&2
    return 1
  fi
}

configure_browser_runtime

# Only the Hermes service writes the shared configuration. The Dashboard waits
# for the same versioned readiness marker before it can start.
runtime_configuration_pending=false
if [[ "${HERMES_INFERENCE_PROVIDER:-}" == "openai-codex" \
  && -z "${HERMES_INFERENCE_MODEL:-}" \
  && -z "${HERMES_REASONING_EFFORT:-}" \
  && -z "${HERMES_SERVICE_TIER:-}" ]]; then
  runtime_configuration_pending=true
fi

if [[ "$runtime_configuration_pending" != "true" \
  && ( -n "${HERMES_INFERENCE_MODEL:-}" \
  || -n "${HERMES_INFERENCE_PROVIDER:-}" \
  || -n "${HERMES_REASONING_EFFORT:-}" \
  || -n "${HERMES_SERVICE_TIER:-}" ) ]]; then
  config_owner="${HERMES_FLEET_CONFIG_OWNER:-}"
  if [[ -z "$config_owner" && "${1:-}" == "hermes" && "${2:-}" == "gateway" ]]; then
    config_owner="true"
  fi
  if [[ "$config_owner" == "true" ]]; then
    hermes-fleet-runtime-configure apply
  else
    hermes-fleet-runtime-configure verify
  fi
fi

exec "$@"
