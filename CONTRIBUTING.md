# Contributing

## Scope

Fleet Manager provisions **new**, isolated Hermes instances. Do not add
discovery, import, or mutation of existing unmanaged Hermes deployments.

Keep the local control plane bound to `127.0.0.1` unless the change is a
reviewed remote-access design.

## Development

Requirements match [README.md](README.md): Docker Desktop or Engine with
Compose, Go 1.22+, OpenSSL, and Python 3. The web UI uses Node 24.

```sh
make test
go vet ./...
docker compose config --quiet
cd web && npm ci && npm run lint && npm run build
```

Before proposing a deployable change, also run a live `/healthz` check and
confirm existing managed instances stay running.

## Patches

- Use English for code, comments, tests, commits, PRs, issues, and docs.
- Do not commit `.env`, `.state/`, `bin/`, databases, backups, or Host Agent
  enrollment files.
- Prefer small, reviewable PRs. Lease fencing, encryption-at-rest, and
  allowlisted Host Agent jobs are invariants, not optional style.
- Add or update tests with the behavior change.

## Reporting bugs

Use GitHub issues for functional defects. Use [SECURITY.md](SECURITY.md) for
anything that can leak tokens, provider keys, or instance data.
