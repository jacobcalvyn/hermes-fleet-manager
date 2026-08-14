#!/usr/bin/env bash
set -euo pipefail

runtime_identity_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

runtime_build_id() {
  local root_dir="${1:-$runtime_identity_root}"
  local build_id

  build_id="$(
    command cat \
      "$root_dir/runtime/Dockerfile" \
      "$root_dir/runtime/entrypoint.sh" \
      "$root_dir/runtime/configure.py" \
      | shasum -a 256 \
      | awk '{print $1}'
  )"
  if [[ ! "$build_id" =~ ^[0-9a-f]{64}$ ]]; then
    echo "Hermes runtime build identity could not be calculated." >&2
    return 1
  fi
  printf '%s\n' "$build_id"
}

runtime_image_reference() {
  local version="$1"
  local commit="$2"
  local build_id="$3"

  if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
    || [[ ! "$commit" =~ ^[0-9a-fA-F]{40}$ ]] \
    || [[ ! "$build_id" =~ ^[0-9a-f]{64}$ ]]; then
    echo "Invalid Hermes release or runtime identity." >&2
    return 1
  fi
  printf 'local/hermes-fleet-runtime:%s-%s-%s\n' \
    "$version" \
    "$(printf '%s' "$commit" | tr '[:upper:]' '[:lower:]' | cut -c1-12)" \
    "${build_id:0:12}"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  if [[ "${1:-}" == "--root" ]]; then
    if [[ $# -lt 3 ]]; then
      echo "Usage: runtime-identity.sh [--root <checkout-root>] build-id" >&2
      exit 2
    fi
    runtime_identity_root="$(cd "$2" && pwd -P)"
    shift 2
  fi
  case "${1:-}" in
    build-id)
      [[ $# -eq 1 ]] || {
        echo "Usage: runtime-identity.sh [--root <checkout-root>] build-id" >&2
        exit 2
      }
      runtime_build_id "$runtime_identity_root"
      ;;
    image)
      [[ $# -eq 3 ]] || {
        echo "Usage: runtime-identity.sh image <version> <commit>" >&2
        exit 2
      }
      runtime_image_reference "$2" "$3" "$(runtime_build_id "$runtime_identity_root")"
      ;;
    *)
      echo "Usage: runtime-identity.sh [--root <checkout-root>] {build-id|image <version> <commit>}" >&2
      exit 2
      ;;
  esac
fi
