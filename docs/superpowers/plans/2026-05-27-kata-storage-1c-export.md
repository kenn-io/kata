# Storage Phase 1c-export Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the current-schema JSONL export reads behind backend-neutral `db.Storage` iterator methods so `jsonl.Export` depends only on `db.Storage` and holds no raw SQL.

**Architecture:** Add one `iter.Seq2[T, error]` streaming reader per export entity to `*db.DB` (new file `internal/db/export.go`), each producing an export-shaped row struct that carries the wire JSON tags. Rewrite `jsonl.Export` to range those iterators and wrap each row in its envelope via the existing `writeRecord`/`Encoder`. The pre-v10 legacy projections (used only by cutover, which is SQLite-file-bound and out of scope for backend-neutrality) relocate verbatim into a SQLite-bound `exportForCutover`.

**Tech Stack:** Go 1.26, `database/sql` over `modernc.org/sqlite`, Go 1.23 range-over-func iterators (`iter.Seq2`), `github.com/stretchr/testify`.

**Design spec:** `docs/superpowers/specs/2026-05-27-kata-storage-1c-export-design.md`.

**TDD is mandatory (no exceptions).** Every new method gets a failing test first, watched fail, then minimal code to pass. Where an assertion could pass against a stub that ignores the database, prove it is non-vacuous by mutating the implementation and watching it go red, then revert.

**Verification baseline (run once before starting):**

Run: `go build ./... && go test ./...`
Expected: exit 0; every package prints `ok` or `no test files`, no `FAIL`. This is the authoritative green baseline. (To skim only failures: `go test ./... 2>&1 | rg -v "^ok |no test files"`, but trust the bare `go test` exit status — a pipe's status comes from its last stage, not `go test`.)

---

## File structure

- **Create** `internal/db/export.go` (package `db`): `ExportFilter`, the per-entity `XExport` row structs, and the 12 `Export*` iterator methods on `*DB`. One focused file for export reads.
- **Create** `internal/db/export_test.go` (package `db_test`): unit tests for each iterator method.
- **Modify** `internal/db/storage.go`: add the 12 `Export*` signatures to the `Storage` interface (Task 10).
- **Create** `internal/jsonl/cutover_export.go` (package `jsonl`): the relocated legacy exporter `exportForCutover` + all `…V1..V8` variants + the legacy-only helpers (Task 10).
- **Modify** `internal/jsonl/export.go`: becomes the backend-neutral `Export(store db.Storage, …)` that ranges the iterators; keeps `writeRecord` and the `export_version` record (Task 10).
- **Modify** `internal/jsonl/cutover.go`: call `exportForCutover` instead of `Export` (Task 10).

The iterator methods are added to `*db.DB` in Tasks 1-9 and tested on the concrete type (so `var _ Storage = (*DB)(nil)` keeps holding); they join the `Storage` interface in Task 10, when `jsonl.Export` switches to `db.Storage` and needs them there.

Every `XExport` struct carries the **same JSON field tags** as the corresponding local `record` struct in today's `internal/jsonl/export.go` (including `omitempty`), so `writeRecord(enc, Kind, row)` produces byte-identical output and the existing roundtrip tests stay green.

---

### Task 1: `ExportFilter` + `ExportMeta` (scaffolding + iterator template)

This task establishes the iterator + error-propagation pattern every later reader follows.

**Files:**
- Create: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/db/export_test.go`:

```go
package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestExportMeta(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	// Open seeds meta (schema_version, instance_uid). Add a probe that sorts last.
	_, err := d.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('zzz_probe', 'v1')`)
	require.NoError(t, err)

	var got []db.MetaKV
	for rec, err := range d.ExportMeta(ctx) {
		require.NoError(t, err)
		got = append(got, rec)
	}

	require.NotEmpty(t, got)
	// ORDER BY key ASC: keys strictly ascending, probe last.
	for i := 1; i < len(got); i++ {
		require.Less(t, got[i-1].Key, got[i].Key)
	}
	require.Equal(t, "zzz_probe", got[len(got)-1].Key)
	require.Equal(t, "v1", got[len(got)-1].Value)
}
```

This first test ranges the iterator inline and names `db.MetaKV` directly. A generic `collectExport[T]` helper is tempting, but with only this test the `db` import would be inferred-away and flagged unused, and an explicit `[db.MetaKV]` type argument then trips `infertypeargs`. The helper is introduced in Task 2, where the project tests reference `db` types directly, so inference is clean.

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportMeta`
Expected: FAIL to compile — `d.ExportMeta` undefined (and `db.MetaKV` undefined).

- [ ] **Step 3: Write the minimal implementation**

In `internal/db/export.go`:

```go
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
)

// ExportFilter scopes a JSONL export. The zero value exports every project's
// live (non-deleted) rows.
type ExportFilter struct {
	ProjectID      *int64 // nil = all projects
	IncludeDeleted bool
}

// MetaKV is one meta key/value row.
type MetaKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ExportMeta streams every meta row ordered by key.
func (d *DB) ExportMeta(ctx context.Context) iter.Seq2[MetaKV, error] {
	return func(yield func(MetaKV, error) bool) {
		rows, err := d.QueryContext(ctx, `SELECT key, value FROM meta ORDER BY key ASC`)
		if err != nil {
			yield(MetaKV{}, fmt.Errorf("export meta: %w", err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rec MetaKV
			if err := rows.Scan(&rec.Key, &rec.Value); err != nil {
				yield(MetaKV{}, fmt.Errorf("scan meta: %w", err))
				return
			}
			if !yield(rec, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(MetaKV{}, fmt.Errorf("export meta: %w", err))
		}
	}
}
```

Note: `encoding/json` is imported now because later structs in this file use `json.RawMessage`; if the linter flags it as unused after this task only, add the next struct in the same commit or import it when first needed. (Projects, the very next task, uses it.)

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportMeta`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportFilter and ExportMeta export reader"
```

---

### Task 2: `ExportProjects`

Mirrors the v10 `exportProjects` projection at `internal/jsonl/export.go:132-166`: `ProjectID` filter, no soft-delete filter (projects always include archived), metadata JSON validation.

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

Add the shared `collectExport` helper (first used here; later reader tests reuse it) and the `encoding/json` import to `export_test.go`, then the test. Inference is clean here because the test references `db` types directly, so no explicit type argument is needed.

```go
// collectExport drains an export iterator, failing on the first error.
func collectExport[T any](t *testing.T, seq func(yield func(T, error) bool)) []T {
	t.Helper()
	var out []T
	for v, err := range seq {
		require.NoError(t, err)
		out = append(out, v)
	}
	return out
}

func TestExportProjects(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	other, err := d.CreateProject(ctx, "other")
	require.NoError(t, err)

	// No filter: both projects, ordered by id.
	all := collectExport(t, d.ExportProjects(ctx, db.ExportFilter{}))
	require.Len(t, all, 2)
	require.Equal(t, p.ID, all[0].ID)
	require.Equal(t, other.ID, all[1].ID)
	require.Equal(t, p.UID, all[0].UID)
	require.True(t, json.Valid(all[0].Metadata), "metadata must be valid JSON")

	// ProjectID filter: only that project.
	one := collectExport(t, d.ExportProjects(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, one, 1)
	require.Equal(t, p.ID, one[0].ID)
}
```

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportProjects`
Expected: FAIL — `d.ExportProjects` / `db.ProjectExport` undefined.

- [ ] **Step 3: Write the minimal implementation**

Append to `internal/db/export.go`:

```go
// ProjectExport is one project row in export shape.
type ProjectExport struct {
	ID        int64           `json:"id"`
	UID       string          `json:"uid"`
	Name      string          `json:"name"`
	CreatedAt string          `json:"created_at"`
	DeletedAt *string         `json:"deleted_at,omitempty"`
	Metadata  json.RawMessage `json:"metadata"`
	Revision  int64           `json:"revision"`
}

// ExportProjects streams projects ordered by id. Archived (soft-deleted)
// projects are always included; ExportFilter.IncludeDeleted does not apply.
func (d *DB) ExportProjects(ctx context.Context, f ExportFilter) iter.Seq2[ProjectExport, error] {
	return func(yield func(ProjectExport, error) bool) {
		query := `SELECT id, uid, name, CAST(created_at AS TEXT),
		                 CAST(deleted_at AS TEXT), metadata, revision FROM projects`
		var args []any
		if f.ProjectID != nil {
			query += ` WHERE id = ?`
			args = append(args, *f.ProjectID)
		}
		query += ` ORDER BY id ASC`
		rows, err := d.QueryContext(ctx, query, args...)
		if err != nil {
			yield(ProjectExport{}, fmt.Errorf("export projects: %w", err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rec ProjectExport
			var metadata string
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.Name, &rec.CreatedAt, &rec.DeletedAt,
				&metadata, &rec.Revision); err != nil {
				yield(ProjectExport{}, fmt.Errorf("scan project: %w", err))
				return
			}
			if !json.Valid([]byte(metadata)) {
				yield(ProjectExport{}, fmt.Errorf("project %d metadata is invalid JSON", rec.ID))
				return
			}
			rec.Metadata = json.RawMessage(metadata)
			if !yield(rec, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(ProjectExport{}, fmt.Errorf("export projects: %w", err))
		}
	}
}
```

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportProjects`
Expected: PASS.

- [ ] **Step 5: Add iterator error-path tests (the pattern every reader follows)**

These pin the query-error and early-break semantics. They live in `export_test.go` and target `ExportProjects`, but the same two shapes apply to every reader; spot-check one or two other readers (e.g. `ExportIssues`) the same way.

```go
func TestExportProjectsContextCanceledErrors(t *testing.T) {
	d, _, _ := setupTestProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // force QueryContext to fail

	var sawErr error
	for _, err := range d.ExportProjects(ctx, db.ExportFilter{}) {
		sawErr = err
	}
	require.Error(t, sawErr, "a canceled context must surface as a terminal iterator error")
}

func TestExportProjectsEarlyBreak(t *testing.T) {
	d, ctx, _ := setupTestProject(t)
	_, err := d.CreateProject(ctx, "second")
	require.NoError(t, err)

	// Break after the first row; the deferred rows.Close must run cleanly.
	count := 0
	for _, err := range d.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		count++
		break
	}
	require.Equal(t, 1, count)
}
```

Run: `go test ./internal/db/ -run 'TestExportProjects'`
Expected: PASS (all three project tests). Add `"context"` to the test imports if not already present.

- [ ] **Step 6: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportProjects export reader"
```

---

### Task 3: `ExportIssues`

Mirrors the v10 `exportIssues` projection at `internal/jsonl/export.go:440-491`: `LEFT JOIN recurrences` for the portable `recurrence_uid`, the `issueExportWhere` filter (`project_id` + `deleted_at IS NULL` unless `IncludeDeleted`), metadata validation.

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestExportIssues(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	deleted, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "d", Author: "a"})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, deleted.ID, "a")
	require.NoError(t, err)

	// Default filter excludes soft-deleted issues.
	live := collectExport(t, d.ExportIssues(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, live, 1)
	require.Equal(t, issue.ID, live[0].ID)
	require.Equal(t, issue.UID, live[0].UID)
	require.True(t, json.Valid(live[0].Metadata))

	// IncludeDeleted surfaces the soft-deleted issue too, ordered by id.
	all := collectExport(t, d.ExportIssues(ctx, db.ExportFilter{ProjectID: &p.ID, IncludeDeleted: true}))
	require.Len(t, all, 2)
	require.Equal(t, issue.ID, all[0].ID)
	require.Equal(t, deleted.ID, all[1].ID)
	require.NotNil(t, all[1].DeletedAt)
}
```

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportIssues`
Expected: FAIL — `d.ExportIssues` / `db.IssueExport` undefined.

- [ ] **Step 3: Write the minimal implementation**

Append to `internal/db/export.go`:

```go
// IssueExport is one issue row in export shape (recurrence_uid resolved via join).
type IssueExport struct {
	ID            int64           `json:"id"`
	UID           string          `json:"uid"`
	ProjectID     int64           `json:"project_id"`
	ShortID       string          `json:"short_id"`
	Title         string          `json:"title"`
	Body          string          `json:"body"`
	Status        string          `json:"status"`
	ClosedReason  *string         `json:"closed_reason"`
	Owner         *string         `json:"owner"`
	Priority      *int64          `json:"priority,omitempty"`
	Author        string          `json:"author"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	ClosedAt      *string         `json:"closed_at"`
	DeletedAt     *string         `json:"deleted_at"`
	Metadata      json.RawMessage `json:"metadata"`
	Revision      int64           `json:"revision"`
	RecurrenceID  *int64          `json:"recurrence_id,omitempty"`
	RecurrenceUID *string         `json:"recurrence_uid,omitempty"`
	OccurrenceKey *string         `json:"occurrence_key,omitempty"`
}

// ExportIssues streams issues ordered by id, scoped and filtered by f.
func (d *DB) ExportIssues(ctx context.Context, f ExportFilter) iter.Seq2[IssueExport, error] {
	return func(yield func(IssueExport, error) bool) {
		query := `SELECT i.id, i.uid, i.project_id, i.short_id, i.title, i.body,
		                 i.status, i.closed_reason, i.owner, i.priority, i.author,
		                 CAST(i.created_at AS TEXT), CAST(i.updated_at AS TEXT),
		                 CAST(i.closed_at AS TEXT), CAST(i.deleted_at AS TEXT),
		                 i.metadata, i.revision,
		                 i.recurrence_id, r.uid, i.occurrence_key
		          FROM issues i
		          LEFT JOIN recurrences r ON r.id = i.recurrence_id`
		query += exportWhere("i", f) + ` ORDER BY i.id ASC`
		rows, err := d.QueryContext(ctx, query, exportArgs(f)...)
		if err != nil {
			yield(IssueExport{}, fmt.Errorf("export issues: %w", err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rec IssueExport
			var metadata string
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.ProjectID, &rec.ShortID, &rec.Title, &rec.Body,
				&rec.Status, &rec.ClosedReason, &rec.Owner, &rec.Priority, &rec.Author, &rec.CreatedAt,
				&rec.UpdatedAt, &rec.ClosedAt, &rec.DeletedAt, &metadata, &rec.Revision,
				&rec.RecurrenceID, &rec.RecurrenceUID, &rec.OccurrenceKey); err != nil {
				yield(IssueExport{}, fmt.Errorf("scan issue: %w", err))
				return
			}
			if !json.Valid([]byte(metadata)) {
				yield(IssueExport{}, fmt.Errorf("issue %d metadata is invalid JSON", rec.ID))
				return
			}
			rec.Metadata = json.RawMessage(metadata)
			if !yield(rec, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(IssueExport{}, fmt.Errorf("export issues: %w", err))
		}
	}
}

// exportWhere builds the standard "project scope + soft-delete" WHERE for a
// table alias, mirroring the old issueExportWhere. exportArgs returns its args.
func exportWhere(table string, f ExportFilter) string {
	clauses := exportScopeClauses(table, f)
	if len(clauses) == 0 {
		return ""
	}
	out := " WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		out += " AND " + c
	}
	return out
}

func exportScopeClauses(table string, f ExportFilter) []string {
	var clauses []string
	if f.ProjectID != nil {
		clauses = append(clauses, table+`.project_id = ?`)
	}
	if !f.IncludeDeleted {
		clauses = append(clauses, table+`.deleted_at IS NULL`)
	}
	return clauses
}

func exportArgs(f ExportFilter) []any {
	if f.ProjectID != nil {
		return []any{*f.ProjectID}
	}
	return nil
}
```

Note: `exportWhere`/`exportArgs` are the package-`db` analog of the old `issueExportWhere`. Later readers (comments, labels) reuse them; links and events need their own clause shapes (Tasks 4 and 6).

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportIssues`
Expected: PASS.

- [ ] **Step 5: Prove the filter assertion is non-vacuous**

Temporarily change `ExportIssues` to ignore the filter (`query += " ORDER BY i.id ASC"`, dropping `exportWhere`). Run the test: the default-filter case must FAIL (soft-deleted issue leaks). Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportIssues export reader"
```

---

### Task 4: `ExportEvents`

The most intricate reader. Mirrors the v10 `exportEvents` at `internal/jsonl/export.go:823-883`: the denormalized `events.project_name` (no join, no PRAGMA at v10), the `subject_issue`/`peer` LEFT JOINs, the orphan filter `(events.issue_id IS NULL OR subject_issue.id IS NOT NULL)`, the `related_issue_id`/`related_issue_uid` CASE scrub, the `eventExportWhereClauses` soft-delete predicates, and payload validation.

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestExportEvents(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	// setupTestIssue creates an issue, which emits an issue.created event.
	evs := collectExport(t, d.ExportEvents(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.NotEmpty(t, evs)
	last := evs[len(evs)-1]
	require.Equal(t, p.ID, last.ProjectID)
	require.NotEmpty(t, last.ProjectName, "denormalized project_name must be populated")
	require.NotNil(t, last.IssueID)
	require.Equal(t, issue.ID, *last.IssueID)
	require.True(t, json.Valid(last.Payload))
	// ordered by id ascending
	for i := 1; i < len(evs); i++ {
		require.Less(t, evs[i-1].ID, evs[i].ID)
	}
}
```

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportEvents`
Expected: FAIL — `d.ExportEvents` / `db.EventExport` undefined.

- [ ] **Step 3: Write the minimal implementation**

Append to `internal/db/export.go`:

```go
// EventExport is one event row in export shape. related_issue_id/uid are
// scrubbed to NULL when the peer row is missing (any type) or, on live-only
// export, when an issue.links_changed peer is soft-deleted.
type EventExport struct {
	ID                int64           `json:"id"`
	UID               string          `json:"uid"`
	OriginInstanceUID string          `json:"origin_instance_uid"`
	ProjectID         int64           `json:"project_id"`
	ProjectName       string          `json:"project_name"`
	IssueID           *int64          `json:"issue_id"`
	IssueUID          *string         `json:"issue_uid"`
	RelatedIssueID    *int64          `json:"related_issue_id"`
	RelatedIssueUID   *string         `json:"related_issue_uid"`
	Type              string          `json:"type"`
	Actor             string          `json:"actor"`
	Payload           json.RawMessage `json:"payload"`
	CreatedAt         string          `json:"created_at"`
}

// ExportEvents streams events ordered by id, reproducing the orphan filter and
// related-id scrub from the v10 jsonl export.
func (d *DB) ExportEvents(ctx context.Context, f ExportFilter) iter.Seq2[EventExport, error] {
	return func(yield func(EventExport, error) bool) {
		scrub := `(peer.id IS NULL AND events.related_issue_id IS NOT NULL)`
		if !f.IncludeDeleted {
			scrub += ` OR (events.type = 'issue.links_changed' AND peer.deleted_at IS NOT NULL)`
		}
		relatedID := `CASE WHEN ` + scrub + ` THEN NULL ELSE events.related_issue_id END`
		relatedUID := `CASE WHEN ` + scrub + ` THEN NULL ELSE events.related_issue_uid END`
		query := `SELECT events.id, events.uid, events.origin_instance_uid, events.project_id,
		                 events.project_name, events.issue_id, events.issue_uid,
		                 ` + relatedID + `, ` + relatedUID + `,
		                 events.type, events.actor, events.payload, CAST(events.created_at AS TEXT)
		          FROM events
		          LEFT JOIN issues subject_issue ON subject_issue.id = events.issue_id
		          LEFT JOIN issues peer ON peer.id = events.related_issue_id`

		clauses := []string{`(events.issue_id IS NULL OR subject_issue.id IS NOT NULL)`}
		var args []any
		if f.ProjectID != nil {
			clauses = append(clauses, `events.project_id = ?`)
			args = append(args, *f.ProjectID)
		}
		if !f.IncludeDeleted {
			clauses = append(clauses,
				`(events.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues WHERE issues.id = events.issue_id AND issues.deleted_at IS NULL))`,
				`(events.related_issue_id IS NULL OR events.type = 'issue.links_changed' OR NOT EXISTS (SELECT 1 FROM issues WHERE issues.id = events.related_issue_id AND issues.deleted_at IS NOT NULL))`,
			)
		}
		query += " WHERE " + clauses[0]
		for _, c := range clauses[1:] {
			query += " AND " + c
		}
		query += ` ORDER BY events.id ASC`

		rows, err := d.QueryContext(ctx, query, args...)
		if err != nil {
			yield(EventExport{}, fmt.Errorf("export events: %w", err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rec EventExport
			var payload string
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.ProjectName,
				&rec.IssueID, &rec.IssueUID, &rec.RelatedIssueID, &rec.RelatedIssueUID,
				&rec.Type, &rec.Actor, &payload, &rec.CreatedAt); err != nil {
				yield(EventExport{}, fmt.Errorf("scan event: %w", err))
				return
			}
			if !json.Valid([]byte(payload)) {
				yield(EventExport{}, fmt.Errorf("event %d payload is invalid JSON", rec.ID))
				return
			}
			rec.Payload = json.RawMessage(payload)
			if !yield(rec, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(EventExport{}, fmt.Errorf("export events: %w", err))
		}
	}
}
```

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportEvents`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportEvents export reader"
```

---

### Task 5: `ExportRecurrences`

Mirrors the v10 `exportRecurrences` at `internal/jsonl/export.go:351-425`. Copy the local `record` struct there verbatim into `db.RecurrenceExport` (same fields and JSON tags). Scope clause is `project_id`; the soft-delete clause is special: deleted recurrences are kept when still referenced by a live issue.

> For this and the other anchored readers (Tasks 6-8): line numbers are approximate and may drift — the authoritative anchor is the **function name** (`exportRecurrences`, `exportLinks`, `exportPurgeLog`, `exportProjectAliases`, `exportComments`, `exportIssueLabels`, `exportImportMappings`). Locate the function, then copy its local `record` struct and SELECT verbatim.

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestExportRecurrences(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	rec, err := d.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: p.ID, Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "t"},
	})
	require.NoError(t, err)

	got := collectExport(t, d.ExportRecurrences(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, rec.ID, got[0].ID)
	require.Equal(t, rec.UID, got[0].UID)
}
```

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportRecurrences`
Expected: FAIL — undefined.

- [ ] **Step 3: Write the minimal implementation**

Append `db.RecurrenceExport` (the verbatim copy of the local `record` struct at `export.go:351-425`, fields + JSON tags) and `ExportRecurrences`, following the Task 3 iterator skeleton. Port the SELECT verbatim. Build the WHERE as:

```go
clauses := []string{}
var args []any
if f.ProjectID != nil {
	clauses = append(clauses, `project_id = ?`)
	args = append(args, *f.ProjectID)
}
if !f.IncludeDeleted {
	clauses = append(clauses, `(deleted_at IS NULL OR id IN (SELECT DISTINCT recurrence_id FROM issues WHERE recurrence_id IS NOT NULL AND deleted_at IS NULL))`)
}
```

then `ORDER BY id ASC`. Reproduce any `json.Valid` validation present in the source scan function (template_labels / template_metadata).

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportRecurrences`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportRecurrences export reader"
```

---

### Task 6: `ExportLinks`

Mirrors the v10 `exportLinks` at `internal/jsonl/export.go:698-731`. Copy the local `record` struct verbatim into `db.LinkExport`. The filter mirrors `linkExportWhere`: `links.project_id = ?` (when scoped) and, unless `IncludeDeleted`, **both** `from_issues.deleted_at IS NULL` and `to_issues.deleted_at IS NULL` (the query joins `issues AS from_issues` and `issues AS to_issues`).

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestExportLinks(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "a", Author: "x"})
	require.NoError(t, err)
	b, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "b", Author: "x"})
	require.NoError(t, err)
	_, err = d.CreateLink(ctx, db.CreateLinkParams{ProjectID: p.ID, FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks"})
	require.NoError(t, err)

	got := collectExport(t, d.ExportLinks(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, a.ID, got[0].FromIssueID)
	require.Equal(t, b.ID, got[0].ToIssueID)

	// Soft-deleting an endpoint drops the link under the default filter.
	_, _, _, err = d.SoftDeleteIssue(ctx, b.ID, "x")
	require.NoError(t, err)
	require.Empty(t, collectExport(t, d.ExportLinks(ctx, db.ExportFilter{ProjectID: &p.ID})))
}
```

The `CreateLinkParams` field is `Type` (not `LinkType`). Adjust `LinkExport` field names (`FromIssueID`, `ToIssueID`, etc.) to match the `record` struct copied from the source.

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportLinks`
Expected: FAIL — undefined.

- [ ] **Step 3: Write the minimal implementation**

Append `db.LinkExport` (verbatim from `export.go:698-731`) and `ExportLinks` following the Task 3 skeleton. Port the `FROM links JOIN issues AS from_issues … JOIN issues AS to_issues …` query verbatim. WHERE:

```go
clauses := []string{}
var args []any
if f.ProjectID != nil {
	clauses = append(clauses, `links.project_id = ?`)
	args = append(args, *f.ProjectID)
}
if !f.IncludeDeleted {
	clauses = append(clauses, `from_issues.deleted_at IS NULL`, `to_issues.deleted_at IS NULL`)
}
```

then `ORDER BY links.id ASC`.

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportLinks`
Expected: PASS.

- [ ] **Step 5: Prove the endpoint-delete filter**

Temporarily drop the `from_issues/to_issues` delete clauses; the soft-delete sub-case must FAIL. Revert.

- [ ] **Step 6: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportLinks export reader"
```

---

### Task 7: `ExportPurgeLog`

Mirrors the v10 `exportPurgeLog` at `internal/jsonl/export.go:1050-1112` (and its scan at `:1248-1258`). Copy the local `record` struct verbatim into `db.PurgeLogExport` (the full ~21-column set). At v10 the project name is the denormalized `purge_log.project_name` (no join, no PRAGMA). Scope clause: `purge_log.project_id = ?`; `ORDER BY purge_log.id ASC`. There is no soft-delete clause on purge_log.

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

Seed a purge: create an issue, then `PurgeIssue`. Assert one `PurgeLogExport` row scoped to the project, with `IssueUID`/`ProjectName` populated.

```go
func TestExportPurgeLog(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	_, err := d.PurgeIssue(ctx, issue.ID, "x", nil)
	require.NoError(t, err)

	got := collectExport(t, d.ExportPurgeLog(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, p.ID, got[0].ProjectID)
	require.NotEmpty(t, got[0].ProjectName)
}
```

Adjust field names to the struct copied from source.

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportPurgeLog`
Expected: FAIL — undefined.

- [ ] **Step 3: Write the minimal implementation**

Append `db.PurgeLogExport` (verbatim) and `ExportPurgeLog`. Port the v10 SELECT verbatim using `purge_log.project_name` directly (drop the legacy `purgeProjectNameExpr` join branch — that is pre-v7 only and stays in cutover). WHERE: `purge_log.project_id = ?` when `f.ProjectID != nil`; `ORDER BY purge_log.id ASC`. Reproduce any payload/JSON validation in the source scan.

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportPurgeLog`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportPurgeLog export reader"
```

---

### Task 8: Simple uniform readers — aliases, comments, labels, import_mappings

Four single-table (or single-join) readers that follow the Task 2 / Task 3 pattern exactly. Do each as its own RED→GREEN→commit cycle. For each: copy the local `record` struct verbatim into the named `XExport` type (same fields + JSON tags), port the SELECT verbatim, fold `ExportFilter`, preserve `ORDER BY`.

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **8a: `ExportProjectAliases`** — source `export.go:320-349`. Struct `db.AliasExport`. Query `SELECT id, project_id, alias_identity, alias_kind, root_path, CAST(created_at AS TEXT), CAST(last_seen_at AS TEXT) FROM project_aliases`; scope `project_id = ?` when set; `ORDER BY id ASC`. No soft-delete clause. Test: attach an alias to a project (`AttachAlias`), assert one row with the right `project_id`/`root_path`. RED→GREEN. Commit: `feat(db): add ExportProjectAliases export reader`.

- [ ] **8b: `ExportComments`** — source `export.go:651-673`. Struct `db.CommentExport`. Query `… FROM comments JOIN issues ON issues.id = comments.issue_id` with `exportWhere("issues", f)` (soft-delete rides on the parent issue) and `ORDER BY comments.id ASC`. Test: create issue + two comments, assert two rows in id order; soft-delete the issue and assert default filter yields none. RED→GREEN. Commit: `feat(db): add ExportComments export reader`.

- [ ] **8c: `ExportIssueLabels`** — source `export.go:675-696`. Struct `db.IssueLabelExport`. Query `… FROM issue_labels JOIN issues …` with `exportWhere("issues", f)` and `ORDER BY issue_labels.issue_id ASC, issue_labels.label ASC`. Test: add two labels to an issue, assert both rows ordered by label. RED→GREEN. Commit: `feat(db): add ExportIssueLabels export reader`.

- [ ] **8d: `ExportImportMappings`** — source `export.go:762-810`. Struct `db.ImportMappingExport`. Port the SELECT verbatim including the two `!IncludeDeleted` EXISTS filters that drop mappings whose underlying issue/link is soft-deleted; scope `project_id = ?` when set; `ORDER BY id ASC`. Test: `UpsertImportMapping` for an issue, assert one row; (optional) soft-delete the issue and assert it drops under the default filter. RED→GREEN. Commit: `feat(db): add ExportImportMappings export reader`.

After each sub-step run its test (`go test ./internal/db/ -run TestExport<Name>`), watch it fail before implementing, then pass.

---

### Task 9: `ExportSequences`

Mirrors `exportSQLiteSequence` at `internal/jsonl/export.go:1378-1392`. SQLite-only data; on the future Postgres backend this method yields nothing.

**Files:**
- Modify: `internal/db/export.go`
- Test: `internal/db/export_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestExportSequences(t *testing.T) {
	d, ctx, _, _ := setupTestIssue(t) // creating rows advances sqlite_sequence
	got := collectExport(t, d.ExportSequences(ctx))
	require.NotEmpty(t, got)
	for i := 1; i < len(got); i++ {
		require.Less(t, got[i-1].Name, got[i].Name) // ORDER BY name ASC
	}
}
```

- [ ] **Step 2: Run the test, watch it fail**

Run: `go test ./internal/db/ -run TestExportSequences`
Expected: FAIL — undefined.

- [ ] **Step 3: Write the minimal implementation**

```go
// SequenceExport is one sqlite_sequence row (SQLite-only; KindSQLiteSequence).
type SequenceExport struct {
	Name string `json:"name"`
	Seq  int64  `json:"seq"`
}

// ExportSequences streams sqlite_sequence rows ordered by name. SQLite-only.
func (d *DB) ExportSequences(ctx context.Context) iter.Seq2[SequenceExport, error] {
	return func(yield func(SequenceExport, error) bool) {
		rows, err := d.QueryContext(ctx, `SELECT name, seq FROM sqlite_sequence ORDER BY name ASC`)
		if err != nil {
			yield(SequenceExport{}, fmt.Errorf("export sqlite_sequence: %w", err))
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var rec SequenceExport
			if err := rows.Scan(&rec.Name, &rec.Seq); err != nil {
				yield(SequenceExport{}, fmt.Errorf("scan sqlite_sequence: %w", err))
				return
			}
			if !yield(rec, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(SequenceExport{}, fmt.Errorf("export sqlite_sequence: %w", err))
		}
	}
}
```

- [ ] **Step 4: Run the test, watch it pass**

Run: `go test ./internal/db/ -run TestExportSequences`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/export.go internal/db/export_test.go
git commit -m "feat(db): add ExportSequences export reader"
```

---

### Task 10: Neutralize `jsonl.Export`; relocate legacy export for cutover

Switch `jsonl.Export` to `db.Storage` + iterators (current schema only), and move the version-dispatch / legacy raw-SQL export into a SQLite-bound `exportForCutover` that cutover uses. Behavior is preserved; the existing `export_test.go` / `roundtrip_test.go` / `cutover_test.go` are the guard.

**Execution note (addresses review jobs 4937-4940 — do this as TWO commits, not one):**

- **10a (pure move, no behavior change):** Extract the current `Export` body + every `…V1..V8` variant + the legacy helpers + the v10 inline readers + shared helpers into `exportForCutover(ctx, d *db.DB, w, opts)` in `cutover_export.go`. Make `Export` a thin delegator (`return exportForCutover(...)`), keeping its `*db.DB` signature for now. Point `cutover.go` at `exportForCutover`. Verify `go build ./... && go test ./internal/jsonl/ ./cmd/...` is green (the suite is unchanged because behavior is identical). Commit: `refactor(jsonl): extract exportForCutover (pure move)`.
- **10b (neutralize):** Add the 12 methods to the `Storage` interface (Step 1 below), rewrite `Export` to the `db.Storage` + iterator form (Step 4 below) so it no longer delegates, and add the byte-fidelity guard + legacy-test handling below. Commit: `refactor(jsonl): route Export through db.Storage iterators`.

**Byte-fidelity guard (new, in 10b):** add `internal/jsonl/export_internal_test.go` (package `jsonl`, so it can call the unexported `exportForCutover`). Seed a representative current-schema DB (a project; one live and one soft-deleted issue; a comment; a label; a link; a recurrence; rely on the auto-emitted events), then:

```go
var neu, old bytes.Buffer
require.NoError(t, Export(ctx, d, &neu, ExportOptions{IncludeDeleted: true}))
require.NoError(t, exportForCutover(ctx, d, &old, ExportOptions{IncludeDeleted: true}))
require.Equal(t, old.String(), neu.String(), "neutral export must be byte-identical to the legacy exporter for current schema")
```

This directly proves the entity order, per-entity id order, and JSON field tags are byte-for-byte stable (roundtrip tests alone only prove semantic equivalence).

**Legacy-test access (Step 5 below):** the jsonl tests are `package jsonl_test` and cannot reach the unexported `exportForCutover`. Existing `jsonl.Export(ctx, d, …)` callers pass a concrete `*db.DB`, which satisfies the new `db.Storage` parameter, so they compile unchanged. If any `jsonl_test` case builds a **pre-v10** schema and calls `jsonl.Export` expecting legacy dispatch, route it through the public `AutoCutover` API or move it into the internal test file — do NOT attempt to call `exportForCutover` from `jsonl_test`. Verify which case applies with the `rg` in Step 5 before editing.

**Files:**
- Modify: `internal/db/storage.go`
- Create: `internal/jsonl/cutover_export.go`
- Modify: `internal/jsonl/export.go`
- Modify: `internal/jsonl/cutover.go`
- Modify: `internal/jsonl/export_test.go` (repoint any pre-v10-schema cases)

- [ ] **Step 1: Add the 12 export methods to the `Storage` interface**

In `internal/db/storage.go`, under a new `// export (JSONL)` group, add the signatures exactly as implemented:

```go
	ExportMeta(ctx context.Context) iter.Seq2[MetaKV, error]
	ExportProjects(ctx context.Context, f ExportFilter) iter.Seq2[ProjectExport, error]
	ExportProjectAliases(ctx context.Context, f ExportFilter) iter.Seq2[AliasExport, error]
	ExportRecurrences(ctx context.Context, f ExportFilter) iter.Seq2[RecurrenceExport, error]
	ExportIssues(ctx context.Context, f ExportFilter) iter.Seq2[IssueExport, error]
	ExportComments(ctx context.Context, f ExportFilter) iter.Seq2[CommentExport, error]
	ExportIssueLabels(ctx context.Context, f ExportFilter) iter.Seq2[IssueLabelExport, error]
	ExportLinks(ctx context.Context, f ExportFilter) iter.Seq2[LinkExport, error]
	ExportImportMappings(ctx context.Context, f ExportFilter) iter.Seq2[ImportMappingExport, error]
	ExportEvents(ctx context.Context, f ExportFilter) iter.Seq2[EventExport, error]
	ExportPurgeLog(ctx context.Context, f ExportFilter) iter.Seq2[PurgeLogExport, error]
	ExportSequences(ctx context.Context) iter.Seq2[SequenceExport, error]
```

Add `"iter"` to the `storage.go` import block. Run `go build ./internal/db/` — the `var _ Storage = (*DB)(nil)` assertion must still compile (it proves `*DB` implements all 12).

- [ ] **Step 2: Relocate the legacy exporter**

Create `internal/jsonl/cutover_export.go`. Move from `export.go` into it, unchanged except the rename: the entire current `Export` function (renamed `exportForCutover`), every `…V1..V8` variant, and the legacy-only helpers `schemaVersion`, `tableHasColumn`, `eventProjectNameExpr`, `purgeProjectNameExpr`, plus the v10 inline readers and shared helpers it still calls (`exportMeta`, `exportProjects`, … `exportSQLiteSequence`, `issueExportWhere`, `linkExportWhere`, `eventExportWhereClauses`, `whereClause`, `joinClauses`, `scanRecords`). `exportForCutover` keeps the `*db.DB` signature and the full version dispatch, so cutover's output is byte-identical. Keep `writeRecord` and `metaRecord` accessible (same package).

- [ ] **Step 3: Point cutover at `exportForCutover`**

In `internal/jsonl/cutover.go`, change the `Export(ctx, src, …)` call to `exportForCutover(ctx, src, …)`.

- [ ] **Step 4: Write the new neutral `Export`**

In `internal/jsonl/export.go`, replace the old `Export` with:

```go
func Export(ctx context.Context, store db.Storage, w io.Writer, opts ExportOptions) error {
	enc := NewEncoder(w)
	f := db.ExportFilter{IncludeDeleted: opts.IncludeDeleted}
	if opts.ProjectID > 0 {
		f.ProjectID = &opts.ProjectID
	}

	// export_version mirrors the DB's stored schema_version, matching the old
	// behavior (which read meta.schema_version directly).
	v, err := store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if err := writeRecord(enc, KindMeta, metaRecord{Key: "export_version", Value: strconv.Itoa(v)}); err != nil {
		return err
	}

	if err := streamExport(enc, KindMeta, store.ExportMeta(ctx)); err != nil {
		return err
	}
	if err := streamExport(enc, KindProject, store.ExportProjects(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindProjectAlias, store.ExportProjectAliases(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindRecurrence, store.ExportRecurrences(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindIssue, store.ExportIssues(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindComment, store.ExportComments(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindIssueLabel, store.ExportIssueLabels(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindLink, store.ExportLinks(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindImportMapping, store.ExportImportMappings(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindEvent, store.ExportEvents(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindPurgeLog, store.ExportPurgeLog(ctx, f)); err != nil {
		return err
	}
	return streamExport(enc, KindSQLiteSequence, store.ExportSequences(ctx))
}

func streamExport[T any](enc *Encoder, kind Kind, seq iter.Seq2[T, error]) error {
	for rec, err := range seq {
		if err != nil {
			return err
		}
		if err := writeRecord(enc, kind, rec); err != nil {
			return err
		}
	}
	return nil
}
```

After the move, `export.go`'s imports should be `context`, `encoding/json`, `fmt`, `io`, `iter`, `strconv`, and `go.kenn.io/kata/internal/db` (`writeRecord` keeps `encoding/json` + `fmt`; the new `Export` uses `strconv` + `iter`). `database/sql` and `strings` move to `cutover_export.go` with the relocated code. `writeRecord`, `metaRecord`, and the `Encoder` stay in the neutral files so both exporters share them. The neutral path's `export_version` comes from `store.SchemaVersion`; cutover (which reads older `export_version`s from old files) goes through `exportForCutover`, which preserves the source version string.

- [ ] **Step 5: Repoint pre-v10 export tests**

Run: `rg -n "jsonl\.Export\(|exportProjectsV|sourceSchemaVersion" internal/jsonl/*_test.go`
Any test that builds a pre-v10 schema and calls `jsonl.Export` must call `exportForCutover` instead (it exercises the legacy path). Current-schema export/roundtrip cases stay on `jsonl.Export`. `cmd/kata/export.go` is unchanged: it passes a concrete `*db.DB`, which satisfies the new `db.Storage` parameter.

- [ ] **Step 6: Build and run the full suite**

Run: `go build ./... && go test ./internal/jsonl/ ./internal/db/ ./cmd/...`
Expected: exit 0; `ok` for each. The roundtrip and cutover tests prove byte-fidelity and cutover behavior are preserved.

- [ ] **Step 7: Commit**

```bash
git add internal/db/storage.go internal/jsonl/export.go internal/jsonl/cutover_export.go internal/jsonl/cutover.go internal/jsonl/export_test.go
git commit -m "refactor(jsonl): route Export through db.Storage iterators; isolate legacy cutover export"
```

---

### Task 11: Verify the boundary and the suite

**Files:** none (verification only)

- [ ] **Step 1: Confirm the neutral export path holds no raw SQL**

Run: `rg -n "QueryContext|QueryRowContext|ExecContext|BeginTx|PRAGMA|sqlite_sequence" internal/jsonl/export.go`
Expected: no hits. (All raw SQL now lives in `cutover_export.go`, used solely by cutover.)

- [ ] **Step 2: Confirm `jsonl.Export` depends on `db.Storage`, not `*db.DB`**

Run: `rg -n "func Export\(" internal/jsonl/export.go`
Expected: the signature reads `store db.Storage`.

- [ ] **Step 3: Confirm the caller-selection rule holds**

Run: `rg -n "jsonl\.Export\b|exportForCutover" internal/jsonl/cutover.go cmd/kata/export.go`
Expected: `cutover.go` references only `exportForCutover` (never the package-local `Export`); `cmd/kata/export.go` references only the public `Export` (never `exportForCutover`). This guards the by-caller separation the design depends on.

- [ ] **Step 4: Full build, test, lint**

Run: `go build ./... && go test ./...`
Expected: exit 0; every package `ok` or `no test files`, no `FAIL`.

Run: `golangci-lint run` (via `nix run 'nixpkgs#golangci-lint' -- run` if not on PATH)
Expected: 0 issues.

- [ ] **Step 5: No commit** (verification only).

---

## What this plan leaves for later phases

- **1c-import** — the coarse atomic `ImportReplay` method and its SQLite-only internals (sequence reconciliation, deferred-FK, validation); rewrite `jsonl.ImportWithOptions` to decode → map → call it.
- **1d** — move the SQLite impl into `internal/db/sqlitestore`; `db.Open`/`OpenReadOnly` return `db.Storage`; flip the `cmd/kata` export/import holders and `internal/testenv`.
