#!/usr/bin/env bash

fleet_edge_network_name="hermes-fleet-edge"
fleet_edge_network_label="io.hermes-fleet.edge-network"

ensure_fleet_edge_network() {
  local existing_label=""
  local existing_internal=""
  if docker network inspect "$fleet_edge_network_name" >/dev/null 2>&1; then
    existing_label="$(docker network inspect --format '{{ index .Labels "io.hermes-fleet.edge-network" }}' "$fleet_edge_network_name")"
    if [[ "$existing_label" != "true" ]]; then
      echo "Docker network $fleet_edge_network_name already exists but is not owned by Hermes Fleet." >&2
      return 1
    fi
    existing_internal="$(docker network inspect --format '{{ .Internal }}' "$fleet_edge_network_name")"
    if [[ "$existing_internal" != "true" ]]; then
      echo "Docker network $fleet_edge_network_name exists without the required internal-only boundary." >&2
      echo "Disconnect its containers and recreate the network through Hermes Fleet before enabling Cloudflare." >&2
      return 1
    fi
    return 0
  fi
  docker network create --internal --label "$fleet_edge_network_label=true" "$fleet_edge_network_name" >/dev/null
}
