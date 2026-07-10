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
  daemon API auth surface. `kata federation enroll` is special only in target
  selection: its `--hub-url` flag selects the hub daemon for the enrollment API
  call while the CLI's default daemon remains the spoke being enrolled.
- **Federation enrollment tokens** authorize one spoke to call hub federation
  transport routes for an enrolled scope and capability set. They are created by
  `kata federation enroll`, stored hashed on the hub, stored plaintext only in
  the spoke federation credentials file, and used for pull, push, join metadata
  fetches, and forwarded lease actions. Each enrollment is bound to one actor;
  pushed event actors and forwarded lease holders must match that bound actor.
  They are not general daemon API tokens.

Lease commands have two hops on a spoke. The CLI first talks to the local spoke
daemon using the normal daemon API auth rules. The spoke then forwards the lease
request to the hub with its federation enrollment token; the hub derives
`holder_instance_uid` and holder from that enrollment. A hub-local lease
command has only the first hop and uses daemon API auth. When
`[auth].require_token_identity = true`, local operator and lease commands must
use DB-backed API tokens from
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

1. Create or register the project first when you want a separate setup step. In
   a workspace that should become the hub project, use the normal project setup
   command:

   ```bash
   kata init --project fedlab
   ```

2. Enable federation for a project when you want an explicit enable step:

   ```bash
   kata federation enable --project fedlab
   ```

   This records `project.federation_enabled` and baseline `issue.snapshot`
   events at the replay horizon. This step is optional when the next command is
   `kata federation enroll`, because enrollment creates the hub project when it
   does not already exist and auto-enables the project if it is not already
   federated.
3. Create one enrollment token per trusted spoke:

   ```bash
   kata federation enroll --project fedlab \
     --spoke-instance 01H... \
     --hub-url http://<private-hub-ip>:7787 \
     --actor wesm
   ```

   The hub stores only the token hash. The enrollment records the spoke
   instance UID, optional project scope, capabilities, and bound actor. The CLI
   prints a pasteable `kata federation join ...` command using the binary name
   that invoked `enroll`, and containing the generated token and actor; treat
   that command as secret-bearing material.

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
     --actor wesm \
     --push
   ```

   `join` fetches the hub project UID and replay horizon from the hub using the
   enrollment token, so the hub must be reachable at join time and the token
   must include `pull`. The metadata flags (`--hub-project-uid`,
   `--replay-horizon`, and `--baseline-through`) remain available as explicit
   overrides for scripts. The command creates a spoke replica project bound to
   the hub project UID and replay horizon, stores the hub URL/project/token in
   the local federation credentials file, and enables push only when `--push` is
   present.

   If the spoke already has a non-federated project that should join the
   hub under the same name, opt in explicitly with `--adopt-existing`, which
   requires `--push`. For differently named spoke projects, create the
   enrollment with `kata federation enroll --adopt-existing`, then edit the
   printed join command's `--project` value:

   ```bash
   kata federation join --project fedlab \
     --hub-url http://<private-hub-ip>:7787 \
     --hub-project-id 1 \
     --token ... \
     --actor wesm \
     --push \
     --adopt-existing
   ```

   Adoption works even when the spoke project name differs from the hub project
   name; choose the spoke project with `--project` and the hub with the hub
   selector. The enrollment must have been created with `--adopt-existing` so
   the hub can accept adoption snapshot authors:

   ```bash
   kata federation join --project shared-foo \
     --hub-url http://<private-hub-ip>:7787 \
     --hub-project-id 1 \
     --token ... \
     --push \
     --adopt-existing
   ```

   Adoption preserves the current state of local issues, including closed and
   soft-deleted issues, comments, labels, metadata, priority, owner, and
   links. It does not preserve the old local event history.
   Instead it removes those pre-adoption local events and queues fresh snapshots
   for the hub with links embedded in the snapshot payloads. A cross-project
   edge materializes after both endpoint projects reach the same hub; until
   then its event is retained and the edge is absent. After
   adoption, existing issues are ordinary federated spoke issues. Future edits
   remain local-first; use live hub leases only when you want exclusive
   coordination.

   Adoption is a current-state cutover rather than a history merge. Operators
   who need the pre-adoption event timeline for audit or rollback context should
   run a scoped JSONL export before `--adopt-existing`, for example
   `kata --project <project> export --output <path>.jsonl`. The federated event
   stream starts from the adoption snapshots; kata does not currently retain a
   separate in-product archive of pre-adoption events.

Enrollment capabilities and local spoke behavior are separate knobs:
`--capabilities pull,push,lease` on the hub says what the token may do, while
`--push` on the spoke says this replica should actually push local-origin events
back to the hub. If the token has `push` but `join` is run without `--push`, the
spoke remains pull-only and the CLI prints a warning.

The transport routes use enrollment bearer tokens. Local operator routes,
including `kata federation status`, quarantine skip, force-release, and purge,
use the normal daemon local/admin auth surface. Federated hub purge uses the
same live-lease conflict gate as other issue mutations; an operator can
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
The hub rejects requests that omit it, send a non-positive value, or report a
schema newer than the hub's own schema. This is intentionally conservative: an
old hub must not blindly materialize events from a newer spoke whose payloads or
fold semantics may have changed.

### Forward And Backward Compatibility

Federation compatibility is asymmetric during rolling upgrades:

- **Older spoke, newer hub:** push is accepted when the spoke's
  `schema_version` is positive and not newer than the hub. The hub remains
  responsible for preserving ingest compatibility with older event envelopes and
  payloads that it still accepts.
- **Newer spoke, older hub:** push is rejected with
  `unsupported_federation_schema`. The spoke treats that as transient version
  skew: it records the sync error, leaves the push cursor unchanged, does not
  create quarantine, and retries on later sync passes. Once the hub is upgraded,
  the same pending events are sent again.
- **Malformed schema declarations:** missing schema versions fail request
  validation, and explicit non-positive versions fail with
  `invalid_federation_schema`. These are protocol errors, not rolling-upgrade
  skew.

This compatibility policy does not extend to legacy v0.9 directional unlink
payloads. `issue.unlinked` events for `blocks` and `parent` must carry
`link_from_uid` and `link_to_uid` in storage orientation. The hub rejects a
payload that omits them; it does not infer orientation from current graph state
or rewrite the event. This is a deliberate pre-1.0 contract boundary, not a
transient version-skew condition.

Upgrade hubs before push-enabled spokes when rolling out a new federation
schema. If an older build already quarantined a batch because of transient
schema skew, upgrade the hub and then restart each spoke on a build with this
compatibility behavior. The next sync auto-releases the stale schema-skew stop
marker without advancing the push cursor, then re-sends the same events.

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

### Cross-Project Link Convergence

A missing link peer is not a permanent validation failure. The hub accepts the
event, advances the push cursor, and keeps the edge absent until both endpoint
issues are materialized inside one compatible federation group. Hub projects
on one daemon form a group. Spoke projects form a group when they are enabled
and their stored hub URLs have the same scheme, lowercase hostname, and
effective port.

Link folding and reconciliation run across the complete group whenever any
member project materializes. The event log therefore acts as durable pending
state: whichever endpoint arrives second causes the edge to appear. An
unfederated peer, a peer on a different hub origin, or a peer that never arrives
stays absent without blocking later events. Clients must enroll both endpoint
projects through the same hub to project the edge locally. Different DNS names
and IP aliases are deliberately different spoke groups even if they route to
the same daemon.

## Leases And Write Gates

Leases are hub-authoritative. A spoke forwards acquire, renew, release, and status
requests to the hub using an enrollment token with `claim` capability. The hub
derives `holder_instance_uid` from that enrollment token; clients provide the
human-readable holder string.

Leases exist for agent coordination, not for general edit permission. An agent
acquires a lease to signal "I am actively working this issue" and to get
temporary exclusivity against other non-comment mutations while the lease is
live. Status and audit surfaces can then show who is actively working. A lease
does not replace durable `owner`, does not serialize all collaboration, and is
not required before ordinary federated edits.

For federated projects, ordinary edits of existing issues are local-first and
converge by LWW. Creating new issues is also local-first. A lease is optional
coordination: when another holder has a live lease on an affected existing
issue, non-comment mutations are denied until the lease is released or expires.
Comment creation and comment body edits bypass leases because they remain
comment-level collaboration and maintenance actions rather than leased issue
work.

Spokes refresh cached lease state before checking exclusivity when online.
When offline, cached hard leases can still be used as a continuity hint, but
they are not proof that exclusivity still holds. Timed leases expire by hub
time and stop blocking edits once expired.

The hub checks pushed work against the live lease state at ingest time. Work
that conflicts with another holder's live lease is not dropped; the hub records
`claim.violated`. Work on unleased issues is normal and is not a violation.
For link work, the audit checks every materialized endpoint in the compatible
federation group and records the violation in the endpoint's owning project.
Timed leases are expired in each affected owning project before that check.
This is best-effort, not a causal proof that the work was unauthorized when
originally performed. An offline edit that was covered at edit time can arrive
after another holder acquires a lease and be marked violated because the hub
checks current state during ingest.

`kata show` surfaces the current lease and recent unresolved lease violations
for a federated issue. `kata federation status` shows project-level counts and
recent violation summaries.

## Operator Commands

```bash
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url> \
  --actor <actor> [--allow-insecure]
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> --actor <actor> [--push]
kata federation join --project <existing-project> --hub-url <url> --hub-project-id <id> \
  --token <token> --actor <actor> --push --adopt-existing
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation leave <project> [--delete [--force]] [--local-only] [--hub <name>]
kata federation status
kata federation status --json
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease release <issue-ref>
```

`kata federation enrollments list` audits hub-side spoke grants without showing
token hashes or plaintext tokens. `kata federation revoke <enrollment-id>` marks
an enrollment inactive so the token no longer authorizes pull, push, or lease
transport calls.

`kata federation leave <project>` is the spoke-side inverse of `join`. It
revokes the matching hub enrollment, then detaches the local spoke back to a
standalone project — removing the binding, sync-status, quarantine, and claim
rows and deleting the daemon-local hub credential in one daemon-route operation
(`POST /api/v1/federation/replicas/{id}/actions/leave`). Leaving is revoke-first,
so a hub failure leaves local state intact for a clean retry; it is idempotent
(a project with no binding reports "already standalone", and the daemon route
still runs so a stale hub credential left by a partial leave is deleted on the
retry). `--delete` also archives the
now-standalone project (reversible via `kata projects restore`), with archive
eligibility checked before any detach so an open-issue refusal cannot leave the
project half-torn-down; `--force` overrides that refusal. An advisory daemon
preflight (`preflight=true` on the leave route) runs before the hub revoke for
every hub-contacting leave — detach refusals (spoke-role drift, vanished
project, actor validation) and the archive's open-issue check alike — so a
predictable local refusal cannot strand a hub-revoked, locally bound spoke. Retrying an
archive-leave whose archive already committed resumes rather than refusing:
the already-archived step is skipped (that call reports `archived=false`) and
any surviving detach and credential cleanup still run. The join-time
`--allow-insecure` transport opt-in is persisted on the binding (schema 15)
and surfaced in status as the union with the credential's copy (only when
that credential names the binding's hub URL and project); the leave hub
client further unions a same-origin catalog entry's `allow_insecure` and the
explicit `leave --allow-insecure` flag, so a credential loss cannot strand a
plain-HTTP overlay hub's enrollment behind the plaintext-bearer refusal. `--local-only` tears
down locally when the hub is unreachable, leaving the enrollment token to be
revoked manually. A binding-aware guard stops an in-flight sync pass from
re-creating `federation_sync_status` rows after a leave, and quarantine
recording no-ops the same way so a poisoned push response landing after the
leave cannot recreate active quarantine state for a standalone or archived
project. A user-facing project purge for the `--delete` path is deferred.

Leave keeps the project's shared identity (its UID stays the hub project's
UID), which makes the round-trip contract hold: **enroll → leave → enroll
works**. A `join` that targets a hub project whose UID is held by an unbound
local project of the same name rebinds it (rejoin): pull restarts just below
the replay horizon and event-UID dedup absorbs the overlap, while a
push-enabled rejoin restarts the push cursor at 0 so the hub deduplicates
re-offered events and absorbs edits made while standalone. The same path
recovers a partially-failed join (project created, binding never written). A
join naming a different project refuses with
`federation_rejoin_name_mismatch`, naming the holder; an archived holder must
be restored first. In the TUI, adoption (history-rewriting) is additionally
gated behind a typed-confirmation screen stating the local-INTO-hub
relationship, and the enrollment recovery screen prints the actual join error
(the token-expiry hint is reserved for 401/403).

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
it with status and release it for retry after fixing the root cause:

```bash
kata federation quarantine retry <id> \
  --confirm "RETRY FEDERATION BATCH <id>" \
  --reason "hub upgraded"
```

Retry is only defined for push quarantines. It marks the quarantine resolved
without advancing the push cursor, so the same local events are sent again on
the next sync. Stale push quarantines whose stored error is the legacy
`unsupported_federation_schema` response are released automatically on sync;
operators use `retry` for other fixed push quarantines.

Older builds may have quarantined a valid batch because a cross-project link
peer had not reached the hub. After upgrading the hub and spoke, use `retry`,
not `skip`. The original events are resent without advancing the cursor and
the edge materializes when both endpoint projects reach the upgraded hub.

Operators can also intentionally skip a quarantined batch:

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

## Design Rationale

The operational sections above describe what federation does. This section
records why it converges and why the lease model is shaped the way it is —
the reasoning that is not recoverable from the behavior alone.

### Why Federation Converges

For federated projects the event log is the source of truth and the
issue/comment/label/link/metadata tables are a deterministic projection — a pure
`Fold` over the events. All mutable state is modeled as a CRDT: scalar fields and
metadata leaves are last-writer-wins registers, labels and links are per-element
LWW sets with tombstones, and comments are a grow-only log keyed by UID. Given a
deterministic total order over the retained event set, the fold is
commutative-after-sort: any two nodes holding the same events compute identical
state regardless of arrival order, so there is no irreconcilable conflict to
resolve by hand. A late event with a lower clock simply loses to an
already-applied higher-clock write for the same field, without being discarded —
it stays in the audit log and remains visible. This is property-tested by
shuffling arrival order and asserting a byte-identical projection.

Each mutation appends a complete event and applies its projection delta in one
transaction, never "mutate the row, best-effort event afterward." That keeps the
local write path and the replay path from drifting apart.

### The Hybrid Logical Clock And Merge Order

The total order is `(hlc, origin_instance_uid, event_uid)`. The HLC is a hybrid
logical clock — a `(physical_ms, counter)` pair stamped when an event is emitted
and advanced past the clock of every foreign event applied — so causally later
work always sorts after what it saw. The local autoincrement event id is only a
delivery cursor, never the merge order, and wall-clock `created_at` is kept for
display and digests but is too skew-prone to order merges. An immutable
`content_hash` over each event's identity and canonical contents lets a duplicate
UID with the same hash be treated as a harmless retry, while a duplicate UID with
a different hash is rejected as an integrity violation before it can be folded.
An earlier groundwork column, `origin_seq`, was deliberately **not** revived as
the merge clock; if gap detection is ever needed it can return as an optional
per-origin continuity counter, orthogonal to the HLC.

### The Drift-Guard Invariant

The guarantee that event-truth can be trusted rests on one invariant: for every
non-federated project, which still uses the direct-write path,
`direct_write_projection == Fold(project_events)`. It is asserted at **project**
scope, not per issue, because links and project metadata are project-scoped — an
event on one issue can affect another issue's projection — so a per-issue
comparison would be too narrow to catch a missing field. Holding this invariant
across every mutation type proves events are replay-complete and keeps the
direct-write and replay paths from diverging. It is the gate that had to pass
before federation could be turned on.

### Replay-Complete Events

Before federation, events did not carry enough to reconstruct state:
`issue.updated` was written with an empty payload and `issue.created` omitted the
title and body, so folding from zero was impossible. Federation required every
mutation event to carry the resulting state of what it touched, and added a `uid`
to comments so they have a stable identity to fold on. Because pre-federation
history is not replay-complete, enabling federation emits a baseline of
`issue.snapshot` events at a replay horizon, captured from one consistent
single-writer view; replicas fold forward from that horizon and keep older events
as audit-only.

### Metadata Merge Semantics

Issue and project metadata is a per-key map, and unreserved keys may hold
arbitrary nested JSON. To salvage concurrent edits to different sub-fields of the
same key, the fold diffs each metadata event's per-key `{from, to}` down to
JSON-pointer paths and resolves each leaf as an LWW register. Deletion keys off
**structural absence, never off JSON `null`**: a path present in `from` but
absent in `to` is a tombstone — covering the whole subtree if it was an object —
while a leaf whose value is `null` in `to` is a real null value and is preserved.
Arrays are atomic leaves, with no element-level array CRDT, so a checklist is
replaced wholesale. The only place `null` acts as a marker is the top-level
"clear this key" convention, which surfaces as structural absence anyway.

### Why Leases Are Optional Coordination, Not An Edit Gate

A lease answers "who is actively working this issue right now," which is a
different question from `owner` ("who is responsible"). Leases exist to help
agents avoid double-work; they are deliberately **not** a prerequisite for
editing a federated issue. This is the point the implementation and docs were
realigned to after some early drift toward treating a lease as a write gate.

The reason is the local-first goal. Edits are asynchronous and may happen
offline, so requiring a live lease before every edit would force a synchronous
hub round-trip and trade away the offline editing that federation exists to
preserve. Ordinary edits are therefore always local-first and converge by LWW. A
lease adds only temporary exclusivity against *conflicting* non-comment work by
another holder while it is live — the hub guarantees at most one live holder.
Unleased work is normal and is never a violation; pending or expired leases do
not block edits; and comment creation or body edits always pass as comment-level
work.

Because the hub cannot reject a mutation that already happened offline, it does
not try to. When pushed work conflicts with another holder's live lease the hub
keeps the data and records a best-effort `claim.violated` annotation rather than
dropping anything. Lease state — especially timed-lease expiry after renewals —
is authoritative from the hub, not folded from events, so a spoke treats cached
lease state as a hint and confirms against the hub before relying on
exclusivity, never as proof that exclusivity still holds.

### Rejected And Deferred Alternatives

Hard purge of federated history is hub-admin-only and establishes a reset
boundary rather than being routine, because deleting replay history fights
event-truth. Recurrence materialization stays hub-only: running the scheduler on
multiple spokes would each materialize duplicate occurrences, since the
occurrence-uniqueness constraint is per-database and UIDs differ across nodes.
Issue moves are allowed only between two federated projects on the same hub; a
move across a federation boundary has no coherent single event log spanning it
and is disallowed. Deferred work includes element-level array CRDTs for metadata,
HLC skew-guard thresholds, a snapshot fast-path for very large projects, and
federated authoring of recurrences from spokes.

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

Use a shared daemon when every write must be serialized through one online
authority and stale offline lease state is unacceptable.

### Lease Violation Signals Are Best-Effort

Violation annotation checks live hub lease state at ingest time, not historical
lease state at the event's HLC timestamp. It is an operational signal for
"work arrived while another holder currently has a live lease", not a complete
audit proof of user intent or causal authorization.

Use a shared daemon when lease compliance must be decided at the exact write
time.

### Poisoned Push Batches Require Operator Choice

A validation error, hash conflict, or other permanently poisoned push batch can
quarantine push. Until an operator fixes the data and retries the batch, or
skips the batch, push remains blocked and reset may be blocked. Retrying is
non-stranding: the same local events are re-sent. Skipping is explicit data
divergence: the hub will not receive those local events.

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

## Historical Design Context

This file is the canonical description of implemented federation behavior, its
design rationale, and its limitations. The original phased design spec and the
per-phase implementation plans were folded into this document and the
[architecture](architecture.md) and [data model](data-model.md) notes after the
work shipped; the superseded drafts remain available in version control.
