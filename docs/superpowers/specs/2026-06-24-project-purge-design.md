# Design: `kata projects purge` — permanent hard-delete of archived projects

Status: proposed (working spec; not part of the published docs nav)
Issue: kenn-io/kata#116 — "Add supported hard-delete command for archived projects"
Date: 2026-06-24

## Motivation

`kata projects remove <project>` archives (sets `projects.deleted_at`) rather than
deleting, which preserves audit history but keeps the project **name reserved**:
`kata init` then returns `project_archived` (`handlers_projects.go` `upsertProject`).
The only way to free the name today is hand-editing SQLite. This adds a supported,
gated, permanent delete for **archived** projects that frees the name and records
an audit tombstone.

## Goals / non-goals

In scope:

- `kata projects purge <project>` — permanent hard-delete of an **archived** project.
- Same destructive-confirmation friction as `kata purge` (issues): `--force` +
  `X-Kata-Confirm: PURGE <project>`.
- FK-safe deletion of all project-dependent rows; frees the name for re-`init`.
- A durable `project_purge_log` tombstone (identity + row counts + SSE cursor).
- Refuse when the project is still federated, with a role-aware hint.

Out of scope (YAGNI / separate issues):

- Purging non-archived (active) projects (no `--include-active`).
- Bulk / multi-project purge; undo.
- A full `pgstore` (Postgres) implementation — stub only (matches `PurgeIssue` /
  `RemoveProject`, which are already pgstore stubs).
- Federation hub teardown (`kata federation disband`) — hubs stay un-purgeable; see
  "Federation policy".

## CLI surface

```
kata projects purge <project> --force --confirm "PURGE <project>" [--reason "..."] [--json]
```

- New subcommand in the `projects` group (`projectsPurgeCmd`), registered alongside
  `remove` / `restore` / `detach` in `newProjectsCmd` (`cmd/kata/projects.go`).
- Resolves the project with **`resolveProjectSelectorIncludingArchived`**
  (`projects.go:427`) — the active-only resolver can't see archived projects.
- Safety gate mirrors `kata purge` exactly (reuse `resolveConfirm` /
  `confirmPromptFull` / `httpDoJSONWithHeader` from `delete.go`):
  - `--force` required, else exit 3 `validation`.
  - `--confirm` carries the exact string `PURGE <project>`, sent as the
    `X-Kata-Confirm` header.
  - Interactive TTY prompts for the full string; no-TTY without `--confirm` →
    exit 6 `confirm_required`; mismatch → exit 6 `confirm_mismatch`.
- `--reason` (optional) recorded in the tombstone, like issue purge.
- Output via a **dedicated project-purge printer** (not the issue-shaped
  `printDestructive`):
  - human: `<project> purged (irreversible); name is now free`
  - `--agent`: `OK project action=purged name=<project> ...`
  - `--json`: emits the `project_purge_log` tombstone (identity + counts).
- After purge, `kata init --project <name>` creates a fresh project.

## Daemon API

`POST /api/v1/projects/{project_id}/actions/purge`

- Header `X-Kata-Confirm: PURGE <project>`; body `{actor, reason?}`.
- Parallel to the issue purge route `/projects/{pid}/issues/{ref}/actions/purge`;
  no conflict with `DELETE /projects/{id}` (archive).
- Handler resolves the project with **`store.ProjectByID`** (archived-inclusive) —
  NOT `activeProjectByID`, which rejects archived rows
  (`handlers_projects.go:23-35`). Validates the confirm header equals
  `PURGE <project.Name>`, then calls `DB.PurgeProject(...)` and returns the
  tombstone. The store enforces the archived/federation preconditions
  transactionally (authoritative; preflight in the handler is optional).

## Storage: `PurgeProject` (sqlitestore — real impl)

New `db.Storage` method `PurgeProject(ctx, params) (db.ProjectPurgeLog, error)`.
Modeled on `PurgeIssue`'s `BEGIN IMMEDIATE` transaction on a dedicated conn with
detached-context rollback (`queries_delete.go`). New file
`queries_projects_purge.go`.

Preconditions (inside the tx):

1. Load project by id (archived-inclusive). Missing → `ErrNotFound` (404).
2. `isSystemProject` → `ErrNotFound` (mirror `RemoveProject`).
3. `deleted_at IS NULL` → `ErrProjectNotArchived` (409).
4. Any `federation_bindings` row → `ErrProjectFederated{Role}` (409) — see policy.

Let `S = (SELECT id FROM issues WHERE project_id = B)`.

Count snapshot (stable under `BEGIN IMMEDIATE`) for the tombstone:
`issue_count`, `event_count` (events with `project_id = B`, i.e. rows actually
deleted), `alias_count`, `comment_count`, `link_count`, `label_count`,
`claim_count`, `pending_claim_request_count`, plus `MIN/MAX(id)` of events with
`project_id = B`.

FK-safe cleanup (FKs enforced; most lack `ON DELETE CASCADE`). Cross-project
events from single-issue moves are **detached, not deleted** (review Finding 2):

```sql
-- 1. delete project-local events (the reset-relevant set)
DELETE FROM events WHERE project_id = B;
-- 2. detach external events that still reference purged issues (moved-in issues'
--    pre-move events live in their origin project; NULL the FK cols, keep rows)
UPDATE events SET issue_id = NULL, issue_uid = NULL
  WHERE issue_id IN (S);
UPDATE events SET related_issue_id = NULL, related_issue_uid = NULL
  WHERE related_issue_id IN (S);
-- 3. issue-level children (import_mappings rows cascade off these + off issues)
DELETE FROM comments     WHERE issue_id IN (S);
DELETE FROM links        WHERE from_issue_id IN (S) OR to_issue_id IN (S);
DELETE FROM issue_labels WHERE issue_id IN (S);
DELETE FROM issue_claims           WHERE project_id = B OR issue_id IN (S);
DELETE FROM pending_claim_requests WHERE project_id = B OR issue_id IN (S);
-- 4. issues (cascades import_mappings.issue_id)
DELETE FROM issues WHERE project_id = B;
-- 5. remaining project-scoped rows (federation_bindings already absent)
DELETE FROM project_aliases        WHERE project_id = B;
DELETE FROM federation_sync_status WHERE project_id = B;
DELETE FROM federation_quarantine  WHERE project_id = B;
DELETE FROM federation_enrollments WHERE project_id = B;
-- 6. reserve SSE cursor (reuse reserveEventSequence) when project-local events existed
-- 7. INSERT project_purge_log tombstone
-- 8. delete the project (cascades recurrences, issue_sync_bindings,
--    issue_sync_status, import_mappings.project_id)
DELETE FROM projects WHERE id = B;
```

Exact column lists and order are verified in tests with `PRAGMA foreign_key_check`.
Existing `purge_log` (issue tombstones) are **left in place** — `purge_log` has no
FK to `projects` precisely so "audit survives any future project cleanup"
(`schema.sql:239`); `AUTOINCREMENT` guarantees a recreated name gets a fresh
`project_id`, so stale short-id tombstones can't collide (review Finding 3).

`pgstore`: add a generated stub via `go run ./stubgen` (returns not-implemented),
matching the existing `PurgeIssue` / `RemoveProject` stubs.

## Audit tombstone + schema change

New table `project_purge_log` (snapshot, no FK so it survives the delete):

```
id, uid, origin_instance_uid,
project_id, project_uid, project_name,
issue_count, event_count, alias_count, comment_count, link_count, label_count,
claim_count, pending_claim_request_count,
events_deleted_min_id, events_deleted_max_id,
purge_reset_after_event_id,
actor, reason, purged_at
```

New `db.ProjectPurgeLog` type in `internal/db/types.go`.

`currentSchemaVersion` 19 → 20 (`internal/db/schema_version.go`). Existing DBs take
the established JSONL cutover on next open; the new table has no v19 rows to migrate.
Wire it through the cutover so round-trips stay complete and the guardrail tests
pass:

- `ProjectPurgeLogExport` (`export_types.go`), `ExportProjectPurgeLog` (`export.go`)
- `importProjectPurgeLog` + `ImportKindProjectPurgeLog` (`import_replay.go`,
  `import_types.go`); add to the import sequence-reset table list
- register the table in `schema_completeness_test.go` (sqlitestore + pgstore)
- add the table to `pgstore/schema.sql`

## SSE reset

Deletions are scoped to `project_id = B`, so only the (now-gone) project-B stream
and the global stream are affected. Extend `PurgeResetCheck`
(`queries_events.go:326`) to take `MAX(purge_reset_after_event_id)` across **both**
`purge_log` and `project_purge_log` (the `projectID == 0` global stream sees the
project tombstone's cursor). No broadcaster change (purge emits no event, exactly
like `PurgeIssue`; clients discover the reset via `PurgeResetCheck` on resume).

Documented residual (same scope as today's issue purge): a client subscribed
*only* to the per-project stream of a project that was the **origin** of a
moved-in, then-purged issue may keep a stale local copy until its next full sync.
External event rows are detached (not deleted), so per-project resume by id stays
valid.

## Federation policy (refuse + document)

`kata federation leave` tears down **spokes** only; hub disbanding is explicitly
out of scope (`federation.go:784,827`). Therefore purge refuses while any
`federation_bindings` row exists, with a role-aware hint:

- spoke → "run `kata federation leave <project>` first"
- hub → "this project is a federation hub; remove federation before purging
  (hub teardown is not yet supported)"

Consequence (documented limitation): a hub-federated archived project cannot be
purged until a hub-teardown path exists. Acceptable for the issue's target
(test / throwaway / accidental projects, which are not hubs). No stranded-state
risk.

## Error model

`project_not_found` (404), `project_not_archived` (409), `project_federated` (409,
role in detail), `confirm_required` / `confirm_mismatch` (exit 6), `validation`
when `--force` omitted (exit 3) — reusing `cliError` / exit-code plumbing.

## Testing (TDD: red → green → refactor)

Storage (`queries_projects_purge_test.go`):

- purges an archived project: every dependent table emptied, name freed (re-`init`
  succeeds), `PRAGMA foreign_key_check` clean post-purge.
- refuses active project (`project_not_archived`); refuses federated (spoke + hub,
  role-aware); refuses system project; missing → not found; re-purge → 404.
- moved-issue cases: issue moved **in** (origin-project events detached, FK-safe
  issues delete); issue moved **out** (B-era events deleted); cross-project link
  events detached.
- tombstone counts correct; `purge_reset_after_event_id` reserved; existing
  `purge_log` rows preserved.
- `PurgeResetCheck` (global, `projectID == 0`) returns the project cursor.

Cutover: `project_purge_log` JSONL export→import round-trip; completeness tests
green (sqlitestore + pgstore).

Daemon (`handlers_*_test.go`): route resolves archived project; confirm-header
required / validated; error codes / status.

CLI (`projects_test.go`): `--force` / `--confirm` TTY + non-TTY; mismatch; `--json`
shape; archived-only message; "name freed" end-to-end.

## Files touched (summary)

- `cmd/kata/projects.go` (+ purge command, printer), possibly a new
  `cmd/kata/projects_purge.go`
- `internal/api/types.go` (request / response)
- `internal/daemon/handlers_projects.go` (route + handler)
- `internal/db/storage.go` (+ `PurgeProject`), `internal/db/types.go`
  (+ `ProjectPurgeLog`), `internal/db/params.go` (params + errors)
- `internal/db/sqlitestore/queries_projects_purge.go` (impl),
  `queries_events.go` (`PurgeResetCheck`), `schema.sql` (+ table),
  `export.go` / `export_types.go` / `import_replay.go` / `import_types.go`
  (cutover), `schema_completeness_test.go`
- `internal/db/schema_version.go` (19 → 20)
- `internal/db/pgstore/schema.sql`, `internal/db/pgstore/stubs_gen.go` (regen stub)
- docs: CLI reference / projects guide updated as the feature lands (not pre-emptively)
