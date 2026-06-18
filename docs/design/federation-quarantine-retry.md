# Federation push quarantine: classify transient errors + non-stranding retry

Status: Accepted (2026-06-18)
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
- Provide a non-stranding operator recovery for push quarantines whose root
  cause was fixed, kept distinct from `skip`.
- No schema migration. `db.CurrentSchemaVersion()` is also the federation ingest
  version, so bumping it would re-introduce the exact skew this change fixes.

## Non-goals

- Clean resolution columns (`resolved_at`/`resolution`) — deferred to a later
  schema cutover.
- Decoupling the federation ingest/protocol version from
  `db.CurrentSchemaVersion()` — noted as follow-up.
- Any change to `skip` semantics.

## Design

### 1. Reclassify transient push errors (root cause)

In `isPoisonedFederationPushError` (`federation_sync.go`), when the error is a
`*HubStatusError`, parse `Body` as `api.ErrorEnvelope` and treat
`error.code == "unsupported_federation_schema"` as **not** poison. Match on the
structured code, never on message text. The hub must reserve that code for the
transient "spoke schema is newer than hub schema" case.

Malformed protocol requests are permanent errors, not transient skew:

- missing `schema_version` is rejected by request validation as `validation`;
- explicit non-positive `schema_version` is rejected as
  `invalid_federation_schema`;
- only `schema_version > db.CurrentSchemaVersion()` returns
  `unsupported_federation_schema`.

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
  existing columns to avoid a schema bump; the `retry:` reason prefix is the
  resolution marker until a future schema cutover adds explicit columns.
- **API** — `POST /api/v1/projects/{project_id}/federation/quarantine/{quarantine_id}/retry`
  in `handlers_federation.go`, mirroring the skip handler. Retry is push-only:
  attempting to retry an active pull quarantine returns `409
  federation_quarantine_retry_unsupported`. Retrying a quarantine that is
  missing or already resolved returns the existing `404
  federation_quarantine_not_found`.
- **CLI** — `kata federation quarantine retry <id>` in `cmd/kata/federation.go`,
  mirroring `skip`:
  - confirmation string `RETRY FEDERATION BATCH <id>` (distinct from skip's, to
    prevent mixups);
  - optional `--reason` flag;
  - help text states explicitly that retry applies to push quarantines and
    leaves the push cursor unchanged so the same events are resent on the next
    sync.

After retry, the quarantine is inactive (`skipped_at` set), so
`federation_sync.go:82` no longer short-circuits. The next sync re-attempts the
push from the unchanged cursor and, against a fixed hub, succeeds and advances
normally.

### 3. Operator output

The current federation status API exposes only active quarantines, not
quarantine history, so this change does not add resolved-quarantine rendering to
`kata federation status`. The retry command should print "released for retry"
after a successful retry so operators do not confuse it with skip. If a future
history view exposes resolved rows, it should label rows whose `skip_reason` has
the `retry:` prefix as "released for retry" rather than "skipped". Rows without
that prefix, including empty or manually edited reasons, must remain "skipped"
unless a future schema cutover records an explicit resolution enum.

## Naming note

`skipped_at`/`skipped_by`/`skip_reason` now serve as the physical "resolved"
marker even though their names still reflect the original skip-only operation.
Code comments on the retry path must say this explicitly, so a retried
quarantine is not misread as always skipped/stranded.

## Test plan (TDD — write first)

1. Too-new `unsupported_federation_schema` `400` does **not** create a
   quarantine (`isPoisonedFederationPushError` is false; no quarantine row after
   a push that returns it).
2. Generic validation `400`, zero-schema `invalid_federation_schema` `400`, and
   `409` conflict **still** quarantine.
3. `RetryFederationQuarantine` marks the active quarantine inactive **without**
   advancing `push_cursor_event_id`.
4. After retry, the next `SyncFederationOnce` performs a network push and
   advances the cursor normally (contrast with
   `TestSyncFederationOnceActiveQuarantineStopsPushBeforeNetwork`).
5. Retrying a non-push quarantine returns `409
   federation_quarantine_retry_unsupported` and leaves the quarantine active.
6. CLI/API confirmation: a wrong or missing confirm string is rejected, and the
   retry and skip confirmation strings are not interchangeable.
7. OpenAPI and generated client artifacts are regenerated and checked with the
   daemon OpenAPI drift tests.

## Future work

- Schema cutover adding `resolved_at`, `resolved_by`, `resolution`,
  `resolution_reason`; update JSONL export/import so historical skipped and
  retried rows round-trip with explicit resolution state.
- A dedicated federation protocol version distinct from
  `db.CurrentSchemaVersion()`, so local schema changes do not gate federation
  compatibility.

## Operational follow-up

After this lands and the spoke is rebuilt at schema 16 (no migration), run
`kata federation quarantine retry <id>` on the live spoke to clear the stuck
quarantine. The four parent issues then push to the hub, and the 16 child issues
can be created with `--parent`.
