#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_CACHE="${ROOT_DIR}/.cache/go-build"
mkdir -p "${GO_CACHE}"

if [[ "$(uname -s)" == "Darwin" ]]; then
  env GOCACHE="${GO_CACHE}" CGO_ENABLED=1 go test -ldflags=-linkmode=external ./...
else
  env GOCACHE="${GO_CACHE}" CGO_ENABLED=0 go test ./...
fi
env GOCACHE="${GO_CACHE}" go vet ./...

npm --prefix "${ROOT_DIR}/web" run lint
npm --prefix "${ROOT_DIR}/web" run build

while IFS= read -r script; do
  bash -n "${script}"
done < <(find "${ROOT_DIR}/scripts" "${ROOT_DIR}/runtime" -type f -name '*.sh' -print)

make -C "${ROOT_DIR}" test-shell

docker compose --project-directory "${ROOT_DIR}" config --quiet

if [[ -n "${HERMES_FLEET_RECOVERY_TEST_IMAGE:-}" ]]; then
  make -C "${ROOT_DIR}" test-recovery HERMES_FLEET_RECOVERY_TEST_IMAGE="${HERMES_FLEET_RECOVERY_TEST_IMAGE}"
else
  echo "Docker recovery round-trip skipped; set HERMES_FLEET_RECOVERY_TEST_IMAGE before a recovery-sensitive release."
fi

echo "Reliability qualification passed: all Go packages, vet, UI, shell regressions, and Compose."
