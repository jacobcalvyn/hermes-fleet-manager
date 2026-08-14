#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"
export FLEET_VULN_CHECK_NONCE="$(date -u +%Y%m%dT%H%M%SZ)"

fleet_setup_action="${FLEET_SETUP_ACTION:-}"
case "$fleet_setup_action" in
  bootstrap|upgrade) ;;
  *)
    cat >&2 <<'EOF'
scripts/fleet-maintenance.sh is internal and requires an explicit action.
Use scripts/fleet-bootstrap.sh for the first installation or
scripts/fleet-upgrade.sh for an existing Fleet installation.
EOF
    exit 2
    ;;
esac

source "$ROOT_DIR/scripts/setup-lock.sh"
configure_setup_lock "/tmp/hermes-fleet-manager-data.maintenance.lock"
host_tools_output_dir=""
host_tools_image=""
host_tools_container=""
cleanup_fleet_maintenance() {
  if [[ -n "$host_tools_container" ]]; then
    docker rm -f "$host_tools_container" >/dev/null 2>&1 || true
  fi
  if [[ -n "$host_tools_image" ]]; then
    docker image rm "$host_tools_image" >/dev/null 2>&1 || true
  fi
  if [[ -n "$host_tools_output_dir" && -d "$host_tools_output_dir" ]]; then
    rm -rf "$host_tools_output_dir"
  fi
  release_setup_lock
}
trap cleanup_fleet_maintenance EXIT
acquire_setup_lock

source "$ROOT_DIR/scripts/assert-compose-owner.sh"
source "$ROOT_DIR/scripts/host-agent-bootstrap.sh"
source "$ROOT_DIR/scripts/fleet-edge-network.sh"
source "$ROOT_DIR/scripts/control-plane-upgrade.sh"
assert_compose_owner "$ROOT_DIR"
assert_protected_fleet_environment "$ROOT_DIR/.env" "$fleet_data_volume_name"

if [[ "$fleet_setup_action" == "upgrade" && ! -f .env ]]; then
  echo "Fleet upgrade requires the existing .env; restore it before retrying." >&2
  exit 1
fi
if [[ "$fleet_setup_action" == "bootstrap" ]] && docker inspect hermes-fleet-control-plane >/dev/null 2>&1; then
  echo "Fleet is already installed; use scripts/fleet-upgrade.sh." >&2
  exit 1
fi
if [[ ! -f .env ]]; then
	cp .env.example .env
fi

set_env_value() {
  local key="$1"
  local value="$2"
  local temporary
  temporary="$(mktemp)"
  awk -v key="$key" -v value="$value" \
    'BEGIN { found=0 } $0 ~ "^" key "=" { if (!found) print key "=" value; found=1; next } { print } END { if (!found) print key "=" value }' \
    .env > "$temporary"
  mv "$temporary" .env
  chmod 600 .env
}

remove_env_value() {
  local key="$1"
  local temporary
  temporary="$(mktemp)"
  awk -F= -v key="$key" '$1 != key { print }' .env > "$temporary"
  mv "$temporary" .env
  chmod 600 .env
}

read_env_value() {
  local key="$1"
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' .env
}

build_go_binary() {
  local argument=""
  local output_path=""
  local package_path=""
  local read_output_path=0

  for argument in "$@"; do
    package_path="$argument"
    if [[ "$read_output_path" == "1" ]]; then
      output_path="$argument"
      read_output_path=0
      continue
    fi
    if [[ "$argument" == "-o" ]]; then
      read_output_path=1
    fi
  done
  if [[ -z "$output_path" ]]; then
    echo "Fleet Go builds must declare their output with -o." >&2
    return 1
  fi

  if [[ "$(uname -s)" == "Darwin" ]]; then
    CGO_ENABLED=1 go build -trimpath -ldflags=-linkmode=external "$@"
    /usr/bin/codesign --force --sign - "$output_path"
    /usr/bin/codesign --verify --strict "$output_path"
    return
  fi

  if [[ -z "$host_tools_output_dir" ]]; then
    host_tools_output_dir="$(mktemp -d)"
    host_tools_image="local/hermes-fleet-host-tools:$(printf '%s' "$FLEET_VULN_CHECK_NONCE" | tr '[:upper:]' '[:lower:]')"
    if ! docker build \
      --build-arg "FLEET_VULN_CHECK_NONCE=$FLEET_VULN_CHECK_NONCE" \
      --target host-tools \
      -t "$host_tools_image" \
      .; then
      echo "Supported-toolchain Fleet host-tools build failed." >&2
      return 1
    fi
    if ! host_tools_container="$(docker create "$host_tools_image" /bin/true)"; then
      echo "Supported-toolchain Fleet host-tools container could not be created." >&2
      return 1
    fi
    local exported_host_tool=""
    for exported_host_tool in hermes-fleet-agent fleet-upgrade-guard hermes-release-catalog; do
      if ! docker cp "$host_tools_container:/$exported_host_tool" "$host_tools_output_dir/$exported_host_tool"; then
        echo "Supported-toolchain Fleet host tool could not be exported: $exported_host_tool." >&2
        return 1
      fi
    done
    docker rm "$host_tools_container" >/dev/null
    host_tools_container=""
    docker image rm "$host_tools_image" >/dev/null
    host_tools_image=""
  fi
  local exported_binary=""
  case "$package_path" in
    ./cmd/host-agent) exported_binary="hermes-fleet-agent" ;;
    ./cmd/fleet-upgrade-guard) exported_binary="fleet-upgrade-guard" ;;
    ./cmd/hermes-release-catalog) exported_binary="hermes-release-catalog" ;;
    *)
      echo "Unsupported containerized Fleet build target: $package_path" >&2
      return 1
      ;;
  esac
  install -m 0755 "$host_tools_output_dir/$exported_binary" "$output_path"
}

build_managed_host_binaries() {
  build_go_binary -o bin/hermes-fleet-agent ./cmd/host-agent
  build_go_binary -o .state/fleet-upgrade-guard ./cmd/fleet-upgrade-guard
}

admin_token="$(read_env_value FLEET_ADMIN_TOKEN)"
enrollment_token="$(read_env_value FLEET_ENROLLMENT_TOKEN)"
secret_encryption_key="$(read_env_value FLEET_SECRET_ENCRYPTION_KEY)"
recovery_encryption_key="$(read_env_value FLEET_RECOVERY_ENCRYPTION_KEY)"
assert_fleet_marker_keys "$ROOT_DIR" "$secret_encryption_key" "$recovery_encryption_key"
for removed_key in \
  FLEET_HERMES_VERSION \
  FLEET_HERMES_REF \
  FLEET_DEFAULT_HERMES_IMAGE \
  FLEET_CLOUDFLARE_ACCOUNT_ID \
  FLEET_CLOUDFLARE_ZONE_ID \
  FLEET_CLOUDFLARE_ADMIN_API_TOKEN \
  FLEET_CLOUDFLARE_INSTANCES_API_TOKEN \
  FLEET_CLOUDFLARE_ADMIN_TUNNEL_ID \
  FLEET_CLOUDFLARE_INSTANCES_TUNNEL_ID \
  FLEET_CLOUDFLARE_ADMIN_HOSTNAME \
  FLEET_CLOUDFLARE_INSTANCE_DOMAIN \
  FLEET_CLOUDFLARE_ADMIN_ACCESS_TEAM \
  FLEET_CLOUDFLARE_ADMIN_ACCESS_AUD \
  FLEET_CLOUDFLARE_INSTANCES_ACCESS_TEAM \
  FLEET_CLOUDFLARE_INSTANCES_ACCESS_AUD; do
  remove_env_value "$removed_key"
done
if [[ "$fleet_setup_action" == "upgrade" ]] && {
  [[ ${#admin_token} -lt 32 ]] ||
  [[ ${#enrollment_token} -lt 32 ]] ||
  [[ ! "$secret_encryption_key" =~ ^[0-9a-fA-F]{64}$ ]] ||
  [[ ! "$recovery_encryption_key" =~ ^[0-9a-fA-F]{64}$ ]];
}; then
  echo "Fleet upgrade requires the existing valid credentials and encryption keys; restore .env before retrying." >&2
  exit 1
fi
if [[ ${#admin_token} -lt 32 ]]; then
  admin_token="$(openssl rand -hex 32)"
fi
if [[ ${#enrollment_token} -lt 32 ]]; then
  enrollment_token="$(openssl rand -hex 32)"
fi
if [[ ! "$secret_encryption_key" =~ ^[0-9a-fA-F]{64}$ ]]; then
  secret_encryption_key="$(openssl rand -hex 32)"
fi
if [[ ! "$recovery_encryption_key" =~ ^[0-9a-fA-F]{64}$ ]]; then
  recovery_encryption_key="$(openssl rand -hex 32)"
fi
set_env_value FLEET_ADMIN_TOKEN "$admin_token"
set_env_value FLEET_ENROLLMENT_TOKEN "$enrollment_token"
set_env_value FLEET_SECRET_ENCRYPTION_KEY "$secret_encryption_key"
set_env_value FLEET_RECOVERY_ENCRYPTION_KEY "$recovery_encryption_key"

host_name="$(read_env_value FLEET_HOST_NAME)"
control_plane_url="$(read_env_value FLEET_CONTROL_PLANE_URL)"

catalog_file="$ROOT_DIR/.state/hermes-releases.json"
catalog_tsv_file="$ROOT_DIR/.state/hermes-releases.tsv"
catalog_temp=""
catalog_tsv_temp="$(mktemp)"
catalog_helper="$ROOT_DIR/.state/hermes-release-catalog"
catalog_helper_ready=0
catalog_fetch_status=1
publish_catalog=0
if [[ "$fleet_setup_action" == "bootstrap" ]]; then
  catalog_temp="$(mktemp)"
  if install_catalog_helper_candidate "$catalog_helper" build_go_binary; then
    catalog_helper_ready=1
  else
    echo "The new Hermes release helper failed its execution check; the previous helper and release cache were preserved." >&2
  fi
  if [[ "$catalog_helper_ready" == "1" ]]; then
    if "$catalog_helper" --output "$catalog_temp"; then
      catalog_fetch_status=0
    else
      catalog_fetch_status=$?
    fi
  fi
  if [[ "$catalog_fetch_status" == "0" ]] \
    && portable_catalog_json_to_tsv "$catalog_temp" "$catalog_tsv_temp"; then
    publish_catalog=1
  else
    rm -f "$catalog_temp" "$catalog_tsv_temp"
    catalog_tsv_temp="$(mktemp)"
    if [[ ! -s "$catalog_file" ]] \
      || ! portable_catalog_json_to_tsv "$catalog_file" "$catalog_tsv_temp"; then
      rm -f "$catalog_tsv_temp"
      echo "The official Hermes release check failed and no independently readable validated cache exists." >&2
      exit 1
    fi
    echo "Official Hermes release check failed; using the previously validated portable catalog cache." >&2
  fi
else
  if [[ ! -s "$catalog_file" ]] \
    || ! portable_catalog_json_to_tsv "$catalog_file" "$catalog_tsv_temp"; then
    rm -f "$catalog_tsv_temp"
    echo "Fleet upgrade requires the existing validated Hermes release cache; run bootstrap only for a new installation." >&2
    exit 1
  fi
fi
if [[ "$publish_catalog" == "1" ]]; then
  # The catalog contains public release metadata and is bind-mounted into a
  # non-root container. It must be host-readable on native Linux; secrets and
  # credentials remain in their dedicated mode-0600 stores.
  publish_bind_mounted_file "$catalog_temp" "$catalog_file" 644
fi
if [[ ! -f "$catalog_file" ]] || [[ -L "$catalog_file" ]]; then
  echo "The validated Hermes release catalog must be a regular file." >&2
  exit 1
fi
chmod 644 "$catalog_file"
mv "$catalog_tsv_temp" "$catalog_tsv_file"
chmod 600 "$catalog_tsv_file"
release_catalog_fingerprint="$(release_catalog_fingerprint_tsv "$catalog_tsv_file")"
if [[ ! "$release_catalog_fingerprint" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Hermes release catalog fingerprint could not be calculated." >&2
  exit 1
fi
set_env_value FLEET_HERMES_RELEASE_CATALOG_FINGERPRINT "$release_catalog_fingerprint"

mkdir -p bin .state .state/cloudflared-credentials
chmod 700 .state/cloudflared-credentials
build_managed_host_binaries

fleet_build_id="$(
  {
    find cmd internal runtime web/src -type f -print
    printf '%s\n' Dockerfile Dockerfile.cloudflare-connector docker-compose.yml go.mod go.sum web/package.json web/package-lock.json
  } \
    | LC_ALL=C sort -u \
    | xargs shasum -a 256 \
    | shasum -a 256 \
    | awk '{print substr($1, 1, 16)}'
)"
if [[ ! "$fleet_build_id" =~ ^[0-9a-f]{16}$ ]]; then
  echo "Hermes Fleet Manager build identity could not be calculated." >&2
  exit 1
fi
set_env_value FLEET_BUILD_ID "$fleet_build_id"

# Revalidate the shared ownership/key boundary immediately before the first
# control-plane volume mutation. The host-wide lock prevents cooperating
# checkouts from passing this fence concurrently.
assert_compose_owner "$ROOT_DIR"
assert_protected_fleet_environment "$ROOT_DIR/.env" "$fleet_data_volume_name"
assert_fleet_marker_keys "$ROOT_DIR" "$secret_encryption_key" "$recovery_encryption_key"
ensure_fleet_edge_network
upgrade_control_plane "$ROOT_DIR" "$control_plane_url" "$admin_token" "$fleet_build_id" "$ROOT_DIR/.state/fleet-upgrade-guard"
write_fleet_owner_marker "$ROOT_DIR" "$secret_encryption_key" "$recovery_encryption_key"

agent_config="${FLEET_AGENT_CONFIG_PATH:-${HOME}/.config/hermes-fleet/agent.json}"
managed_root="${FLEET_MANAGED_ROOT:-${HOME}/.local/share/hermes-fleet/instances}"
ensure_host_agent_config \
  "$ROOT_DIR/bin/hermes-fleet-agent" \
  "$agent_config" \
  "$control_plane_url" \
  "$enrollment_token" \
  "$admin_token" \
  "$host_name" \
  "$managed_root"

if ! bin/hermes-fleet-agent probe \
  --config "$agent_config" \
  --url "$control_plane_url" \
  --name "$host_name" \
  --managed-root "$managed_root"; then
  echo "Host Agent config is stale or is not accepted by this Fleet Manager." >&2
  echo "Re-enroll the Host Agent explicitly before retrying setup; the existing credential was preserved." >&2
  exit 1
fi

host_agent_supervisor="${FLEET_HOST_AGENT_SUPERVISOR:-auto}"
case "$host_agent_supervisor" in
  auto)
    if [[ "$(uname -s)" == "Darwin" ]]; then
      ./scripts/install-launchd-agent.sh
    else
      if [[ -f .state/host-agent.pid ]] && kill -0 "$(cat .state/host-agent.pid)" 2>/dev/null; then
        echo "Host Agent is already running."
      else
        nohup bin/hermes-fleet-agent run \
          --config "$agent_config" \
          --log-path "$ROOT_DIR/.state/host-agent.log" \
          --log-max-bytes 26214400 \
          --log-max-files 4 \
          >/dev/null 2>&1 &
        echo $! > .state/host-agent.pid
      fi
    fi
    ;;
  launchd)
    if [[ "$(uname -s)" != "Darwin" ]]; then
      echo "The launchd supervisor is available only on macOS." >&2
      exit 1
    fi
    ./scripts/install-launchd-agent.sh
    ;;
  systemd)
    if [[ "$(uname -s)" != "Linux" ]]; then
      echo "The systemd supervisor is available only on Linux." >&2
      exit 1
    fi
    ./scripts/install-systemd-agent.sh
    ;;
  none)
    echo "Host Agent installation was skipped by FLEET_HOST_AGENT_SUPERVISOR=none."
    ;;
  *)
    echo "FLEET_HOST_AGENT_SUPERVISOR must be auto, launchd, systemd, or none." >&2
    exit 1
    ;;
esac

echo "Hermes Fleet Manager is ready at http://127.0.0.1:9180"
echo "Use FLEET_ADMIN_TOKEN from .env to sign in."
