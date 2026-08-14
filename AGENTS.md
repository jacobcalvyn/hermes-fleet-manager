# Agent Instructions

This project is the independent Hermes Fleet control plane and native Host
Agent. It provisions only new, isolated Fleet-owned Hermes instances.

## Boundaries

- Do not adopt or mutate the existing Nara, Sena, Raka, or Arka deployments.
- Keep control-plane state in the `hermes-fleet-manager-data` Docker volume.
- Preserve `.env`, Host Agent enrollment state, and managed instance data during
  upgrades.
- Keep provider API keys encrypted at rest. Codex OAuth remains per instance.
- Require lease fencing for Host Agent job acknowledgement, renewal, metadata,
  and completion.
- Keep the local control plane bound to `127.0.0.1` unless a reviewed remote
  access design is explicitly requested.

## Validation

Before deploying changes, run the Go tests and vet checks, frontend lint and
build, shell syntax checks, `docker compose config --quiet`, and a live
`/healthz` check. Verify the Host Agent version and confirm existing managed
instances remain running.
