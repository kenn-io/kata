# Data model and durability

This note explains the parts of kata's data model that are easy to get wrong and
hard to reconstruct from the schema alone: how identity works, why the audit log
is the spine of the system, and the cursor invariant that lets a client miss a
purge without missing data. It complements the
[architecture principles](architecture.md) and the
[federation design notes](federation.md), which build on the event model
described here.

## Two identities per issue

Every issue (and project) has a stable ULID `uid` that never changes. The
`uid` is the authoritative identity: it survives schema cutovers, it is unique
across instances, and it is what event payloads and links reference. Agents and
humans, however, work with a `short_id` — derived from the lowercased suffix of
the ULID and extended only as far as needed to stay unique within the project.
The short ID is a display label, not the identity; legacy numeric references no
longer resolve. See [issue identity](../guide/concepts.md#issue-identity) for the
reference forms.

Keeping the two separate is what makes the rest of the model portable. Links
store both endpoint UIDs and the integer foreign keys used for hot-path joins,
and database triggers reject any drift between them, so the UID view and the
local-id view can never disagree. When an issue is purged, its `short_id` is
retained in a tombstone so external references stay meaningful and a future issue
whose ULID suffix would collide is steered to a different short ID.

## The event log is the spine

State changes are not just stored; they are recorded. Three rules follow:

- The **events** table is append-only and is the authoritative record of every
  state change. Each event carries the actor, a stable event `uid`, the
  originating instance UID, and a payload with the field-level diff.
- The **comments** table is append-only — no edit, no delete short of purge.
- Issues themselves are mutable current-state rows, but every mutation emits an
  event, so the current row is always reconstructable from history.

This is what makes kata auditable by construction, and it is the foundation the
[federation fold engine](federation.md) later relies on to converge replicas.

### The destructive ladder

Removal is staged so that the reversible and irreversible steps are clearly
separated:

1. **Close** — the issue is closed but fully visible.
2. **Delete** (`--force`) — sets `deleted_at`; the issue is hidden but
   recoverable. Emits `issue.soft_deleted`.
3. **Restore** — clears `deleted_at`. Emits `issue.restored`.
4. **Purge** (`--force --confirm`) — irreversible. In one transaction it
   cascade-deletes the issue's comments, links, labels, and events, then writes a
   `purge_log` row that is intentionally **outside** the cascade so the audit of
   the deletion survives the data it describes.

Both destructive verbs require an exact confirmation string (or an interactive
prompt with a TTY), so neither can fire by accident or from a careless script.

## The purge cursor reservation invariant

Events are broadcast over SSE only after the database commit, and each event's
monotonic id doubles as the stream cursor. Purge complicates this: deleting
events leaves a hole in the id sequence, and a client reconnecting with an old
cursor must not silently skip the fact that history changed underneath it.

kata solves this by reserving a synthetic cursor at purge time. In the same
transaction that deletes events, the daemon advances the events table's
autoincrement sequence by one **without inserting a row**, and stores that
reserved value as `purge_log.purge_reset_after_event_id`. The reserved value is
therefore strictly greater than every real event id that existed at the moment of
the purge, and the next genuine event continues from `reserved + 1`, so the
synthetic cursor is unique and can never be assigned to a real event.

On reconnect (over SSE or polling), the daemon computes the maximum
`purge_reset_after_event_id` greater than the client's cursor. If one exists, the
client's cursor predates a purge: the daemon sends a single `sync.reset_required`
control signal carrying that maximum value and the client drops its cache,
refetches state, and adopts the reserved id as its new cursor. Using the maximum
collapses any number of accumulated purge gaps into one reset.

Two details make this correct rather than merely plausible:

- **Strict `>` is exactly right.** Because the reserved value is strictly greater
  than every event id that existed at purge time, even a client sitting at the
  maximum real id when the purge happened is still below the reserved value and is
  correctly reset. There is no off-by-one and no need for `>=`.
- **Resets are scoped per project.** A per-project stream adds the project
  predicate when searching for reset boundaries, so a purge in one project cannot
  invalidate a client following another. The cross-project stream omits the
  predicate and sees every boundary.

## Polling and streaming stay in parity

The non-streaming event endpoints apply the identical purge-invalidation rule.
A polling response sets `reset_required: true` with the new baseline `after_id`
when the caller's cursor has fallen inside a purged range, and an empty event
list. An agent that polls therefore cannot silently miss events any more than a
TUI tailing the SSE stream can: both paths honor the same reset boundary, so the
choice between polling and streaming is a performance decision, never a
correctness one.

## Idempotency and duplicate avoidance

Two independent mechanisms keep agents from creating duplicate issues.

**Idempotency keys.** A create request may carry an idempotency key. kata stores
a fingerprint alongside it that covers every creation-affecting field — title,
body, owner, labels, and initial links, each canonicalized so cosmetic
whitespace differences do not matter. Replaying the same key with the same
fingerprint returns the existing issue and emits no new event; replaying the same
key with a *different* fingerprint is an error rather than a silent reuse, because
the inputs materially disagree. This means a retried request is safe but a
mistaken reuse is caught.

**Look-alike soft-block.** Independently, `create` runs a full-text and
similarity search and refuses when an existing issue is too close, returning the
candidates so the caller can comment instead. This check is bypassable with
`force_new`.

Where the two interact, **idempotency wins**: an idempotent reuse never emits a
duplicate even when `force_new` is set. The retry-safety guarantee takes
precedence over the force-new escape hatch.

## Schema evolution by JSONL cutover

kata does not evolve its schema with in-place table-rebuild migrations. Instead
it exports the current database to JSONL exactly as stored, imports that JSONL
into a fresh database at the binary's current schema, applies deterministic fill
rules for anything an older export version lacked, validates, and atomically
swaps the database files into place.

This choice does several jobs at once. The JSONL export is git-friendly and
doubles as the supported [backup and restore](../operations/backup-restore.md)
format. Importing into a fresh database sidesteps the fragility of rewriting live
tables. And the fill rules are deterministic — backfilled identifiers are derived
from a stable seed rather than generated randomly — so the same legacy record
produces the same UID on every machine and every re-run, which is precisely what
lets independently upgraded instances later federate without identity conflicts.

A single `instance_uid` written at first initialization identifies the
installation, and every event and purge-log row carries its own `uid` plus the
`origin_instance_uid` that produced it. That provenance is recorded from day one,
in single-user installs that may never federate, so the audit log is sync-ready
without a later disruptive migration.

## Storage backends

The durable domain contract lives in `internal/db` as `db.Storage`: domain
types, parameter/result structs, sentinel errors, and backend-neutral helpers.
Concrete SQL implementations live beside it (`internal/db/sqlitestore`,
`internal/db/pgstore`), and production entry points select a backend through
`internal/db/storeopen` from the resolved DSN.

This boundary is intentionally narrow in production code: daemon and CLI paths
should hold `db.Storage` after opening. Concrete store types are appropriate
inside backend packages, SQLite-specific JSONL cutover code, and tests that
assert SQL details.

Both backends bootstrap fresh databases by applying their canonical
`schema.sql` inside a transaction and stamping `meta.schema_version` to the
binary's `db.CurrentSchemaVersion()`. Existing SQLite databases with older
schema versions are upgraded through the JSONL cutover path described above.
Postgres has no historical on-disk kata schema, so a Postgres database whose
`schema_version` disagrees with the binary is refused; operators should restore
from backup or run an explicit external migration before reopening.
