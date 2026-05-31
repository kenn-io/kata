# Kata Federation

Federation lets multiple kata daemons share selected projects while each user
keeps a local daemon and local database. It is opt-in per project. A normal
single-user kata installation with no federated bindings should not read
federation credentials or make federation network calls unless an operator
invokes federation commands.

Federation is not a replacement for a shared daemon. Use a shared daemon when
users need immediate single-copy reads and writes, centralized authorization, or
strict online-only arbitration. Federation deliberately chooses local-first
availability, durable offline queues, and deterministic convergence for routine
work, with documented consistency limits.

## Terms

- **Hub**: the authoritative daemon for a federated project. It owns enrollment
  tokens, lease arbitration, purge/reset authority, and the canonical project
  event stream.
- **Spoke**: a daemon with a local replica bound to a hub project.
- **Binding**: a local row in `federation_bindings` that marks one project as a
  hub or spoke replica and stores pull/push cursors.
- **Enrollment**: a hub-side credential in `federation_enrollments`. A token is
  bound to one spoke instance UID, optional project scope, and capabilities
  such as `pull`, `push`, and `claim` (the transport capability name for leases).
- **Origin instance UID**: the durable daemon identity stamped on events. It is
  how replicas distinguish local-origin from foreign-origin work.
- **Pull cursor**: the highest hub event ID consumed by a spoke.
- **Push cursor**: the highest spoke-local event ID accepted by the hub.
- **Replay horizon**: the hub event ID from which a spoke can bootstrap. History
  before that point is represented by baseline snapshot events rather than by a
  replay-complete event stream.
- **Lease**: a hub-authoritative write lease for one existing issue. Mutating
  existing issue work on federated projects, including comments, requires a live
  lease. Creating new issues remains lease-free because there is no existing
  issue to lease. The internal storage and audit events still use the `claim`
  name.
- **Quarantine**: a local operator stop marker for a poisoned federation batch.
  It prevents hot-looping and requires an explicit operator decision.

## Tokens And Trust Boundaries

Kata federation uses two different bearer-token systems. They are intentionally
separate:

- **Daemon API tokens** identify clients talking to a daemon's normal API. They
  are configured with `KATA_AUTH_TOKEN` or `[auth].token` and managed with
  `kata tokens ...` when token identity is required. Operator commands such as
  `kata federation enroll`, `kata federation revoke`, `kata federation status`,
  `kata federation quarantine skip`, and hub-local force-release use this normal
  daemon API auth surface.
- **Federation enrollment tokens** authorize one spoke to call hub federation
  transport routes for an enrolled scope and capability set. They are created by
  `kata federation enroll`, stored hashed on the hub, stored plaintext only in
  the spoke federation credentials file, and used for pull, push, join metadata
  fetches, and forwarded lease actions. They are not general daemon API tokens.

Lease commands have two hops on a spoke. The CLI first talks to the local spoke
daemon using the normal daemon API auth rules. The spoke then forwards the lease
request to the hub with its federation enrollment token; the hub derives
`holder_instance_uid` from that enrollment. A hub-local lease command has only
the first hop and uses daemon API auth. When `[auth].require_token_identity =
true`, local operator and lease commands must use DB-backed API tokens from
`kata tokens create`; enrollment tokens still only authenticate spoke-to-hub
federation transport.

When a daemon listens on a non-loopback address, configure a daemon API token
and explicitly trust the private network:

```toml
[auth]
token = "..."
trust_private_network = true
```

The equivalent environment variables are `KATA_AUTH_TOKEN` and
`KATA_TRUST_PRIVATE_NETWORK=1`. Plain HTTP private-network clients that attach
bearer tokens also require this trust opt-in. That includes normal CLI/TUI
access to a remote daemon and spoke-to-hub federation calls that use enrollment
tokens. Without the opt-in, kata refuses to put bearer tokens on plaintext
non-loopback HTTP connections. HTTPS, Unix sockets, and loopback HTTP do not
need the private-network trust opt-in.

## Implementation Map

- `internal/db`: schema, federation bindings/enrollments, event ingest,
  materialization, lease state, quarantine, reset guards, and JSONL cutover.
- `internal/daemon`: daemon HTTP routes, enrollment-aware transport auth,
  local/admin operator APIs, lease forwarding, purge/reset behavior, and status
  responses.
- `internal/client`: generic first-party daemon discovery, auto-start, bearer
  attachment, Unix-socket HTTP clients, TCP remote selection, and SSE clients.
- `internal/federation`: spoke-side hub HTTP client, pull/push runner, pending
  lease retry, failpoints, and federation runner tests.
- `cmd/kata`: federation operator CLI, lease CLI, daemon runner startup, and
  normal CLI client wiring.
- `e2e` and `docker/federation`: multi-daemon, randomized stress, failpoint, and
  Docker Compose smoke coverage.

## Setup Model

Federation setup is an operator workflow. The `kata federation` command is
visible in CLI help, but it remains separate from ordinary issue commands so
users who never opt into a federated project do not see daemon prompts,
credential reads, or network calls.

On the hub:

1. Create or register the project first. In a workspace that should become the
   hub project, use the normal project setup command:

   ```bash
   kata init --project fedlab
   ```

2. Enable federation for a project when you want an explicit enable step:

   ```bash
   kata federation enable --project fedlab
   ```

   This records `project.federation_enabled` and baseline `issue.snapshot`
   events at the replay horizon. This step is optional when the next command is
   `kata federation enroll`, because enrollment auto-enables the project if it
   is not already federated.
3. Create one enrollment token per trusted spoke:

   ```bash
   kata federation enroll --project fedlab \
     --spoke-instance 01H... \
     --hub-url http://<private-hub-ip>:7787
   ```

   The hub stores only the token hash. The enrollment records the spoke
   instance UID, optional project scope, and capabilities. The CLI prints a
   pasteable `kata federation join ...` command using the binary name that
   invoked `enroll`, and containing the generated token; treat that command as
   secret-bearing material.

On each spoke:

1. Read the spoke instance UID when creating the hub enrollment:

   ```bash
   kata federation identity
   ```

2. Run the join command printed by the hub:

   ```bash
   kata federation join --project fedlab \
     --hub-url http://<private-hub-ip>:7787 \
     --hub-project-id 1 \
     --token ... \
     --push
   ```

   `join` fetches the hub project UID and replay horizon from the hub using the
   enrollment token, so the hub must be reachable at join time and the token
   must include `pull`. The metadata flags (`--hub-project-uid`,
   `--replay-horizon`, and `--baseline-through`) remain available as explicit
   overrides for scripts. The command creates a local replica project bound to
   the hub project UID and replay horizon, stores the hub URL/project/token in
   the local federation credentials file, and enables push only when `--push` is
   present.

Enrollment capabilities and local spoke behavior are separate knobs:
`--capabilities pull,push,lease` on the hub says what the token may do, while
`--push` on the spoke says this replica should actually push local-origin events
back to the hub. If the token has `push` but `join` is run without `--push`, the
spoke remains pull-only and the CLI prints a warning.

The transport routes use enrollment bearer tokens. Local operator routes,
including `kata federation status`, quarantine skip, force-release, and purge,
use the normal daemon local/admin auth surface. Federated hub purge also
requires the acting actor to hold the live issue lease; an operator can
force-release first when an abandoned lease blocks destructive maintenance.

The daemon federation runner polls every 30 seconds by default. For tests,
short-lived labs, or latency-sensitive private deployments, set
`KATA_FEDERATION_PULL_INTERVAL_MS=<milliseconds>` on the daemon process. Very
short intervals are useful for smoke tests but increase wakeups and database
traffic.

## Schema And Upgrade

Federation is one schema bump: upstream schema `11` upgrades to schema `12`.
Existing schema-11 databases upgrade through the JSONL cutover path. The v11
source schema does not have `comments.uid` or event replay fields
(`events.hlc_physical_ms`, `events.hlc_counter`, `events.content_hash`), so the
exporter keeps v11 on the legacy comment/event projections and the importer
backfills those fields while loading the fresh schema-12 database.

Federation push requests also carry a `schema_version` field in the wire body.
The hub rejects requests that omit it or report a schema newer than the hub's
own schema. This is intentionally conservative: an old hub must not blindly
materialize events from a newer spoke whose payloads or fold semantics may have
changed. Upgrade hubs before push-enabled spokes when rolling out a new
federation schema.

## Pull Replication

A spoke polls the hub transport route for events after its pull cursor. It
applies hub events in order, deduplicates by event UID and content hash, folds
portable payloads into the local projection, and advances its pull cursor only
after successful application.

Baseline snapshots bridge the fact that pre-horizon history is not
replay-complete. Events after the baseline are ordered by their event/HLC data
and materialized normally. If the hub reports `reset_required`, the spoke
refreshes hub metadata and re-bootstrap from the current horizon.

## Push Replication

A push-enabled spoke scans for local-origin events above its push cursor and
sends them to the hub as an all-or-nothing batch. The hub authenticates the
enrollment token, verifies project scope and capability, checks that each event
belongs to the bound spoke origin, verifies the spoke's declared schema version,
deduplicates same-hash retries, rejects same-UID/different-hash conflicts,
materializes the batch, and returns the advanced push cursor.

If the response is lost after the hub commits, retrying the same batch is safe:
the hub treats fully duplicated same-hash batches as successful and returns an
advanced cursor. Permanent validation failures or hash conflicts record a
quarantine on the spoke instead of retrying forever.

## Leases And Write Gates

Leases are hub-authoritative. A spoke forwards acquire, renew, release, and status
requests to the hub using an enrollment token with `claim` capability. The hub
derives `holder_instance_uid` from that enrollment token; clients provide the
human-readable holder string.

For federated projects, mutations of existing issues, including comments,
require a live lease. Creating new issues does not require a lease. Spokes use
their cached lease state to gate local writes. When online, stale status is
refreshed from the hub. When offline, cached hard leases can still be used so
work is not lost. Timed leases expire by hub time.

The hub checks pushed work against the live lease state at ingest time. Work
that is not covered is not dropped; the hub records `claim.violated`. This is
best-effort, not a causal proof that the work was unauthorized when originally
performed. An offline edit that was covered at edit time can arrive after a
release or force-release and be marked violated because the hub checks current
state during ingest.

`kata show` surfaces the current lease and recent unresolved lease violations
for a federated issue. `kata federation status` shows project-level counts and
recent violation summaries.

## Operator Commands

```bash
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url>
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> [--push]
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation status
kata federation status --json
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease release <issue-ref>
```

`kata federation enrollments list` audits hub-side spoke grants without showing
token hashes or plaintext tokens. `kata federation revoke <enrollment-id>` marks
an enrollment inactive so the token no longer authorizes pull, push, or lease
transport calls. Project-level `leave` and `disable` teardown commands are not
currently exposed; operators should treat those as a separate reset/removal
workflow because local pending pushes and replicated state need explicit policy.

`kata federation status` reports one entry for each local federation binding:

- project name, role, enabled state, and push-enabled state
- pull and push cursors
- pending push depth and high-water event ID
- last sync, pull, push, reset, and error timestamps
- enrollment count on hubs
- live and pending lease counts
- active quarantine count and reset blocker
- unresolved lease violation count and recent violation summaries

A daemon with no federation bindings returns an empty status list and prints
`no federation bindings` in text mode.

## Quarantine

A spoke records an active quarantine when it sees a permanently poisoned push
batch. Quarantine blocks further push and can block reset. Operators can inspect
it with status and intentionally skip it:

```bash
kata federation quarantine skip <id> \
  --confirm "SKIP FEDERATION BATCH <id>" \
  --reason "operator accepted the skipped outbound batch"
```

Skipping advances the spoke push cursor past the quarantined event range and
marks the quarantine skipped. It does not delete local events and it does not
make the skipped work appear on the hub. Use it only when the operator accepts
that the local batch will not be federated.

## Purge And Reset

Hard purge is hub-admin-only for federated projects. A spoke rejects hard purge
with `federated_admin_required`. A hub purge uses the normal local/admin daemon
auth surface, the existing exact confirmation string, and the same live-lease
gate as other issue mutations.

When a hub purge removes replay history, it records a reset boundary in
`purge_log` and writes a fresh federation baseline for the remaining project
state. A spoke whose pull cursor is below that boundary receives
`reset_required` and re-bootstrap from the current federation horizon. A
push-enabled spoke refuses to reset while it has unaccepted local-origin events
or an active quarantine. `kata federation status` reports these reset blockers.

## Recurrences, Merge, And Other Boundaries

Recurrences remain hub-owned for federated projects. Spoke recurrence mutation
is blocked rather than partially synchronized. Project merge refuses projects
with federation bindings because local integer identities, remote UIDs, and
federation cursors would otherwise become ambiguous.

Federation does not add a global user registry. Actor strings remain audit
metadata. Origin instance UIDs disambiguate daemon identity.

## Consistency Limitations

The following are expected federation behaviors. They are the main reasons to
prefer a shared daemon for stricter collaboration.

### Reads Can Be Stale

Spokes read from their local database. A hub or another spoke can accept work
that has not reached this spoke yet. Users may see stale title, status, labels,
priority, lease status, or violation counts until the next pull succeeds.

Use a shared daemon when every user must see the latest committed state before
acting.

### Writes Are Not Globally Serialized At Edit Time

Local spoke writes happen before the hub has accepted them. The hub serializes
event ingest later. For ordinary fields, deterministic materialization converges
replicas, but users can temporarily see different local projections and the
later accepted event order may not match what each user saw locally.

Use a shared daemon when conflicting edits must be prevented synchronously.

### Offline Cached Hard Leases Can Be Superseded

A spoke can allow work under a cached hard lease while offline. During the
outage, a hub operator can force-release the lease or another actor can acquire
a later lease. When the offline work reconnects, the hub keeps the data but can
record `claim.violated`.

Use a shared daemon when leases must be enforced strictly online and stale
offline leases are unacceptable.

### Lease Violation Signals Are Best-Effort

Violation annotation checks live hub lease state at ingest time, not historical
lease state at the event's HLC timestamp. It is an operational signal for
"work arrived without currently valid coverage", not a complete audit proof of
user intent or causal authorization.

Use a shared daemon when lease compliance must be decided at the exact write
time.

### Poisoned Push Batches Require Operator Choice

A validation error, hash conflict, or schema divergence can quarantine a push
batch. Until an operator fixes the data or skips the batch, push remains
blocked and reset may be blocked. Skipping is explicit data divergence: the hub
will not receive those local events.

Use a shared daemon when local writes must either commit centrally or fail
synchronously with no later operator reconciliation.

### Hub Outage Changes The User Experience

During a hub outage, spokes can still read local state and may perform allowed
local work, but lease acquisition, timed-lease refresh, push, pull, and status
freshness degrade. Pending lease requests are not authoritative until the hub
accepts them.

Use a shared daemon when lack of hub connectivity should stop all shared work.

### Purge Causes Re-bootstrap

Hub purge creates a reset boundary. Spokes may temporarily show old state until
they pull the reset signal and re-bootstrap. Push-enabled spokes can delay reset
if they have unaccepted local work or quarantine.

Use a shared daemon when destructive administrative actions must be instantly
visible to all users.

### No Multi-Tenant Authorization Model

Enrollment tokens authorize spokes, not individual global users. The local
daemon remains a single-user local tool. Project-scoped user ACLs and a global
identity model belong to shared-daemon or later hardening work.

Use a shared daemon when different human users require centrally enforced roles
on the same project.

## Verification

Routine checks:

```bash
make test
make vet
make lint
make nilaway
```

Federation-specific checks:

```bash
make test-stress
make test-federation-docker
```

`make test-stress` runs Go-based randomized and failpoint tests. When Rapid
prints a failing seed, reproduce it with:

```bash
RAPID_SEED=<seed> go test -tags federation_stress ./e2e -run TestFederationStressRandomizedWorkload -count=1 -timeout 2m
```

`make test-federation-docker` builds the current checkout into Docker images,
starts one hub daemon and two spoke daemons in separate containers, and drives a
lease-gated convergence scenario across real process and network boundaries.

For manual Docker debugging:

```bash
docker compose -f docker/federation/docker-compose.yml -p kata-federation-smoke up --build
docker compose -f docker/federation/docker-compose.yml -p kata-federation-smoke down --volumes --remove-orphans
```

Use an isolated project name for parallel runs:

```bash
KATA_FEDERATION_DOCKER_PROJECT=kata-federation-smoke-$USER make test-federation-docker
```

## Historical Design Context

The original design discussion remains in
`docs/superpowers/specs/2026-05-20-kata-federation-design.md`. That document is
historical context. This file is the canonical description of the implemented
federation behavior and limitations.
