# Security Policy

## Supported versions

Report vulnerabilities against the default `main` branch. There is no long-term
support train; operators are expected to run a current checkout.

## Reporting a vulnerability

Do not open a public issue for a security problem.

Use GitHub's private vulnerability reporting on this repository. Include:

- Affected commit or release
- Impact and a minimal reproduction
- Whether you believe secrets, operator tokens, or managed-instance data can
  leave the host

You should receive an acknowledgement within 7 days.

## Out of scope

- The upstream [Hermes Agent](https://github.com/NousResearch/hermes-agent)
  runtime, except Fleet's wrapper, image pinning, and job fencing around it
- Compromised operator workstations, leaked `.env` files, or Docker-group
  access on the host
- Findings that require binding the control plane beyond `127.0.0.1` without
  the documented remote-access path

## Operator secrets

Never commit `.env`, Host Agent enrollment state, Cloudflare connector tokens,
SQLite databases, or recovery artifacts. Bootstrap writes control-plane tokens
and encryption keys with user-only file permissions. Rotate `FLEET_ADMIN_TOKEN`,
`FLEET_ENROLLMENT_TOKEN`, and both encryption keys if they are ever exposed.
