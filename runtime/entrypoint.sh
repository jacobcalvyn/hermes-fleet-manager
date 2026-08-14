#!/usr/bin/env bash
set -euo pipefail

mkdir -p "${HERMES_HOME:-/data}" "${HERMES_WORKSPACE:-/workspace}"

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
