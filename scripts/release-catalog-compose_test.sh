#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/setup-lib.sh"

test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT

write_catalog() {
  local path="$1"
  local checked_at="$2"
  local oldest_version="$3"
  local oldest_commit="$4"

  python3 - "$path" "$checked_at" "$oldest_version" "$oldest_commit" <<'PY'
import json
import sys

path, checked_at, oldest_version, oldest_commit = sys.argv[1:5]
versions = [
    ("0.19.0", "a" * 40, "2026-07-26T00:00:00Z"),
    ("0.18.2", "b" * 40, "2026-07-25T00:00:00Z"),
    (oldest_version, oldest_commit, "2026-07-24T00:00:00Z"),
]
catalog = {
    "source": "NousResearch/hermes-agent GitHub Releases",
    "checked_at": checked_at,
    "releases": [
        {
            "version": version,
            "tag": f"v{version}",
            "commit": commit,
            "image": f"local/hermes-fleet-runtime:{version}-{commit[:12]}",
            "url": f"https://github.com/NousResearch/hermes-agent/releases/tag/v{version}",
            "published_at": published_at,
        }
        for version, commit, published_at in versions
    ],
}
with open(path, "w", encoding="utf-8") as destination:
    json.dump(catalog, destination, separators=(",", ":"))
PY
}

catalog_fingerprint() {
  local catalog="$1"
  local tsv="$2"

  portable_catalog_json_to_tsv "$catalog" "$tsv"
  release_catalog_fingerprint_tsv "$tsv"
}

write_compose_env() {
  local path="$1"
  local fingerprint="$2"

  {
    printf 'FLEET_ADMIN_TOKEN=%064d\n' 1
    printf 'FLEET_ENROLLMENT_TOKEN=%064d\n' 2
    printf 'FLEET_SECRET_ENCRYPTION_KEY=%064d\n' 3
    printf 'FLEET_RECOVERY_ENCRYPTION_KEY=%064d\n' 4
    printf 'FLEET_HERMES_RELEASE_CATALOG_FINGERPRINT=%s\n' "$fingerprint"
  } > "$path"
}

rendered_fingerprint() {
  local env_file="$1"

  docker compose \
    --project-directory "$ROOT_DIR" \
    --env-file "$env_file" \
    -f "$ROOT_DIR/docker-compose.yml" \
    config --format json \
    | python3 -c '
import json
import sys

config = json.load(sys.stdin)
print(config["services"]["control-plane"]["labels"]["io.hermes-fleet.release-catalog-fingerprint"])
'
}

base_catalog="$test_root/base.json"
timestamp_catalog="$test_root/timestamp.json"
changed_catalog="$test_root/changed.json"
write_catalog "$base_catalog" "2026-07-27T00:00:00Z" "0.18.1" "$(printf 'c%.0s' {1..40})"
write_catalog "$timestamp_catalog" "2026-07-27T01:00:00Z" "0.18.1" "$(printf 'c%.0s' {1..40})"
write_catalog "$changed_catalog" "2026-07-27T01:00:00Z" "0.18.0" "$(printf 'd%.0s' {1..40})"

base_fingerprint="$(catalog_fingerprint "$base_catalog" "$test_root/base.tsv")"
timestamp_fingerprint="$(catalog_fingerprint "$timestamp_catalog" "$test_root/timestamp.tsv")"
changed_fingerprint="$(catalog_fingerprint "$changed_catalog" "$test_root/changed.tsv")"

if [[ "$base_fingerprint" != "$timestamp_fingerprint" ]]; then
  echo "FAIL: checked_at-only catalog change altered the release-set fingerprint." >&2
  exit 1
fi
if [[ "$base_fingerprint" == "$changed_fingerprint" ]]; then
  echo "FAIL: installable release-set change did not alter the fingerprint." >&2
  exit 1
fi

write_compose_env "$test_root/base.env" "$base_fingerprint"
write_compose_env "$test_root/timestamp.env" "$timestamp_fingerprint"
write_compose_env "$test_root/changed.env" "$changed_fingerprint"

base_rendered="$(rendered_fingerprint "$test_root/base.env")"
timestamp_rendered="$(rendered_fingerprint "$test_root/timestamp.env")"
changed_rendered="$(rendered_fingerprint "$test_root/changed.env")"

if [[ "$base_rendered" != "$timestamp_rendered" ]]; then
  echo "FAIL: checked_at-only catalog change altered rendered Compose config." >&2
  exit 1
fi
if [[ "$base_rendered" == "$changed_rendered" ]]; then
  echo "FAIL: installable release-set change did not alter rendered Compose config." >&2
  exit 1
fi
if [[ "$changed_rendered" != "$changed_fingerprint" ]]; then
  echo "FAIL: rendered Compose fingerprint does not match the release set." >&2
  exit 1
fi

echo "release catalog Compose fingerprint regression test passed."
