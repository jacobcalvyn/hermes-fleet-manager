#!/usr/bin/env bash

fleet_data_volume_name="hermes-fleet-manager-data"

fleet_env_value() {
  local env_file="$1"
  local key="$2"
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$env_file"
}

fleet_env_key_count() {
  local env_file="$1"
  local key="$2"
  awk -F= -v key="$key" '$1 == key { count++ } END { print count + 0 }' "$env_file"
}

fleet_valid_encryption_key() {
  [[ "$1" =~ ^[0-9a-fA-F]{64}$ ]]
}

fleet_sha256_text() {
  printf '%s' "$1" | shasum -a 256 | awk '{print $1}'
}

fleet_key_fingerprint() {
  local normalized
  normalized="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  fleet_sha256_text "$normalized"
}

fleet_volume_exists() {
  local volume_name="${1:-$fleet_data_volume_name}"
  local volumes=""

  if docker volume inspect "$volume_name" >/dev/null 2>&1; then
    return 0
  fi
  if ! volumes="$(docker volume ls --format '{{.Name}}' 2>/dev/null)"; then
    echo "Docker is unavailable; persistent Fleet state cannot be inspected safely." >&2
    return 2
  fi
  if grep -Fxq "$volume_name" <<< "$volumes"; then
    echo "Docker listed the persistent Fleet volume, but its metadata could not be inspected safely." >&2
    return 2
  fi
  return 1
}

assert_protected_fleet_environment() {
  local env_file="$1"
  local volume_name="${2:-$fleet_data_volume_name}"
  local volume_status=0
  local secret_key=""
  local recovery_key=""

  if fleet_volume_exists "$volume_name"; then
    volume_status=0
  else
    volume_status=$?
  fi
  if [[ "$volume_status" == "2" ]]; then
    return 1
  fi
  if [[ "$volume_status" == "1" ]]; then
    return 0
  fi
  if [[ ! -f "$env_file" ]]; then
    cat >&2 <<EOF
Refusing to create a new .env while the persistent Fleet volume exists:
  $volume_name

Restore the original .env before running setup. Generating new encryption keys
over existing Fleet state would make stored secrets and recovery points
unreadable.
EOF
    return 1
  fi

  secret_key="$(fleet_env_value "$env_file" FLEET_SECRET_ENCRYPTION_KEY)"
  recovery_key="$(fleet_env_value "$env_file" FLEET_RECOVERY_ENCRYPTION_KEY)"
  if [[ "$(fleet_env_key_count "$env_file" FLEET_SECRET_ENCRYPTION_KEY)" != "1" ]] \
    || [[ "$(fleet_env_key_count "$env_file" FLEET_RECOVERY_ENCRYPTION_KEY)" != "1" ]] \
    || ! fleet_valid_encryption_key "$secret_key" \
    || ! fleet_valid_encryption_key "$recovery_key"; then
    cat >&2 <<EOF
Refusing to replace missing, duplicated, or invalid encryption keys while the
persistent Fleet volume exists:
  $volume_name

Restore FLEET_SECRET_ENCRYPTION_KEY and FLEET_RECOVERY_ENCRYPTION_KEY from the
original .env. Setup did not modify the file.
EOF
    return 1
  fi
}

version_is_older() {
  local left="$1"
  local right="$2"
  local left_major left_minor left_patch
  local right_major right_minor right_patch

  IFS=. read -r left_major left_minor left_patch <<< "$left"
  IFS=. read -r right_major right_minor right_patch <<< "$right"
  if ((10#$left_major != 10#$right_major)); then
    ((10#$left_major < 10#$right_major))
    return
  fi
  if ((10#$left_minor != 10#$right_minor)); then
    ((10#$left_minor < 10#$right_minor))
    return
  fi
  ((10#$left_patch < 10#$right_patch))
}

validate_release_catalog_tsv() {
  local path="$1"
  local count=0
  local previous_version=""
  local seen_versions="|"
  local version commit image release_url extra

  if [[ ! -s "$path" ]]; then
    echo "The portable Hermes release cache is missing or empty." >&2
    return 1
  fi
  while IFS=$'\t' read -r version commit image release_url extra || [[ -n "$version$commit$image$release_url$extra" ]]; do
    count=$((count + 1))
    if [[ -n "$extra" ]] \
      || [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      || [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]] \
      || [[ "$image" != "local/hermes-fleet-runtime:${version}-${commit:0:12}" ]] \
      || [[ "$release_url" != https://github.com/NousResearch/hermes-agent/releases/* ]] \
      || [[ "$seen_versions" == *"|$version|"* ]]; then
      echo "The portable Hermes release cache contains an invalid entry." >&2
      return 1
    fi
    if [[ -n "$previous_version" ]] && ! version_is_older "$version" "$previous_version"; then
      echo "The portable Hermes release cache is not ordered newest first." >&2
      return 1
    fi
    seen_versions="${seen_versions}${version}|"
    previous_version="$version"
  done < "$path"
  if [[ "$count" -ne 3 ]]; then
    echo "The portable Hermes release cache must contain exactly three installable releases." >&2
    return 1
  fi
}

release_catalog_fingerprint_tsv() {
  local path="$1"

  if ! validate_release_catalog_tsv "$path"; then
    return 1
  fi
  LC_ALL=C awk -F $'\t' 'BEGIN { OFS="\t" } { print $1, $2, $3 }' "$path" \
    | shasum -a 256 \
    | awk '{print $1}'
}

publish_bind_mounted_file() {
  local source_path="$1"
  local target_path="$2"
  local mode="${3:-600}"

  if [[ ! -f "$source_path" ]] || [[ -L "$source_path" ]]; then
    echo "Refusing to publish a missing or non-regular staged file: $source_path" >&2
    return 1
  fi
  mkdir -p "$(dirname "$target_path")"
  if [[ -e "$target_path" ]] || [[ -L "$target_path" ]]; then
    if [[ ! -f "$target_path" ]] || [[ -L "$target_path" ]]; then
      echo "Refusing to replace a non-regular published file: $target_path" >&2
      return 1
    fi
    # Docker Desktop bind mounts follow the existing inode. Replacing the path
    # with mv(1) leaves the running container attached to an unlinked inode,
    # so update the existing regular file in place instead.
    if ! cp "$source_path" "$target_path" || ! cmp -s "$source_path" "$target_path"; then
      echo "The staged file could not be published completely: $target_path" >&2
      return 1
    fi
    rm -f "$source_path"
  else
    mv "$source_path" "$target_path"
  fi
  chmod "$mode" "$target_path"
}

portable_catalog_json_to_tsv() {
  local input_path="$1"
  local output_path="$2"

  if [[ ! -s "$input_path" ]]; then
    return 1
  fi
  if ! command -v python3 >/dev/null 2>&1; then
    echo "Python 3 is required to validate the portable Hermes release cache safely." >&2
    return 1
  fi
  if ! python3 - "$input_path" "$output_path" <<'PY'
import datetime
import json
import os
import re
import sys

input_path, output_path = sys.argv[1:3]
with open(input_path, encoding="utf-8") as handle:
    if os.fstat(handle.fileno()).st_size > 1 << 20:
        raise ValueError("catalog exceeds the size limit")
    catalog = json.load(handle)

if set(catalog) != {"source", "checked_at", "releases"}:
    raise ValueError("unexpected catalog fields")
if catalog["source"] != "NousResearch/hermes-agent GitHub Releases":
    raise ValueError("unexpected catalog source")

def timestamp(value):
    if not isinstance(value, str) or not re.fullmatch(
        r"[0-9]{4}-[0-9]{2}-[0-9]{2}T"
        r"[0-9]{2}:[0-9]{2}:[0-9]{2}"
        r"(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:[0-9]{2})",
        value,
    ):
        raise ValueError("timestamp must be RFC3339 with a timezone")
    normalized = value[:-1] + "+00:00" if value.endswith("Z") else value
    parsed = datetime.datetime.fromisoformat(normalized)
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise ValueError("timestamp must include a timezone")

timestamp(catalog["checked_at"])
releases = catalog["releases"]
if not isinstance(releases, list) or len(releases) != 3:
    raise ValueError("catalog must contain three releases")

rows = []
seen = set()
previous = None
expected_fields = {"version", "tag", "commit", "image", "url", "published_at"}
for release in releases:
    if not isinstance(release, dict) or set(release) != expected_fields:
        raise ValueError("unexpected release fields")
    version = release["version"]
    commit = release["commit"]
    if not isinstance(version, str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise ValueError("invalid release version")
    if not isinstance(release["tag"], str) or not release["tag"]:
        raise ValueError("invalid release tag")
    if not isinstance(commit, str) or not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("invalid release commit")
    if release["image"] != f"local/hermes-fleet-runtime:{version}-{commit[:12]}":
        raise ValueError("invalid portable image")
    if not isinstance(release["url"], str) or not release["url"].startswith(
        "https://github.com/NousResearch/hermes-agent/releases/"
    ):
        raise ValueError("invalid release URL")
    timestamp(release["published_at"])
    version_tuple = tuple(int(part) for part in version.split("."))
    if version in seen or (previous is not None and version_tuple >= previous):
        raise ValueError("release versions must be unique and newest first")
    seen.add(version)
    previous = version_tuple
    rows.append((version, commit, release["image"], release["url"]))

with open(output_path, "w", encoding="utf-8", newline="\n") as handle:
    for row in rows:
        handle.write("\t".join(row) + "\n")
PY
  then
    rm -f "$output_path"
    return 1
  fi
  if ! validate_release_catalog_tsv "$output_path"; then
    rm -f "$output_path"
    return 1
  fi
}

install_catalog_helper_candidate() {
  local target="$1"
  local builder="$2"
  local candidate

  mkdir -p "$(dirname "$target")"
  candidate="$(mktemp "${target}.candidate.XXXXXX")"
  rm -f "$candidate"
  if ! "$builder" -o "$candidate" ./cmd/hermes-release-catalog; then
    rm -f "$candidate"
    return 1
  fi
  if ! "$candidate" --self-test; then
    rm -f "$candidate"
    return 1
  fi
  chmod 700 "$candidate"
  mv "$candidate" "$target"
}

runtime_image_plan() {
  local image_ref="$1"
  local image_id="$2"
  local current_version="$3"
  local current_commit="$4"
  local current_build_id="$5"
  local expected_version="$6"
  local expected_commit="$7"
  local expected_build_id="$8"

  if [[ -z "$image_id" ]]; then
    printf 'build\n'
    return 0
  fi
  if [[ "$current_version" != "$expected_version" ]] \
    || [[ "$current_commit" != "$expected_commit" ]] \
    || [[ "$current_build_id" != "$expected_build_id" ]]; then
    cat >&2 <<EOF
Refusing to replace an existing protected Hermes runtime image tag:
  $image_ref

The tag resolves to image $image_id, but its identity labels do not match the
requested Hermes version, source commit, and runtime build. Remove or repair
the conflicting local image explicitly before running setup again. This fence
protects Fleet's source/build-qualified tag; it is not a registry signature or
a claim that local Docker tags cannot be changed outside Fleet.
EOF
    return 1
  fi
  printf 'reuse\n'
}
