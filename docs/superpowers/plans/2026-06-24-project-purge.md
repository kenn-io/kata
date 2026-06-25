# `kata projects purge` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a supported, gated, permanent hard-delete (`kata projects purge <project>`) for archived projects that removes all project-scoped data, records an audit tombstone, and frees the name for reuse.

**Architecture:** A new `Store.PurgeProject` runs a `BEGIN IMMEDIATE` transaction (modeled on `PurgeIssue`) that FK-safely deletes a project's rows, reserves an SSE reset cursor, and writes a `project_purge_log` tombstone. A new daemon route `POST /api/v1/projects/{project_id}/actions/purge` (X-Kata-Confirm gated) drives it and broadcasts a live reset. A new `kata projects purge` CLI subcommand reuses the issue-purge confirmation plumbing. A new table means a schema bump (19→20) with JSONL cutover wiring.

**Tech Stack:** Go, SQLite (`internal/db/sqlitestore`), huma v2 (daemon), cobra (CLI), oapi-codegen (generated client).

**Spec:** `docs/superpowers/specs/2026-06-24-project-purge-design.md`

## Global Constraints

- TDD: write a failing test before implementation (project rule: "Test First").
- Function ≤100 lines, cyclomatic complexity ≤8, ≤5 positional params, 100-char lines (user CLAUDE.md).
- Commit every code-producing step group; never `--amend`/squash; never `--no-verify`.
- No private project names in tests/fixtures — use `spoke-project`, `hub-project`, `example-workspace` (project rule).
- Confirmation token for projects is the exact string `PURGE <project_name>` (no `#shortID`).
- Purge is **archived-only**; refuse active projects and any federated project.
- Cross-project (moved-issue) events are **detached, not deleted** (NULL both id+uid columns); the reset scope is project-B + global only (accepted, tested limitation).
- New table's legacy JSONL export MUST be gated `sourceSchemaVersion >= 20`.
- A new daemon route requires regenerating OpenAPI + client (`make api-generate`); `make api-check` must pass.

## File structure

| File | Responsibility |
|------|----------------|
| `internal/db/sqlitestore/schema.sql` | + `project_purge_log` table + 2 reset indexes |
| `internal/db/pgstore/schema.sql` | same table + indexes (parity) |
| `internal/db/schema_version.go` | `currentSchemaVersion` 19 → 20 |
| `internal/db/types.go` | + `db.ProjectPurgeLog` |
| `internal/db/export_types.go` | + `db.ProjectPurgeLogExport` |
| `internal/db/params.go` | + `db.PurgeProjectParams` |
| `internal/db/errors.go` | + `ErrProjectNotArchived`, `ProjectFederatedError` |
| `internal/db/storage.go` | + `PurgeProject` interface method |
| `internal/db/pgstore/stubs_gen.go` | regenerated stub |
| `internal/db/sqlitestore/queries_projects_purge.go` | `PurgeProject` impl + helpers |
| `internal/db/sqlitestore/queries_events.go` | `PurgeResetCheck` reads both tables |
| `internal/db/sqlitestore/export.go` | + `Store.ExportProjectPurgeLog` |
| `internal/jsonl/types.go` | + `KindProjectPurgeLog`; bump `KindSQLiteSequence` order |
| `internal/jsonl/export.go` | + gated `exportProjectPurgeLog` |
| `internal/jsonl/storage_export.go` | stream the new kind |
| `internal/db/import_types.go` | + `ImportKindProjectPurgeLog`, `ImportRecord.ProjectPurgeLog` |
| `internal/jsonl/<decoder>` | Kind→ImportRecord decode case |
| `internal/db/sqlitestore/import_replay.go` | `importProjectPurgeLog` + table list |
| `internal/api/types.go` | + `ProjectPurgeRequest`/`ProjectPurgeResponse` |
| `internal/daemon/handlers_projects.go` | + route, handler, reset broadcast |
| `cmd/kata/projects_purge.go` | + `projectsPurgeCmd` + printer |
| `cmd/kata/projects.go` | register `projectsPurgeCmd` |
| `api/openapi.yaml`, `pkg/client/openapi.yaml`, `pkg/client/generated/*` | regenerated |
| `docs/...` | CLI reference / projects guide (feature lands) |

---

## Task 1: Schema — `project_purge_log` table + version bump

**Files:**
- Modify: `internal/db/sqlitestore/schema.sql`
- Modify: `internal/db/pgstore/schema.sql`
- Modify: `internal/db/schema_version.go`
- Test: `internal/db/sqlitestore/schema_completeness_test.go:20-27`

**Interfaces:**
- Produces: a `project_purge_log` table at schema version 20.

- [ ] **Step 1: Add the table to the completeness test (failing).** In `schema_completeness_test.go`, add `"project_purge_log"` to the `wanted` slice (after `"purge_log"`):

```go
	wanted := []string{
		"projects", "project_aliases", "issues", "comments",
		"links", "issue_labels", "events", "purge_log", "project_purge_log",
		"api_tokens", "federation_bindings", "federation_sync_status",
		"federation_quarantine", "federation_enrollments",
		"issue_sync_bindings", "issue_sync_status",
		"issue_claims", "pending_claim_requests",
		"meta", "issues_fts", "import_mappings", "recurrences",
	}
```

- [ ] **Step 2: Run it to verify it fails.**

Run: `go test ./internal/db/sqlitestore/ -run TestAllSchemaTablesExist -v`
Expected: FAIL (`schema object "project_purge_log" ... not found`).

- [ ] **Step 3: Add the table + indexes.** In `internal/db/sqlitestore/schema.sql`, immediately after the `purge_log` indexes block (after line 276), insert:

```sql
CREATE TABLE project_purge_log (
  id                          INTEGER PRIMARY KEY AUTOINCREMENT,
  uid                         TEXT NOT NULL UNIQUE,
  origin_instance_uid         TEXT NOT NULL,
  project_id                  INTEGER NOT NULL,   -- snapshot; no FK so audit survives the delete
  project_uid                 TEXT,               -- snapshot; project row is gone after purge
  project_name                TEXT NOT NULL,      -- snapshot of projects.name at purge time
  issue_count                 INTEGER NOT NULL,
  event_count                 INTEGER NOT NULL,   -- project-local events deleted (project_id = B)
  alias_count                 INTEGER NOT NULL,
  comment_count               INTEGER NOT NULL,
  link_count                  INTEGER NOT NULL,
  label_count                 INTEGER NOT NULL,
  claim_count                 INTEGER NOT NULL,
  pending_claim_request_count INTEGER NOT NULL,
  events_deleted_min_id       INTEGER,            -- min events.id deleted; NULL if none
  events_deleted_max_id       INTEGER,            -- max events.id deleted; NULL if none
  purge_reset_after_event_id  INTEGER,            -- SSE reset cursor; subscribers with cursor < this must reset
  actor                       TEXT NOT NULL,
  reason                      TEXT,
  purged_at                   DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  CHECK (length(trim(actor)) > 0),
  CHECK (length(uid) = 26),
  CHECK (length(origin_instance_uid) = 26)
);
CREATE INDEX idx_project_purge_log_reset
  ON project_purge_log(purge_reset_after_event_id) WHERE purge_reset_after_event_id IS NOT NULL;
CREATE INDEX idx_project_purge_log_project_reset
  ON project_purge_log(project_id, purge_reset_after_event_id) WHERE purge_reset_after_event_id IS NOT NULL;
```

- [ ] **Step 4: Add the same table + indexes to `internal/db/pgstore/schema.sql`.** Find the `purge_log` table there and add an analogous `project_purge_log` after it. Use Postgres types: `id BIGSERIAL PRIMARY KEY`, `INTEGER`→`BIGINT`, `DATETIME`→`TIMESTAMPTZ NOT NULL DEFAULT now()`, keep `CHECK` constraints. Mirror the exact column set above. (If pgstore has its own completeness test list, add `project_purge_log` there too — check `internal/db/pgstore/schema_test.go`.)

- [ ] **Step 5: Bump the schema version.** In `internal/db/schema_version.go`:

```go
const currentSchemaVersion = 20
```

- [ ] **Step 6: Run the completeness + schema tests to verify pass.**

Run: `go test ./internal/db/sqlitestore/ -run 'TestAllSchemaTablesExist|TestSchema' -v` and `go test ./internal/db/pgstore/ -run TestSchema -v`
Expected: PASS.

- [ ] **Step 7: Commit.**

```bash
git add internal/db/sqlitestore/schema.sql internal/db/pgstore/schema.sql internal/db/schema_version.go internal/db/sqlitestore/schema_completeness_test.go internal/db/pgstore/schema_test.go
git commit -m "Add project_purge_log table and bump schema to v20"
```

---

## Task 2: db types, params, errors, export type

**Files:**
- Modify: `internal/db/types.go`
- Modify: `internal/db/export_types.go`
- Modify: `internal/db/params.go`
- Modify: `internal/db/errors.go`

**Interfaces:**
- Produces: `db.ProjectPurgeLog`, `db.ProjectPurgeLogExport`, `db.PurgeProjectParams`, `db.ErrProjectNotArchived`, `db.ProjectFederatedError{Role db.FederationRole}`.

- [ ] **Step 1: Add `db.ProjectPurgeLog`** to `internal/db/types.go` (after the `PurgeLog` struct ~line 515):

```go
// ProjectPurgeLog is the durable audit tombstone written when a project is
// permanently purged. No FK to projects: the row outlives the deleted project.
type ProjectPurgeLog struct {
	ID                       int64     `json:"id"`
	UID                      string    `json:"uid"`
	OriginInstanceUID        string    `json:"origin_instance_uid"`
	ProjectID                int64     `json:"project_id"`
	ProjectUID               *string   `json:"project_uid,omitempty"`
	ProjectName              string    `json:"project_name"`
	IssueCount               int64     `json:"issue_count"`
	EventCount               int64     `json:"event_count"`
	AliasCount               int64     `json:"alias_count"`
	CommentCount             int64     `json:"comment_count"`
	LinkCount                int64     `json:"link_count"`
	LabelCount               int64     `json:"label_count"`
	ClaimCount               int64     `json:"claim_count"`
	PendingClaimRequestCount int64     `json:"pending_claim_request_count"`
	EventsDeletedMinID       *int64    `json:"events_deleted_min_id,omitempty"`
	EventsDeletedMaxID       *int64    `json:"events_deleted_max_id,omitempty"`
	PurgeResetAfterEventID   *int64    `json:"purge_reset_after_event_id,omitempty"`
	Actor                    string    `json:"actor"`
	Reason                   *string   `json:"reason,omitempty"`
	PurgedAt                 time.Time `json:"purged_at"`
}
```

- [ ] **Step 2: Add `db.ProjectPurgeLogExport`** to `internal/db/export_types.go` (after `PurgeLogExport` ~line 282). Identical fields but `PurgedAt string` (cutover carries it as text):

```go
// ProjectPurgeLogExport is one project_purge_log row in export shape.
type ProjectPurgeLogExport struct {
	ID                       int64   `json:"id"`
	UID                      string  `json:"uid"`
	OriginInstanceUID        string  `json:"origin_instance_uid"`
	ProjectID                int64   `json:"project_id"`
	ProjectUID               *string `json:"project_uid"`
	ProjectName              string  `json:"project_name"`
	IssueCount               int64   `json:"issue_count"`
	EventCount               int64   `json:"event_count"`
	AliasCount               int64   `json:"alias_count"`
	CommentCount             int64   `json:"comment_count"`
	LinkCount                int64   `json:"link_count"`
	LabelCount               int64   `json:"label_count"`
	ClaimCount               int64   `json:"claim_count"`
	PendingClaimRequestCount int64   `json:"pending_claim_request_count"`
	EventsDeletedMinID       *int64  `json:"events_deleted_min_id"`
	EventsDeletedMaxID       *int64  `json:"events_deleted_max_id"`
	PurgeResetAfterEventID   *int64  `json:"purge_reset_after_event_id"`
	Actor                    string  `json:"actor"`
	Reason                   *string `json:"reason"`
	PurgedAt                 string  `json:"purged_at"`
}
```

- [ ] **Step 3: Add `db.PurgeProjectParams`** to `internal/db/params.go` (after `RemoveProjectParams` ~line 304):

```go
// PurgeProjectParams are the inputs to Storage.PurgeProject.
type PurgeProjectParams struct {
	ProjectID int64
	Actor     string
	Reason    *string
}
```

- [ ] **Step 4: Add errors** to `internal/db/errors.go` (near `ErrProjectAlreadyArchived` ~line 91 and the `ProjectHasOpenIssuesError` ~line 233):

```go
var ErrProjectNotArchived = errors.New("project is not archived")
```

```go
// ProjectFederatedError reports that a project still has a federation binding
// and must be detached before it can be purged.
type ProjectFederatedError struct {
	Role FederationRole
}

func (e *ProjectFederatedError) Error() string {
	return fmt.Sprintf("project is federated (role=%s)", e.Role)
}
```

(`fmt` is already imported in `errors.go`; if not, add it.)

- [ ] **Step 5: Verify it compiles.**

Run: `go build ./internal/db/...`
Expected: success (no test yet; these types are exercised in later tasks).

- [ ] **Step 6: Commit.**

```bash
git add internal/db/types.go internal/db/export_types.go internal/db/params.go internal/db/errors.go
git commit -m "Add ProjectPurgeLog types, params, and purge errors"
```

---

## Task 3: Storage interface method + pgstore stub

**Files:**
- Modify: `internal/db/storage.go` (near `PurgeIssue` ~line 77)
- Modify: `internal/db/pgstore/stubs_gen.go` (regenerated)

**Interfaces:**
- Produces: `PurgeProject(ctx context.Context, p PurgeProjectParams) (ProjectPurgeLog, error)` on `db.Storage`.

- [ ] **Step 1: Add the interface method** to `internal/db/storage.go`, grouped with the destructive/project methods (e.g. right after the `PurgeIssue` line):

```go
	// PurgeProject permanently deletes an archived project and all its
	// project-scoped rows, writing a project_purge_log tombstone. Refuses
	// active (ErrProjectNotArchived) or federated (*ProjectFederatedError)
	// projects; ErrNotFound if the project does not exist.
	PurgeProject(ctx context.Context, p PurgeProjectParams) (ProjectPurgeLog, error)
```

- [ ] **Step 2: Run build to confirm pgstore now fails the interface.**

Run: `go build ./internal/db/pgstore/`
Expected: FAIL (`*Store does not implement db.Storage (missing method PurgeProject)`).

- [ ] **Step 3: Regenerate the pgstore stub.**

Run: `cd internal/db/pgstore && go run ./stubgen && cd -`
(If `./stubgen` is not the dir, check the `//go:generate` directive at the top of `pgstore/store.go`; run that command. The generator skips methods listed in `stubgen/main.go`'s `alreadyImplemented`, so the new method gets a stub returning a not-implemented error.)

- [ ] **Step 4: Verify build passes.**

Run: `go build ./internal/db/...`
Expected: success. Confirm `stubs_gen.go` now contains a `PurgeProject` stub.

- [ ] **Step 5: Commit.**

```bash
git add internal/db/storage.go internal/db/pgstore/stubs_gen.go
git commit -m "Add PurgeProject to Storage interface; regen pgstore stub"
```

---

## Task 4: `PurgeProject` storage implementation (sqlitestore)

This is the core. Build it test-first. Mirror `PurgeIssue`'s `BEGIN IMMEDIATE` conn pattern (`queries_delete.go:200-243`) and `reserveEventSequence`/`scanCount` helpers (`queries_delete.go:399-426`).

**Files:**
- Create: `internal/db/sqlitestore/queries_projects_purge.go`
- Create: `internal/db/sqlitestore/queries_projects_purge_test.go`

**Interfaces:**
- Consumes: `db.PurgeProjectParams`, `db.ProjectPurgeLog`, `db.ErrProjectNotArchived`, `db.ProjectFederatedError`, `reserveEventSequence`, `scanCount`, `scanProject`, `isSystemProject`, `projectSelect`, `Store.instanceUID`, `katauid.New`.
- Produces: `(*Store).PurgeProject`.

- [ ] **Step 1: Write the failing happy-path test.** Create `queries_projects_purge_test.go`. Mirror the harness in `queries_projects_remove_test.go` (use `openTestDB(t)` and the `Store` methods). This test purges an archived project with one issue and asserts the name frees + tombstone counts + FK integrity:

```go
package sqlitestore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestPurgeProject_DeletesArchivedProjectAndFreesName(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)

	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID, Title: "doomed", Author: "tester",
	})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: iss.ID, Author: "tester", Body: "bye",
	})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	pl, err := d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	assert.Equal(t, p.ID, pl.ProjectID)
	assert.Equal(t, "spoke-project", pl.ProjectName)
	assert.Equal(t, int64(1), pl.IssueCount)
	assert.Equal(t, int64(1), pl.CommentCount)
	assert.Len(t, pl.UID, 26)
	assert.NotEmpty(t, pl.OriginInstanceUID)

	// Project row gone; name is free for reuse.
	_, err = d.ProjectByID(ctx, p.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	fresh, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	assert.NotEqual(t, p.ID, fresh.ID)

	// No dangling FKs.
	var violations int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations))
	assert.Equal(t, 0, violations)
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./internal/db/sqlitestore/ -run TestPurgeProject_DeletesArchivedProjectAndFreesName -v`
Expected: FAIL (`PurgeProject` not yet implemented / returns stub error... it's implemented in sqlitestore here, so: compile error "undefined: (*Store).PurgeProject").

- [ ] **Step 3: Implement `PurgeProject` + entry/guards.** Create `internal/db/sqlitestore/queries_projects_purge.go`:

```go
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// PurgeProject permanently deletes an archived project and every project-scoped
// row, reserves an SSE reset cursor, and writes a project_purge_log tombstone.
// Mirrors PurgeIssue's BEGIN IMMEDIATE pattern so count snapshots are stable.
func (d *Store) PurgeProject(ctx context.Context, p db.PurgeProjectParams) (db.ProjectPurgeLog, error) {
	return retryWrite1(ctx, d, func() (db.ProjectPurgeLog, error) {
		return d.purgeProject(ctx, p)
	})
}

func (d *Store) purgeProject(ctx context.Context, p db.PurgeProjectParams) (db.ProjectPurgeLog, error) {
	conn, err := d.Conn(ctx)
	if err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE TRANSACTION"); err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	project, err := scanProject(conn.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, p.ProjectID))
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	if isSystemProject(project) {
		return db.ProjectPurgeLog{}, db.ErrNotFound
	}
	if project.DeletedAt == nil {
		return db.ProjectPurgeLog{}, db.ErrProjectNotArchived
	}
	role, err := federationBindingRole(ctx, conn, project.ID)
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	if role != "" {
		return db.ProjectPurgeLog{}, &db.ProjectFederatedError{Role: db.FederationRole(role)}
	}

	plID, err := purgeProjectCascade(ctx, conn, project, p.Actor, p.Reason, d.instanceUID)
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	pl, err := scanProjectPurgeLog(ctx, conn, plID)
	if err != nil {
		return db.ProjectPurgeLog{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return pl, nil
}

// federationBindingRole returns the role of any federation_bindings row for the
// project (hub or spoke), or "" when none exists. Presence of any binding
// blocks purge — federation must be torn down first.
func federationBindingRole(ctx context.Context, q sqlReader, projectID int64) (string, error) {
	var role string
	err := q.QueryRowContext(ctx,
		`SELECT role FROM federation_bindings WHERE project_id = ?`, projectID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check federation binding: %w", err)
	}
	return role, nil
}
```

- [ ] **Step 4: Implement the cascade + counts.** Append to the same file. `S` is expressed as the subquery `(SELECT id FROM issues WHERE project_id = ?)`:

```go
type projectPurgeCounts struct {
	issues, events, aliases, comments, links, labels, claims, pendingClaims int64
	minEventID, maxEventID                                                  sql.NullInt64
}

func countProjectPurge(ctx context.Context, c connExec, projectID int64) (projectPurgeCounts, error) {
	var n projectPurgeCounts
	const sub = `(SELECT id FROM issues WHERE project_id = ?)`
	get := func(dst *int64, q string, args ...any) error {
		v, err := scanCount(ctx, c, q, args...)
		*dst = v
		return err
	}
	if err := errors.Join(
		get(&n.issues, `SELECT count(*) FROM issues WHERE project_id = ?`, projectID),
		get(&n.events, `SELECT count(*) FROM events WHERE project_id = ?`, projectID),
		get(&n.aliases, `SELECT count(*) FROM project_aliases WHERE project_id = ?`, projectID),
		get(&n.comments, `SELECT count(*) FROM comments WHERE issue_id IN `+sub, projectID),
		get(&n.links, `SELECT count(*) FROM links WHERE from_issue_id IN `+sub+` OR to_issue_id IN `+sub, projectID, projectID),
		get(&n.labels, `SELECT count(*) FROM issue_labels WHERE issue_id IN `+sub, projectID),
		get(&n.claims, `SELECT count(*) FROM issue_claims WHERE project_id = ? OR issue_id IN `+sub, projectID, projectID),
		get(&n.pendingClaims, `SELECT count(*) FROM pending_claim_requests WHERE project_id = ? OR issue_id IN `+sub, projectID, projectID),
	); err != nil {
		return projectPurgeCounts{}, fmt.Errorf("count project purge rows: %w", err)
	}
	if err := c.QueryRowContext(ctx,
		`SELECT MIN(id), MAX(id) FROM events WHERE project_id = ?`, projectID).
		Scan(&n.minEventID, &n.maxEventID); err != nil {
		return projectPurgeCounts{}, fmt.Errorf("scan event id range: %w", err)
	}
	return n, nil
}

// deleteProjectScoped removes every project-scoped row in FK-safe order. Events
// physically in the project are deleted; events in OTHER projects that reference
// purged issues are DETACHED (both id and uid columns nulled) so per-project
// resume stays valid. federation_bindings is absent (refused upfront).
// NOTE: purge_log (issue tombstones) is intentionally NOT deleted — it has no FK
// to projects so it survives, preserving prior-purge audit history (spec Finding 3).
// recurrences / issue_sync_bindings / issue_sync_status / import_mappings are not
// listed here: they ON DELETE CASCADE off the final `DELETE FROM projects`.
func deleteProjectScoped(ctx context.Context, c connExec, projectID int64) error {
	const sub = `(SELECT id FROM issues WHERE project_id = ?)`
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM events WHERE project_id = ?`, []any{projectID}},
		{`UPDATE events SET issue_id = NULL, issue_uid = NULL WHERE issue_id IN ` + sub, []any{projectID}},
		{`UPDATE events SET related_issue_id = NULL, related_issue_uid = NULL WHERE related_issue_id IN ` + sub, []any{projectID}},
		{`DELETE FROM comments WHERE issue_id IN ` + sub, []any{projectID}},
		{`DELETE FROM links WHERE from_issue_id IN ` + sub + ` OR to_issue_id IN ` + sub, []any{projectID, projectID}},
		{`DELETE FROM issue_labels WHERE issue_id IN ` + sub, []any{projectID}},
		{`DELETE FROM issue_claims WHERE project_id = ? OR issue_id IN ` + sub, []any{projectID, projectID}},
		{`DELETE FROM pending_claim_requests WHERE project_id = ? OR issue_id IN ` + sub, []any{projectID, projectID}},
		{`DELETE FROM issues WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM project_aliases WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM federation_sync_status WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM federation_quarantine WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM federation_enrollments WHERE project_id = ?`, []any{projectID}},
		{`DELETE FROM projects WHERE id = ?`, []any{projectID}},
	}
	for _, s := range stmts {
		if _, err := c.ExecContext(ctx, s.q, s.args...); err != nil {
			return fmt.Errorf("project purge delete (%s): %w", s.q, err)
		}
	}
	return nil
}
```

- [ ] **Step 5: Implement the tombstone insert + scan.** Append:

```go
func purgeProjectCascade(ctx context.Context, c connExec, project db.Project,
	actor string, reason *string, originInstanceUID string) (int64, error) {
	counts, err := countProjectPurge(ctx, c, project.ID)
	if err != nil {
		return 0, err
	}
	if err := deleteProjectScoped(ctx, c, project.ID); err != nil {
		return 0, err
	}
	reservedCursor, err := reserveEventSequence(ctx, c, counts.minEventID.Valid)
	if err != nil {
		return 0, err
	}
	purgeUID, err := katauid.New()
	if err != nil {
		return 0, fmt.Errorf("generate project purge uid: %w", err)
	}
	res, err := c.ExecContext(ctx,
		`INSERT INTO project_purge_log(
		   uid, origin_instance_uid, project_id, project_uid, project_name,
		   issue_count, event_count, alias_count, comment_count, link_count, label_count,
		   claim_count, pending_claim_request_count,
		   events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
		   actor, reason)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		purgeUID, originInstanceUID, project.ID, project.UID, project.Name,
		counts.issues, counts.events, counts.aliases, counts.comments, counts.links, counts.labels,
		counts.claims, counts.pendingClaims,
		counts.minEventID, counts.maxEventID, reservedCursor,
		actor, reason)
	if err != nil {
		return 0, fmt.Errorf("insert project_purge_log: %w", err)
	}
	return res.LastInsertId()
}

func scanProjectPurgeLog(ctx context.Context, r sqlReader, id int64) (db.ProjectPurgeLog, error) {
	const q = `
		SELECT id, uid, origin_instance_uid, project_id, project_uid, project_name,
		       issue_count, event_count, alias_count, comment_count, link_count, label_count,
		       claim_count, pending_claim_request_count,
		       events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
		       actor, reason, purged_at
		FROM project_purge_log WHERE id = ?`
	var pl db.ProjectPurgeLog
	err := r.QueryRowContext(ctx, q, id).Scan(
		&pl.ID, &pl.UID, &pl.OriginInstanceUID, &pl.ProjectID, &pl.ProjectUID, &pl.ProjectName,
		&pl.IssueCount, &pl.EventCount, &pl.AliasCount, &pl.CommentCount, &pl.LinkCount, &pl.LabelCount,
		&pl.ClaimCount, &pl.PendingClaimRequestCount,
		&pl.EventsDeletedMinID, &pl.EventsDeletedMaxID, &pl.PurgeResetAfterEventID,
		&pl.Actor, &pl.Reason, &pl.PurgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return db.ProjectPurgeLog{}, db.ErrNotFound
	}
	if err != nil {
		return db.ProjectPurgeLog{}, fmt.Errorf("scan project_purge_log: %w", err)
	}
	return pl, nil
}
```

(`connExec`, `sqlReader`, `scanCount`, `reserveEventSequence` are defined in `queries_delete.go`. If `project.UID` should be stored as `*string`, note `project_uid` is `TEXT` nullable but we always pass the non-empty `project.UID`; `database/sql` stores the Go string fine.)

- [ ] **Step 6: Run the happy-path test.**

Run: `go test ./internal/db/sqlitestore/ -run TestPurgeProject_DeletesArchivedProjectAndFreesName -v`
Expected: PASS.

- [ ] **Step 7: Add guard + edge tests.** Append to the test file:

```go
func TestPurgeProject_RefusesActiveProject(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.ErrorIs(t, err, db.ErrProjectNotArchived)
}

func TestPurgeProject_NotFound(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	_, err := d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: 9999, Actor: "tester"})
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestPurgeProject_RefusesFederated(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	// Insert a hub federation_bindings row directly (mirror columns from schema.sql).
	_, err = d.ExecContext(ctx,
		`INSERT INTO federation_bindings(project_id, role, enabled, hub_url, push_enabled)
		 VALUES(?, 'hub', 1, '', 0)`, p.ID)
	require.NoError(t, err)

	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	var fed *db.ProjectFederatedError
	require.ErrorAs(t, err, &fed)
	assert.Equal(t, db.FederationRoleHub, fed.Role)
}

func TestPurgeProject_DetachesMovedInIssueEvents(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	origin, err := d.CreateProject(ctx, "origin-project")
	require.NoError(t, err)
	dest, err := d.CreateProject(ctx, "dest-project")
	require.NoError(t, err)
	iss, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: origin.ID, Title: "mover", Author: "tester"})
	require.NoError(t, err)
	_, err = d.MoveIssueProject(ctx, db.MoveIssueParams{
		IssueID: iss.ID, FromProjectID: origin.ID, ToProjectID: dest.ID,
		IfMatchRev: 1, Actor: "tester",
	})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: dest.ID, Actor: "tester"})
	require.NoError(t, err)

	_, err = d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: dest.ID, Actor: "tester"})
	require.NoError(t, err)

	// origin's pre-move events survive (rows kept) but are detached.
	var orphaned int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE project_id = ? AND issue_id IS NULL`, origin.ID).Scan(&orphaned))
	assert.Positive(t, orphaned, "moved-in issue's origin events should be detached, not deleted")
	var violations int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations))
	assert.Equal(t, 0, violations)
}
```

(Verify `MoveIssueParams` field names against `internal/db/params.go` before running; adjust if the real struct differs.)

- [ ] **Step 8: Run all purge tests + vet.**

Run: `go test ./internal/db/sqlitestore/ -run TestPurgeProject -v && go vet ./internal/db/sqlitestore/`
Expected: PASS, no vet warnings.

- [ ] **Step 9: Commit.**

```bash
git add internal/db/sqlitestore/queries_projects_purge.go internal/db/sqlitestore/queries_projects_purge_test.go
git commit -m "Implement PurgeProject storage operation"
```

---

## Task 5: `PurgeResetCheck` reads both tombstone tables

**Files:**
- Modify: `internal/db/sqlitestore/queries_events.go:326-342`
- Test: `internal/db/sqlitestore/queries_projects_purge_test.go`

**Interfaces:**
- Consumes: `(*Store).PurgeProject` (Task 4).
- Produces: `PurgeResetCheck` returns the max cursor across `purge_log` and `project_purge_log`.

- [ ] **Step 1: Write the failing test** (append to `queries_projects_purge_test.go`):

```go
func TestPurgeProject_ReservesResetCursorForBothStreams(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "x", Author: "tester"})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	pl, err := d.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	require.NotNil(t, pl.PurgeResetAfterEventID)

	// Global stream sees the cursor.
	global, err := d.PurgeResetCheck(ctx, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, *pl.PurgeResetAfterEventID, global)
	// Purged project's own stream sees it too (defensive SSE check uses subscriber projectID).
	scoped, err := d.PurgeResetCheck(ctx, 0, p.ID)
	require.NoError(t, err)
	assert.Equal(t, *pl.PurgeResetAfterEventID, scoped)
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./internal/db/sqlitestore/ -run TestPurgeProject_ReservesResetCursorForBothStreams -v`
Expected: FAIL (`global`/`scoped` are 0 — `PurgeResetCheck` only reads `purge_log`).

- [ ] **Step 3: Extend `PurgeResetCheck`.** Replace the body in `queries_events.go` so it takes the max across both tables:

```go
func (d *Store) PurgeResetCheck(ctx context.Context, afterID, projectID int64) (int64, error) {
	q := `SELECT MAX(c) FROM (
	        SELECT MAX(purge_reset_after_event_id) AS c FROM purge_log
	         WHERE purge_reset_after_event_id IS NOT NULL AND purge_reset_after_event_id > ?` +
		projectFilter(projectID) + `
	        UNION ALL
	        SELECT MAX(purge_reset_after_event_id) AS c FROM project_purge_log
	         WHERE purge_reset_after_event_id IS NOT NULL AND purge_reset_after_event_id > ?` +
		projectFilter(projectID) + `
	      )`
	args := []any{afterID}
	if projectID != 0 {
		args = append(args, projectID)
	}
	args = append(args, afterID)
	if projectID != 0 {
		args = append(args, projectID)
	}
	var n sql.NullInt64
	if err := d.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("purge reset check: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

func projectFilter(projectID int64) string {
	if projectID == 0 {
		return ""
	}
	return ` AND project_id = ?`
}
```

- [ ] **Step 4: Run the new test + the existing PurgeResetCheck tests.**

Run: `go test ./internal/db/sqlitestore/ -run 'PurgeReset|PurgeProject_ReservesReset' -v`
Expected: PASS (existing issue-purge reset tests still green).

- [ ] **Step 5: Commit.**

```bash
git add internal/db/sqlitestore/queries_events.go internal/db/sqlitestore/queries_projects_purge_test.go
git commit -m "Extend PurgeResetCheck to read project_purge_log"
```

---

## Task 6: JSONL cutover wiring

Mirror the existing `purge_log` path exactly, renaming to `project_purge_log`, with one difference: the legacy export is **gated `sourceSchemaVersion >= 20`**.

**Files:**
- Modify: `internal/db/sqlitestore/export.go` (+ `ExportProjectPurgeLog`)
- Modify: `internal/jsonl/types.go` (+ kind, bump sequence order)
- Modify: `internal/jsonl/export.go` (+ gated `exportProjectPurgeLog`)
- Modify: `internal/jsonl/storage_export.go` (stream the kind)
- Modify: `internal/db/import_types.go` (+ kind + record field)
- Modify: `internal/jsonl/<decoder>` (Kind→ImportRecord case)
- Modify: `internal/db/sqlitestore/import_replay.go` (+ `importProjectPurgeLog` + table list)
- Test: `internal/jsonl/cutover_test.go`, `internal/db/sqlitestore/import_replay_test.go`

**Interfaces:**
- Consumes: `db.ProjectPurgeLogExport`, `KindPurgeLog` pattern.
- Produces: round-trippable `project_purge_log` through export→import + a v19 auto-cutover regression.

- [ ] **Step 1: Add `Store.ExportProjectPurgeLog`** to `internal/db/sqlitestore/export.go` (copy `ExportPurgeLog` at lines 546-567, rename, swap columns):

```go
func (d *Store) ExportProjectPurgeLog(ctx context.Context, f db.ExportFilter) iter.Seq2[db.ProjectPurgeLogExport, error] {
	query := `SELECT project_purge_log.id, project_purge_log.uid, project_purge_log.origin_instance_uid,
	                 project_purge_log.project_id, project_uid, project_purge_log.project_name,
	                 issue_count, event_count, alias_count, comment_count, link_count, label_count,
	                 claim_count, pending_claim_request_count,
	                 events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
	                 actor, reason, CAST(purged_at AS TEXT)
	          FROM project_purge_log`
	query, args := withProjectIDFilter(query, f, "project_purge_log.project_id")
	query += ` ORDER BY project_purge_log.id ASC`
	return streamRows(ctx, d.readQ, "project_purge_log", query, args,
		func(rows *sql.Rows) (db.ProjectPurgeLogExport, error) {
			var rec db.ProjectPurgeLogExport
			if err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID,
				&rec.ProjectUID, &rec.ProjectName, &rec.IssueCount, &rec.EventCount, &rec.AliasCount,
				&rec.CommentCount, &rec.LinkCount, &rec.LabelCount, &rec.ClaimCount, &rec.PendingClaimRequestCount,
				&rec.EventsDeletedMinID, &rec.EventsDeletedMaxID, &rec.PurgeResetAfterEventID,
				&rec.Actor, &rec.Reason, &rec.PurgedAt); err != nil {
				return db.ProjectPurgeLogExport{}, scanError("project_purge_log", err)
			}
			return rec, nil
		})
}
```

- [ ] **Step 2: Add the Kind + bump sequence order** in `internal/jsonl/types.go`. Add the constant after `KindPurgeLog`:

```go
	KindPurgeLog             Kind = "purge_log"
	KindProjectPurgeLog      Kind = "project_purge_log"
	KindSQLiteSequence       Kind = "sqlite_sequence"
```

And in `kindOrder`, insert `project_purge_log` at 19 and bump `sqlite_sequence` to 20:

```go
	KindPurgeLog:             18,
	KindProjectPurgeLog:      19,
	KindSQLiteSequence:       20,
```

- [ ] **Step 3: Stream the kind** in `internal/jsonl/storage_export.go` (between purge_log and sqlite_sequence, lines 112-114):

```go
	if err := streamExport(enc, KindPurgeLog, store.ExportPurgeLog(ctx, f)); err != nil {
		return err
	}
	if err := streamExport(enc, KindProjectPurgeLog, store.ExportProjectPurgeLog(ctx, f)); err != nil {
		return err
	}
	return streamExport(enc, KindSQLiteSequence, store.ExportSequences(ctx))
```

- [ ] **Step 4: Add the gated legacy export** in `internal/jsonl/export.go`. After the `exportPurgeLog` call in `exportSnapshot` (lines 137-139), add:

```go
	if err := exportPurgeLog(ctx, d, enc, opts, sourceSchemaVersion); err != nil {
		return err
	}
	if sourceSchemaVersion >= 20 {
		if err := exportProjectPurgeLog(ctx, d, enc, opts); err != nil {
			return err
		}
	}
```

And define the function (mirror the current-schema branch of `exportPurgeLog`; no sub-version handling — the table exists only at v20+):

```go
func exportProjectPurgeLog(ctx context.Context, d exportQuerier, enc *Encoder, opts ExportOptions) error {
	type record struct {
		ID                       int64   `json:"id"`
		UID                      string  `json:"uid"`
		OriginInstanceUID        string  `json:"origin_instance_uid"`
		ProjectID                int64   `json:"project_id"`
		ProjectUID               *string `json:"project_uid"`
		ProjectName              string  `json:"project_name"`
		IssueCount               int64   `json:"issue_count"`
		EventCount               int64   `json:"event_count"`
		AliasCount               int64   `json:"alias_count"`
		CommentCount             int64   `json:"comment_count"`
		LinkCount                int64   `json:"link_count"`
		LabelCount               int64   `json:"label_count"`
		ClaimCount               int64   `json:"claim_count"`
		PendingClaimRequestCount int64   `json:"pending_claim_request_count"`
		EventsDeletedMinID       *int64  `json:"events_deleted_min_id"`
		EventsDeletedMaxID       *int64  `json:"events_deleted_max_id"`
		PurgeResetAfterEventID   *int64  `json:"purge_reset_after_event_id"`
		Actor                    string  `json:"actor"`
		Reason                   *string `json:"reason"`
		PurgedAt                 string  `json:"purged_at"`
	}
	query := `SELECT id, uid, origin_instance_uid, project_id, project_uid, project_name,
	                 issue_count, event_count, alias_count, comment_count, link_count, label_count,
	                 claim_count, pending_claim_request_count,
	                 events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
	                 actor, reason, CAST(purged_at AS TEXT)
	          FROM project_purge_log`
	args := []any{}
	if opts.ProjectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, opts.ProjectID)
	}
	query += ` ORDER BY id ASC`
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("export project_purge_log: %w", err)
	}
	return scanRecords(rows, KindProjectPurgeLog, enc, func(rows *sql.Rows) (record, error) {
		var rec record
		err := rows.Scan(&rec.ID, &rec.UID, &rec.OriginInstanceUID, &rec.ProjectID, &rec.ProjectUID,
			&rec.ProjectName, &rec.IssueCount, &rec.EventCount, &rec.AliasCount, &rec.CommentCount,
			&rec.LinkCount, &rec.LabelCount, &rec.ClaimCount, &rec.PendingClaimRequestCount,
			&rec.EventsDeletedMinID, &rec.EventsDeletedMaxID, &rec.PurgeResetAfterEventID,
			&rec.Actor, &rec.Reason, &rec.PurgedAt)
		return rec, err
	})
}
```

- [ ] **Step 5: Add the import kind + record field** in `internal/db/import_types.go`: add `ProjectPurgeLog *ProjectPurgeLogExport` to `ImportRecord` (after `PurgeLog`), and `ImportKindProjectPurgeLog = "project_purge_log"` to the const block (after `ImportKindPurgeLog`).

- [ ] **Step 6: Add the decode case.** Grep for where `KindPurgeLog` is decoded into an `ImportRecord` (the JSONL reader that maps `Kind` → `ImportRecord`; search `ImportKindPurgeLog` and `KindPurgeLog` in `internal/jsonl/`). Add the parallel case for `KindProjectPurgeLog`/`ImportKindProjectPurgeLog`, unmarshaling into `ImportRecord.ProjectPurgeLog`. Mirror the existing purge_log case exactly.

- [ ] **Step 7: Add `importProjectPurgeLog` + dispatch + table list** in `internal/db/sqlitestore/import_replay.go`. Add the dispatch case (near line 167):

```go
	case db.ImportKindProjectPurgeLog:
		return linkSkipNone, importProjectPurgeLog(ctx, tx, r.ProjectPurgeLog)
```

Add the function (no COALESCE issue/project lookups — the project is gone; insert snapshot columns verbatim):

```go
func importProjectPurgeLog(ctx context.Context, tx *sql.Tx, pl *db.ProjectPurgeLogExport) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO project_purge_log(id, uid, origin_instance_uid, project_id, project_uid, project_name,
		                               issue_count, event_count, alias_count, comment_count, link_count, label_count,
		                               claim_count, pending_claim_request_count,
		                               events_deleted_min_id, events_deleted_max_id, purge_reset_after_event_id,
		                               actor, reason, purged_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pl.ID, pl.UID, pl.OriginInstanceUID, pl.ProjectID, stringPtrValue(pl.ProjectUID), pl.ProjectName,
		pl.IssueCount, pl.EventCount, pl.AliasCount, pl.CommentCount, pl.LinkCount, pl.LabelCount,
		pl.ClaimCount, pl.PendingClaimRequestCount,
		pl.EventsDeletedMinID, pl.EventsDeletedMaxID, pl.PurgeResetAfterEventID,
		pl.Actor, pl.Reason, pl.PurgedAt)
	return wrapImportErr(db.ImportKindProjectPurgeLog, err)
}
```

Add `"project_purge_log"` to the `reconcileSequences` table list (line 933), after `"purge_log"`.

- [ ] **Step 8: Round-trip test.** In `internal/db/sqlitestore/import_replay_test.go` (mirror an existing export→import round-trip test), create a project, archive+purge it, export to JSONL, import into a fresh DB, assert the `project_purge_log` row matches (uid, counts, cursor).

- [ ] **Step 9: v19 auto-cutover regression.** In `internal/jsonl/cutover_test.go`, mirror `TestAutoCutoverUpgradesLegacyV11DB` (lines 79-118 + `trimCurrentDBToV11Shape`): create a current DB, then `trimCurrentDBToV19Shape` = `DROP TABLE project_purge_log; UPDATE meta SET value='19' WHERE key='schema_version';`, run `jsonl.AutoCutover`, assert it upgrades to v20 cleanly (no "no such table") and `project_purge_log` exists. This proves the `>= 20` export gate.

```go
func TestAutoCutoverUpgradesLegacyV19DB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d := openCutoverTargetDB(t, ctx, path)
	_, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	trimCurrentDBToV19Shape(t, path) // DROP project_purge_log; set schema_version='19'

	require.NoError(t, jsonl.AutoCutover(ctx, path))

	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	assertCurrentSchemaVersion(t, path)
	var n int
	require.NoError(t, upgraded.QueryRowContext(ctx,
		`SELECT count(*) FROM project_purge_log`).Scan(&n))
	assert.Equal(t, 0, n)
}
```

Add the `trimCurrentDBToV19Shape` helper next to `trimCurrentDBToV11Shape`.

- [ ] **Step 10: Run cutover + import tests.**

Run: `go test ./internal/jsonl/... ./internal/db/sqlitestore/ -run 'Cutover|Import|RoundTrip|ProjectPurge' -v`
Expected: PASS.

- [ ] **Step 11: Commit.**

```bash
git add internal/db/sqlitestore/export.go internal/jsonl/ internal/db/import_types.go internal/db/sqlitestore/import_replay.go internal/db/sqlitestore/import_replay_test.go
git commit -m "Wire project_purge_log through JSONL cutover (gated v20)"
```

---

## Task 7: Daemon route, handler, reset broadcast

**Files:**
- Modify: `internal/api/types.go` (+ request/response)
- Modify: `internal/daemon/handlers_projects.go` (+ route in `registerProjectsHandlers`)
- Test: `internal/daemon/handlers_projects_test.go` (or a new `handlers_project_purge_test.go`)

**Interfaces:**
- Consumes: `db.PurgeProject`, `validateExactConfirm`, `attributedActor`, `db.ProjectFederatedError`, `db.ErrProjectNotArchived`.
- Produces: `POST /api/v1/projects/{project_id}/actions/purge` returning `ProjectPurgeResponse`.

- [ ] **Step 1: Add API types** to `internal/api/types.go` (near the project request structs ~line 677):

```go
type ProjectPurgeRequest struct {
	ProjectID int64  `path:"project_id" required:"true"`
	Confirm   string `header:"X-Kata-Confirm"`
	Body      struct {
		Actor  string `json:"actor" required:"true"`
		Reason string `json:"reason,omitempty"`
	}
}

type ProjectPurgeResponse struct {
	Body struct {
		ProjectPurgeLog db.ProjectPurgeLog `json:"project_purge_log"`
	}
}
```

- [ ] **Step 2: Write the failing handler test.** Mirror an existing project handler test (spin up the test server, archive a project, POST the purge). Assert 200 + name freed, plus the confirm + archived-only + federated error paths:

```go
func TestPurgeProjectHandler_PurgesArchived(t *testing.T) {
	// ... use the package's existing daemon test harness (mirror handlers_projects_test.go) ...
	// 1. create + archive "spoke-project"
	// 2. POST /api/v1/projects/{id}/actions/purge with header X-Kata-Confirm: "PURGE spoke-project", body {"actor":"tester"}
	//    -> 200, body.project_purge_log.project_name == "spoke-project"
	// 3. POST again -> 404 project_not_found
	// 4. archived-only: create active project, purge -> 409 project_not_archived
	// 5. missing confirm header -> 412 confirm_required; wrong value -> 412 confirm_mismatch
}
```

(Fill the body using the harness in `handlers_projects_test.go` — same `huma`/httptest setup other handler tests use.)

- [ ] **Step 3: Run to verify it fails.**

Run: `go test ./internal/daemon/ -run TestPurgeProjectHandler -v`
Expected: FAIL (route 404 — not registered).

- [ ] **Step 4: Register the route + handler** in `registerProjectsHandlers` (`handlers_projects.go`). Add after the `removeProject` registration:

```go
	huma.Register(humaAPI, huma.Operation{
		OperationID: "purgeProject",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/actions/purge",
	}, func(ctx context.Context, in *api.ProjectPurgeRequest) (*api.ProjectPurgeResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		// Archived-inclusive lookup: the project is archived by precondition,
		// so activeProjectByID (which 404s archived rows) must not be used.
		project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "project_not_found", "project not found", "", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if err := validateExactConfirm(in.Confirm, "PURGE "+project.Name); err != nil {
			return nil, err
		}
		var reasonPtr *string
		if in.Body.Reason != "" {
			r := in.Body.Reason
			reasonPtr = &r
		}
		pl, err := cfg.DB.PurgeProject(ctx, db.PurgeProjectParams{
			ProjectID: in.ProjectID, Actor: actor, Reason: reasonPtr,
		})
		switch {
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(404, "project_not_found", "project not found", "", nil)
		case errors.Is(err, db.ErrProjectNotArchived):
			return nil, api.NewError(409, "project_not_archived",
				"project is not archived",
				"run `kata projects remove "+project.Name+"` first", nil)
		}
		var fedErr *db.ProjectFederatedError
		if errors.As(err, &fedErr) {
			hint := "run `kata federation leave " + project.Name + "` first"
			if fedErr.Role == db.FederationRoleHub {
				hint = "remove federation before purging (hub teardown is not yet supported)"
			}
			return nil, api.NewError(409, "project_federated",
				fedErr.Error(), hint, map[string]any{"role": string(fedErr.Role)})
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if pl.PurgeResetAfterEventID != nil {
			cfg.Broadcaster.Broadcast(StreamMsg{
				Kind:      "reset",
				ResetID:   *pl.PurgeResetAfterEventID,
				ProjectID: in.ProjectID,
			})
		}
		out := &api.ProjectPurgeResponse{}
		out.Body.ProjectPurgeLog = pl
		return out, nil
	})
```

- [ ] **Step 5: Run the handler test.**

Run: `go test ./internal/daemon/ -run TestPurgeProjectHandler -v`
Expected: PASS.

- [ ] **Step 6: Add the live-SSE reset test.** Mirror the existing reset/broadcast test in `handlers_destructive_test.go` / `handlers_events_test.go`: subscribe a global subscriber and a project-B subscriber, purge, assert both receive a `reset` frame; an origin-project subscriber (moved-in case) does not. (Locks in the Finding-2 scope.)

- [ ] **Step 7: Run daemon tests + vet.**

Run: `go test ./internal/daemon/ -run 'PurgeProject|Reset' -v && go vet ./internal/daemon/`
Expected: PASS.

- [ ] **Step 8: Commit.**

```bash
git add internal/api/types.go internal/daemon/handlers_projects.go internal/daemon/*_test.go
git commit -m "Add project purge daemon route, handler, and reset broadcast"
```

---

## Task 8: Regenerate OpenAPI + client

**Files:**
- Modify (generated): `api/openapi.yaml`, `pkg/client/openapi.yaml`, `pkg/client/generated/*`

- [ ] **Step 1: Confirm the drift gate currently fails** (the new route isn't in the committed spec).

Run: `make api-check`
Expected: FAIL (drift detected).

- [ ] **Step 2: Regenerate.**

Run: `make api-generate`

- [ ] **Step 3: Verify the gate passes and the route is present.**

Run: `make api-check && rg -n "actions/purge" api/openapi.yaml`
Expected: PASS; the project purge path appears.

- [ ] **Step 4: Build everything.**

Run: `go build ./...`
Expected: success.

- [ ] **Step 5: Commit.**

```bash
git add api/openapi.yaml pkg/client/openapi.yaml pkg/client/generated
git commit -m "Regenerate OpenAPI and client for project purge route"
```

---

## Task 9: CLI `kata projects purge`

**Files:**
- Create: `cmd/kata/projects_purge.go`
- Modify: `cmd/kata/projects.go:46-51` (register the command)
- Test: `cmd/kata/projects_test.go`

**Interfaces:**
- Consumes: `ensureDaemon`, `httpClientFor`, `resolveProjectSelectorIncludingArchived`, `resolveActor`, `resolveConfirm`, `confirmPromptFull`, `httpDoJSONWithHeader`, `apiErrFromBody`, `currentOutputMode`, `emitJSON`, `writeAgentProjectAction`, `agentRowField`.

- [ ] **Step 1: Write the failing CLI test** (append to `projects_test.go`):

```go
func TestProjects_PurgeArchivedFreesName(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	out := requireCmdOutput(t, env, "projects", "purge", "spoke-project",
		"--force", "--confirm", "PURGE spoke-project")
	assert.Contains(t, out, "purged")
	assert.Contains(t, out, "name is now free")

	_, err = env.DB.ProjectByID(ctx, p.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestProjects_PurgeRequiresForce(t *testing.T) {
	env := testenv.New(t)
	_, err := runCmdOutput(t, env, "projects", "purge", "spoke-project")
	require.Error(t, err)
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./cmd/kata/ -run TestProjects_Purge -v`
Expected: FAIL (`unknown command "purge"`).

- [ ] **Step 3: Implement the command.** Create `cmd/kata/projects_purge.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

// projectsPurgeCmd permanently deletes an archived project and frees its name.
// Gated by --force plus an X-Kata-Confirm header whose value is the exact
// string "PURGE <project>", mirroring `kata purge` for issues.
func projectsPurgeCmd() *cobra.Command {
	var force bool
	var confirm string
	var reason string
	cmd := &cobra.Command{
		Use:   "purge <project>",
		Short: "permanently delete an archived project (irreversible; frees the name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				return &cliError{
					Message:  "purge requires --force; this is irreversible",
					Code:     "validation",
					Kind:     kindValidation, ExitCode: ExitValidation,
				}
			}
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			project, err := resolveProjectSelectorIncludingArchived(ctx, client, baseURL, args[0])
			if err != nil {
				return err
			}
			expected := fmt.Sprintf("PURGE %s", project.Name)
			confirm, err = resolveConfirm(cmd, confirm, expected,
				fmt.Sprintf("Type %q to confirm: ", expected), confirmPromptFull)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			body := map[string]any{"actor": actor}
			if reason != "" {
				body["reason"] = reason
			}
			postURL := fmt.Sprintf("%s/api/v1/projects/%d/actions/purge", baseURL, project.ID)
			status, bs, err := httpDoJSONWithHeader(ctx, client, http.MethodPost, postURL,
				map[string]string{"X-Kata-Confirm": confirm}, body)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			return printProjectPurge(cmd, project.Name, bs)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "required to perform the purge")
	cmd.Flags().StringVar(&confirm, "confirm", "", `exact confirmation string ("PURGE <project>")`)
	cmd.Flags().StringVar(&reason, "reason", "", "free-text reason recorded in the audit tombstone")
	return cmd
}

func printProjectPurge(cmd *cobra.Command, name string, bs []byte) error {
	if flags.JSON {
		var buf bytes.Buffer
		if err := emitJSON(&buf, json.RawMessage(bs)); err != nil {
			return err
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), buf.String())
		return err
	}
	if currentOutputMode() == outputAgent {
		return writeAgentProjectAction(cmd.OutOrStdout(), "purge",
			agentRowField("project", name),
		)
	}
	if flags.Quiet {
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"%s purged (irreversible); name is now free\n", textsafe.Line(name))
	return err
}
```

(The `project.Name` goes in the `X-Kata-Confirm` header value and the actor in the JSON body — no query escaping needed, so `net/url` is not imported.)

- [ ] **Step 4: Register the command** in `cmd/kata/projects.go`:

```go
	cmd.AddCommand(projectsListCmd(), projectsShowCmd(), projectsRenameCmd(),
		projectsMergeCmd(), projectsRemoveCmd(), projectsRestoreCmd(),
		projectsDetachCmd(), projectsPurgeCmd())
```

- [ ] **Step 5: Run CLI tests + the non-TTY confirm path test.** Add a test that, with no `--confirm` and no TTY, exit is `ExitConfirm`:

```go
func TestProjects_PurgeNoConfirmNonTTYFails(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	p, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)
	_, err = runCmdOutput(t, env, "projects", "purge", "spoke-project", "--force")
	require.Error(t, err) // confirm_required (ExitConfirm) in non-TTY test runs
}
```

Run: `go test ./cmd/kata/ -run TestProjects_Purge -v`
Expected: PASS.

- [ ] **Step 6: Run vet/build.**

Run: `go vet ./cmd/kata/ && go build ./cmd/...`
Expected: clean.

- [ ] **Step 7: Commit.**

```bash
git add cmd/kata/projects_purge.go cmd/kata/projects.go cmd/kata/projects_test.go
git commit -m "Add kata projects purge CLI command"
```

---

## Task 10: Documentation

**Files:**
- Modify: the projects CLI reference / guide (find the page documenting `kata projects remove`/`restore` — search `docs/` for "projects remove").

- [ ] **Step 1: Document the command** where `remove`/`restore` are documented: the `kata projects purge <project> --force --confirm "PURGE <project>"` syntax, that it is archived-only and irreversible, that it frees the name, the `--reason`/`--json` flags, and the federation refusal (incl. the hub limitation). Keep copy factual; no superlatives.

- [ ] **Step 2: Run the docs check if present.**

Run: `make docs-check` (skip if it requires a docs toolchain not installed; note that in the commit).

- [ ] **Step 3: Commit.**

```bash
git add docs
git commit -m "Document kata projects purge"
```

---

## Final verification

- [ ] Run the full affected test set:

```bash
go test ./internal/db/... ./internal/jsonl/... ./internal/daemon/... ./cmd/kata/...
go vet ./...
make api-check
```
Expected: all PASS, no vet warnings, no API drift.

- [ ] Manual smoke (optional): build, `kata init` a throwaway project, `kata projects remove`, `kata projects purge <name> --force --confirm "PURGE <name>"`, then `kata init --project <name>` succeeds (name freed).
