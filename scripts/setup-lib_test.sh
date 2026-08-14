#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/setup-lib.sh"

test_root="$(mktemp -d)"
trap 'rm -rf "$test_root"' EXIT
tests_run=0

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

run_test() {
  local name="$1"
  shift
  "$@" || fail "$name"
  tests_run=$((tests_run + 1))
}

with_existing_volume() {
  fleet_volume_exists() { return 0; }
  "$@"
}

with_missing_volume() {
  fleet_volume_exists() { return 1; }
  "$@"
}

test_existing_volume_rejects_missing_env() {
  local env_file="$test_root/missing.env"
  if with_existing_volume assert_protected_fleet_environment "$env_file" test-volume >/dev/null 2>&1; then
    return 1
  fi
  [[ ! -e "$env_file" ]]
}

test_existing_volume_rejects_invalid_keys_without_mutation() {
  local env_file="$test_root/invalid.env"
  local before after
  printf 'FLEET_SECRET_ENCRYPTION_KEY=invalid\nFLEET_RECOVERY_ENCRYPTION_KEY=%064d\n' 0 > "$env_file"
  before="$(shasum -a 256 "$env_file")"
  if with_existing_volume assert_protected_fleet_environment "$env_file" test-volume >/dev/null 2>&1; then
    return 1
  fi
  after="$(shasum -a 256 "$env_file")"
  [[ "$before" == "$after" ]]
}

test_existing_volume_accepts_valid_keys() {
  local env_file="$test_root/valid.env"
  printf 'FLEET_SECRET_ENCRYPTION_KEY=%064d\nFLEET_RECOVERY_ENCRYPTION_KEY=%064d\n' 1 2 > "$env_file"
  with_existing_volume assert_protected_fleet_environment "$env_file" test-volume
}

test_existing_volume_rejects_duplicate_keys_without_mutation() {
  local env_file="$test_root/duplicate.env"
  local before after
  printf 'FLEET_SECRET_ENCRYPTION_KEY=%064d\nFLEET_SECRET_ENCRYPTION_KEY=%064d\nFLEET_RECOVERY_ENCRYPTION_KEY=%064d\n' 1 1 2 > "$env_file"
  before="$(shasum -a 256 "$env_file")"
  if with_existing_volume assert_protected_fleet_environment "$env_file" test-volume >/dev/null 2>&1; then
    return 1
  fi
  after="$(shasum -a 256 "$env_file")"
  [[ "$before" == "$after" ]]
}

test_first_install_does_not_require_existing_env() {
  local env_file="$test_root/first-install.env"
  with_missing_volume assert_protected_fleet_environment "$env_file" test-volume
  [[ ! -e "$env_file" ]]
}

test_volume_inspect_error_fails_closed_when_volume_is_listed() (
  source "$ROOT_DIR/scripts/setup-lib.sh"
  docker() {
    if [[ "$1 $2" == "volume inspect" ]]; then
      return 55
    fi
    if [[ "$1 $2" == "volume ls" ]]; then
      printf 'test-volume\n'
      return 0
    fi
    return 56
  }
  local status=0
  fleet_volume_exists test-volume >/dev/null 2>&1 || status=$?
  [[ "$status" == "2" ]]
)

test_volume_absence_requires_successful_exact_listing() (
  source "$ROOT_DIR/scripts/setup-lib.sh"
  docker() {
    if [[ "$1 $2" == "volume inspect" ]]; then
      return 1
    fi
    if [[ "$1 $2" == "volume ls" ]]; then
      printf 'another-volume\n'
      return 0
    fi
    return 56
  }
  local status=0
  fleet_volume_exists test-volume >/dev/null 2>&1 || status=$?
  [[ "$status" == "1" ]]
)

write_valid_catalog() {
  local path="$1"
  printf '%s\n' \
    $'0.19.0\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tlocal/hermes-fleet-runtime:0.19.0-aaaaaaaaaaaa\thttps://github.com/NousResearch/hermes-agent/releases/tag/v0.19.0' \
    $'0.18.2\tbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tlocal/hermes-fleet-runtime:0.18.2-bbbbbbbbbbbb\thttps://github.com/NousResearch/hermes-agent/releases/tag/v0.18.2' \
    $'0.18.1\tcccccccccccccccccccccccccccccccccccccccc\tlocal/hermes-fleet-runtime:0.18.1-cccccccccccc\thttps://github.com/NousResearch/hermes-agent/releases/tag/v0.18.1' \
    > "$path"
}

test_portable_catalog_validation() {
  local catalog="$test_root/releases.tsv"
  write_valid_catalog "$catalog"
  validate_release_catalog_tsv "$catalog"
  sed 's#local/hermes-fleet-runtime:0.18.2-bbbbbbbbbbbb#local/attacker:latest#' "$catalog" > "$catalog.invalid"
  if validate_release_catalog_tsv "$catalog.invalid" >/dev/null 2>&1; then
    return 1
  fi
}

file_inode() {
  local path="$1"

  if stat -f '%i' "$path" >/dev/null 2>&1; then
    stat -f '%i' "$path"
  else
    stat -c '%i' "$path"
  fi
}

test_bind_mounted_file_publication_preserves_existing_inode() {
  local staged="$test_root/catalog-staged.json"
  local published="$test_root/catalog-published.json"
  local before_inode=""

  printf 'old catalog\n' > "$published"
  printf 'new catalog\n' > "$staged"
  before_inode="$(file_inode "$published")"
  publish_bind_mounted_file "$staged" "$published" 600

  [[ "$(file_inode "$published")" == "$before_inode" ]]
  [[ "$(<"$published")" == "new catalog" ]]
  [[ ! -e "$staged" ]]
}

test_bind_mounted_file_publication_rejects_symlink_target() {
  local staged="$test_root/catalog-symlink-staged.json"
  local protected="$test_root/catalog-protected.json"
  local published="$test_root/catalog-symlink.json"

  printf 'protected\n' > "$protected"
  printf 'new catalog\n' > "$staged"
  ln -s "$protected" "$published"
  if publish_bind_mounted_file "$staged" "$published" 600 >/dev/null 2>&1; then
    return 1
  fi
  [[ "$(<"$protected")" == "protected" ]]
  [[ -e "$staged" ]]
}

write_valid_catalog_json() {
  local path="$1"
  cat > "$path" <<'EOF'
{
  "source": "NousResearch/hermes-agent GitHub Releases",
  "checked_at": "2026-07-27T00:00:00Z",
  "releases": [
    {
      "version": "0.19.0",
      "tag": "v0.19.0",
      "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "image": "local/hermes-fleet-runtime:0.19.0-aaaaaaaaaaaa",
      "url": "https://github.com/NousResearch/hermes-agent/releases/tag/v0.19.0",
      "published_at": "2026-07-26T00:00:00Z"
    },
    {
      "version": "0.18.2",
      "tag": "v0.18.2",
      "commit": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "image": "local/hermes-fleet-runtime:0.18.2-bbbbbbbbbbbb",
      "url": "https://github.com/NousResearch/hermes-agent/releases/tag/v0.18.2",
      "published_at": "2026-07-25T00:00:00Z"
    },
    {
      "version": "0.18.1",
      "tag": "v0.18.1",
      "commit": "cccccccccccccccccccccccccccccccccccccccc",
      "image": "local/hermes-fleet-runtime:0.18.1-cccccccccccc",
      "url": "https://github.com/NousResearch/hermes-agent/releases/tag/v0.18.1",
      "published_at": "2026-07-24T00:00:00Z"
    }
  ]
}
EOF
}

test_portable_catalog_json_conversion_does_not_need_helper() {
  local catalog="$test_root/releases.json"
  local output="$test_root/releases-from-json.tsv"
  local expected="$test_root/releases-expected.tsv"
  write_valid_catalog_json "$catalog"
  write_valid_catalog "$expected"
  portable_catalog_json_to_tsv "$catalog" "$output"
  cmp -s "$expected" "$output"
}

test_portable_catalog_json_conversion_accepts_minified_json() {
  local catalog="$test_root/releases-pretty.json"
  local minified="$test_root/releases-minified.json"
  local output="$test_root/releases-minified.tsv"
  local expected="$test_root/releases-minified-expected.tsv"
  write_valid_catalog_json "$catalog"
  python3 - "$catalog" "$minified" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    value = json.load(source)
with open(sys.argv[2], "w", encoding="utf-8") as destination:
    json.dump(value, destination, separators=(",", ":"))
PY
  write_valid_catalog "$expected"
  portable_catalog_json_to_tsv "$minified" "$output"
  cmp -s "$expected" "$output"
}

test_portable_catalog_json_conversion_rejects_wrong_source() {
  local catalog="$test_root/releases-wrong-source.json"
  local output="$test_root/releases-wrong-source.tsv"
  write_valid_catalog_json "$catalog"
  sed 's#NousResearch/hermes-agent GitHub Releases#untrusted feed#' "$catalog" > "$catalog.invalid"
  if portable_catalog_json_to_tsv "$catalog.invalid" "$output" >/dev/null 2>&1; then
    return 1
  fi
  [[ ! -e "$output" ]]
}

write_catalog_json_variant() {
  local source="$1"
  local destination="$2"
  local variant="$3"
  python3 - "$source" "$destination" "$variant" <<'PY'
import json
import sys

source_path, destination_path, variant = sys.argv[1:4]
with open(source_path, encoding="utf-8") as source:
    catalog = json.load(source)

if variant == "date-only":
    catalog["checked_at"] = "2026-07-27"
elif variant == "naive":
    catalog["releases"][0]["published_at"] = "2026-07-26T00:00:00"
elif variant == "timestamp-trailing":
    catalog["checked_at"] = "2026-07-27T00:00:00Z trailing"
elif variant == "malformed-timestamp":
    catalog["releases"][0]["published_at"] = "2026-13-26T00:00:00Z"
elif variant == "two-releases":
    catalog["releases"] = catalog["releases"][:2]
else:
    raise ValueError(f"unknown catalog fixture variant: {variant}")

with open(destination_path, "w", encoding="utf-8") as destination:
    json.dump(catalog, destination, separators=(",", ":"))
PY
}

assert_portable_catalog_json_rejected() {
  local input_path="$1"
  local output_path="$2"
  rm -f "$output_path"
  if portable_catalog_json_to_tsv "$input_path" "$output_path" >/dev/null 2>&1; then
    return 1
  fi
  [[ ! -e "$output_path" ]]
}

test_portable_catalog_json_conversion_matches_loader_rejections() {
  local valid="$test_root/releases-parity-valid.json"
  local output="$test_root/releases-parity.tsv"
  local variant
  write_valid_catalog_json "$valid"

  for variant in date-only naive timestamp-trailing malformed-timestamp two-releases; do
    local fixture="$test_root/releases-parity-$variant.json"
    write_catalog_json_variant "$valid" "$fixture" "$variant"
    assert_portable_catalog_json_rejected "$fixture" "$output"
  done

  local trailing="$test_root/releases-parity-trailing.json"
  cp "$valid" "$trailing"
  printf '%s' '{"unexpected":true}' >> "$trailing"
  assert_portable_catalog_json_rejected "$trailing" "$output"

  local oversized="$test_root/releases-parity-oversized.json"
  python3 - "$oversized" <<'PY'
import sys

with open(sys.argv[1], "wb") as destination:
    destination.write(b" " * ((1 << 20) + 1))
PY
  assert_portable_catalog_json_rejected "$oversized" "$output"
}

killed_builder() {
  local output="$2"
  printf '#!/bin/sh\nkill -9 $$\n' > "$output"
  chmod 700 "$output"
}

healthy_builder() {
  local output="$2"
  printf '#!/bin/sh\n[ "$1" = "--self-test" ]\n' > "$output"
  chmod 700 "$output"
}

test_killed_candidate_preserves_verified_helper() {
  local helper="$test_root/catalog-helper"
  printf '#!/bin/sh\nexit 0\n' > "$helper"
  chmod 700 "$helper"
  local before
  before="$(shasum -a 256 "$helper")"
  if install_catalog_helper_candidate "$helper" killed_builder >/dev/null 2>&1; then
    return 1
  fi
  [[ "$before" == "$(shasum -a 256 "$helper")" ]]
}

test_healthy_candidate_replaces_helper() {
  local helper="$test_root/catalog-helper-success"
  printf '#!/bin/sh\nexit 9\n' > "$helper"
  chmod 700 "$helper"
  install_catalog_helper_candidate "$helper" healthy_builder
  "$helper" --self-test
}

test_runtime_image_plan_builds_only_when_tag_is_absent() {
  [[ "$(runtime_image_plan local/runtime:test "" "" "" "" 0.19.0 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa build-id)" == "build" ]]
}

test_runtime_image_plan_reuses_matching_protected_tag() {
  [[ "$(runtime_image_plan \
    local/runtime:test \
    sha256:known \
    0.19.0 \
    aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    build-id \
    0.19.0 \
    aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    build-id)" == "reuse" ]]
}

test_runtime_image_plan_rejects_conflicting_protected_tag() {
  if runtime_image_plan \
    local/runtime:test \
    sha256:conflict \
    0.19.0 \
    aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    different-build \
    0.19.0 \
    aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    build-id >/dev/null 2>&1; then
    return 1
  fi
}

test_fleet_data_volume_name_cannot_be_overridden() {
  FLEET_DATA_VOLUME_NAME=another-volume bash -c \
    'source "$1/scripts/setup-lib.sh"; [[ "$fleet_data_volume_name" == "hermes-fleet-manager-data" ]]' \
    _ "$ROOT_DIR"
}

run_test existing_volume_rejects_missing_env test_existing_volume_rejects_missing_env
run_test existing_volume_rejects_invalid_keys_without_mutation test_existing_volume_rejects_invalid_keys_without_mutation
run_test existing_volume_accepts_valid_keys test_existing_volume_accepts_valid_keys
run_test existing_volume_rejects_duplicate_keys_without_mutation test_existing_volume_rejects_duplicate_keys_without_mutation
run_test first_install_does_not_require_existing_env test_first_install_does_not_require_existing_env
run_test volume_inspect_error_fails_closed_when_volume_is_listed test_volume_inspect_error_fails_closed_when_volume_is_listed
run_test volume_absence_requires_successful_exact_listing test_volume_absence_requires_successful_exact_listing
run_test portable_catalog_validation test_portable_catalog_validation
run_test bind_mounted_file_publication_preserves_existing_inode test_bind_mounted_file_publication_preserves_existing_inode
run_test bind_mounted_file_publication_rejects_symlink_target test_bind_mounted_file_publication_rejects_symlink_target
run_test portable_catalog_json_conversion_does_not_need_helper test_portable_catalog_json_conversion_does_not_need_helper
run_test portable_catalog_json_conversion_accepts_minified_json test_portable_catalog_json_conversion_accepts_minified_json
run_test portable_catalog_json_conversion_rejects_wrong_source test_portable_catalog_json_conversion_rejects_wrong_source
run_test portable_catalog_json_conversion_matches_loader_rejections test_portable_catalog_json_conversion_matches_loader_rejections
run_test killed_candidate_preserves_verified_helper test_killed_candidate_preserves_verified_helper
run_test healthy_candidate_replaces_helper test_healthy_candidate_replaces_helper
run_test runtime_image_plan_builds_only_when_tag_is_absent test_runtime_image_plan_builds_only_when_tag_is_absent
run_test runtime_image_plan_reuses_matching_protected_tag test_runtime_image_plan_reuses_matching_protected_tag
run_test runtime_image_plan_rejects_conflicting_protected_tag test_runtime_image_plan_rejects_conflicting_protected_tag
run_test fleet_data_volume_name_cannot_be_overridden test_fleet_data_volume_name_cannot_be_overridden

echo "setup-lib regression tests passed ($tests_run tests)."
