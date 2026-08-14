# Hermes Fleet Manager V1 Architecture

## Ownership

Fleet Manager owns only instances it creates. Existing agent deployments remain
external and are not discovered, adopted, consolidated, or mutated.

```text
Control Plane
    |
    | typed leased jobs over HTTP
    v
Native Host Agent
    |
    +-- Managed Hermes instance A
    +-- Managed Hermes instance B
```

## Resource Model

```text
Host 1 --- N ManagedInstance
ManagedInstance 1 --- N Operation
Operation 1 --- 1 Job
ManagedInstance 1 --- 1 RuntimeObservation
ManagedInstance 1 --- 1 isolated data volume
ManagedInstance 1 --- 1 encrypted MCP desired configuration
ControlPlane 1 --- N BackupArtifact
ManagedInstance 1 --- N encrypted RecoveryPoint
```

The control plane stores desired lifecycle state, observations, jobs, and audit
records in SQLite. Provider credentials, OAuth state, memory, sessions, cron,
and workspaces remain on the target host.

## V1 Job Contract

The Host Agent currently accepts these job types:

```text
instance.provision
instance.provision (retry for a failed instance)
instance.start
instance.stop
instance.delete
instance.credentials.inspect
instance.recovery.create
instance.recovery.restore
instance.auth.codex
instance.image.reconcile
instance.image.repair
instance.runtime.sync
instance.runtime.repair
instance.messaging.configure
instance.mcp.configure
instance.hermes.update
```

Jobs use explicit states, a five-minute lease, and an opaque lease token that
changes on every claim. Acknowledgement, renewal, and completion require the
current unexpired token, so a stale Host Agent cannot commit results after a
job is reclaimed. Lifecycle jobs also use a transactional expected-state check
when queued. All job types share a per-instance active-job fence. The Host
Agent can execute jobs for different instances concurrently, but the control
plane never leases two live jobs for the same instance. Heartbeats and lease
renewal run independently from job execution. The Host Agent retries its
initial heartbeat with bounded backoff
instead of relying on the service manager to restart it. Provisioning refuses
to overwrite existing secret files, validates the complete runtime payload,
and delete removes containers and networks while preserving the data volume.

`instance.recovery.create` is queued transactionally from `STOPPED` to
`BACKING_UP`; completion always returns the controller state to `STOPPED`.
This prevents start, delete, or a second recovery point from
racing the export. The Host Agent also queries the exact Compose project and
refuses the operation if any container is running, so control-plane desired
state alone is never treated as proof that the data is quiescent.

`instance.credentials.inspect` is a read-only instance operation. It validates
the Fleet-managed path and reads only these keys from the instance `.env`:

```text
HERMES_DASHBOARD_BASIC_AUTH_USERNAME
HERMES_DASHBOARD_BASIC_AUTH_PASSWORD
API_SERVER_KEY
```

The Host Agent never returns the complete file. The control plane encrypts the
allowlisted result with AES-256-GCM, uses the operation ID as associated data,
and stores ciphertext with a five-minute expiry. Successful reveal responses
set `Cache-Control: no-store`; plaintext exists only in the authenticated HTTP
response and browser state. The UI follows the server operation until it
reaches a terminal state instead of using a fixed client-side timeout.

New instances are created with `openai-codex` as the authentication method but
without a model, reasoning level, or service tier. After per-instance OAuth
succeeds, a separate lease-fenced configuration job applies the operator's
selection and persists it only after Host Agent success. Authentication and
configuration remain separate states; there is no shared profile or
provider-binding flow.

System has one explicit control-plane boundary. `GET /api/v1/system` returns
only Fleet Manager state: the control-plane version and operator URL, database path,
and control-plane backup retention. Host Agent versions stay in the global host
inventory. Hermes version, source commit, and Codex state are read from each
instance and are never projected into System.

Optional remote access preserves that ownership split. `fleet-bootstrap.sh`
installs two connector supervisors, but they remain idle until an operator
configures the module through **System > Remote access**. Cloudflare values are
not accepted from `.env` and there is no separate Cloudflare setup path. The
control plane encrypts each supplied tunnel token in SQLite and writes it only
to the matching isolated runtime volume. The admin tunnel remains an
independent boundary with a manual Cloudflare route. The shared instance
connector, account ID, zone ID, and scoped API token form one **Instance
publishing** transaction. The tunnel UUID is extracted from the connector
token, while the publishing zone name is resolved from the verified Cloudflare
Zone ID and stored as non-secret metadata. The browser can observe configured
flags, the zone name, and non-secret fingerprints, but can never retrieve
either secret.

One remotely managed Cloudflare tunnel exposes only the Fleet Manager control
plane. A second tunnel exposes only managed dashboard containers through one
explicit hostname rule per instance and a final `http_status:404` rule. Each
instance stores its own explicit `public_hostname`; there is no user-entered
shared-domain setting. Before publication, the UI may suggest
`<instance-name>.<verified-zone>` from the Cloudflare zone metadata. This
suggestion remains editable and is not persisted until the operator explicitly
publishes it. The control plane derives the exact Docker origin, then uses the
scoped API token to reconcile DNS and remotely managed tunnel ingress. An
ownership ledger prevents Fleet from changing or deleting unrelated records.
After a route has passed DNS, ingress, and endpoint verification, later
reconciliation preserves that last verified publication while displaying it as
revalidating. Newly-created DNS and transient Cloudflare 5xx responses receive
a bounded endpoint retry window before the publication operation fails. Routine
revalidation also preserves the last globally synced remote-access health;
connector transitions and actual reconciliation failures still replace it.
Fleet never exposes an instance API route or manages Cloudflare Access
policies.

The instance connector and dashboard containers share an internal-only Docker
edge network. The connector also keeps its normal outbound network so the edge
network itself has no external gateway. Hermes runtime containers remain only
on their private Compose networks. A ready connector proves only that the local
process accepted its token. Per-instance publication separately verifies DNS,
ingress, and the public endpoint; only all three passing yields `Published`.
Hostname replacement and instance deletion remove only ledger-owned DNS and
ingress entries. Disable removes Fleet-owned local connector token files and
does not claim to remove Cloudflare Access policies or unrelated resources.

Active instance names and both host ports are unique allocations. API and
dashboard ports share one collision domain per host. `DELETED` instances remain
as audit tombstones but no longer reserve their name or ports.

Codex OAuth is excluded from the control-plane credential vault. Fleet creates
the instance without runtime model defaults while OAuth state remains isolated
in each instance data volume. `instance.auth.codex` is a non-lifecycle
job: it cannot change instance status or runtime metadata. Its STARTING,
AWAITING_USER, and VERIFYING progress updates are lease-fenced. The Host Agent
runs `hermes auth add openai-codex --no-browser` only inside the exact
Fleet-owned Hermes container and exposes only the fixed OpenAI device URL,
validated one-time code, and expiry. Completion requires a separate `hermes
auth status openai-codex` verification. `instance.runtime.configure` then
applies and records the selected model, reasoning, and service tier; a failed
job leaves the instance configuration unset.

`instance.messaging.configure` is a per-instance configuration job for the
official Hermes Telegram and WhatsApp Baileys settings. The control plane
normalizes strict user/chat identifiers and international WhatsApp numbers,
rejects proxy URLs containing credentials, encrypts the complete desired
configuration, and places only an immutable revision plus non-secret runtime
identity in the job payload. After acknowledgement, the Host Agent must present
the active lease token to fetch the protected configuration. It updates only
the allowlisted messaging environment variables and YAML keys, writes a
revision readiness marker, and verifies the exact Fleet-owned runtime. A
running instance is force-recreated and health-checked; a stopped instance
remains stopped. Any post-write failure restores the previous environment,
manifest, and Hermes YAML before the job completes. WhatsApp session pairing is
not inferred from configuration and remains a separate interactive Hermes
operation.

`instance.mcp.configure` is a per-instance installation boundary for remote
HTTPS MCP servers. Fleet rejects `stdio`, executable, package-installation,
embedded-credential, and plaintext HTTP definitions. Every enabled server has
an explicit native-tool allowlist; MCP resources and prompts are disabled. The
server is marked untrusted, while sampling and server-initiated elicitation are
disabled. The complete desired definition and optional bearer tokens are encrypted in SQLite
and fetched only by the exact Host Agent job holding the active lease. The job
payload and operation metadata contain only runtime identity, a configuration
revision, and non-secret server names.

The Host Agent snapshots the instance-owned `/data/config.yaml` and
`/data/.env`, writes bearer tokens only into a Fleet-managed `.env` block, and
places `${VAR}` references in the Hermes HTTP headers. It verifies the
revision marker and effective allowlist, recreates and health-checks only a
running instance, then runs `hermes mcp test` for every enabled server. A
stopped instance stays stopped. Any post-write failure restores both files and
the prior runtime state before completion.

## Runtime Observation Contract

Lifecycle state and runtime observation are deliberately separate. The existing
instance `status` remains the desired/controller lifecycle state. The latest
Host Agent report is stored independently as one of:

```text
IN_SYNC
DEGRADED
MISSING
UNKNOWN
```

The control plane supplies the authenticated Host Agent with exact observation
targets from its own instance records. The agent does not enumerate arbitrary
Compose projects, discover external deployments, or adopt containers. For each
target it validates the deterministic Fleet project, data-volume, and managed
path names, then performs read-only checks of regular instance files, workspace,
the exact data volume, containers selected by both Fleet labels, Compose
ownership labels, image IDs, service state and health, and the loopback Hermes
`/health` endpoint. For a running `openai-codex` instance, the only allowlisted
container exec is the read-only `hermes auth status openai-codex` check against
the exact Hermes container identity.

Observation reports are bound to the authenticated host and to an instance
generation derived from the current lifecycle `updated_at`. A report from
another host is rejected; a report for an older lifecycle generation or with an
older `observed_at` value is ignored. Server receipt time determines freshness,
so agent clock skew cannot keep an old observation fresh. Offline hosts,
transitional lifecycle states, stale reports, and instances without a report
are presented to operators as `UNKNOWN` without destroying the last recorded
check details.

The Observer loop runs independently from heartbeat and leased lifecycle jobs.
Its Docker allowlist contains only read operations such as exact-label `ps`,
`inspect`, `volume inspect`, and `version`. It never runs `up`, `start`, `stop`,
`restart`, `down`, creates files, or repairs drift. Manual refresh creates a
request marker that the next matching agent report clears; it is not a hidden
lifecycle operation.

An accepted observation records the `io.hermes-fleet.hermes-version`,
`io.hermes-fleet.hermes-ref`, and `io.hermes-fleet.runtime-build-id` labels
inherited from the exact owned Hermes container image. Setup checks those
source/build identity labels before reuse and immediately after a build. The
instance Overview presents only the upstream identity as **Hermes version** and
**Source commit**. Mutable image tags and Fleet wrapper build IDs are not
presented as Hermes versions.

Image drift has a separate explicit, lease-fenced repair job. The control plane
accepts it only for a fresh `image=DRIFT` observation and an exact instance-name
confirmation. The Host Agent verifies the Fleet identity, Compose image
references, exact container and volume ownership, container state, and the
current immutable digest before any lifecycle mutation. A running instance is
then stopped, verified again, restarted, and health-checked; a stopped instance
is never started. Preflight failure causes no lifecycle change, and a failed
post-stop verification uses an independent bounded context to restore the
original running state. The control plane records the new digest only after a
successful result reports the requested final lifecycle state.

Runtime-health drift uses a separate bounded automatic repair state machine.
Two consecutive fresh drift observations may queue a lease-fenced
`instance.runtime.repair` job. Fleet permits three attempts in each of three
phases, waits five minutes between phases, records every attempt under one
workflow identity, and exposes an operator cancellation action. Missing
ownership, manifest, environment, workspace, Docker, or data-volume
prerequisites prevents automatic mutation.

## Operations API Pagination

`GET /api/v1/operations` without query parameters preserves the original
response contract: a JSON array containing at most 100 operations. New clients
request cursor pagination with `?limit=50` and receive
`{"items": [...], "next_cursor": "..."}`. A client passes the opaque
`next_cursor` value back together with a limit to load the following page.
Limits must be between 1 and 100; the default is 50 for a cursor request.

The cursor encodes the last `(created_at, id)` boundary and the store orders by
both fields descending. This prevents duplicate or skipped rows when operations
share a timestamp. Cursor payloads and query parameters are strictly validated,
and all list responses use `Cache-Control: no-store`.

## Control-plane Backup Contract

Control-plane backups are consistent SQLite snapshots created inside the
existing Fleet data volume. Every artifact has immutable creation metadata,
size, and SHA-256 digest. Creation and download both require successful SQLite
integrity and foreign-key checks. Backup files and metadata use mode `0600`,
and creation stops at the configured retention limit instead of automatically
discarding an older control-plane backup.

The control-plane backup boundary is deliberately narrower than an instance backup. It does
not include the external `.env` encryption key, native Host Agent enrollment,
encrypted managed instance-backup artifacts, managed Docker volumes, workspaces,
or per-instance OAuth state. Recovery
therefore requires the matching `.env`, and SQLite replacement is permitted
only while the control plane is stopped. V1 exposes no live restore endpoint.

## Managed Instance Backup Contract

A backup reservation binds one generated backup ID to one instance, host,
operation, job, image ID, Compose project, data volume, managed path, Host Agent
version, and creation time. The Host Agent accepts only this exact payload. It
validates Fleet naming and Docker ownership, exports the isolated data volume
only when stopped container image IDs still match the immutable Fleet record,
uses a network-disabled, read-only helper container, and captures the
managed `.env`, Compose manifest, and workspace without following symlinks.

Both Host Agent staging files are chunk-encrypted with an in-memory random key
and distinct associated-data contexts. The agent keeps renewing the job lease
during artifact upload; the control-plane upload endpoint requires the same
active host, job ID, and opaque lease token used by acknowledgement, renewal,
metadata lookup, and completion.

The current control plane remains bound to loopback. It encrypts each incoming
archive chunk with AES-256-GCM and a separate recovery key before publishing an
artifact. A backup becomes `READY` only after decryption, SHA-256 and size
comparison, safe TAR path/type validation, and exact manifest comparison. The
download API authenticates the operator and streams decrypted plaintext with
`Cache-Control: no-store`; the encrypted file remains the only persistent
control-plane copy.

Retention is explicit and per instance. Manual creation stops at the configured
limit; deletion requires the exact generated filename. An automatic Hermes
update may rotate only the oldest terminal automatic backup and never removes a
manual or in-progress point. Restore requires the exact stopped instance and a
name confirmation. The active job lease gates the artifact stream, and the Host
Agent independently verifies the manifest, checksum, managed identity, Compose
state, volume ownership, TAR paths, and the locally resolved runtime's release
labels and supported configuration schema before mutation. The source-host
image ID remains immutable audit metadata, but manual recovery can resolve the
same versioned image reference to a different local image ID on a clean host.
It snapshots the current workspace and volume locally, publishes the recovered
state, records the resolved host-local image ID, validates that no container was
started, and uses an independent bounded context to roll back partial failure.
The automatic rollback nested inside an in-place Hermes update remains
same-host-only and requires the exact recorded pre-update image ID.

## Local-to-VPS Path

The local phase validates lifecycle, stopped-instance recovery, and explicit
per-instance Hermes updates against a native Mac Host Agent. An update target
is pinned by image reference, semantic version, and Git commit from the schema-
and provenance-validated release catalog. One durable
`instance.hermes.update` job prepares the target image on the host, stops the
selected instance when needed, creates and verifies a fresh automatic backup,
installs and validates Hermes, and restores the original running or stopped
state. A reclaimed retry reuses the same verified backup under the new active
lease. Only successful lease-fenced completion records the new image reference
and immutable ID.

Recovery artifacts contain instance state and stable release identity, not
Docker image layers or host-local image IDs as portable runtime content. A clean
host prepares the cataloged release normally and then restores the saved state.

The same control-plane image and SQLite backup contract also applies on a
dedicated VPS. Official release discovery stays inside the control plane and
uses its durable last-known-good cache. Remote enrollment, mTLS, and unattended
runtime-image upgrades remain outside this implementation.

Credential profiles must not be exposed remotely in this phase. Before the
control plane moves to a VPS, terminate TLS at a trusted edge, replace the
single admin token with operator identities, and protect Host Agent traffic
with mTLS.
