# Storage Phase 1c-import — backend-neutral JSONL import replay

Sub-phase of the Postgres backend spec
(`docs/superpowers/specs/2026-05-26-kata-postgres-backend.md`, §2 and §10) and
the sibling of 1c-export. The JSONL import is a single atomic transaction and
shares no code with the external-tool importer (`db.ImportBatch`). This phase
moves that transaction behind one coarse `db.Storage` method so
`jsonl.ImportWithOptions` depends only on `db.Storage`.

## Goal

`jsonl.ImportWithOptions` depends only on `db.Storage` and holds no raw SQL or
`*sql.Tx`. The whole-import transaction moves into `db.Storage.ImportReplay`.
After this phase, `internal/jsonl` has no `*db.DB` dependency in either Export or
Import.

## Background (from code exploration)

- `ImportWithOptions` decodes the whole stream (`NewDecoder.ReadAll`) into
  `[]Envelope`, then opens **one** `target.BeginTx` and runs the entire import
  inside it: `PRAGMA defer_foreign_keys=ON`; capture the local `instance_uid`;
  per-envelope inserts in stream order; `recordImportSchemaVersion`;
  `reconcileSequences` (`sqlite_sequence`); `validateBeforeCommit` (`PRAGMA
  foreign_key_check` + `integrity_check`); `commit`; then `RefreshInstanceUID`.
  All-or-nothing.
- Insert order = export order = FK-satisfaction order. Inline `COALESCE(?,
  (SELECT uid FROM issues WHERE id = ?))` backfills (links, events, purge_log)
  and `recurrence_uid`→id resolution depend on same-transaction state and
  deferred FKs.
- Helpers run inside the tx: `uniqueProjectName` (name-collision dedup),
  `fillEventUIDs`/`lookupIssueUID`, `importedProjectName`,
  `upsertSequence`/`reconcileSequences`, `recordImportSchemaVersion`,
  `readMetaInstanceUID`, `validateBeforeCommit`.
- Pre-v8 streams are reshaped to v8 by `applyCutoverV7toV8` **before** the tx
  opens — pure in-memory envelope rewriting, no SQL.
- Beyond cutover, the per-envelope loop also runs version-keyed UID
  normalization: `fillProjectUID`/`fillIssueUID` synthesize UIDs for pre-v2
  sources (seeded from the legacy `identity`/`number` fields), and
  `fillEventV3Identity`/`fillPurgeLogV3Identity` backfill `uid` +
  `origin_instance_uid` for pre-v3 events/purge_log. All four are pure
  in-memory record rewrites; the only external input is the target's local
  `instance_uid`, which the two pre-v3 fills use as the origin (today read via
  `readMetaInstanceUID` at tx start, equal to the cached `InstanceUID()`; this
  phase moves the fills into jsonl, which sources the same value from
  `store.InstanceUID()` before replay — the single post-refactor path). The
  importer decodes each envelope into jsonl-local record types
  (`projectRecord`, `issueRecord`, …) that still carry these legacy fields
  (`identity`, `number`, `next_issue_number`), which the current-shape export
  structs drop.
- `db.ImportBatch` (the external-tool importer: new UIDs, source-vs-local
  conflict resolution, live events) is a separate feature; jsonl shares no code
  with it, and it is untouched here.

## Design

### Storage addition

```
ImportReplay(ctx context.Context, recs []ImportRecord, opts ImportOptions) error
```

One method owning the entire atomic import. It is **version-agnostic**: jsonl
normalizes every source version to the current shape before building `recs` (see
"Version normalization stays in jsonl" below), so `ImportReplay` never sees a
source `exportVersion`. The SQLite implementation: open the transaction, defer
FKs, iterate `recs` in slice order dispatching by record kind (insert with
project-name dedup, `recurrence_uid`→id resolution, issue-UID and project-name
`COALESCE` backfills, and absent-field defaulting — `metadata`→`{}`,
`revision`→1, `template_labels`→`[]`, `template_metadata`→`{}`), then ignore any
incoming `export_version`/`schema_version` meta record and stamp the **current**
`schema_version`, reconcile sequences, validate, commit, and finally
`RefreshInstanceUID`. The absent-field defaulting keys on the zero/empty
sentinel (an empty `json.RawMessage`, or `revision == 0`), which is safe because
those are never valid stored values — `revision` is ≥1 by schema invariant and
the JSON columns are non-empty; it is version-agnostic, since version-dependent
backfills are normalized in jsonl before `recs` is built. The `instance_uid`
meta record is applied in default mode and skipped when
`ImportOptions.NewInstance` is set. The SQLite-only mechanics
(`PRAGMA defer_foreign_keys`, `sqlite_sequence` upsert/reconcile, `PRAGMA
foreign_key_check`/`integrity_check`) are **private to the SQLite impl** — not
on the interface. A future Postgres impl uses `SET CONSTRAINTS ALL DEFERRED`,
identity `setval`, and commit-time FK validation. The re-expose-transactions
alternative (a generic `Tx` interface) was rejected in the parent spec.

`ImportReplay` is large (the whole import); decompose it into private per-entity
insert helpers within the SQLite impl for readability, but keep a single
transaction owned by `ImportReplay`.

`ImportReplay` owns `recs` and may mutate the pointed-to payloads in place while
inserting (notably backfilling an event's `issue_uid`/`related_issue_uid` from
same-transaction state); callers build the slice fresh and must not reuse it
afterward.

### Types (package `db`)

- `ImportRecord` — a tagged union: a `Kind` discriminator plus one-of payload
  pointers reusing the 1c-export row structs:

  ```
  type ImportRecord struct {
      Kind          string
      Meta          *MetaKV
      Project       *ProjectExport
      Alias         *AliasExport
      Recurrence    *RecurrenceExport
      Issue         *IssueExport
      Comment       *CommentExport
      Label         *IssueLabelExport
      Link          *LinkExport
      ImportMapping *ImportMappingExport
      Event         *EventExport
      PurgeLog      *PurgeLogExport
      Sequence      *SequenceExport
  }
  ```

  Exactly one payload pointer is set per record, matching `Kind`. The reuse is
  scoped to the **normalized** `db.ImportRecord`: once jsonl has normalized a
  stream (cutover + version fills), each current-shape payload is byte-identical
  to what export produced, so reusing the export structs is sound (DRY) — there
  are no separate import-only *payload* structs in `db`. This does **not** mean
  import lacks legacy types: jsonl deliberately keeps its own legacy-capable
  decode structs (`projectRecord`, `issueRecord`, … carrying `identity`,
  `number`, `next_issue_number`) for source envelopes and maps to the reused
  export structs only **after** the fills consume those legacy fields. Decoding
  an old envelope straight into an export struct would drop the fields the
  pre-v2/pre-v3 backfills need, so that shortcut is disallowed.

  `ImportReplay` validates each record's tagged-union invariant before any
  mutation (validating the whole batch before opening the transaction is the
  natural placement): `Kind` is recognized and exactly the one matching payload
  pointer is set (zero, multiple, or mismatched payloads are rejected). A
  violation fails the whole import — with an error naming the offending record's
  `Kind` and slice ordinal — leaving no partial rows, so malformed-batch
  behavior is identical across backends.
- `ImportOptions` — mirrors today's `jsonl.ImportOptions`, whose only field is
  `NewInstance`. `ImportReplay` applies the source's `instance_uid` in default
  mode and skips it when `NewInstance` is set; it always ignores any incoming
  `export_version`/`schema_version` meta and stamps the current
  `schema_version` (the downlevel distinction is gone because jsonl normalized
  the stream first).

### `jsonl.ImportWithOptions` after

`ImportWithOptions` (and its `Import` convenience wrapper) take
`store db.Storage` in place of `target *db.DB`. `ImportWithOptions` decodes the
stream (`NewDecoder.ReadAll`), runs `validateExportVersion`, then normalizes
every record to the current shape in memory: `applyCutoverV7toV8` for pre-v8
streams, then the pre-v2 UID fills (`fillProjectUID`/`fillIssueUID`) and pre-v3
identity fills (`fillEventV3Identity`/`fillPurgeLogV3Identity`), reading
`store.InstanceUID()` once up front to supply the pre-v3 `origin_instance_uid`.
It maps each normalized record to a `db.ImportRecord` (set `Kind` + the reused
export-struct payload) and calls `store.ImportReplay(ctx, recs, dbOpts)`. jsonl
retains the `Decoder`, the `Envelope`/`Kind`/`types.go` definitions, its
jsonl-local decode record types (which carry the legacy fields the fills
consume), `validateExportVersion`, `cutover_v7` shaping, the version fills, and
its `ImportOptions` (mapped to `db.ImportOptions`). It loses `target.BeginTx`,
every insert, the tx helpers (`uniqueProjectName`, `fillEventUIDs`/
`lookupIssueUID`, `importedProjectName`, `recordImportSchemaVersion`,
`reconcileSequences`/`upsertSequence`, `readMetaInstanceUID`,
`validateBeforeCommit`), and all direct `*sql` access.

`cmd/kata/import.go`'s `db.Open` holder stays `*db.DB` (it satisfies the new
`db.Storage` parameter; it flips in 1d). `cutover.go` imports the reshaped JSONL
into a fresh current-schema target via the public `Import`; that target is a
`*db.DB` satisfying `db.Storage`, so cutover routes through `ImportReplay`
unchanged.

### Version normalization stays in jsonl

Import always targets the **current** schema. jsonl normalizes every source
version to the current envelope shape before replay: `applyCutoverV7toV8`
reshapes pre-v8 streams, and the pre-v2 UID backfills
(`fillProjectUID`/`fillIssueUID`) and pre-v3 identity backfills
(`fillEventV3Identity`/`fillPurgeLogV3Identity`) synthesize the fields older
sources omit. All of this is pure in-memory record rewriting with no SQL; the
only external input is the target's local `instance_uid`, which jsonl reads via
`store.InstanceUID()` and feeds to the pre-v3 origin backfill. Because
normalization happens in jsonl and the target DB is always current-schema,
`ImportReplay` is version-agnostic and needs **no** legacy raw-SQL path —
unlike export, which needed `exportForCutover` to read pre-v10 on-disk
databases. `ImportReplay` is the only import path.

### Instance UID

jsonl reads the target's local `instance_uid` up front via `store.InstanceUID()`
and passes it to the pre-v3 identity backfill (the local UID becomes the
`origin_instance_uid` for pre-v3 events/purge_log that lack one). On the write
side, `ImportReplay` applies the source's `instance_uid` meta row in default
mode (skips it under `NewInstance`) and, after its internal commit, calls
`RefreshInstanceUID` so the handle's cached UID follows the row it just wrote.
jsonl does not call `RefreshInstanceUID` separately.

## Out of scope (later phases)

- The package move to `internal/db/sqlitestore` and `db.Open`/`OpenReadOnly`
  returning `db.Storage` (1d); the `cmd/kata` import holder flip and
  `internal/testenv` (1d).
- The external-tool importer (`db.ImportBatch`) is unrelated and untouched.

## Testing (TDD)

Test-first for `ImportReplay` and its observable behavior:

- Build a `[]ImportRecord` covering each entity, call `ImportReplay`, and assert
  the rows are inserted correctly, the sequence high-water mark is reconciled (a
  subsequent live insert does not collide with an explicitly-imported id), and
  the `instance_uid` behavior is right (default applies the source UID;
  `NewInstance` keeps the local one).
- **Atomicity:** an invalid batch (a record that violates a constraint) must
  roll the whole import back — no partial rows. Where the rollback assertion
  could pass vacuously, mutation-prove it.
- **Malformed `ImportRecord` rejection:** a record with an unrecognized `Kind`,
  no payload set, multiple payloads set, or a `Kind`/payload mismatch must fail
  the import with an error naming the record's kind and ordinal, leaving no
  partial rows. Assert both the error and that no rows were written. The
  observable guarantee is no-partial-mutation, not the `BeginTx` boundary
  specifically — validating before opening the transaction and validating just
  inside it both leave zero rows, so a test cannot (and need not) distinguish
  them.
- The existing `internal/jsonl/roundtrip_test.go`, `import_test.go`, and
  `cutover_test.go` are the integration guard: export→import→compare and cutover
  must stay green (import behavior unchanged). Because the version-normalization
  fills now live in jsonl rather than `ImportReplay`, the cutover and
  pre-v2/pre-v3 cases in these tests are what guard the jsonl normalization
  path; `ImportReplay`'s own tests use current-shape `ImportRecord`s only. These
  normalization tests must exercise the legacy fields that current export
  structs omit (`identity`, `number`, `next_issue_number`), so a regression that
  decoded old envelopes straight into export structs — dropping those fields —
  would fail rather than silently produce missing UIDs.
- A round-trip-through-`ImportReplay` guard mirrors 1c-export's byte-fidelity
  test end-to-end: export a seeded current-schema DB, import into a fresh DB via
  the new path, re-export, and assert the two exports are byte-identical.

## Success criteria

- `jsonl.ImportWithOptions` takes `db.Storage`; import raw SQL is gone from
  `internal/jsonl` (`rg` finds no `BeginTx`/`ExecContext`/`PRAGMA`/
  `sqlite_sequence` in `import.go`).
- `go build ./...` and `go test ./...` are green; `golangci-lint run` reports 0
  issues.
- The roundtrip, cutover, and import tests are unchanged in intent and green.

## Risks

- The tx-dependent helpers (dedup via `uniqueProjectName`, `recurrence_uid`→id
  resolution, the same-transaction issue-UID and project-name backfills) must
  move into `ImportReplay` preserving insert order and deferred-FK semantics;
  the roundtrip and round-trip-through-`ImportReplay` tests guard correctness.
- The version-normalization fills move the **other** way — out of the
  per-envelope tx loop and into jsonl, ahead of the `db.ImportRecord` mapping.
  They must keep using the target's local `instance_uid` (now
  `store.InstanceUID()`, matching today's `readMetaInstanceUID` value) as the
  pre-v3 origin so re-imports stay byte-identical; the cutover and round-trip
  tests guard this.
- `ImportReplay` is a large method. Keep the transaction single and owned by it,
  but decompose the body into private per-entity insert helpers so each stays
  reviewable.
