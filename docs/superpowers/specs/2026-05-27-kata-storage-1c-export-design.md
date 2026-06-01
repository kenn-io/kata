# Storage Phase 1c-export — backend-neutral JSONL export readers

Sub-phase of the Postgres backend spec
(`docs/superpowers/specs/2026-05-26-kata-postgres-backend.md`, §2 and §10). Phase
1a removed the daemon's raw SQL; 1b extracted `db.Storage` and routed the daemon
plus the clean `cmd/kata` helpers through it. §10's step "1c" ports the
`internal/jsonl` export/import SQL into storage methods. Export and import are
independent (export runs no transaction; import is one atomic transaction and
shares no code with export), so they are split: **this doc covers export
(1c-export); import is a separate sub-phase (1c-import).**

## Goal

`jsonl.Export` depends only on `db.Storage` and holds no raw SQL or `*sql.DB`
access. The current-schema export reads move behind per-entity streaming
`Storage` methods. The legacy pre-v10 projections stay SQLite-bound for cutover.

## Background (from code exploration)

- `internal/jsonl` reaches the embedded `*sql.DB` directly. Export runs ~12
  per-entity `QueryContext` SELECTs with no transaction, each `ORDER BY id ASC`
  (the deterministic wire contract), streaming row-by-row through a generic
  `scanRecords[T]` helper that writes one envelope per row. No table is
  materialized whole.
- `jsonl.Export`'s only production callers are `kata export` (always the
  current-schema DB) and `cutover.go` (pre-v10 files, reached from the daemon via
  `AutoCutover`). The legacy `…V1..V8` version-gated projections and `PRAGMA
  table_info` exist solely to let cutover read old on-disk SQLite files.
- SQLite-isms baked into the export SQL: `CAST(col AS TEXT)` timestamp coercion,
  the `recurrence_uid` LEFT JOIN, the events orphan-filter and related-id
  scrubbing, the denormalized `project_name` expression, and `SELECT … FROM
  sqlite_sequence`.

## Design

### Storage iterator methods

One streaming reader per export entity, added to the `db.Storage` interface and
implemented on `*db.DB` (the SQLite-backed concrete type today; the package move
to `internal/db/sqlitestore` is 1d). Each returns `iter.Seq2[T, error]` (Go 1.23
range-over-func; the module is on Go 1.26). The consumer ranges
`for row, err := range it { … }`, checks `err`, and stops on the first error.

```
ExportMeta(ctx) iter.Seq2[MetaKV, error]
ExportProjects(ctx, ExportFilter) iter.Seq2[ProjectExport, error]
ExportProjectAliases(ctx, ExportFilter) iter.Seq2[AliasExport, error]
ExportRecurrences(ctx, ExportFilter) iter.Seq2[RecurrenceExport, error]
ExportIssues(ctx, ExportFilter) iter.Seq2[IssueExport, error]
ExportComments(ctx, ExportFilter) iter.Seq2[CommentExport, error]
ExportIssueLabels(ctx, ExportFilter) iter.Seq2[IssueLabelExport, error]
ExportLinks(ctx, ExportFilter) iter.Seq2[LinkExport, error]
ExportImportMappings(ctx, ExportFilter) iter.Seq2[ImportMappingExport, error]
ExportEvents(ctx, ExportFilter) iter.Seq2[EventExport, error]
ExportPurgeLog(ctx, ExportFilter) iter.Seq2[PurgeLogExport, error]
ExportSequences(ctx) iter.Seq2[SequenceExport, error]
```

`ExportSequences` carries SQLite-only data (the `sqlite_sequence` rows that
become `KindSQLiteSequence` envelopes). The Postgres backend will yield nothing
from it; per the parent spec, `KindSQLiteSequence` is a SQLite-only record that
PG export omits and PG import ignores.

### Row structs (package `db`)

Each `XExport` struct is the current SELECT projection turned into a typed row,
carrying export-shaped fields: timestamps as canonical TEXT, `recurrence_uid`
resolved, `related_issue_id`/`related_issue_uid` scrubbed per the current rules,
and the denormalized `project_name`. The SQLite impl's query produces exactly
these fields, so the SQLite-isms never leak past the method boundary. Per-struct
field lists are plan-level detail; they map 1:1 from the current `scanRecords`
scan targets.

### ExportFilter

```
type ExportFilter struct {
    ProjectID      *int64 // nil = all projects
    IncludeDeleted bool
}
```

Replaces the options currently threaded into the ad-hoc WHERE builders
(`issueExportWhere`, `linkExportWhere`, `eventExportWhereClauses`, and the
recurrence "deleted but still referenced by a live issue" clause). The impl
folds the filter into each query's WHERE, applied per entity as today: projects
honor `ProjectID` (scope to one project) but always include archived rows
matching that scope (they ignore `IncludeDeleted`), and `ExportMeta` /
`ExportSequences` take no filter at all.

### Iterator semantics and error handling

The impl opens its rows and defers their close. The iterator yields `(row, nil)`
per scanned row; on a scan error it yields `(zero, err)` and returns; after the
loop it surfaces `rows.Err()` as a terminal `(zero, err)` yield. A consumer that
breaks early triggers the deferred close. There is no transaction, matching
today's behavior.

### `jsonl.Export` after the port

`Export(ctx, store db.Storage, w io.Writer, opts ExportOptions) error` ranges
each iterator in the established entity order, wraps each row in its envelope
(Kind tag + JSON via the `Encoder`), and emits the synthetic `export_version`
record first. `internal/jsonl` retains what is genuinely wire-format: the
`Encoder`, the `Envelope`/`Kind`/`types.go` definitions, the `export_version`
record, the entity-group ordering, and any v8→v10 wire shaping. It loses all
SQL, the WHERE builders, `scanRecords`, the project-name expression helpers, the
`CAST` handling, and direct `*sql.DB` use.

`jsonl.Export`'s signature changes from `*db.DB` to `db.Storage`. The `cmd/kata`
export holder is still a concrete `*db.DB` from `db.Open` (the `Open`-returns-
`Storage` switch is 1d) and satisfies the new parameter without change.

### Legacy / cutover (relocated, not rewritten)

The `…V1..V8` projections, `PRAGMA table_info`, and the version dispatch move
verbatim into a new unexported function `exportForCutover(ctx, d *db.DB, w,
opts)` in `internal/jsonl/cutover_export.go` (package `jsonl`). They keep their
raw SQLite SQL because cutover is SQLite-file-bound and out of scope for
backend-neutrality (a Postgres deployment has no on-disk file to rebuild).

The selection rule is by caller, not by runtime branching: `cutover.go` calls
`exportForCutover` (it reads arbitrary pre-v10 on-disk SQLite files and needs the
version dispatch); `kata export` calls the backend-neutral `jsonl.Export`, which
handles the current schema only. `jsonl.Export` never inspects the schema
version to pick a path, and `cutover.go` never calls `jsonl.Export`. This keeps
the two exporters fully separate with no shared dispatch. `preflight.go` and
`cutover_v7.go` are otherwise unchanged.

## Out of scope (later phases)

- The package move to `internal/db/sqlitestore` and `db.Open`/`OpenReadOnly`
  returning `db.Storage` via the DSN selector (1d).
- The import port: the coarse atomic replay method and its SQLite-only internals
  (1c-import).
- The `cmd/kata` export/import holder type flip and `internal/testenv` (1d).

## Testing (TDD)

Test-first for every new method, no exceptions:

- Each `Storage` iterator gets a failing test before its implementation: seed
  representative rows (including soft-deleted and cross-entity references), range
  the iterator, and assert the shaped output, the `ORDER BY` ordering, and
  `ExportFilter` behavior (project scope and `IncludeDeleted`). Where an
  assertion could pass against a stub that ignores the database, prove it is
  non-vacuous by mutating the implementation and watching it go red.
- Iterator error paths get explicit coverage, not just happy-path shape:
  forcing the underlying query to fail (a pre-canceled `context`) and asserting
  the iterator surfaces a terminal non-nil error (the `(zero, err)` yield),
  plus a consumer `break` mid-range to confirm the deferred `rows.Close` runs
  without panic. This pins the query-error / early-break semantics the Risks
  section depends on. (Invalid-JSON metadata is not used as the trigger because
  the schema's `json_valid` CHECK constraints prevent seeding it.)
- The existing `internal/jsonl/export_test.go` and `roundtrip_test.go` are the
  integration guard. They must stay green after `jsonl.Export` is rewritten, and
  they pin the wire byte-order contract (entity-group order plus per-entity id
  order).

## Success criteria

- `jsonl.Export` takes `db.Storage`; export raw SQL is gone from the
  backend-neutral path (only the relocated SQLite-bound legacy exporter retains
  raw SQL, used solely by cutover).
- `go build ./...` and `go test ./...` are green; `golangci-lint run` reports 0
  issues.
- The export and roundtrip tests are unchanged in intent and green. The wire
  output for a current-schema export is byte-for-byte stable: same entity-group
  order, same per-entity id ordering, same JSON field tags (the roundtrip tests
  enforce this, and the `XExport` structs reuse the existing wire JSON tags).

## Risks

- Iterator error propagation (`rows.Err()`, early break, close) must be correct;
  the per-method tests cover it.
- The events / purge_log project-name and related-id scrubbing is subtle; the
  `XExport` structs must reproduce it exactly. The roundtrip tests guard against
  drift.
- The legacy relocation must not change cutover behavior; the cutover tests
  guard it.
