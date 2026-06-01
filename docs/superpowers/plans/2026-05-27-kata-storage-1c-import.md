# Storage Phase 1c-import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the whole-import atomic transaction out of `internal/jsonl/import.go` behind one coarse `db.Storage.ImportReplay` method, so `jsonl.ImportWithOptions` depends only on `db.Storage` and holds no raw SQL or `*sql.Tx`.

**Architecture:** `ImportReplay(ctx, recs []ImportRecord, opts ImportOptions) error` on `*db.DB` owns the entire atomic import (open tx, defer FKs, per-entity inserts in slice order, reconcile `sqlite_sequence`, validate, commit, refresh instance UID). It is **version-agnostic**: jsonl normalizes every source version to the current shape first (`applyCutoverV7toV8` + the pre-v2/pre-v3 fills), then maps each `Envelope` to a `db.ImportRecord` that reuses the 1c-export row structs as payloads. The FK-column resolver shared by import validation and cutover preflight moves into `db`.

**Tech Stack:** Go 1.26, `database/sql` over `modernc.org/sqlite`, `github.com/stretchr/testify`.

**Design spec:** `docs/superpowers/specs/2026-05-27-kata-storage-1c-import-design.md`.

**TDD is mandatory (no exceptions).** Every new method gets a failing test first, watched fail, then minimal code to pass. Where an assertion could pass against a stub that ignores the database (e.g. atomicity, sequence reconciliation), prove it is non-vacuous by mutating the implementation and watching it go red, then revert.

**Verification baseline (run once before starting):**

Run: `go build ./... && go test ./...`
Expected: exit 0; every package prints `ok` or `no test files`, no `FAIL`. This is the authoritative green baseline. (To skim only failures: `go test ./... 2>&1 | rg -v "^ok |no test files"`, but trust the bare `go test` exit status — a pipe's status comes from its last stage, not `go test`.)

---

## Background the implementer needs

`internal/jsonl/import.go` today does the whole import in one function (`ImportWithOptions`, ~lines 36-98): decode the stream → `validateExportVersion` → `applyCutoverV7toV8` (pre-v8 only) → `BeginTx` → `PRAGMA defer_foreign_keys=ON` → read the target's local `instance_uid` → per-envelope inserts in stream order (`importEnvelope`, ~lines 128-386) → `recordImportSchemaVersion` → `reconcileSequences` → `validateBeforeCommit` → `Commit` → `RefreshInstanceUID`.

This plan splits that across the `db`/`jsonl` boundary:

- **Moves into `db` (the atomic transaction):** every SQL insert, `uniqueProjectName`, `fillEventUIDs`/`lookupIssueUID`, `importedProjectName`, `recordImportSchemaVersion`, `upsertSequence`/`reconcileSequences`, `validateBeforeCommit`/`checkForeignKeyViolations`, `stringPtrValue`, `wrapImportErr`, and the FK-column resolver (`fkmeta.go`).
- **Stays in `jsonl` (pure, in-memory normalization):** the `Decoder`, `Envelope`/`Kind`, `validateExportVersion`, `applyCutoverV7toV8`, the four version fills (`fillProjectUID`/`fillIssueUID`/`fillEventV3Identity`/`fillPurgeLogV3Identity`), and `parseExportTime`. jsonl reads the target's local `instance_uid` via `store.InstanceUID()` (replacing `readMetaInstanceUID`, which is deleted).

**Reuse-of-export-structs key fact:** the 1c-export structs in `internal/db/export.go` (`MetaKV`, `ProjectExport`, `AliasExport`, `RecurrenceExport`, `IssueExport`, `CommentExport`, `IssueLabelExport`, `LinkExport`, `ImportMappingExport`, `EventExport`, `PurgeLogExport`, `SequenceExport`) carry exactly the current-schema fields each insert needs. Only two legacy fields are still functionally consumed — project `identity` (by `fillProjectUID`) and issue `number` (by `fillIssueUID`) — so jsonl keeps **two** thin wrapper structs that embed the export struct plus that one legacy field. The other historical legacy fields (`next_issue_number`, event/purge `project_identity`, `issue_number`) are decoded-and-ignored today and are simply dropped (they never reach an insert; `project_identity` only ever fed an error message via `importedProjectName`).

**Error-format preservation (important for keeping existing tests green):** in `importEnvelope` today, `decodeData`, `fillEventUIDs`, `fillEventV3Identity`, the recurrence-resolution, and `importedProjectName` errors propagate **raw** (e.g. `corrupt_event_fk: …`); only the final per-row `INSERT` error is wrapped as `import <kind>: …` via `wrapImportErr`. Preserve that exactly when porting, so the substring assertions in `internal/jsonl/import_test.go` still pass. The **only** new error wrapping is the malformed-record validation in `ImportReplay`, which adds the slice ordinal (a new code path with no existing assertions).

---

## File structure

- **Create** `internal/db/import_replay.go` (package `db`): `ImportOptions`, `ImportRecord`, the import kind constants, `ImportRecord.validate()`, `(*DB).ImportReplay`, the per-entity insert helpers, and the relocated tx helpers. One file for the whole import transaction.
- **Create** `internal/db/import_replay_test.go` (package `db_test`): direct `ImportReplay` contract tests.
- **Move** `internal/jsonl/fkmeta.go` → `internal/db/fkmeta.go` (package `db`): the FK-column resolver, exported (`NewFKColumnResolver`/`FKColumnResolver`/`Resolve`).
- **Modify** `internal/jsonl/preflight.go`: use `db.NewFKColumnResolver` (it already opens the source via `db.OpenReadOnly`).
- **Modify** `internal/db/storage.go`: add `ImportReplay` to the `Storage` interface (Task 4).
- **Modify** `internal/jsonl/import.go`: becomes decode → normalize (cutover + fills) → map `Envelope`→`db.ImportRecord` → `store.ImportReplay`; keeps the two wrapper structs, the four fills, `parseExportTime`, `validateExportVersion`. Loses all SQL, the tx helpers, `readMetaInstanceUID`, and the old per-entity record structs (Task 4).

`ImportReplay` and its helpers are added to `*db.DB` in Tasks 1-3 and tested on the concrete type (so `var _ Storage = (*DB)(nil)` keeps holding); `ImportReplay` joins the `Storage` interface in Task 4, when `jsonl.ImportWithOptions` switches to `db.Storage` and needs it there. This mirrors how 1c-export added its readers.

---

### Task 1: `ImportOptions`, `ImportRecord`, and the tagged-union `validate()`

Establish the import types and the pre-transaction validation contract. New code, no port.

**Files:**
- Create: `internal/db/import_replay.go`
- Create: `internal/db/import_replay_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/db/import_replay_test.go`:

```go
package db_test

import (
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"
)

// The tagged-union contract is exercised end-to-end through ImportReplay in
// Task 3, but pin it directly here first. validate() is unexported, so the test
// calls it through the ValidateImportRecordForTest shim (added in Step 3).
func TestImportRecordValidate(t *testing.T) {
	id := int64(1)
	cases := []struct {
		name    string
		rec     db.ImportRecord
		wantErr string
	}{
		{
			name:    "unknown kind",
			rec:     db.ImportRecord{Kind: "bogus", Meta: &db.MetaKV{Key: "k", Value: "v"}},
			wantErr: "unknown kind",
		},
		{
			name:    "no payload",
			rec:     db.ImportRecord{Kind: "meta"},
			wantErr: "no payload set",
		},
		{
			name: "multiple payloads",
			rec: db.ImportRecord{
				Kind:    "meta",
				Meta:    &db.MetaKV{Key: "k", Value: "v"},
				Project: &db.ProjectExport{ID: id},
			},
			wantErr: "multiple payloads set",
		},
		{
			name:    "kind/payload mismatch",
			rec:     db.ImportRecord{Kind: "project", Meta: &db.MetaKV{Key: "k", Value: "v"}},
			wantErr: "does not match",
		},
		{
			name:    "valid",
			rec:     db.ImportRecord{Kind: "meta", Meta: &db.MetaKV{Key: "k", Value: "v"}},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := db.ValidateImportRecordForTest(tc.rec)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
```

(`testify` is the project default, but plain `t.Fatalf` keeps this table terse; either is fine.)

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestImportRecordValidate`
Expected: FAIL to compile — `db.ImportRecord`, `db.MetaKV` (exists), `db.ValidateImportRecordForTest` undefined.

- [ ] **Step 3: Write the minimal implementation**

Create `internal/db/import_replay.go`:

```go
package db

import (
	"fmt"
	"strings"
)

// ImportOptions controls optional ImportReplay behaviors.
type ImportOptions struct {
	// NewInstance keeps the target's existing meta.instance_uid (the value
	// db.Open wrote on first open) instead of applying the source's. The
	// imported events/purge_log origin_instance_uid columns are NOT rewritten:
	// they keep the original origins so a future federation loop-detector can
	// tell which rows came from the cloned-from instance.
	NewInstance bool
}

// ImportRecord is one normalized, current-shape import row: a Kind discriminator
// plus exactly one payload pointer reusing the 1c-export row structs. jsonl
// normalizes every source version to the current shape before building these,
// so ImportReplay never sees a source export_version.
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

// Import kind discriminators. These mirror the wire Kind strings produced by
// internal/jsonl (jsonl.Kind); db cannot import jsonl (that would be a cycle),
// so the contract is the shared NDJSON kind string, asserted by the roundtrip
// tests.
const (
	importKindMeta           = "meta"
	importKindProject        = "project"
	importKindProjectAlias   = "project_alias"
	importKindRecurrence     = "recurrence"
	importKindIssue          = "issue"
	importKindComment        = "comment"
	importKindIssueLabel     = "issue_label"
	importKindLink           = "link"
	importKindImportMapping  = "import_mapping"
	importKindEvent          = "event"
	importKindPurgeLog       = "purge_log"
	importKindSQLiteSequence = "sqlite_sequence"
)

// validate enforces the tagged-union invariant: Kind is recognized and exactly
// the one matching payload pointer is set. It returns a clear error naming the
// offending Kind; ImportReplay adds the slice ordinal.
func (r ImportRecord) validate() error {
	payloads := []struct {
		kind string
		set  bool
	}{
		{importKindMeta, r.Meta != nil},
		{importKindProject, r.Project != nil},
		{importKindProjectAlias, r.Alias != nil},
		{importKindRecurrence, r.Recurrence != nil},
		{importKindIssue, r.Issue != nil},
		{importKindComment, r.Comment != nil},
		{importKindIssueLabel, r.Label != nil},
		{importKindLink, r.Link != nil},
		{importKindImportMapping, r.ImportMapping != nil},
		{importKindEvent, r.Event != nil},
		{importKindPurgeLog, r.PurgeLog != nil},
		{importKindSQLiteSequence, r.Sequence != nil},
	}
	known := false
	var set []string
	for _, p := range payloads {
		if p.kind == r.Kind {
			known = true
		}
		if p.set {
			set = append(set, p.kind)
		}
	}
	if !known {
		return fmt.Errorf("unknown kind %q", r.Kind)
	}
	if len(set) == 0 {
		return fmt.Errorf("kind %q: no payload set", r.Kind)
	}
	if len(set) > 1 {
		return fmt.Errorf("kind %q: multiple payloads set (%s)", r.Kind, strings.Join(set, ", "))
	}
	if set[0] != r.Kind {
		return fmt.Errorf("kind %q: payload does not match (got %s)", r.Kind, set[0])
	}
	return nil
}
```

Add a test seam in a new `internal/db/import_replay_internal_test.go` (package `db`, so it can call the unexported `validate`):

```go
package db

// ValidateImportRecordForTest exposes ImportRecord.validate to the external
// db_test package, which pins the tagged-union contract directly.
func ValidateImportRecordForTest(r ImportRecord) error { return r.validate() }
```

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestImportRecordValidate`
Expected: PASS (all five subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/db/import_replay.go internal/db/import_replay_internal_test.go internal/db/import_replay_test.go
git commit -m "feat(db): add ImportRecord/ImportOptions types and tagged-union validation"
```

---

### Task 2: Move the FK-column resolver into `db`

`internal/jsonl/fkmeta.go`'s resolver is used by both the import FK validation (moving to `db`) and `preflight.go` (cutover source check, staying in `jsonl`). `db` cannot import `jsonl`, so the shared resolver moves to `db` and `preflight` imports it from there. This is a pure move + export-rename + repoint; behavior is unchanged.

**Files:**
- Move: `internal/jsonl/fkmeta.go` → `internal/db/fkmeta.go`
- Modify: `internal/jsonl/preflight.go`
- Test: `internal/db/import_replay_test.go` (small direct resolver test)

- [ ] **Step 1: Move and re-export the resolver**

```bash
git mv internal/jsonl/fkmeta.go internal/db/fkmeta.go
```

In `internal/db/fkmeta.go`: change `package jsonl` → `package db`, and rename the exported surface:
- `fkColumnResolver` → `FKColumnResolver`
- `newFKColumnResolver` → `NewFKColumnResolver`
- method `resolve` → `Resolve`

Keep `fkColumnQuerier` and `safeIdent` unexported (external callers pass a `*db.DB` or `*sql.Tx`, which satisfy the unexported interface structurally — Go allows calling an exported func whose parameter type is an unexported interface). The body is otherwise unchanged. The file's imports (`context`, `database/sql`, `fmt`, `regexp`) are unchanged.

- [ ] **Step 2: Repoint `preflight.go`**

In `internal/jsonl/preflight.go`, the source is already opened via `db.OpenReadOnly` (returns `*db.DB`). Change:

```go
	resolver := newFKColumnResolver(source)
```
to
```go
	resolver := db.NewFKColumnResolver(source)
```
and the call site `resolver.resolve(ctx, r.Table, r.FKID)` → `resolver.Resolve(ctx, r.Table, r.FKID)`. `db` is already imported in `preflight.go`.

- [ ] **Step 3: Write a direct resolver test (newly-public API)**

Append to `internal/db/import_replay_test.go`:

```go
func TestFKColumnResolverResolvesIssuesProjectID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	resolver := db.NewFKColumnResolver(d)

	// issues has FK columns project_id and recurrence_id. foreign_key_list
	// indices are assigned by SQLite; at least one valid index must resolve to
	// one of those columns, and an out-of-range index must resolve to "".
	var found string
	for fkid := 0; fkid < 4; fkid++ {
		col, err := resolver.Resolve(ctx, "issues", fkid)
		if err != nil {
			t.Fatalf("Resolve(issues, %d): %v", fkid, err)
		}
		if col == "project_id" || col == "recurrence_id" {
			found = col
		}
	}
	if found == "" {
		t.Fatal("expected an FK column of issues to resolve")
	}
	col, err := resolver.Resolve(ctx, "issues", 999)
	if err != nil {
		t.Fatalf("out-of-range Resolve: %v", err)
	}
	if col != "" {
		t.Fatalf("out-of-range fkid should resolve to empty, got %q", col)
	}
}
```

Add `"context"` to the test imports if not already present.

- [ ] **Step 4: Build and run**

Run: `go build ./... && go test ./internal/db/ ./internal/jsonl/ -run 'TestFKColumnResolver|Preflight|Cutover'`
Expected: exit 0; the moved resolver compiles in `db`, `preflight` uses it, and the cutover/preflight tests stay green.

- [ ] **Step 5: Commit**

```bash
git add internal/db/fkmeta.go internal/jsonl/preflight.go internal/db/import_replay_test.go
git commit -m "refactor: move FK-column resolver into db, share with cutover preflight"
```

---

### Task 3: Implement `(*db.DB).ImportReplay`

Port the entire import transaction from `internal/jsonl/import.go` into `db`, driven by `[]ImportRecord` (reading export-struct payloads) instead of `[]Envelope` (decoding). The version fills do **not** move here — they stay in jsonl (Task 4) — so `ImportReplay` is version-agnostic.

**Files:**
- Modify: `internal/db/import_replay.go`
- Test: `internal/db/import_replay_test.go`

- [ ] **Step 1: Write the failing test (full per-entity batch)**

Append to `internal/db/import_replay_test.go`. This seeds a source DB, exports it to records via the existing iterators, replays into a fresh DB, and asserts row counts match — the most direct ImportReplay smoke test.

```go
func TestImportReplayInsertsEveryEntity(t *testing.T) {
	ctx := context.Background()
	// Source: a project with a live issue, a comment, a label, a link to a
	// second issue, and a recurrence. Open auto-emits events.
	src, _, p, issue := setupTestIssue(t)
	other, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	_, err = src.CreateLink(ctx, db.CreateLinkParams{ProjectID: p.ID, FromIssueID: issue.ID, ToIssueID: other.ID, Type: "blocks"})
	require.NoError(t, err)
	_, err = src.AddLabel(ctx, issue.ID, "urgent", "a")
	require.NoError(t, err)
	_, _, err = src.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "a", Body: "hi"})
	require.NoError(t, err)

	recs := collectImportRecords(t, ctx, src)

	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{}))

	for _, table := range []string{"projects", "issues", "comments", "issue_labels", "links", "events"} {
		require.Equal(t, tableCount(t, ctx, src, table), tableCount(t, ctx, dst, table), table)
	}
}

// collectImportRecords drains the db export iterators into the current-shape
// ImportRecord slice (no version fills needed — the source is current schema).
func collectImportRecords(t *testing.T, ctx context.Context, d *db.DB) []db.ImportRecord {
	t.Helper()
	var recs []db.ImportRecord
	for rec, err := range d.ExportMeta(ctx) {
		require.NoError(t, err)
		m := rec
		recs = append(recs, db.ImportRecord{Kind: "meta", Meta: &m})
	}
	for rec, err := range d.ExportProjects(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "project", Project: &v})
	}
	for rec, err := range d.ExportProjectAliases(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "project_alias", Alias: &v})
	}
	for rec, err := range d.ExportRecurrences(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "recurrence", Recurrence: &v})
	}
	for rec, err := range d.ExportIssues(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "issue", Issue: &v})
	}
	for rec, err := range d.ExportComments(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "comment", Comment: &v})
	}
	for rec, err := range d.ExportIssueLabels(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "issue_label", Label: &v})
	}
	for rec, err := range d.ExportLinks(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "link", Link: &v})
	}
	for rec, err := range d.ExportImportMappings(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "import_mapping", ImportMapping: &v})
	}
	for rec, err := range d.ExportEvents(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "event", Event: &v})
	}
	for rec, err := range d.ExportPurgeLog(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "purge_log", PurgeLog: &v})
	}
	for rec, err := range d.ExportSequences(ctx) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "sqlite_sequence", Sequence: &v})
	}
	return recs
}

func tableCount(t *testing.T, ctx context.Context, d *db.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n))
	return n
}
```

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestImportReplayInsertsEveryEntity`
Expected: FAIL to compile — `dst.ImportReplay` undefined.

- [ ] **Step 3: Implement `ImportReplay` and its dispatch**

First, replace the file's existing `import` block (Task 1 had only `fmt` +
`strings`) with the full set this task needs:

```go
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)
```

Then append the orchestrator and dispatch:

```go
// ImportReplay performs the entire JSONL import as one atomic transaction. recs
// must already be normalized to the current shape (jsonl does cutover + version
// fills before calling). It validates the tagged-union invariant of every
// record before any mutation, then inserts in slice order under deferred FKs,
// reconciles sqlite_sequence, validates, commits, and refreshes the cached
// instance UID.
func (d *DB) ImportReplay(ctx context.Context, recs []ImportRecord, opts ImportOptions) error {
	for i, r := range recs {
		if err := r.validate(); err != nil {
			return fmt.Errorf("import record %d: %w", i, err)
		}
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys=ON`); err != nil {
		return fmt.Errorf("defer foreign keys: %w", err)
	}

	for _, r := range recs {
		if err := importRecord(ctx, tx, r, opts); err != nil {
			return err
		}
	}
	if err := recordImportSchemaVersion(ctx, tx); err != nil {
		return err
	}
	if err := reconcileSequences(ctx, tx); err != nil {
		return err
	}
	if err := validateBeforeCommit(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import: %w", err)
	}
	// Default mode may have overwritten meta.instance_uid with the source's
	// value; the cached InstanceUID() must follow so later inserts on this
	// handle stamp the right origin. (--new-instance leaves the row at
	// db.Open's value, so this is a no-op there.)
	return d.RefreshInstanceUID(ctx)
}

func importRecord(ctx context.Context, tx *sql.Tx, r ImportRecord, opts ImportOptions) error {
	switch r.Kind {
	case importKindMeta:
		return importMeta(ctx, tx, r.Meta, opts)
	case importKindProject:
		return importProject(ctx, tx, r.Project)
	case importKindProjectAlias:
		return importAlias(ctx, tx, r.Alias)
	case importKindRecurrence:
		return importRecurrence(ctx, tx, r.Recurrence)
	case importKindIssue:
		return importIssue(ctx, tx, r.Issue)
	case importKindComment:
		return importComment(ctx, tx, r.Comment)
	case importKindIssueLabel:
		return importLabel(ctx, tx, r.Label)
	case importKindLink:
		return importLink(ctx, tx, r.Link)
	case importKindImportMapping:
		return importMapping(ctx, tx, r.ImportMapping)
	case importKindEvent:
		return importEvent(ctx, tx, r.Event)
	case importKindPurgeLog:
		return importPurgeLog(ctx, tx, r.PurgeLog)
	case importKindSQLiteSequence:
		return upsertSequence(ctx, tx, r.Sequence.Name, r.Sequence.Seq)
	default:
		// Unreachable: validate() already rejected unknown kinds.
		return fmt.Errorf("import: unsupported kind %q", r.Kind)
	}
}
```

Then the per-entity insert helpers. The non-trivial ones in full:

```go
func importMeta(ctx context.Context, tx *sql.Tx, m *MetaKV, opts ImportOptions) error {
	// Always ignore the source's export_version/schema_version; ImportReplay
	// stamps the current schema_version after the loop (recordImportSchemaVersion).
	if m.Key == "export_version" || m.Key == "schema_version" {
		return nil
	}
	// --new-instance keeps the target's instance_uid (the value db.Open wrote).
	if m.Key == "instance_uid" && opts.NewInstance {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		m.Key, m.Value)
	return wrapImportErr(importKindMeta, err)
}

func importProject(ctx context.Context, tx *sql.Tx, p *ProjectExport) error {
	name, renamed, err := uniqueProjectName(ctx, tx, p.ID, p.Name)
	if err != nil {
		return err
	}
	if renamed {
		fmt.Fprintf(os.Stderr, "note: project #%d renamed from %q to %q during import\n", p.ID, p.Name, name)
	}
	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	revision := p.Revision
	if revision == 0 {
		revision = 1
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO projects(id, uid, name, created_at, deleted_at, metadata, revision)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UID, name, p.CreatedAt, p.DeletedAt, string(metadata), revision)
	return wrapImportErr(importKindProject, err)
}

func importIssue(ctx context.Context, tx *sql.Tx, i *IssueExport) error {
	if i.ShortID == "" {
		return fmt.Errorf("import issue %d: missing short_id (older envelopes must go through cutover)", i.ID)
	}
	metadata := i.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	revision := i.Revision
	if revision == 0 {
		revision = 1
	}
	if i.OccurrenceKey != nil && i.RecurrenceUID == nil && i.RecurrenceID == nil {
		return fmt.Errorf("import issue %d (uid=%s): occurrence_key set without recurrence_uid", i.ID, i.UID)
	}
	recurrenceID := i.RecurrenceID
	if i.RecurrenceUID != nil {
		var resolvedID int64
		if qErr := tx.QueryRowContext(ctx,
			`SELECT id FROM recurrences WHERE uid = ?`, *i.RecurrenceUID,
		).Scan(&resolvedID); qErr != nil {
			return fmt.Errorf("import issue %d: recurrence_uid %q not found: %w", i.ID, *i.RecurrenceUID, qErr)
		}
		if i.RecurrenceID != nil && *i.RecurrenceID != resolvedID {
			return fmt.Errorf("import issue %d: recurrence_uid %q resolves to id %d, but record carries recurrence_id %d",
				i.ID, *i.RecurrenceUID, resolvedID, *i.RecurrenceID)
		}
		recurrenceID = &resolvedID
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO issues(id, uid, project_id, short_id, title, body, status, closed_reason, owner, priority, author,
		                    created_at, updated_at, closed_at, deleted_at, metadata, revision,
		                    recurrence_id, occurrence_key)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.UID, i.ProjectID, i.ShortID, i.Title, i.Body, i.Status, i.ClosedReason,
		i.Owner, i.Priority, i.Author, i.CreatedAt, i.UpdatedAt, i.ClosedAt, i.DeletedAt,
		string(metadata), revision, recurrenceID, i.OccurrenceKey)
	return wrapImportErr(importKindIssue, err)
}

func importEvent(ctx context.Context, tx *sql.Tx, e *EventExport) error {
	if err := fillEventIssueUIDs(ctx, tx, e); err != nil {
		return err // raw: preserves the "corrupt_event_fk: …" prefix asserted by import_test.go
	}
	projectName, err := importedProjectName(ctx, tx, e.ProjectID, e.ProjectName)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(id, uid, origin_instance_uid, project_id, project_name, issue_id, issue_uid, related_issue_id, related_issue_uid,
		                    type, actor, payload, created_at)
		 VALUES(
		   ?, ?, ?, ?, ?, ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   ?, ?, ?, ?
		)`,
		e.ID, e.UID, e.OriginInstanceUID,
		e.ProjectID, projectName, e.IssueID,
		stringPtrValue(e.IssueUID), e.IssueID,
		e.RelatedIssueID,
		stringPtrValue(e.RelatedIssueUID), e.RelatedIssueID,
		e.Type, e.Actor, string(e.Payload), e.CreatedAt)
	return wrapImportErr(importKindEvent, err)
}

func importPurgeLog(ctx context.Context, tx *sql.Tx, pl *PurgeLogExport) error {
	projectName, err := importedProjectName(ctx, tx, pl.ProjectID, pl.ProjectName)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO purge_log(id, uid, origin_instance_uid, project_id, purged_issue_id, issue_uid, project_uid, project_name, short_id, issue_title,
		                       issue_author, comment_count, link_count, label_count, event_count,
		                       events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
		                       actor, reason, purged_at)
		 VALUES(
		   ?, ?, ?, ?, ?,
		   COALESCE(?, (SELECT uid FROM issues WHERE id = ?)),
		   COALESCE(?, (SELECT uid FROM projects WHERE id = ?)),
		   ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 )`,
		pl.ID, pl.UID, pl.OriginInstanceUID,
		pl.ProjectID, pl.PurgedIssueID,
		stringPtrValue(pl.IssueUID), pl.PurgedIssueID,
		stringPtrValue(pl.ProjectUID), pl.ProjectID,
		projectName, stringPtrValue(pl.ShortID),
		pl.IssueTitle, pl.IssueAuthor, pl.CommentCount, pl.LinkCount, pl.LabelCount,
		pl.EventCount, pl.EventsDeletedMinID, pl.EventsDeletedMaxID, pl.PurgeResetAfterEventID,
		pl.Actor, pl.Reason, pl.PurgedAt)
	return wrapImportErr(importKindPurgeLog, err)
}
```

The four simple inserts — port each verbatim from `internal/jsonl/import.go`, reading the payload struct instead of a decoded record, wrapping the final `INSERT` error with `wrapImportErr(importKind…, err)`:

- `importAlias(ctx, tx, a *AliasExport)` — `INSERT INTO project_aliases(id, project_id, alias_identity, alias_kind, root_path, created_at, last_seen_at)` from `import.go:188-191`. Fields: `a.ID, a.ProjectID, a.AliasIdentity, a.AliasKind, a.RootPath, a.CreatedAt, a.LastSeenAt`.
- `importRecurrence(ctx, tx, rc *RecurrenceExport)` — `INSERT INTO recurrences(...)` from `import.go:204-216`. Default `template_labels`→`[]` and `template_metadata`→`{}` when empty (`import.go:198-203`), then insert all fields from `rc`.
- `importComment(ctx, tx, c *CommentExport)` — `INSERT INTO comments(id, issue_id, author, body, created_at)` from `import.go:272-274`. Fields `c.ID, c.IssueID, c.Author, c.Body, c.CreatedAt`.
- `importLabel(ctx, tx, l *IssueLabelExport)` — `INSERT INTO issue_labels(issue_id, label, author, created_at)` from `import.go:281-283`. Fields `l.IssueID, l.Label, l.Author, l.CreatedAt`.
- `importLink(ctx, tx, lk *LinkExport)` — `INSERT INTO links(... COALESCE(NULLIF(?, ''), (SELECT uid FROM issues WHERE id = ?)) ...)` from `import.go:290-300`. Fields per that statement, reading `lk.*`.
- `importMapping(ctx, tx, m *ImportMappingExport)` — `INSERT INTO import_mappings(...)` from `import.go:307-309`. Fields from `m.*`.

Finally, port these tx helpers **verbatim** from `internal/jsonl/import.go` into `import_replay.go` (they are version-agnostic and tx-bound), with the two noted changes:

- `uniqueProjectName` (`import.go:413-429`) — verbatim.
- `lookupIssueUID` (`import.go:532-541`) — verbatim.
- `fillEventIssueUIDs` — the body of `fillEventUIDs` (`import.go:463-479`), renamed to avoid confusion with the jsonl version fills; takes `*EventExport`. Verbatim body (it reads `rec.IssueID`/`IssueUID`/`RelatedIssueID`/`RelatedIssueUID`, all present on `EventExport`).
- `importedProjectName` (`import.go:745-758`) — **simplified**: drop the `legacyProjectIdentity` parameter (it only ever fed an error message and is never populated for normalized records). New signature `importedProjectName(ctx, tx, projectID int64, projectName string) (string, error)`; keep the project-id lookup, then the `projectName` fallback, then the plain `"project %d not imported before project snapshot"` error.
- `recordImportSchemaVersion` (`import.go:388-397`) — verbatim (uses `db.CurrentSchemaVersion()`, now a same-package call: `CurrentSchemaVersion()`).
- `upsertSequence` (`import.go:760-776`) — verbatim.
- `reconcileSequences` (`import.go:778-801`) — verbatim.
- `validateBeforeCommit` (`import.go:803-822`) — verbatim.
- `checkForeignKeyViolations` (`import.go:829-889`) — verbatim **except** `newFKColumnResolver(tx)` → `NewFKColumnResolver(tx)` and `resolver.resolve(...)` → `resolver.Resolve(...)` (the resolver moved to `db` in Task 2; these are now same-package calls).
- `stringPtrValue` (`import.go:552-557`) — verbatim.
- `wrapImportErr` — the body of `import.go:406-411`, but typed `wrapImportErr(kind string, err error) error` (kind is now a plain string, not `jsonl.Kind`).

Reconcile the import block at the top of the file to exactly what is used (`context`, `database/sql`, `encoding/json`, `errors`, `fmt`, `os`, `strconv`, `strings`).

- [ ] **Step 4: Run the smoke test, watch it pass**

Run: `go test ./internal/db/ -run TestImportReplayInsertsEveryEntity`
Expected: PASS.

- [ ] **Step 5: Add the instance_uid behavior test**

```go
func TestImportReplayInstanceUID(t *testing.T) {
	ctx := context.Background()
	src, _, _, _ := setupTestIssue(t)
	srcUID := src.InstanceUID()
	recs := collectImportRecords(t, ctx, src)

	// Default mode: the source's instance_uid is applied to the target.
	def := openTestDB(t)
	require.NoError(t, def.ImportReplay(ctx, recs, db.ImportOptions{}))
	require.Equal(t, srcUID, def.InstanceUID(), "default import adopts the source instance_uid")

	// NewInstance: the target keeps its own instance_uid.
	ni := openTestDB(t)
	localUID := ni.InstanceUID()
	require.NotEqual(t, srcUID, localUID)
	require.NoError(t, ni.ImportReplay(ctx, recs, db.ImportOptions{NewInstance: true}))
	require.Equal(t, localUID, ni.InstanceUID(), "NewInstance keeps the local instance_uid")
}
```

Run: `go test ./internal/db/ -run TestImportReplayInstanceUID`
Expected: PASS.

- [ ] **Step 6: Add the sequence-reconcile test (non-vacuous)**

A naive "import high ids, then assert the next live insert exceeds the imported
max" probe is **vacuous** here: `issues` is `AUTOINCREMENT`, so SQLite never
reuses an id `<= MAX(rowid)` while the rows physically exist — the next insert
lands above the imported ids regardless of `reconcileSequences`. The real,
isolable job of `reconcileSequences` is to **raise the persisted
`sqlite_sequence` to `MAX(id)` when an imported sequence record lags below it**
(the purge-gap case). So force the imported `issues` sequence record below
`MAX(id)` and assert reconcile repairs the persisted value:

```go
func TestImportReplayReconcilesSequence(t *testing.T) {
	ctx := context.Background()
	src, _, p, _ := setupTestIssue(t)
	// Advance the source issue id high so the imported max is well above a
	// fresh DB's starting point.
	for i := 0; i < 3; i++ {
		_, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "x", Author: "a"})
		require.NoError(t, err)
	}
	recs := collectImportRecords(t, ctx, src)
	maxIssueID := tableMax(t, ctx, src, "issues")
	require.Greater(t, maxIssueID, int64(1), "fixture must have several issues")

	// Force the imported issues sqlite_sequence below MAX(id). Records replay
	// in slice order: issues insert first (each explicit-id INSERT bumps the
	// stored seq toward MAX(id) via AUTOINCREMENT), then this sqlite_sequence
	// record LOWERS the stored seq to 1. Only reconcileSequences
	// (seq = max(MAX(id), stored)) raises it back to MAX(id). Asserting on the
	// persisted sqlite_sequence value isolates reconcile: it is the sole step
	// that repairs a stale/lagging source sequence.
	setSequenceRecord(recs, "issues", 1)

	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{}))

	require.Equal(t, maxIssueID, storedSequence(t, ctx, dst, "issues"),
		"reconcile must raise the persisted sqlite_sequence to MAX(id)")
}

// setSequenceRecord rewrites the seq of the sqlite_sequence payload named name
// in place, failing the test if no such record exists.
func setSequenceRecord(recs []db.ImportRecord, name string, seq int64) {
	for _, r := range recs {
		if r.Kind == "sqlite_sequence" && r.Sequence != nil && r.Sequence.Name == name {
			r.Sequence.Seq = seq
			return
		}
	}
	panic("no sqlite_sequence record named " + name)
}

// storedSequence reads the persisted sqlite_sequence value for table.
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func storedSequence(t *testing.T, ctx context.Context, d *db.DB, table string) int64 {
	t.Helper()
	var seq int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&seq))
	return seq
}

//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func tableMax(t *testing.T, ctx context.Context, d *db.DB, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM `+table).Scan(&n))
	return n
}
```

Run: `go test ./internal/db/ -run TestImportReplayReconcilesSequence`
Expected: PASS.

- [ ] **Step 7: Prove the sequence-reconcile assertion is non-vacuous**

Temporarily comment out the `reconcileSequences(ctx, tx)` call in `ImportReplay`. Re-run `TestImportReplayReconcilesSequence`: it must FAIL — without reconcile the persisted `sqlite_sequence.issues` stays at the forced `1` instead of being raised to `MAX(id)`. Revert.

- [ ] **Step 8: Add the atomicity test (non-vacuous)**

A batch that fails mid-way must leave no partial rows. Use an **immediate**
constraint violation — a duplicate `uid` (projects.uid is `UNIQUE`, enforced at
insert time). A deferred-FK violation would only surface at commit, masking
whether the insert-loop error path or commit-time enforcement fired; the
duplicate `uid` isolates the insert-loop rollback path cleanly.

```go
func TestImportReplayIsAtomic(t *testing.T) {
	ctx := context.Background()
	src, _, _, _ := setupTestIssue(t)
	recs := collectImportRecords(t, ctx, src)

	// Append a second project sharing the first's uid (distinct id + name).
	// uniqueProjectName renames the colliding name, but the uid stays, so the
	// insert trips UNIQUE(projects.uid) mid-batch and the whole import must roll
	// back.
	var dup db.ProjectExport
	for _, r := range recs {
		if r.Kind == "project" {
			dup = *r.Project
			break
		}
	}
	require.NotEmpty(t, dup.UID, "fixture must contain a project")
	dup.ID += 1000
	dup.Name += "-dup"
	recs = append(recs, db.ImportRecord{Kind: "project", Project: &dup})

	dst := openTestDB(t)
	err := dst.ImportReplay(ctx, recs, db.ImportOptions{})
	require.Error(t, err)
	require.Equal(t, 0, tableCount(t, ctx, dst, "projects"), "a failed import commits nothing")
	require.Equal(t, 0, tableCount(t, ctx, dst, "issues"))
}
```

Run: `go test ./internal/db/ -run TestImportReplayIsAtomic`
Expected: PASS.

- [ ] **Step 9: Prove atomicity is non-vacuous**

Temporarily change `ImportReplay`'s insert loop to swallow per-record errors
(`if err := importRecord(...); err != nil { continue }` in place of
`return err`). Re-run `TestImportReplayIsAtomic`: the dup-uid insert is skipped,
the rest of the batch commits, and `dst` ends with one project — so both
`require.Error` and the `0 projects` assertion FAIL. That proves the test bites
(it distinguishes the atomic single-transaction design from one that tolerates a
failed insert). Revert.

- [ ] **Step 10: Add the malformed-record test (wires validate into ImportReplay)**

```go
func TestImportReplayRejectsMalformedRecord(t *testing.T) {
	ctx := context.Background()
	dst := openTestDB(t)
	recs := []db.ImportRecord{
		{Kind: "meta", Meta: &db.MetaKV{Key: "instance_uid", Value: "x"}},
		{Kind: "project"}, // malformed: no payload
	}
	err := dst.ImportReplay(ctx, recs, db.ImportOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "import record 1", "error names the slice ordinal")
	require.Contains(t, err.Error(), "no payload set")
	// Pre-transaction rejection leaves no rows: the meta probe was never applied.
	var n int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE value = 'x'`).Scan(&n))
	require.Equal(t, 0, n, "no mutation on a malformed batch")
}
```

This passes already (Step 3 wired `validate()` into `ImportReplay`'s pre-transaction loop). It pins the ordinal-bearing error and the no-mutation guarantee.

Run: `go test ./internal/db/ -run TestImportReplayRejectsMalformedRecord`
Expected: PASS.

- [ ] **Step 11: Run the full db package + lint**

Run: `go test ./internal/db/`
Expected: PASS.

Run: `golangci-lint run ./internal/db/` (via `nix run 'nixpkgs#golangci-lint' -- run ./internal/db/` if not on PATH)
Expected: 0 issues.

- [ ] **Step 12: Commit**

```bash
git add internal/db/import_replay.go internal/db/import_replay_test.go
git commit -m "feat(db): add ImportReplay atomic import on *db.DB"
```

---

### Task 4: Route `jsonl` import through `db.Storage.ImportReplay`

Add `ImportReplay` to the `Storage` interface, then rewrite `internal/jsonl/import.go` to decode → normalize (cutover + version fills) → map `Envelope`→`db.ImportRecord` → `store.ImportReplay`. The existing `roundtrip_test.go` / `import_test.go` / `cutover_test.go` are the behavior guard.

**Files:**
- Modify: `internal/db/storage.go`
- Modify: `internal/jsonl/import.go`
- Modify (only if assertions break on the improved error context): `internal/jsonl/import_test.go`

- [ ] **Step 1: Add `ImportReplay` to the `Storage` interface**

In `internal/db/storage.go`, in the `// import support` group, add:

```go
	ImportReplay(ctx context.Context, recs []ImportRecord, opts ImportOptions) error
```

Run `go build ./internal/db/` — the `var _ Storage = (*DB)(nil)` assertion must still compile (it proves `*DB` implements `ImportReplay`).

- [ ] **Step 2: Rewrite `internal/jsonl/import.go`**

Replace the file's body with the normalize-then-replay form below. Keep `package jsonl` and the `ImportOptions` doc. Delete: `importEnvelope`, `readMetaInstanceUID`, every SQL insert, `uniqueProjectName`, `fillEventUIDs`, `lookupIssueUID`, `importedProjectName`, `recordImportSchemaVersion`, `upsertSequence`, `reconcileSequences`, `validateBeforeCommit`, `checkForeignKeyViolations`, `stringPtrValue`, `wrapImportErr`, and the old per-entity record structs (`projectRecord`, `projectAliasRecord`, `recurrenceRecord`, `issueRecord`, `commentRecord`, `issueLabelRecord`, `linkRecord`, `importMappingRecord`, `eventRecord`, `purgeLogRecord`, `sqliteSequenceRecord`). Keep `validateExportVersion`, `parseExportTime`, and `metaRecord` (still used by `validateExportVersion` and export).

```go
package jsonl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// ImportOptions controls optional import behaviors.
type ImportOptions struct {
	// NewInstance preserves the target's meta.instance_uid (the value db.Open
	// wrote on first open) instead of overwriting it with the source's. The
	// imported events.origin_instance_uid and purge_log.origin_instance_uid
	// columns are NOT rewritten — they preserve the original origins so a
	// future federation loop-detector can tell which events came from the
	// cloned-from instance versus the new local one.
	NewInstance bool
}

// Import reads JSONL records from r and inserts them into store.
func Import(ctx context.Context, r io.Reader, store db.Storage) error {
	return ImportWithOptions(ctx, r, store, ImportOptions{})
}

// ImportWithOptions decodes the JSONL stream, normalizes every source version to
// the current shape in memory (cutover reshaping + version fills), maps each
// envelope to a backend-neutral db.ImportRecord, and replays them atomically via
// store.ImportReplay.
func ImportWithOptions(ctx context.Context, r io.Reader, store db.Storage, opts ImportOptions) error {
	envs, err := NewDecoder(r).ReadAll(ctx)
	if err != nil {
		return err
	}
	exportVersion, err := validateExportVersion(envs)
	if err != nil {
		return err
	}
	// Pre-v8 envelopes lack short_id; reshape to v8 before mapping so the
	// per-envelope path only ever sees current-version-shaped issue records.
	if exportVersion < 8 {
		if err := applyCutoverV7toV8(envs); err != nil {
			return err
		}
	}
	// Local origin for the pre-v3 identity backfill: the value db.Open wrote,
	// read once up front (equivalent to the old readMetaInstanceUID at tx start).
	localInstanceUID := store.InstanceUID()

	recs := make([]db.ImportRecord, 0, len(envs))
	for _, env := range envs {
		rec, err := toImportRecord(env, exportVersion, localInstanceUID)
		if err != nil {
			return err
		}
		recs = append(recs, rec)
	}
	return store.ImportReplay(ctx, recs, db.ImportOptions{NewInstance: opts.NewInstance})
}

// projectImport / issueImport embed the current-shape export struct and add the
// one legacy field each that the pre-v2 UID fill still consumes. Every other
// kind decodes straight into its export struct (any historical legacy fields in
// the JSON are ignored by the decoder).
type projectImport struct {
	db.ProjectExport
	Identity string `json:"identity,omitempty"`
}

type issueImport struct {
	db.IssueExport
	Number int64 `json:"number,omitempty"`
}

func toImportRecord(env Envelope, exportVersion int, localInstanceUID string) (db.ImportRecord, error) {
	switch env.Kind {
	case KindMeta:
		var rec db.MetaKV
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindMeta), Meta: &rec}, nil
	case KindProject:
		var rec projectImport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillProjectUID(&rec, exportVersion); err != nil {
			return db.ImportRecord{}, err
		}
		p := rec.ProjectExport
		return db.ImportRecord{Kind: string(KindProject), Project: &p}, nil
	case KindProjectAlias:
		var rec db.AliasExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindProjectAlias), Alias: &rec}, nil
	case KindRecurrence:
		var rec db.RecurrenceExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindRecurrence), Recurrence: &rec}, nil
	case KindIssue:
		var rec issueImport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillIssueUID(&rec, exportVersion); err != nil {
			return db.ImportRecord{}, err
		}
		i := rec.IssueExport
		return db.ImportRecord{Kind: string(KindIssue), Issue: &i}, nil
	case KindComment:
		var rec db.CommentExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindComment), Comment: &rec}, nil
	case KindIssueLabel:
		var rec db.IssueLabelExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindIssueLabel), Label: &rec}, nil
	case KindLink:
		var rec db.LinkExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindLink), Link: &rec}, nil
	case KindImportMapping:
		var rec db.ImportMappingExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindImportMapping), ImportMapping: &rec}, nil
	case KindEvent:
		var rec db.EventExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillEventV3Identity(&rec, exportVersion, localInstanceUID); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindEvent), Event: &rec}, nil
	case KindPurgeLog:
		var rec db.PurgeLogExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		if err := fillPurgeLogV3Identity(&rec, exportVersion, localInstanceUID); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindPurgeLog), PurgeLog: &rec}, nil
	case KindSQLiteSequence:
		var rec db.SequenceExport
		if err := decodeData(env, &rec); err != nil {
			return db.ImportRecord{}, err
		}
		return db.ImportRecord{Kind: string(KindSQLiteSequence), Sequence: &rec}, nil
	default:
		return db.ImportRecord{}, fmt.Errorf("import %s: unsupported kind", env.Kind)
	}
}
```

Then keep the four version fills and `parseExportTime` in this file, adapting only the two parameter types (the bodies are unchanged from today's `import.go`):

- `fillProjectUID(rec *projectImport, exportVersion int) error` — body verbatim from `import.go:431-445` (it reads `rec.Identity`, `rec.ID`, `rec.CreatedAt`, sets `rec.UID` — all valid: `Identity` is the wrapper field, the rest are promoted from `db.ProjectExport`).
- `fillIssueUID(rec *issueImport, exportVersion int) error` — body verbatim from `import.go:447-461` (reads `rec.ProjectID`, `rec.Number`, `rec.CreatedAt`, sets `rec.UID`).
- `fillEventV3Identity(rec *db.EventExport, exportVersion int, localInstanceUID string) error` — body verbatim from `import.go:481-507` (reads/sets `rec.UID`, `rec.OriginInstanceUID`, `rec.ProjectID`, `rec.ID`, `rec.CreatedAt`).
- `fillPurgeLogV3Identity(rec *db.PurgeLogExport, exportVersion int, localInstanceUID string) error` — body verbatim from `import.go:509-530` (reads/sets `rec.UID`, `rec.OriginInstanceUID`, `rec.ProjectID`, `rec.ID`, `rec.PurgedAt`).
- `parseExportTime` (`import.go:543-550`) and `decodeData` (`import.go:399-404`) — keep verbatim.

`validateExportVersion` (`import.go:110-126`) stays verbatim. `metaRecord` stays where it is defined (shared with export).

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: exit 0. (`cmd/kata/import.go` passes a `*db.DB`, which satisfies the new `db.Storage` parameter, so it compiles unchanged; `cutover.go`'s `Import(ctx, in, target)` likewise.)

- [ ] **Step 4: Run the jsonl import/roundtrip/cutover suite**

Run: `go test ./internal/jsonl/`
Expected: PASS. The key guards:
- `TestRoundtripRichDatabaseIsByteEquivalent` — export→import→export byte-identical. Because `Import` now routes through `ImportReplay`, this **is** the round-trip-through-`ImportReplay` byte-fidelity guard the spec requires; no separate test is added.
- `TestImportV1FillsUIDsDeterministically` / `TestImportLegacyEventSnapshotsUseFinalProjectName` — guard the jsonl-side version fills.
- `TestImportV1RejectsCorruptEventFK` — guards `fillEventIssueUIDs` (now in `db`); the `corrupt_event_fk:` error must still surface (it propagates raw through `ImportReplay`).
- `TestImportRejectsForeignKeyViolationBeforeCommit` / `…ListsEveryRow` — guard `validateBeforeCommit` + the moved resolver.

If any of these fail **only** because an assertion did an exact (not substring) match on an error string, update the assertion to `require.ErrorContains`/`assert.Contains` against the same substring — do **not** weaken the error. The malformed-record path is the only intentionally new error text, and it has no pre-existing assertion.

- [ ] **Step 5: Commit**

```bash
git add internal/db/storage.go internal/jsonl/import.go internal/jsonl/import_test.go
git commit -m "refactor(jsonl): route Import through db.Storage.ImportReplay"
```

---

### Task 5: Verify the boundary and the whole suite

**Files:** none (verification only)

- [ ] **Step 1: Confirm the import path holds no raw SQL**

Run: `rg -n "BeginTx|ExecContext|QueryContext|QueryRowContext|PRAGMA|sqlite_sequence" internal/jsonl/import.go`
Expected: no hits.

- [ ] **Step 2: Confirm `jsonl` import depends on `db.Storage`, not `*db.DB`**

Run: `rg -n "func Import\(|func ImportWithOptions\(" internal/jsonl/import.go`
Expected: both signatures read `store db.Storage`.

- [ ] **Step 3: Confirm `readMetaInstanceUID` is gone from jsonl**

Run: `rg -n "readMetaInstanceUID" internal/jsonl/`
Expected: no hits (jsonl now sources the local UID from `store.InstanceUID()`).

- [ ] **Step 4: Full build, test, lint**

Run: `go build ./... && go test ./...`
Expected: exit 0; every package `ok` or `no test files`, no `FAIL`.

Run: `golangci-lint run` (via `nix run 'nixpkgs#golangci-lint' -- run` if not on PATH)
Expected: 0 issues.

- [ ] **Step 5: No commit** (verification only).

---

## What this plan leaves for later phases

- **1d** — move the SQLite impl (including `import_replay.go` and `fkmeta.go`) into `internal/db/sqlitestore`; `db.Open`/`OpenReadOnly` return `db.Storage`; flip the `cmd/kata` import holder and `internal/testenv`.
- The external-tool importer (`db.ImportBatch`) is unrelated and untouched.

---

## Self-review (completed during planning)

- **Spec coverage:** `ImportReplay(ctx, recs, opts)` + version-agnostic body (Task 3); `ImportRecord` reusing export structs + `ImportOptions{NewInstance}` (Task 1); tagged-union validation before any mutation with kind+ordinal error (Tasks 1, 3 Step 10); jsonl normalization stays in jsonl — cutover + the four fills reading `store.InstanceUID()` (Task 4); absent-field defaulting on zero/empty sentinels (Task 3 `importProject`/`importIssue`/`importRecurrence`); always-stamp-current schema_version, instance_uid apply/skip, `RefreshInstanceUID` post-commit (Task 3); no legacy raw-SQL import path (Task 4 deletes it); raw SQL gone from `import.go` (Task 5). Testing: per-entity, sequence reconcile (non-vacuous), instance_uid, atomicity (non-vacuous), malformed-record; roundtrip/import/cutover green; the existing byte-equivalent roundtrip is the round-trip-through-`ImportReplay` guard.
- **Type consistency:** `db.ImportReplay`, `db.ImportRecord`, `db.ImportOptions`, the export struct payload names (`Project`/`Alias`/`Recurrence`/`Issue`/`Comment`/`Label`/`Link`/`ImportMapping`/`Event`/`PurgeLog`/`Sequence`), the wire kind strings, and `db.NewFKColumnResolver`/`Resolve` are used identically across Tasks 1-5. `importedProjectName` is intentionally simplified (drops the legacy parameter) and `fillEventUIDs` is renamed `fillEventIssueUIDs` in `db` to distinguish it from the jsonl version fills.
- **No placeholders:** every code step shows the code or names an exact `import.go` line anchor + the mechanical transform to apply; no "handle errors"/"similar to" gaps.
