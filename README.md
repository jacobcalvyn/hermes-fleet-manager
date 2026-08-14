# Hermes Fleet Manager

Hermes Fleet Manager is a local-first control plane that provisions new,
isolated Hermes Agent instances. Existing Nara, Sena, Raka, and Arka
deployments are outside its ownership and are never imported or modified.

## Components

```text
Docker:  Control Plane + Web UI + persistent SQLite
Native:  Host Agent with an allowlisted Docker Compose provisioner
Managed: One Compose project, data volume, workspace, and port pair per instance
```

The control plane is not in the chat data path. A managed Hermes instance keeps
running when Fleet Manager is unavailable.

Docker container names are operator-facing and deterministic:

```text
Fleet UI + API:      hermes-fleet-control-plane
Hermes runtime:      hermes-fleet-instance-<instance-name>-hermes
Hermes dashboard:    hermes-fleet-instance-<instance-name>-dashboard
```

Immutable instance IDs remain in Docker labels, Compose project names, managed
paths, and data-volume ownership metadata. They are not appended to the visible
container names.

## Chat Artifacts

Hermes remains the execution plane; Fleet owns the artifact plane. Hermes may
declare generated output on a standalone `FILE:/absolute/path` line, while
`MEDIA:/absolute/path` remains supported for images. These markers are only
candidate declarations. The native Host Agent independently validates the
owned container, allowlisted path, regular-file type, extension-specific file
content, size, and SHA-256 before an authenticated, lease-fenced upload.

The control plane stores one authoritative manifest per artifact in the
`hermes-fleet-manager-data` volume and emits normalized `ASSISTANT_ARTIFACT`
events. Artifact status is explicit: `preparing`, `ready`, `rejected`,
`missing`, or `expired`. The browser reads the current manifest before enabling
an authenticated download, so a retained chat event cannot present deleted or
expired content as ready.

Each file is limited to 25 MiB. Defaults additionally limit one chat session to
256 MiB, one managed instance to 1 GiB, and Fleet artifact storage to 2 GiB.
`FLEET_CHAT_ARTIFACT_SESSION_MAX_BYTES`,
`FLEET_CHAT_ARTIFACT_INSTANCE_MAX_BYTES`, and
`FLEET_CHAT_ARTIFACT_TOTAL_MAX_BYTES` configure those budgets. Artifacts expire
after 720 hours by default; `FLEET_CHAT_ARTIFACT_RETENTION_HOURS` configures the
retention period. Startup and 15-minute reconciliation remove abandoned staging
files, preserve terminal manifests, and mark missing or expired output
authoritatively. Deleting a chat session deletes its artifact directory.

## Local Setup

Requirements:

- Docker Desktop with Docker Compose
- Go 1.22 or newer
- OpenSSL
- Python 3

Run:

```sh
./scripts/fleet-bootstrap.sh
```

The bootstrap, upgrade, stop, and Makefile Docker commands refuse to operate when the active
`hermes-fleet-manager` Compose project belongs to a different checkout. This
prevents two source directories from replacing the same control-plane
container or persistent volume. Bootstrap and upgrade also use a host-wide lock for the fixed
`hermes-fleet-manager-data` volume. Bootstrap records a durable checkout and
encryption-key fingerprint inside the data volume, so the fence still applies
after `docker compose down`. A reviewed checkout relocation can bypass only the
path guard for one command with `FLEET_ALLOW_PROJECT_RELOCATION=1`; the
original encryption keys are still required.

If the data volume already exists, upgrade refuses to recreate a missing `.env`
or replace invalid encryption keys. Restore the original `.env` instead.
For an older installation that predates the durable volume marker, keep the
original owning control-plane container for one upgrade run. Fleet compares that
container's key fingerprints with `.env` before writing the marker. Neither a
checkout relocation nor `FLEET_ALLOW_PROJECT_RELOCATION=1` bypasses this check.
If both the marker and original owning container are gone, upgrade fails closed;
restore verified legacy container metadata before retrying.

Bootstrap performs the following operations without touching existing Hermes
deployments:

1. Generates local control-plane credentials and separate provider-secret and
   recovery-point encryption keys in `.env`.
2. Seeds the release catalog from the official `NousResearch/hermes-agent`
   GitHub Releases feed. Runtime images are built and verified by the Host Agent
   only when an operator creates or updates an instance.
3. Starts the control plane at `http://127.0.0.1:9180`.
4. Builds and enrolls the native Mac Host Agent.
5. Installs it as `io.hermes-fleet.host-agent` under macOS LaunchAgents.

On macOS 13 and later, bootstrap and upgrade also verify the Host Agent's
Background Task Management authorization through `SMAppService`. If macOS
requires approval, setup fails safely instead of reporting a reboot-persistent
installation. Enable `hermes-fleet-agent` in **System Settings > General >
Login Items & Extensions > Allow in Background**, then rerun the setup command.

Use `FLEET_ADMIN_TOKEN` from `.env` to sign in. Click an instance name to open
its profile. The profile contains endpoints, desired runtime configuration,
host and storage details, operation history, and an on-demand credential
reveal. Do not share or commit the admin token.

## VPS Setup

The production VPS target is Ubuntu or Debian with Docker Engine, the Docker
Compose plugin, and systemd. Copy a clean source tree to the VPS, then run as
root:

```sh
./scripts/setup-vps.sh
```

The VPS installer keeps the Fleet UI and API bound to `127.0.0.1:9180`,
installs only required OS packages, and installs the native Host Agent as the
`hermes-fleet-host-agent.service` system service. The service uses a dedicated
`hermes-fleet-agent` account, has Docker access, restarts after failure and
reboot, and stores managed instance data under
`/var/lib/hermes-fleet-agent/instances`. Membership in the Docker group is a
privileged trust boundary; only the allowlisted Fleet Host Agent is installed
under that account.

The same setup installs a bounded control-plane liveness watchdog. It confirms
three consecutive `/livez` failures before restarting only the existing
control-plane container, never restarts for a readiness-only degradation, and
stops after three recovery attempts in 15 minutes. Setup finishes with a
two-minute soak that checks `/livez`, `/readyz`, the authenticated work queue,
managed instance runtime observations, and unexpected Host Agent or container
restarts. Override the soak only for controlled testing with
`FLEET_VPS_SOAK_SECONDS`, `FLEET_VPS_SOAK_INTERVAL_SECONDS`, and
`FLEET_VPS_SOAK_REQUIRED_SUCCESSES`.

Before configuring a reviewed domain or remote-access provider, connect from an
operator workstation through SSH:

```sh
ssh -L 9180:127.0.0.1:9180 <user>@<vps-ip>
```

Then open `http://127.0.0.1:9180`. Do not expose port 9180 directly through the
VPS firewall. Cloudflare remote access remains optional and is configured from
the Fleet web interface after the loopback deployment is healthy.

## New Instance State

```text
Provider:         OpenAI Codex (per-instance OAuth)
Model:            Not configured
Reasoning:        Not configured
Service tier:     Not configured
Hermes version:   Selected during instance creation
Host ports:       Allocated automatically
```

After provisioning, the operator authenticates Codex from the instance's
**Codex** tab. Model, reasoning, and service tier are selected in a separate
second step and are persisted only after the Host Agent applies them
successfully. Authentication and configuration are independent states; an
older instance can therefore have saved configuration while its Codex session
requires sign-in again.

The **System** area is limited to Fleet Manager itself:

- **General** shows the Hermes Fleet Manager version, operator URL, state
  database path, and control-plane backup retention.
- **Remote access** reports and reconciles the optional Fleet-owned Cloudflare
  admin and instance-dashboard route boundaries.
- **Control-plane backups** contains only Fleet database backup operations.

Instance-owned information stays in the instance profile. The **Overview** tab
shows the observed Hermes version and its source commit, **Codex** separates
per-instance authentication from configuration, **Messaging** applies official
Telegram and WhatsApp channel settings, **MCP** installs bounded remote MCP
servers, and **Backups** is where the operator creates encrypted instance
backups and selects one to restore. System never acts as a container for
per-instance configuration.

The top-level **Policies** module manages global desired-state baselines without
turning System into another instance configuration page. The initial policy
surface supports the latest stable Hermes runtime, an explicit instance scope,
and either one-at-a-time or all-at-once delivery. Fleet previews each target as
compliant, drifted, or blocked before rollout. A rollout is recorded as a
control-plane operation; each actual Hermes update remains a lease-fenced child
operation with its own verified rollback backup. One-at-a-time rollouts resume
from durable target state after a Fleet Manager restart. Public hostnames,
Codex OAuth sessions, and unique secret values remain instance-owned and are
never copied into a policy.

The per-instance **Messaging** page supports the official Hermes Telegram bot
token, user/group allowlists, mention policy, and proxy settings, plus WhatsApp
Baileys mode, number allowlist, unauthorized-DM behavior, and reply prefix.
Fleet encrypts the desired configuration in its database. The Host Agent
downloads plaintext only while holding the active job lease, writes the
runtime-owned values to that instance's protected `.env` and `config.yaml`,
then recreates and health-checks a running instance. A stopped instance stays
stopped. WhatsApp QR pairing remains an explicit interactive Hermes step; Fleet
does not report a session as paired merely because its settings were applied.

The per-instance **MCP** page accepts only remote HTTPS MCP servers. Local
commands, `stdio` servers, package installation, resources, and prompts are not
exposed through Fleet. Sampling and server-initiated elicitation are disabled,
and every server is marked untrusted so write-capable tools remain behind the
Hermes approval boundary. Every enabled server requires an explicit tool
allowlist. Optional bearer tokens are encrypted in the control-plane database,
downloaded only by the Host Agent that holds the active job lease, and written
to the instance-owned `/data/.env`; `config.yaml` contains only the matching
environment-variable reference. Applying an MCP change is transactional: Fleet
snapshots the prior files, validates the effective configuration, restarts only
a running instance, tests each enabled server, and restores the snapshot if a
stage fails.

The Host Agent reports the Hermes source label inherited by each running or
stopped container image. Fleet therefore does not infer an instance version
from a mutable image tag. If the label is absent, the UI says that version
detection is pending instead of displaying a guessed version.

OAuth is intentionally not copied from another agent. Each instance owns its
own `/data` volume and must be authenticated separately after provisioning.
Start authentication from the instance **Codex** tab, then choose the model,
reasoning, and service tier there. Fleet runs the bounded
device flow through the native Host Agent, displays only OpenAI's verification
URL and one-time code, and never sends OAuth tokens through the browser or
control-plane database.

The instance dashboard username, password, and API key remain in that
instance's `.env`. An authenticated operator can request only these three
allowlisted values from the instance profile. The Host Agent reads them on
demand, the control plane stores only AES-256-GCM ciphertext bound to the
operation ID, and the reveal expires after five minutes. The UI keeps plaintext
only in browser memory and never loads the complete `.env` file.

## Optional Remote Access

Remote access is disabled by default. There are no `FLEET_CLOUDFLARE_*`
environment variables and no separate Cloudflare setup script. The normal
`./scripts/fleet-bootstrap.sh` installation includes two hardened connector
supervisors; both remain idle until an operator enables the module from
**System > Remote access**.

The web flow is the only configuration surface and offers two explicit
ownership modes:

1. **Cloudflare tunnels** keeps two trust boundaries. The Fleet Manager admin
   card accepts its pre-created tunnel token and public hostname; its published
   route remains manual in Cloudflare. **Instance publishing** combines the
   shared instance tunnel token, account ID, zone ID, and scoped API token in
   one connect-and-verify flow. Fleet extracts the tunnel UUID from the
   connector token instead of accepting a second tunnel identifier. It also
   resolves and stores the verified zone name as non-secret publishing
   metadata; there is no separate shared-domain input.
2. **Existing public endpoints** registers a Fleet Manager URL and any subset
   of managed instance dashboard URLs that are already published through
   Cloudflare, ngrok, Railway, or another provider. Fleet stores these mappings
   but never creates, verifies, changes, or removes resources at that provider.

Changing ownership mode requires disabling the active mode first. Fleet
encrypts both tunnel tokens and the Cloudflare API token in SQLite and never
returns them through the API. Access identities and account-wide origin
certificates remain outside Fleet.

Fleet deliberately keeps two trust boundaries:

```text
Admin tunnel:      adminhermesfleet.example.com -> control-plane:9180
Instances tunnel:  aksa.example.com -> hermes-fleet-instance-aksa-dashboard:9119
Fallback:          every unmatched hostname -> HTTP 404
```

For Cloudflare tunnel tokens, create two remotely managed tunnels outside
Fleet. In Cloudflare, configure the admin tunnel's published application route
to the exact Fleet Manager origin shown in its card
(`http://control-plane:9180`). For instance dashboards, connect the shared
tunnel and a least-privilege API token in **Instance publishing**, then assign
the exact hostname from **Instance > Access > Public dashboard**. For an
unconfigured instance, Fleet prefills an editable
`<instance-name>.<verified-zone>` suggestion but does not store or publish it
until the operator selects **Publish dashboard**. Fleet creates
or updates the owned CNAME and remotely managed tunnel ingress rule, verifies
both, and probes the public endpoint. Service URLs are derived from Fleet
topology and are read-only. Never publish an instance API endpoint.

Copy each tunnel's connector token from Cloudflare and paste it into **System >
Remote access**. Fleet encrypts the tokens in its database, writes only
permission-`0600` runtime token files, and invokes `cloudflared` with
`--token-file`, so tokens are not placed in process arguments. The API returns
only configured flags and non-secret fingerprints; leaving a secret field
blank preserves the stored value.

The admin connector shares only the control-plane Compose network. The instance
connector additionally joins the internal-only `hermes-fleet-edge` network;
only managed dashboards join that edge. Hermes runtimes and private APIs remain
on per-instance networks. Dashboard authentication stays enabled behind
Cloudflare Access as defense in depth.

The page reports connector, DNS, ingress, and endpoint checks independently.
`Published` is emitted only when the Fleet-owned DNS record, tunnel ingress,
and public endpoint all pass verification. A Cloudflare Access response is
reported as protected, not as a verified dashboard origin. Hostname changes or
instance deletion remove only DNS and ingress entries recorded in Fleet's
ownership ledger; unrelated Cloudflare resources are preserved. Disabling the
module removes local connector token files and stops the connectors.

Encrypted provider-managed and locally managed UUID configurations created by
older Fleet releases remain restorable for upgrade compatibility. The new UI
does not expose or request their provider credentials or credential JSON.
Disable the legacy mode before enabling the connector-token flow.

## Runtime Reconciliation

Fleet shows desired lifecycle state separately from observed runtime state. The
native Host Agent performs a read-only observation when it starts and every 30
seconds afterward. An observation checks only the exact Fleet-owned instance
target supplied by the control plane:

- managed directory, regular `compose.yaml` and `.env` files, and workspace
- exact data volume
- containers carrying both the Fleet-managed and instance-ID labels
- Compose project/service ownership labels and provisioned image ID
- Hermes/dashboard container state, Hermes Docker health, and loopback `/health`

Observed state is `IN_SYNC`, `DEGRADED`, `MISSING`, or `UNKNOWN`. A report is
considered unknown when its host is offline, the desired lifecycle state is
changing, the report no longer matches the current lifecycle generation, or no
fresh report has reached the control plane for two minutes. The instance page
shows individual check results and can request an early refresh; the Host Agent
normally handles that request on its next observation cycle.

Lifecycle and image drift are never changed silently. If a stopped instance
reports only image drift, the instance profile offers an explicit
`Reconcile image` action. It requires
typing the instance name and an online matching Host Agent. The lease-fenced job
verifies exactly two stopped Fleet-owned containers, both Compose image
references, the shared immutable image ID, desired image resolution, and volume
ownership before the control plane records the verified ID. It never starts,
recreates, pulls, or writes instance files. Normal `start` uses
`docker compose up -d --remove-orphans` only after verifying the immutable image
ID and the existing data-volume ownership. This recreates deleted containers
and migrates their names without allowing a moved image tag or missing data
volume to silently replace instance state.

Runtime-configuration drift uses a separate bounded repair: after repeated
matching observations Fleet may queue the same lease-fenced setup job shown by
the explicit **Complete setup** action. Fleet does not discover or adopt
external deployments.

## Per-instance Hermes Updates

The control plane owns one release catalog for **Create instance**, update
status, and per-instance Hermes updates. It refreshes the latest three stable
releases from the official `NousResearch/hermes-agent` GitHub Releases API and
atomically preserves a last-known-good cache. The Host Agent reuses a verified
image or builds exactly the selected release on demand. A Fleet upgrade never
fetches, builds, smoke-tests, or deletes Hermes runtime images.

The JSON catalog contains public release metadata only and uses mode `0644` so
the non-root control-plane process can read its Linux bind mount. The derived
TSV cache, `.env`, enrollment credentials, and encryption keys remain private.

Each release uses a source-and-wrapper-qualified image tag such as
`local/hermes-fleet-runtime:0.19.0-3ef6bbd20126-4d7016611c84`. The final
component identifies the exact Fleet runtime wrapper. A wrapper change therefore
creates a new tag instead of intentionally moving an existing one. Fleet checks
the version, source-commit, and wrapper-build labels before reusing a local tag
and rechecks those labels immediately after every build before running the
smoke test. A conflicting pre-existing tag fails only the selected create or
update operation; it does not rewrite the release cache or control plane. The wrapper build ID covers Fleet-owned runtime source;
it is not a signature for mutable upstream base images or package repositories.
Existing instances and rollback remain bound to their recorded content-addressed
Docker image IDs while those images remain present locally.

The instance **Overview** shows the installed Hermes version and the result of
an official release check: latest installed, update available, or check failed.
It includes the GitHub Releases source and check time; a failed network check is
never presented as "up to date". One explicit **Update** action queues a single
durable Host Agent job. Its persisted stages remain visible after a browser,
control-plane, or Host Agent restart.

The lease-fenced Host Agent prepares and verifies the selected official release
image, stops only the selected instance when required, creates a fresh encrypted
backup, and waits for control-plane integrity verification before changing the
runtime. It then installs and health-checks Hermes and restores the instance to
its original `RUNNING` or `STOPPED` state. Success updates the control-plane
image reference and immutable ID atomically with job completion. A failed
installation uses the verified pre-update archive for rollback. If a lease is
reclaimed after upload, the retry re-downloads and verifies that same backup
instead of creating a second recovery artifact.

Fleet does not auto-update instances. After an explicit per-instance operator
action, the Host Agent reuses the verified target image or builds that one
release before changing the selected instance.

## Instance Defaults

Creating an instance asks for its name, target Host, and one of the three
offered Hermes releases. Fleet prepares the matching source- and
wrapper-qualified image on demand, applies the OpenAI
Codex provider, and collision-free local ports internally. Codex model,
reasoning level, and service tier remain unconfigured until the per-instance
Codex flow is completed. The Host Agent reads the supported Codex model catalog
and recommendation from the active Hermes installation; the UI offers only
those models and the control plane rejects values outside that catalog. The
control plane validates every resolved value and transactionally rejects port
collisions.

OpenAI Codex OAuth is never shared between instances. Each instance keeps its
own OAuth state in its `/data` volume to avoid refresh-token conflicts. The
operator starts authentication from the instance Codex page. Fleet runs the
exact Hermes CLI in the Fleet-owned container, displays only OpenAI's
allowlisted device URL and one-time code, and verifies `hermes auth status`
before completing the operation. Tokens never pass through the control-plane
API or browser.

## Managed Instance Backups

Each instance profile can create, verify, download, and explicitly delete an
encrypted backup from its **Backups** tab. The same tab lists the only sources
that can be selected for restore. Creation is accepted only while the instance is
`STOPPED`; Fleet never stops or restarts an instance implicitly. The queued job
changes the controller state to `BACKING_UP`, which prevents a concurrent
lifecycle action until the recovery job returns the instance to `STOPPED`.

Before reading data, the native Host Agent independently verifies that:

- no container in the exact Compose project is running
- stopped containers still use the immutable image ID recorded by Fleet
- the data volume carries the exact Fleet Compose ownership label
- the immutable provisioned image ID still exists
- `.env`, `compose.yaml`, workspace, instance ID, project, and volume match the
  control-plane target

The recovery archive contains `manifest.json`, the managed workspace including
`.env` and bind-mounted workspace files, and `data-volume.tar` with the isolated
instance volume. This includes per-instance Codex OAuth state stored under
`/data`. Symlinks and special workspace files are rejected to keep future
restore extraction bounded.

The Host Agent encrypts its temporary assembled archive with an in-memory
one-time key and uploads the decrypted stream over the current loopback-only
agent connection. The control plane immediately re-encrypts it with chunked
AES-256-GCM using the separate `FLEET_RECOVERY_ENCRYPTION_KEY`. Restore and
update execution use a mode-`0600` plaintext staging file only for the active
lease, verify its size and SHA-256 before use, and delete it when the job exits.
Persistent artifact and metadata files use mode `0600` inside the existing
`hermes-fleet-manager-data` volume. Authenticated chunks, reserved metadata, and
the archive manifest are verified before a point becomes `READY`.

`FLEET_RECOVERY_RETENTION` defaults to 20 backups per instance and accepts 1
through 100. Manual backup creation is refused at the limit. An automatic update
backup may rotate only the oldest terminal automatic backup; it never deletes a
manual backup or an in-progress artifact. `FLEET_RECOVERY_MAX_BYTES` defaults to
50 GiB. Download decrypts the `.tar` stream, so store the downloaded file as
sensitive plaintext and delete it securely when it is no longer needed.

A `READY` backup can be restored only while the exact instance is `STOPPED` and
after the operator types the instance name. Before mutation, Fleet re-verifies
the encrypted artifact, immutable manifest, host/instance ownership, stopped
Compose state, TAR paths and types, SHA-256, and the locally installed runtime's
version, source commit, wrapper build, and supported configuration schema. The
Docker image ID stored in the backup remains audit evidence for the source
host; a manual restore does not require a clean host to reproduce that
host-local ID. The Host Agent creates local rollback copies of the current
workspace and data volume, replaces both using the resolved compatible runtime,
validates the restored Compose configuration, and leaves the instance stopped.
A failure after mutation triggers an independent bounded rollback; Fleet
records `FAILED` only when that rollback is incomplete.

Restoring an older backup also restores its recorded image, provider, model,
reasoning, and service-tier metadata. Starting the instance remains a separate
operator action, so restore never turns recovery into an implicit deployment.
Automatic rollback inside an in-place Hermes update is intentionally stricter:
it still requires the exact pre-update image ID on that same host.

## Control-plane Backup and Recovery

The **System > Control-plane backups** module can create, verify, download, and delete
consistent control-plane database backups. Backups are created with SQLite `VACUUM INTO`
so committed WAL state is included, then checked with SHA-256, SQLite
`quick_check`, and `foreign_key_check`. Backup database and metadata files are
stored with mode `0600` under the existing `hermes-fleet-manager-data` volume.

The default retention limit is 20 backups. Set `FLEET_BACKUP_RETENTION` to a
value from 1 through 100 to change the limit. The service refuses to create a
new backup at the limit; it does not silently delete an older backup.

**System > General** exposes a recovery drill that restores the latest
control-plane backup into isolated scratch storage and verifies the latest
encrypted backup for every managed instance. A passed drill enables a recovery
kit export containing the verified database backup, encrypted instance backup
artifacts and metadata, the release catalog, and a machine-readable manifest.
The kit intentionally excludes `FLEET_RECOVERY_ENCRYPTION_KEY`; retain that key
separately so theft of the archive alone cannot decrypt instance data.
It also excludes Docker image layers. On a clean recovery host, Fleet installs
or builds the versioned releases listed in the catalog, validates their release
labels, then restores instance configuration and data from the backup artifacts.

A Fleet backup contains only the control-plane SQLite database. It does not
contain:

- `.env`, including `FLEET_SECRET_ENCRYPTION_KEY` and
  `FLEET_RECOVERY_ENCRYPTION_KEY`
- Host Agent enrollment state
- encrypted managed recovery-point artifacts and metadata
- managed instance data volumes or workspaces
- per-instance Codex OAuth state

Keep the matching `.env` in a separate secure offline location. Provider
credentials in a restored database cannot be decrypted without the original
`FLEET_SECRET_ENCRYPTION_KEY`. Managed recovery-point artifacts cannot be
decrypted without the original `FLEET_RECOVERY_ENCRYPTION_KEY`.

Restore is intentionally offline-only in this version. Before restoring:

1. Verify the backup in **System > Control-plane backups**, download it, and compare its local
   `shasum -a 256` output with the full digest shown by the API.
2. Confirm the matching `.env` is available and make an additional recovery
   copy of the current `hermes-fleet-manager-data` volume.
3. Stop Fleet Manager and the Host Agent. Managed Hermes instances continue
   running independently.
4. Replace `fleet.db` only while the control plane is stopped, and remove stale
   `fleet.db-wal` and `fleet.db-shm` files before restart.
5. Restart Fleet Manager, then verify `/readyz`, Host Agent version/status,
   hosts, instances, system state, and operation history before removing
   the pre-restore recovery copy.

There is deliberately no live restore API: replacing an open SQLite database
could combine stale WAL state with a backup and corrupt the control plane.

## Fleet Manager Upgrade Safety

`./scripts/fleet-upgrade.sh` builds the candidate control plane under an immutable
build-qualified image tag. When an existing control plane is ready, upgrade first
requires enough free Docker storage to preserve the configured safety reserve
plus 2 GiB of build headroom. It then creates and downloads a verified database
backup, applies current migrations to an isolated copy, and records stable
instance lifecycle states. The stable image tag and `.env` selection are not
changed until the candidate passes `/readyz`, exact build-ID verification, and
every previously running or stopped instance-state check. A failed candidate is
removed without `--force`; the retained previous image selection is persisted
before rollback starts, even when a global readiness gate remains unhealthy.
Re-running upgrade with the same verified build skips the deployment and does not
consume another backup slot.

If Docker stopped the existing Fleet stack, upgrade first starts that same
control-plane image and its connector containers without recreating them. It
waits for the old readiness endpoint to respond and evaluates the response
before any candidate build. A responding but unhealthy service still blocks
the upgrade and reports its concrete readiness reason.

`/livez` proves only that the HTTP process is alive. `/readyz` additionally
requires a writable SQLite reservation, a safe storage reserve, and a valid
release catalog. `/healthz` remains a compatibility alias for `/readyz`.
State-expanding operations return HTTP 507 when free bytes, free percentage, or
free inodes cross the configured reserve. Docker memory, PID, and JSON-log
rotation limits are configurable through the resource values in `.env.example`.

Authenticated operators can inspect the runtime delivery contract under
**System > Runtime health**. `GET /api/v1/events` emits monotonic state
revisions over SSE; clients reload the authoritative overview after a revision
instead of treating the event payload as state. A 30-second poll remains as a
disconnect fallback. `GET /api/v1/system/runtime-health` reports queue depth,
expired leases, p95/p99 request latency, connected event clients, the active
Host Agent/runtime-schema compatibility manifest, and bounded
health-transition history persisted in SQLite. `GET /metrics` exports the
bounded counters and latency gauges in Prometheus text format. Host queues
reject new work with HTTP 429 at 100 active jobs per host and prioritize
lifecycle safety work. Exhausted expired leases are reconciled by the control
plane without waiting for another Host Agent claim. Cloudflare tunnel mode also
distinguishes synchronized local ingress from the actual readiness of both
`cloudflared` connectors.

Run `make reliability` before a candidate release. The qualification gate
fault-injects queue pressure, slow event consumers, incompatible runtime schema,
and expired leases, then validates frontend lint/build, shell syntax, and the
Compose model.

## Stop Fleet Manager

```sh
./scripts/fleet-stop.sh
```

This stops the control plane and Host Agent. It does not stop or delete managed
Hermes instances. Instance deletion from the UI preserves the data volume.

The LaunchAgent log is stored under `~/Library/Logs/HermesFleet` and is rotated
at 25 MiB with four files retained. Its credential remains in
`~/.config/hermes-fleet/agent.json` with user-only file permissions.

## Security Boundary

- The dashboard and managed endpoints bind to `127.0.0.1`.
- The Host Agent accepts only typed, allowlisted jobs.
- Every claimed job has an opaque lease token. A stale worker cannot
  acknowledge, renew, or complete a job after that lease is reclaimed.
- Lifecycle queues use expected-state fencing, and all queues use a per-instance
  active-job fence. Different instances may progress concurrently; one instance
  never receives two live jobs.
- Recovery-point jobs add `BACKING_UP` or `RESTORING` lifecycle fences,
  independently verify stopped state and exact Fleet ownership, and keep both
  Host Agent staging and control-plane artifacts encrypted at rest.
- Active instance names and host ports are allocated transactionally across API
  and dashboard roles. A successful delete releases them while retaining the
  instance and operation audit records.
- No remote shell endpoint exists.
- Provisioning validates and safely quotes image and runtime fields before the
  Host Agent writes a Compose manifest or environment file.
- The Docker socket is not mounted into the control-plane container.
- Runtime observations are host- and lifecycle-generation-fenced and execute
  only read-only Docker and filesystem checks against exact Fleet targets. The
  sole container exec allowlist is `hermes auth status openai-codex` against the
  exact owned Hermes container.
- Image drift remains an explicit operator action. Runtime-health drift may
  trigger the lease-fenced bounded recovery policy: three attempts per phase,
  up to three phases, with a five-minute cooldown between phases. The operator
  can stop the automatic recovery from the instance page. Repeated
  runtime-configuration drift may queue only the separate bounded setup repair.
- Full per-instance secret files stay on the target host. Only three explicit
  access credentials can be revealed on demand to an authenticated operator.
- Desired messaging configuration is encrypted in the control-plane database.
  Its plaintext is fetched only under the active Host Agent lease and is never
  embedded in a job payload, operation metadata, or API response returned to
  the browser.
- Desired MCP configuration follows the same active-lease secret boundary.
  Fleet permits only remote HTTPS transports and explicit tool allowlists; it
  does not provide an arbitrary command or package-installation path.
- Credential reveal responses disable caching and expire after five minutes.
- Fleet encryption keys and control-plane tokens are generated into the local
  `.env` with user-only file permissions.
- Instance deletion requires explicit instance-name confirmation and preserves
  data. Recovery-point deletion requires its exact generated filename.

See [docs/architecture.md](docs/architecture.md) for the current V1 contract.
