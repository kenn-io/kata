# Federation push quarantine: classify transient errors + non-stranding retry

Status: Proposed (2026-06-18)
Branch: `fix/federation-quarantine-retry`

## Problem

A spoke quarantines a push batch permanently whenever the hub returns any
`400`/`409`, and the only recovery (`skip`) strands the batch by advancing the
push cursor past it. This turns a routine rolling upgrade into a data-stranding
incident.

Root cause, in order:

- `internal/federation/federation_sync.go:300` `isPoisonedFederationPushError`
  treats every hub `400` (BadRequest) and `409` (Conflict) as permanent poison.
- The hub returns `400 unsupported_federation_schema` for version skew
  (`internal/daemon/handlers_federation.go:455`
  `validateFederationIngestSchemaVersion`). That is a transient upgrade-order
  condition — the hub will be upgraded — not poisoned data.
- Once a quarantine is active, `internal/federation/federation_sync.go:82`
  short-circuits the sync **before the network call**, so a since-fixed hub can
  never prove recovery. The quarantine never auto-clears.
- `internal/db/sqlitestore/federation.go:243` `skipFederationQuarantine`
  advances `push_cursor_event_id` to the batch's `last_event_id`; the skipped
  events never reach the hub (`docs/design/federation.md:417`).

Observed: a spoke at federation schema 16 pushing to a hub at 14 returns
`400 unsupported_federation_schema`, producing a permanent quarantine on the
first event; `skip` would advance the cursor past it and strand that event.

## Goals

- Transient hub conditions (schema-too-new) must not create a permanent
  quarantine; push pauses and auto-retries on the next sync.
- Provide a non-stranding operator recovery for any quarantine whose root cause
  was fixed, kept distinct from `skip`.
- No schema migration. `db.CurrentSchemaVersion()` is also the federation ingest
  version, so bumping it would re-introduce the exact skew this change fixes.

## Non-goals

- Clean resolution columns (`resolved_at`/`resolution`) — deferred to a later
  migration.
- Decoupling the federation ingest/protocol version from
  `db.CurrentSchemaVersion()` — noted as follow-up.
- Any change to `skip` semantics.

## Design

### 1. Reclassify transient push errors (root cause)

In `isPoisonedFederationPushError` (`federation_sync.go`), when the error is a
`*HubStatusError`, parse `Body` as `api.ErrorEnvelope` and treat
`error.code == "unsupported_federation_schema"` as **not** poison. Match on the
structured code, never on message text.

Non-poison errors already flow through `recordFederationSyncError` and are
retried on the next sync, so no other change is needed: schema skew now pauses
push and resumes automatically once the hub catches up.

Generic `400` (validation) and `409` (conflict) remain poison and still
quarantine.

### 2. First-class non-stranding retry operation

A new operation, separate from `skip`:

- **Store** — `RetryFederationQuarantine(ctx, params)` in
  `internal/db/sqlitestore/federation.go`. Resolves the active push quarantine
  by setting `skipped_at`, `skipped_by`, and `skip_reason = "retry: <reason>"`
  (reason defaulted when empty). It does **not** touch `push_cursor_event_id`.
  Mirrors `skipFederationQuarantine` minus the cursor `UPDATE`. Reuses the
  existing columns to avoid a migration; the `retry:` reason prefix is the
  resolution marker until a future migration adds explicit columns.
- **API** — `POST /api/v1/projects/{project_id}/federation/quarantine/{quarantine_id}/retry`
  in `handlers_federation.go`, mirroring the skip handler.
- **CLI** — `kata federation quarantine retry <id>` in `cmd/kata/federation.go`,
  mirroring `skip`:
  - confirmation string `RETRY FEDERATION BATCH <id>` (distinct from skip's, to
    prevent mixups);
  - optional `--reason` flag;
  - help text states explicitly that it leaves the push cursor unchanged so the
    same events are resent on the next sync.

After retry, the quarantine is inactive (`skipped_at` set), so
`federation_sync.go:82` no longer short-circuits. The next sync re-attempts the
push from the unchanged cursor and, against a fixed hub, succeeds and advances
normally.

### 3. Status rendering

Where quarantine rows are rendered, label a resolved row whose `skip_reason` has
the `retry:` prefix as "released for retry" rather than "skipped". The active
count is unaffected (resolved rows have `skipped_at` set).

## Naming note

`skipped_at`/`skipped_by`/`skip_reason` now serve as a generic "resolved"
marker. Code comments and API docs must say so explicitly, so a resolved
quarantine is not misread as always skipped/stranded.

## Test plan (TDD — write first)

1. `unsupported_federation_schema` `400` does **not** create a quarantine
   (`isPoisonedFederationPushError` is false; no quarantine row after a push that
   returns it).
2. Generic validation `400` and `409` conflict **still** quarantine.
3. `RetryFederationQuarantine` marks the active quarantine inactive **without**
   advancing `push_cursor_event_id`.
4. After retry, the next `SyncFederationOnce` performs a network push and
   advances the cursor normally (contrast with
   `TestSyncFederationOnceActiveQuarantineStopsPushBeforeNetwork`).
5. CLI/API confirmation: a wrong or missing confirm string is rejected, and the
   retry and skip confirmation strings are not interchangeable.

## Future work

- Migration adding `resolved_at`, `resolved_by`, `resolution`,
  `resolution_reason`; move off the overloaded `skip_*` columns.
- A dedicated federation protocol version distinct from
  `db.CurrentSchemaVersion()`, so local schema changes do not gate federation
  compatibility.

## Operational follow-up

After this lands and the spoke is rebuilt at schema 16 (no migration), run
`kata federation quarantine retry <id>` on the live spoke to clear the stuck
quarantine. The four parent issues then push to the hub, and the 16 child issues
can be created with `--parent`.
