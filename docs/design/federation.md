# Federation architecture

Federation shares selected projects between daemons. Each daemon keeps its own
database, accepts local work and synchronizes changes in the background. It
favors local availability over immediate agreement between replicas. A
non-federated installation does not need federation credentials or network calls.

Use [Share projects between hubs](../guide/project-relays.md) for setup and the
[operator reference](../operations/federation.md) for commands and recovery.
These notes explain identities, delivery, convergence and storage boundaries.

## Project topology

Each project has one **root authority**. A daemon connects directly to that
root or through a **relay**, which stores a local replica and serves it onward.
A **leaf** stores a replica without serving downstream daemons.

```text
Root authority <----> Relay <----> Leaf
       one project UID across all three local databases
```

Roles belong to projects. One daemon can root one project, relay another and
keep a third private. A relay is a spoke upstream and a hub downstream; it does
not become a new root authority for the project.

Events keep their source identity across hops. A hop acknowledgement means the
next daemon stored a delivery. A signed root receipt means the root accepted
the event and its accountable actor. When purge or compaction makes ordinary
replay incomplete, a root-signed checkpoint supplies recovery state.

The storage backend is local to each daemon and is not part of federation
identity or enrollment. SQLite and PostgreSQL daemons use the same event,
snapshot, cursor, credential, and lease protocols, so every hub/spoke backend
pair must interoperate without backend-specific flags. Real-daemon CI covers
SQLite/SQLite, SQLite/PostgreSQL, PostgreSQL/SQLite, and
PostgreSQL/PostgreSQL pairs through initial snapshot, bidirectional mutations,
idempotent retry, lease-authorized edits, restart recovery, and final
visible-state convergence.

## Terms

- **Hub**: the immediate upstream daemon. Each hub owns its downstream
  enrollments; the root owns lease arbitration, purge/reset authority and
  signed acceptance of the project history.
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
- **Lease**: an optional reservation coordinated by the root. A live lease
  blocks other holders' non-comment mutations; ordinary editing does not
  require acquiring one. Storage and audit events use the name `claim`.
- **Quarantine**: a local operator stop marker for a poisoned federation batch.
  It prevents hot-looping and requires an explicit operator decision.
- **Root receipt**: a root-signed record of event acceptance and accountable
  attribution, separate from an immediate-hop acknowledgement.
- **Checkpoint**: a root-signed snapshot, proofs and manifests used for reset.
- **Epoch**: a generation of one binding's delivery streams. Reset prepares a
  new epoch while the old one remains available until activation.

## Tokens And Trust Boundaries

Kata federation uses two different bearer-token systems. They are intentionally
separate:

- **Daemon API tokens** identify clients talking to a daemon's normal API. They
  are configured with `KATA_AUTH_TOKEN` or `[auth].token` and managed with
  `kata tokens ...` when token identity is required. Local operator commands
  such as `kata federation status`, `kata federation quarantine skip`, and
  hub-local force-release use the default daemon's normal API auth surface.
  Federation enrollment and hub revoke requests instead use a target-scoped
  hub admin credential from `--hub-token-env` or a daemon-catalog entry for that
  same hub origin. They never send the default daemon's global token to the
  hub. `kata federation enroll` selects the hub with `--hub-url` while the CLI's
  default daemon remains the spoke being enrolled.
- **Federation enrollment tokens** authorize one spoke to call hub federation transport routes
  for an enrolled scope and capability set. They are created by bridge connection or direct
  enrollment, stored hashed on the hub, and stored plaintext only in the spoke federation
  credentials file. They authorize pull, push, join metadata fetches and forwarded lease
  actions. Each enrollment is bound to an account. Direct-spoke transport requires pushed event
  actors and forwarded lease holders to match that account. Negotiated relays preserve the
  original event actor and origin across hops; the authenticated hop and root receipt establish
  accountable authority. They are not general daemon API tokens. A relay enrollment candidate
  may be retained with `relay_enrollment_pending = true` for an exact enrollment retry. Its
  status is `enrollment_pending`; sync, forwarded claims and shared-project embedding
  generation wait until the negotiated binding activates that credential.

The [lease reference](../operations/federation.md#leases-and-write-gates)
explains request authority for direct spokes and relays. Enrollment credentials
are transport grants, not credentials for local CLI or API commands.

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
  The relay reset protocol (what a reset replaces and keeps, which deliveries
  a forward supersedes, which work blocks an install, how a checkpoint is
  translated per hop) is `RelayResetProtocol` here; the SQLite and PostgreSQL
  stores supply only SQL, locking and error mapping through
  `RelayResetBackend`.
- `internal/daemon`: daemon HTTP routes, enrollment-aware transport auth,
  local/admin operator APIs, lease forwarding, purge/reset behavior, and status
  responses.
- `internal/client`: generic first-party daemon discovery, auto-start, bearer
  attachment, Unix-socket HTTP clients, TCP remote selection, and SSE clients.
- `internal/federation`: spoke-side hub HTTP client, pull/push runner, pending
  lease retry, failpoints, and federation runner tests.
- `cmd/kata`: federation operator CLI, lease CLI, daemon runner startup, and
  normal CLI client wiring.
- `e2e` and `docker/federation`: multi-daemon, mixed-storage matrix, randomized
  stress, failpoint, and Docker Compose smoke coverage.

## Setup Model

Bridge setup requires two kinds of authority: an owner administers the receiving
daemon, and an ordinary upstream account authorizes access to the selected hub
project. The resulting transport grant is restricted to that project and peer.
Parent-token expiry or revocation and loss of project membership stop transport;
[explicit reconnect](../operations/federation.md#connect-a-project-through-the-daemon-catalog)
can bind the retained grant to a new live token for the same account.

Direct `enroll`/`join` uses the event-cursor protocol and hub-owner administration credentials.
An account token cannot mint a legacy enrollment that would outlive its authority. See
[Token boundaries](../operations/federation.md#token-boundaries) and
[the direct-spoke setup](../operations/federation.md#hub-setup).

Adopting an existing project is a current-state cutover, not a history merge. It preserves
current issues, comments, labels and links through fresh snapshot events and replaces the
pre-adoption event timeline. A direct-spoke enrollment's historical-author permission spans the
entire initial baseline, including retries, and is consumed only after its terminal chunk
commits. Negotiated relay adoption rejects cross-project links. Export the old timeline before
adoption when it is needed for audit. The
[adoption procedure](../operations/federation.md#adopting-an-existing-project) owns the commands
and confirmation rules.

## Relay delivery and recovery

Negotiated relays use three independent streams: **events**, **receipts** and
**artifacts** (portable embedding vectors). Each binding and epoch has its own
inbox, outbox and cursors. The immediate receiver accepts a batch and its onward
delivery intent in one transaction. Acknowledgements identify an emitted
endpoint by sequence and digest, so retries can replay retained bytes without
creating another source event.

A leaf may push its own events. A relay may also push its descendants' events,
but cannot push work originating at the root or above its receiving hub.
Root-origin changes flow down authenticated upstream connections. Source actor
labels remain distinct from root-certified accountable attribution.

Pending local work stays pending until its root receipt returns. Acknowledgement
by an intermediate relay is insufficient for destructive lifecycle changes.
This distinction also applies when compaction has removed the local event row:
the retained outbox still carries delivery intent.

The runner gives projects bounded turns and immediately reschedules those with
backlogs. Embedding generation checks upstream metadata without running sync
itself. See [sync scheduling](../operations/federation.md#sync-scheduling) and
[embedding transfer](../operations/federation.md#sharing-embedding-vectors)
for budgets, producer selection and retry behavior.

### Checkpoint installation

A reset replaces replay history with a root-signed checkpoint. The checkpoint
contains issue and cron state, retained history, creation proofs and artifact
manifests; vector bytes travel separately. A relay forwards the original root
manifest and snapshot unchanged, adding a translation for its immediate child's
stream positions.

The issuer prepares a new epoch while keeping the current one live. The
receiver drains pending work, validates the checkpoint and installs it
transactionally. Its first authenticated request in the new epoch activates
that epoch upstream. Lost activation replies resume the installed state;
post-capture writes remain owed.

A relay prepares checkpoints for its downstream enrollments in the same
transaction as its own installation. Event and artifact deliveries represented
by the checkpoint are superseded; unacknowledged receipts remain owed.
Installation checks pending upstream deliveries, local events awaiting root
receipts, open lease requests, downstream event acceptance, quarantine and
external-root bindings before replacing data. Checkpoint receipts can satisfy
the local and downstream event checks; see the full
[reset blockers](../operations/federation.md#reset-recovery).

Existing downstream enrollments alone do not block installation. A child's
credential suspension also leaves its prepared checkpoint intact without
blocking the parent; that child must restore access to download or activate it.
Unaccepted event deliveries owed to the child remain blockers. The
[reset recovery](../operations/federation.md#root-resets-on-a-relay-with-downstream-enrollments)
explains how to resolve them.

### Retention and capacity

Delivery bodies remain for the lifetime of their binding, including after
acknowledgement and compaction. This supports exact retries and late receipt
attribution, at a storage cost proportional to traffic and downstream bindings.
Ending a binding removes its transport rows; recorded provenance remains.

Checkpoints currently have a 64 MiB whole-project limit. Ordinary writes do not
reserve reset capacity, and receipts are never removed. New enrollment and reset
can therefore become impossible even while ordinary batch sync still works.
The [capacity reference](../operations/federation.md#relay-checkpoint-capacity)
owns measurement, warnings and operator options. There is no streaming reset
protocol in this implementation.

## Schema And Upgrade

The original federation rollout moved storage from schema `11` to `12`.
This is historical context, not the current schema version. See
[storage upgrades](../operations/backup-restore.md) for the current workflow.
That rollout used the JSONL cutover path. The v11
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

`GET /api/v1/instance` advertises `relay_protocol_version`,
`provenance_protocol_version`, and `embedding_artifact_protocol_version`
independently of `schema_version`. These fields describe wire support and
currently report version 1. Artifact manifests and complete vector transfers
use the same artifact protocol. Older daemons omit these fields; omission does
not advertise relay support. A hub that requires request signing also reports
`federation_signing_required: true`, so bridge connect can refuse an unsigned
bridge before saving a credential (see
[Native federation request signing](federation-request-signing.md)).

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
or rewrite the event. This is a deliberate legacy compatibility boundary, not
a transient version-skew condition.

Upgrade hubs before push-enabled spokes when rolling out a new federation
schema. If an older build already quarantined a batch because of transient
schema skew, upgrade the hub and then restart each spoke on a build with this
compatibility behavior. The next sync auto-releases the stale schema-skew stop
marker without advancing the push cursor, then re-sends the same events.

## Pull Replication

This section describes direct-spoke event cursors. Negotiated relays use the
[delivery protocol above](#relay-delivery-and-recovery).

A spoke polls the hub transport route for events after its pull cursor. It
applies hub events in order, deduplicates by event UID and content hash, folds
portable payloads into the local projection, and advances its pull cursor only
after successful application.

SQLite direct spokes prepare pulled issue histories and projection differences
before taking the writer lock, then update only the touched issues and changed
comments, labels and links. Ordinary local issue push echoes need no materialization.
Project-wide events and events without a safe issue scope use a complete fold;
link changes still reconcile the federation group's link history. Preparation
retries against a fresh snapshot after concurrent commits, with the same bounded
locked-preparation fallback as hub push ingest.

Remote insertion commits before materialization. The pull cursor advances last,
so an interrupted page is replayed and its already-stored remote events remain
materialization candidates. A reset replays the baseline through the same path.
Local adoption snapshots, global events and echoes without an envelope issue UID
remain repair candidates until their page's cursor advances. They can be reset
replays or have a canonical payload already stored before an interruption;
push acknowledgment alone does not prove their projections were repaired.
This repair does not repeat notifications for acknowledged pushes.
Other storage backends retain full pull folds.

Baseline snapshots bridge the fact that pre-horizon history is not
replay-complete. Events after the baseline are ordered by their event/HLC data
and materialized normally. If the hub reports `reset_required`, the spoke
refreshes hub metadata and re-bootstrap from the current horizon.

## Push Replication

A direct, push-enabled spoke scans for local-origin events above its push cursor and
sends them to the hub as an all-or-nothing batch. The hub authenticates the
enrollment token, verifies project scope and capability, checks that each event
belongs to the bound spoke origin, verifies the spoke's declared schema version,
deduplicates same-hash retries, rejects same-UID/different-hash conflicts,
materializes the batch, and returns the advanced push cursor.

SQLite hubs prepare the event fold and projection differences in the read phase
of a deferred transaction, before the project lock takes the writer lock. Like
incremental direct-spoke materialization, this transaction reads before joining
the SQLite [write queue](data-model.md#storage-backends); it joins at the project lock.
The write phase inserts accepted events and updates only changed issues,
comments, labels, links and status-intent pointers. Validation failures still
roll back the entire batch.

A commit anywhere in the database during preparation invalidates the snapshot.
SQLite then reports busy contention, and the hub retries the whole batch against
a fresh snapshot. The first two attempts prepare outside the writer lock. Later
attempts take the project lock before preparing, so steady unrelated writes
cannot make every attempt fail. Those attempts hold the writer for the whole
preparation, as full materialization does.

For issue-only batches, preparing outside the lock keeps writer time independent
of unrelated event history and untouched projection rows. Batches with cron
events also materialize cron definitions and run observations in the write
phase. Total preparation still reads and folds project history, plus
link-bearing group history when links can change. Existing full-text search
triggers can aggregate comments on a changed issue, and claim auditing can
expire claims. Custom transaction fences that write may acquire the lock before
preparation; native enrollment fences only read. Explicit full materialization
remains available for recovery.

If the response is lost after the hub commits, retrying the same batch is safe:
the hub treats fully duplicated same-hash batches as successful and returns an
advanced cursor. Permanent validation failures or hash conflicts record a
quarantine on the spoke instead of retrying forever.

### Cross-Project Link Convergence

Direct-spoke federation can carry cross-project links; negotiated relay projects reject them.
For direct spokes, a missing link peer is not a permanent validation failure. The hub accepts
the event, advances the push cursor, and keeps the edge absent until both endpoint issues are
materialized inside one compatible federation group. Hub projects on one daemon form a group.
Spoke projects form a group when they are enabled and their stored hub URLs have the same
scheme, lowercase hostname, and effective port.

Link folding and reconciliation run across the complete group whenever any
member project materializes. The event log therefore acts as durable pending
state: whichever endpoint arrives second causes the edge to appear. An
unfederated peer, a peer on a different hub origin, or a peer that never arrives
stays absent without blocking later events. Clients must enroll both endpoint
projects through the same hub to project the edge locally. Different DNS names
and IP aliases are deliberately different spoke groups even if they route to
the same daemon.

## Leases And Write Gates

The root arbitrates leases, while replicas accept ordinary edits locally.
A lease coordinates exclusive non-comment work; it is not required for every
edit. Direct spokes derive the holder from their enrollment, and relays retain
a device-specific holder identity through to the root.

This split means ingest can only audit the lease state it sees when work
arrives. It records conflicts as `claim.violated` without discarding the work;
it cannot prove whether an offline edit was authorized when made. The
[operator reference](../operations/federation.md#leases-and-write-gates) owns
lease commands, write gates, cached reads, expiry and violation reporting.

## Operator Commands

The [operator reference](../operations/federation.md#operator-commands) owns the
command inventory. Use [status](../operations/federation.md#operator-commands)
to inspect pending work, receipts, quarantines and reset blockers. Setup and
recovery must use the API or CLI rather than editing database rows or cursors.

## Quarantine

Permanent validation failures retain the rejected batch. Direct spokes quarantine push;
negotiated relays can quarantine either push or pull, and an active relay quarantine stops
automatic sync for that project. Retry resolves the stop marker without advancing a cursor or
changing the source identities, so the same data is offered again after the cause is fixed.
Transient schema skew is retried after the upstream is upgraded. Retry supports legacy push
quarantines and negotiated relay quarantines. A legacy pull retry returns
`federation_quarantine_retry_unsupported`.

Only legacy push quarantine supports an intentional skip. It advances past the
batch without delivering that work. Negotiated relays reject skip because a
legacy cursor change cannot satisfy their acknowledgement and root-acceptance
rules. See [Quarantine](../operations/federation.md#quarantine) for commands,
confirmation phrases and recovery choices.

## Purge And Reset

Negotiated relays use [signed checkpoint recovery](#checkpoint-installation).
Direct-spoke recovery uses the replay horizon described below.

Hard purge runs at the authoritative hub for federated projects. A spoke rejects
hard purge with `federated_admin_required`. A hub purge uses the normal local/admin daemon
auth surface, the existing exact confirmation string, and the same live-lease
gate as other issue mutations.

When a hub purge removes replay history, it records a reset boundary in
`purge_log` and writes a fresh federation baseline for the remaining project
state. A spoke whose pull cursor is below that boundary receives
`reset_required` and re-bootstrap from the current federation horizon. A
push-enabled spoke refuses to reset while it has unaccepted local-origin events
or an active quarantine. `kata federation status` reports these reset blockers.

## Cron Replay And Compatibility

Cron jobs and workflows replicate as complete documents, including deletion and
restoration. The existing total HLC order chooses one whole document; concurrent
edits never combine fields from different versions. Baseline snapshots retain
the original winning definition event UID and HLC. Enabling federation,
adopting a project, joining a second peer, and rebuilding after a purge all carry
pre-existing cron definitions and portable run history.

Upgrade hubs before publishing cron. Federation clients advertise
`X-Kata-Event-Features: cron_v1`. Successful metadata, event-poll, and
ingest responses advertise server support in that same header. A project's
`X-Kata-Required-Event-Features: cron_v1` header instead means that its
history requires the feature. This requirement latches after the first event
requiring `cron_v1` and survives deletion of its definitions. Missing support causes
HTTP 409 `unsupported_event_features` before returning events or a reset cursor,
or accepting a push. An unknown required response feature stops the new client
before it accepts any response cursor. An empty requirement from an unused old
hub does not prove support: cron publication first fetches metadata and
requires the server's positive support advertisement.

Wire envelope compatibility is independent of database schema upgrades.
`db.FederationEventWireVersion` explicitly maps the supported legacy publication
events to envelope version 30 and cron events to version 31 plus
`cron_v1`. Future event types require a new mapping before publication;
a future storage version does not automatically change existing wire contents.
This keeps ordinary traffic compatible with actual schema30 daemons, whose
ingest handler refuses newer envelope versions before inspecting their events.

`Storage.ReadFederation(ctx, db.FederationReadParams)` captures the project,
binding, required feature latch, purge boundary, baseline and event cursor in one
backend transaction. It also checks the features of the actual event page.
The result is a consistent read. Scheduling and execution remain local to the
plugin.

Run observations contain bounded evidence: the run UID, job and workflow definition
references, occurrence, issue binding, actor and teammate, timestamps, reported
status and summary. Event HLC and event UID order observations, while baseline
envelopes preserve their original observation clocks. Separate executions of
one occurrence have distinct run UIDs and are accepted independently. Retries
of the same observation use the existing run identity and idempotency behavior.
Raw logs, secrets, filesystem paths and process or session handles stay local.

Ordinary spoke pushes publish definitions and run observations. Adoption,
replica reset and backup/restore preserve portable definitions and history
through the existing federation and snapshot mechanisms. Run history does not
reserve an issue or create local process state.
Definition authorship comes only from the create event or an approved
adoption snapshot; folds ignore the author carried by later edits. A pushed
create, or a snapshot outside an approved adoption baseline, is refused for a
definition the hub already holds. An edit that introduces a definition the hub
has not seen must carry the bound actor.

A run's actor is its immutable identity: whoever first observed it. Each
observation event is attributed to the writer's effective actor, which on a
push spoke is the bound actor. A later observation continues the run when the
requested actor, or the bound actor substituted for it, equals the run's
stored actor. So a run started before push enablement or adoption is continued
by requests that name its original actor; a request that names another actor,
including the bound actor itself, is an identity conflict. The exception is a
run whose original actor is the bound actor: every request on the push spoke
is substituted with that actor, so any request continues it. An approved adoption baseline preserves run actors with or without
the author grant. Outside a baseline, a pushed observation either starts a run
as the bound actor or continues a run the hub already holds under the same
actor.

Pages may contain a job before its workflow or a run before its definition. These
records remain visible. `db.CheckCronDependencies(ctx, query, projectID,
jobDefinition)` returns descriptive errors for missing, tombstoned or
cross-project workflow/issue dependencies and preserves database errors. Local
definition writes use the same helper. The plugin owns activation, overlap,
catchup, retries, recovery and process/session management, using ordinary Kata
notify for exact-recipient inbox delivery. This replay layer introduces no
scheduler or process launcher.

## Recurrences, Merge, And Other Boundaries

Recurrences remain hub-owned for federated projects. Spoke recurrence mutation
is blocked rather than partially synchronized. Project merge refuses projects
with federation bindings because local integer identities, remote UIDs, and
federation cursors would otherwise become ambiguous.

Federation does not add a global user registry. Actor strings remain audit
metadata. Origin instance UIDs disambiguate daemon identity.

## Design Rationale

The operational sections above describe what federation does. This section
records why it converges and why the lease model is shaped the way it is:
the reasoning that is not recoverable from the behavior alone.

### Why Federation Converges

For federated projects the event log is the source of truth and the
issue/comment/label/link/metadata tables are a deterministic projection, a pure
`Fold` over the events. Replicated scalar fields and metadata leaves use
last-writer-wins registers. Labels and links use per-element LWW sets with
tombstones. Comments retain stable UIDs and support clock-ordered body edits.
Leases, credentials and project access have separate authority rules. Given a
deterministic total order over the retained event set, the fold is
commutative-after-sort: any two nodes holding the same events compute identical
state regardless of arrival order, so there is no irreconcilable conflict to
resolve by hand. A late event with a lower clock simply loses to an
already-applied higher-clock write for the same field, without being discarded:
it stays in the audit log and remains visible. This is property-tested by
shuffling arrival order and asserting a byte-identical projection.

Each mutation appends a complete event and applies its projection delta in one
transaction, never "mutate the row, best-effort event afterward." That keeps the
local write path and the replay path from drifting apart.

### The Hybrid Logical Clock And Merge Order

The total order is `(hlc, origin_instance_uid, event_uid)`. The HLC is a hybrid
logical clock, a `(physical_ms, counter)` pair stamped when an event is emitted
and advanced past the clock of every foreign event applied, so causally later
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
scope, not per issue, because links and project metadata are project-scoped (an
event on one issue can affect another issue's projection), so a per-issue
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

New baseline snapshots include each relationship's stored `created_at` date.
Both storage backends preserve that date when rebuilding links, including links
whose other endpoint arrives in a later batch. Older persisted snapshots omit
the date and remain readable: a newly materialized link gets its insertion time,
and later rebuilds retain it. Those events cannot recover the original date.
This does not rewrite old events or attempt historical repair; generating a new
baseline on the original hub carries the dates that hub still stores.

### Metadata Merge Semantics

Issue and project metadata is a per-key map, and unreserved keys may hold
arbitrary nested JSON. To salvage concurrent edits to different sub-fields of the
same key, the fold diffs each metadata event's per-key `{from, to}` down to
JSON-pointer paths and resolves each leaf as an LWW register. Deletion keys off
**structural absence, never off JSON `null`**: a path present in `from` but
absent in `to` is a tombstone (covering the whole subtree if it was an object),
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
another holder while it is live; the hub guarantees at most one live holder.
Unleased work is normal and is never a violation; pending or expired leases do
not block edits; and comment creation or body edits always pass as comment-level
work.

Because the hub cannot reject a mutation that already happened offline, it does
not try to. When pushed work conflicts with another holder's live lease the hub
keeps the data and records a best-effort `claim.violated` annotation rather than
dropping anything. Lease state, especially timed-lease expiry after renewals,
is authoritative from the hub, not folded from events, so a spoke treats cached
lease state as a hint and confirms against the hub before relying on
exclusivity, never as proof that exclusivity still holds.

### Rejected And Deferred Alternatives

Hard purge of federated history is hub-admin-only and establishes a reset
boundary rather than being routine, because deleting replay history fights
event-truth. Recurrence materialization stays hub-only: running the scheduler on
multiple spokes would each materialize duplicate occurrences, since the
occurrence-uniqueness constraint is per-database and UIDs differ across nodes.
Cross-project issue moves involving any federated project are unsupported.
The replay protocol cannot apply a move across project event logs. Deferred work
includes element-level array CRDTs for metadata,
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
quarantine direct-spoke push or stop a relay project's sync. An operator must
resolve the cause and retry the retained batch; reset can also remain blocked.
Only legacy push supports skipping. A skip leaves the local events in place
but deliberately prevents their delivery to the hub.

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

### Project Authorization And Trusted Peers

Daemon user credentials carry actor identities. Team membership controls project
visibility on the daemon, and enrollment transport checks the authenticated
credential's bound actor against that policy. See
[project access](../reference/http-api.md#project-access) for the API boundary.

An enrolled federation peer remains trusted with the projects it is authorized
to exchange. Project policy does not turn a personal spoke into an adversarial
sandbox or provide a global identity directory. Daemon-owner authority controls
local policy, credentials and backup restore.

## Historical Design Context

This file is the canonical description of implemented federation behavior, its
design rationale, and its limitations. The original phased design spec and the
per-phase implementation plans were folded into this document and the
[architecture](architecture.md) and [data model](data-model.md) notes after the
work shipped; the superseded drafts remain available in version control.

Native optional signing and restricted ingress are described in
[Native federation request signing](federation-request-signing.md).
