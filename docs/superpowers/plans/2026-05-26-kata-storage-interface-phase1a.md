# Storage Interface — Phase 1a: Eliminate raw-SQL escape hatches & the tx-leaking MaterializeNext

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the **daemon's** raw-SQL escape hatches — six pre-federation call sites across `handlers_health.go`/`handlers_instance.go`/`handlers_issues.go`/`handlers_projects.go` plus five federation-added call sites in `handlers_federation.go` — that reach the embedded `*sql.DB`, and make `MaterializeNext` manage its own transaction, so a transaction-free `db.Storage` interface can be defined cleanly in a later phase. The `internal/jsonl` raw SQL is intentionally out of scope here; it moves into `sqlitestore` in Phase 1c.

**Architecture:** This is a behavior-preserving refactor of the existing SQLite-only code. Eleven daemon call sites currently run raw `QueryContext`/`QueryRowContext`/`ExecContext` against `cfg.DB`/`store` (the embedded `*sql.DB`) — six predate federation (schema-version reads in health and instance, the listComments SELECT, two project-delete DELETEs in `handlers_projects.go`, and the alias-reassign UPDATE) and five were added by the federation merge in `handlers_federation.go` (a max-local-origin event id, a max federation-baseline snapshot id, and three federation-status COUNTs). Each becomes a new domain method on `*db.DB`; related sites collapse into the same method where the SQL is byte-identical (the two project DELETEs share `HardDeleteProject`) or share a task where the methods are conceptually adjacent (Tasks 6 and 7 each group 2–3 federation methods). `MaterializeNext` currently takes a caller-supplied `*sql.Tx` (the only exported method that leaks a transaction); it splits into a private tx-taking helper (kept for `CloseIssue`'s atomicity) plus a public method that owns its transaction. The existing test suite is the safety net; two recurrence tests that drove `MaterializeNext` via `BeginTx` are rewritten against the new public method.

**Tech Stack:** Go, `database/sql` over `modernc.org/sqlite`, `github.com/stretchr/testify` for tests.

**Scope note:** This is plan 1a of the §10 build sequence in `docs/superpowers/specs/2026-05-26-kata-postgres-backend.md`. It does **not** define the `Storage` interface, move packages, or touch the `internal/jsonl` import/export SQL — those are later plans (1b: define interface + switch clean callers; 1c: port jsonl export/import into `sqlitestore` methods; 1d: physical `sqlitestore` package move). Each task keeps `go build ./...` and the full test suite green.

**Verification baseline (run once before starting):**

Run: `go build ./... && go test ./internal/db/... ./internal/daemon/... ./internal/federation/... ./internal/jsonl/... ./cmd/...`
Expected: all packages build; all tests PASS. (Establishes the green baseline this plan preserves.)

---

### Task 1: Add `SchemaVersion` and route the two daemon schema reads through it

**Files:**
- Modify: `internal/db/db.go` (add method near `currentVersion`/`PeekSchemaVersion`)
- Modify: `internal/daemon/handlers_health.go:38-46`
- Modify: `internal/daemon/handlers_instance.go:31-43`
- Test: `internal/db/db_test.go` (add `TestSchemaVersion`)

- [ ] **Step 1: Write the failing test**

Add to `internal/db/db_test.go`:

```go
func TestSchemaVersion(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	v, err := d.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), v)
}
```

All `internal/db/*_test.go` files use `package db_test`, so reference exported symbols with the `db.` qualifier (e.g. `db.Open`, `db.CurrentSchemaVersion`, `db.CreateIssueParams`). If `db_test.go` does not already import them, add `"context"`, `"path/filepath"`, `"github.com/stretchr/testify/require"`, and `"go.kenn.io/kata/internal/db"` to its import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestSchemaVersion -v`
Expected: FAIL — `d.SchemaVersion undefined (type *DB has no field or method SchemaVersion)`.

- [ ] **Step 3: Implement `SchemaVersion`**

Add to `internal/db/db.go`:

```go
// SchemaVersion returns the integer stored in meta.schema_version. It errors
// when the row is absent or unparseable (unlike currentVersion, which treats a
// missing meta table as version 0 for the bootstrap path).
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v string
	if err := d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", v, err)
	}
	return n, nil
}
```

(`db.go` already imports `fmt` and `strconv`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/db/ -run TestSchemaVersion -v`
Expected: PASS.

- [ ] **Step 5: Switch the health handler**

In `internal/daemon/handlers_health.go`, replace the inline read (lines 38-46):

```go
		var v string
		if err := cfg.DB.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		schema, err := strconv.Atoi(v)
		if err != nil {
			return nil, api.NewError(500, "internal", "invalid schema_version: "+v, "", nil)
		}
```

with:

```go
		schema, err := cfg.DB.SchemaVersion(ctx)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
```

- [ ] **Step 6: Switch the instance handler**

In `internal/daemon/handlers_instance.go`, replace the inline read (lines 31-43):

```go
		var schemaValue string
		if err := cfg.DB.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key='schema_version'`,
		).Scan(&schemaValue); err != nil {
			return nil, api.NewError(500, "schema_version_unavailable",
				err.Error(), "", nil)
		}
		sv, err := strconv.ParseInt(schemaValue, 10, 64)
		if err != nil {
			return nil, api.NewError(500, "schema_version_parse",
				fmt.Sprintf("parse schema_version %q: %v", schemaValue, err),
				"", nil)
		}
```

with:

```go
		schema, err := cfg.DB.SchemaVersion(ctx)
		if err != nil {
			return nil, api.NewError(500, "schema_version_unavailable",
				err.Error(), "", nil)
		}
		sv := int64(schema)
```

- [ ] **Step 7: Drop now-unused imports**

`internal/daemon/handlers_instance.go` likely no longer uses `strconv` or `fmt`. Run `goimports -w internal/daemon/handlers_instance.go internal/daemon/handlers_health.go` (via `go run golang.org/x/tools/cmd/goimports@latest -w ...` if not on PATH, or rely on the build error to tell you which import to remove). Confirm with the build in Step 8.

- [ ] **Step 8: Build, test, and check for tests asserting the removed error code**

Run: `rg -n "schema_version_parse" internal/ cmd/`
Expected: no hits (the distinct parse-error code is intentionally consolidated into `schema_version_unavailable`). This consolidation is acceptable: both were `500`s with the underlying message embedded, and no client is documented to branch on the `schema_version_parse` kind. If a test asserts `"schema_version_parse"`, change that assertion to `"schema_version_unavailable"`.

Run: `go build ./... && go test ./internal/db/ ./internal/daemon/`
Expected: build OK; tests PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/db/db.go internal/db/db_test.go internal/daemon/handlers_health.go internal/daemon/handlers_instance.go
git commit -m "refactor(db): add SchemaVersion and route daemon schema reads through it"
```

---

### Task 2: Add `CommentsByIssue` and move `listComments`' raw SELECT into it

**Files:**
- Modify: `internal/db/queries.go` (add method)
- Modify: `internal/daemon/handlers_issues.go:936-954` (`listComments`)
- Test: `internal/db/queries_issues_test.go` (add `TestCommentsByIssue`)

- [ ] **Step 1: Write the failing test**

Add to `internal/db/queries_issues_test.go`:

```go
func TestCommentsByIssue(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	proj, err := d.CreateProject(ctx, "proj")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "t", Author: "a",
	})
	require.NoError(t, err)

	got, err := d.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Empty(t, got)

	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "a", Body: "first",
	})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "a", Body: "second",
	})
	require.NoError(t, err)

	got, err = d.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "first", got[0].Body)
	require.Equal(t, "second", got[1].Body)
	// Federation correlation needs the per-comment UID; assert it's populated so
	// a SELECT that drops the uid column fails this test.
	require.NotEmpty(t, got[0].UID)
	require.NotEmpty(t, got[1].UID)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestCommentsByIssue -v`
Expected: FAIL — `d.CommentsByIssue undefined`.

- [ ] **Step 3: Implement `CommentsByIssue`**

Add to `internal/db/queries.go` (the SQL is moved verbatim from `listComments`, including the `uid` column federation added to the `comments` table — without it, every returned `Comment` would carry an empty `UID` and federation event correlation would silently regress):

```go
// CommentsByIssue returns every comment on issueID in chronological order
// (created_at, then id as a stable tiebreaker).
func (d *DB) CommentsByIssue(ctx context.Context, issueID int64) ([]Comment, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, uid, issue_id, author, body, created_at FROM comments WHERE issue_id = ? ORDER BY created_at ASC, id ASC`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.UID, &c.IssueID, &c.Author, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/db/ -run TestCommentsByIssue -v`
Expected: PASS.

- [ ] **Step 5: Replace `listComments` with a call to the new method**

In `internal/daemon/handlers_issues.go`, replace the whole `listComments` function (lines 936-954) with a thin delegator:

```go
// listComments fetches every comment attached to issueID in chronological
// order. Plan 1 ships no pagination; the show handler embeds the full slice.
func listComments(ctx context.Context, store *db.DB, issueID int64) ([]db.Comment, error) {
	return store.CommentsByIssue(ctx, issueID)
}
```

(Leaving `listComments` as a one-line wrapper keeps its call sites unchanged. If `store` is now the only param besides `ctx`/`issueID` and the wrapper feels redundant, inline `store.CommentsByIssue(...)` at the call sites instead — either is fine.)

- [ ] **Step 6: Build, test, commit**

Run: `go build ./... && go test ./internal/db/ ./internal/daemon/`
Expected: build OK; tests PASS.

```bash
git add internal/db/queries.go internal/db/queries_issues_test.go internal/daemon/handlers_issues.go
git commit -m "refactor(db): add CommentsByIssue and drop the daemon raw comment query"
```

---

### Task 3: Add `HardDeleteProject` and route the orphan-cleanup deletes through it

**Files:**
- Modify: `internal/db/queries.go` (add method)
- Modify: `internal/daemon/handlers_projects.go:705` and `:774`
- Test: `internal/db/queries_projects_test.go` (add `TestHardDeleteProject`)

- [ ] **Step 1: Write the failing test**

Add to `internal/db/queries_projects_test.go`:

```go
func TestHardDeleteProject(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	proj, err := d.CreateProject(ctx, "doomed")
	require.NoError(t, err)

	require.NoError(t, d.HardDeleteProject(ctx, proj.ID))

	_, err = d.ProjectByID(ctx, proj.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestHardDeleteProject -v`
Expected: FAIL — `d.HardDeleteProject undefined`.

- [ ] **Step 3: Implement `HardDeleteProject`**

Add to `internal/db/queries.go`:

```go
// HardDeleteProject permanently removes a project row by id. It exists for the
// init-race orphan-cleanup path (a freshly created project whose alias attach
// then failed); it is NOT the user-facing archival path (see RemoveProject).
func (d *DB) HardDeleteProject(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/db/ -run TestHardDeleteProject -v`
Expected: PASS.

- [ ] **Step 5: Switch both call sites**

In `internal/daemon/handlers_projects.go`, replace the orphan-cleanup line at **:705** (inside `initProject`'s attach-failure branch):

```go
				_, _ = store.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, project.ID)
```

with:

```go
				_ = store.HardDeleteProject(ctx, project.ID)
```

Apply the identical replacement at **:774** (inside `initByName`'s attach-failure branch). The best-effort, error-ignoring behavior is preserved.

- [ ] **Step 6: Build, test, commit**

Run: `rg -n "DELETE FROM projects" internal/daemon/`
Expected: no hits (both inline deletes are gone).

Run: `go build ./... && go test ./internal/db/ ./internal/daemon/`
Expected: build OK; tests PASS.

```bash
git add internal/db/queries.go internal/db/queries_projects_test.go internal/daemon/handlers_projects.go
git commit -m "refactor(db): add HardDeleteProject for init-race orphan cleanup"
```

---

### Task 4: Add `ReassignAlias` and move the alias-reassign UPDATE into it

**Files:**
- Modify: `internal/db/queries.go` (add method)
- Modify: `internal/daemon/handlers_projects.go:901-907` (`applyExistingAlias`)
- Test: `internal/db/queries_projects_test.go` (add `TestReassignAlias`)

- [ ] **Step 1: Write the failing test**

Add to `internal/db/queries_projects_test.go`:

```go
func TestReassignAlias(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	src, err := d.CreateProject(ctx, "src")
	require.NoError(t, err)
	dst, err := d.CreateProject(ctx, "dst")
	require.NoError(t, err)
	alias, err := d.AttachAlias(ctx, src.ID, "git:example.com/repo", "git", "/old/root")
	require.NoError(t, err)

	require.NoError(t, d.ReassignAlias(ctx, alias.ID, dst.ID, "/new/root"))

	got, err := d.AliasByIdentity(ctx, "git:example.com/repo")
	require.NoError(t, err)
	require.Equal(t, dst.ID, got.ProjectID)
	require.Equal(t, "/new/root", got.RootPath)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/db/ -run TestReassignAlias -v`
Expected: FAIL — `d.ReassignAlias undefined`.

- [ ] **Step 3: Implement `ReassignAlias`**

Add to `internal/db/queries.go` (UPDATE moved verbatim from `applyExistingAlias`, including the SQLite `strftime` default — this is correct for the SQLite implementation):

```go
// ReassignAlias moves an existing alias row to a different project and updates
// its root_path and last_seen_at. Used by the reassign=true branch of alias
// attach.
func (d *DB) ReassignAlias(ctx context.Context, aliasID, projectID int64, rootPath string) error {
	_, err := d.ExecContext(ctx,
		`UPDATE project_aliases
		 SET project_id = ?, root_path = ?, last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`,
		projectID, rootPath, aliasID)
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/db/ -run TestReassignAlias -v`
Expected: PASS.

- [ ] **Step 5: Switch the call site**

In `internal/daemon/handlers_projects.go` `applyExistingAlias`, replace the inline UPDATE (lines 901-907):

```go
	if _, execErr := store.ExecContext(ctx,
		`UPDATE project_aliases
		 SET project_id = ?, root_path = ?, last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ?`,
		projectID, info.RootPath, existing.ID); execErr != nil {
		return db.ProjectAlias{}, api.NewError(500, "internal", execErr.Error(), "", nil)
	}
```

with:

```go
	if execErr := store.ReassignAlias(ctx, existing.ID, projectID, info.RootPath); execErr != nil {
		return db.ProjectAlias{}, api.NewError(500, "internal", execErr.Error(), "", nil)
	}
```

- [ ] **Step 6: Build, test, commit**

Run: `rg -n "UPDATE project_aliases" internal/daemon/`
Expected: no hits.

Run: `go build ./... && go test ./internal/db/ ./internal/daemon/`
Expected: build OK; tests PASS.

```bash
git add internal/db/queries.go internal/db/queries_projects_test.go internal/daemon/handlers_projects.go
git commit -m "refactor(db): add ReassignAlias and drop the daemon raw alias update"
```

---

### Task 5: Make `MaterializeNext` own its transaction

`MaterializeNext` is the only exported `*DB` method that takes a `*sql.Tx`. Its sole production caller is `CloseIssue`, which shares one transaction so the `issue.closed` event and the materialized next instance commit atomically. Split the method: keep a private tx-taking helper for `CloseIssue`, and expose a public method that opens its own transaction.

**Files:**
- Modify: `internal/db/store_recurrences.go:645-647` (rename + new public wrapper)
- Modify: `internal/db/queries.go:1208` (`CloseIssue` call site)
- Test: `internal/db/store_recurrences_close_test.go:148-205` (rewrite two tests)

- [ ] **Step 1: Rename the existing method to a private helper**

In `internal/db/store_recurrences.go`, change the declaration at line 645-647 from:

```go
func (d *DB) MaterializeNext(
	ctx context.Context, tx *sql.Tx, recurrenceID int64, afterKey, actor string,
) (MaterializeNextOut, error) {
```

to:

```go
func (d *DB) materializeNextTx(
	ctx context.Context, tx *sql.Tx, recurrenceID int64, afterKey, actor string,
) (MaterializeNextOut, error) {
```

Leave the entire body unchanged.

- [ ] **Step 2: Add the public, transaction-owning `MaterializeNext`**

Immediately above the renamed helper in `internal/db/store_recurrences.go`, add:

```go
// MaterializeNext opens a transaction, materializes the recurrence's next
// instance past afterKey, and commits. CloseIssue uses the private
// materializeNextTx helper instead so the close and the materialization commit
// atomically in one transaction.
func (d *DB) MaterializeNext(
	ctx context.Context, recurrenceID int64, afterKey, actor string,
) (MaterializeNextOut, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return MaterializeNextOut{}, err
	}
	defer func() { _ = tx.Rollback() }()
	out, err := d.materializeNextTx(ctx, tx, recurrenceID, afterKey, actor)
	if err != nil {
		return MaterializeNextOut{}, err
	}
	if err := tx.Commit(); err != nil {
		return MaterializeNextOut{}, err
	}
	return out, nil
}
```

- [ ] **Step 3: Update the `CloseIssue` call site**

In `internal/db/queries.go`, at the materialize call (lines 1207-1212), change:

```go
	if reason == "done" && issue.RecurrenceID != nil && issue.OccurrenceKey != nil {
		if _, err := d.MaterializeNext(ctx, tx, *issue.RecurrenceID,
			*issue.OccurrenceKey, actor); err != nil {
			return Issue{}, nil, false, fmt.Errorf("materialize next recurrence: %w", err)
		}
	}
```

to call the private helper (it already holds `tx`):

```go
	if reason == "done" && issue.RecurrenceID != nil && issue.OccurrenceKey != nil {
		if _, err := d.materializeNextTx(ctx, tx, *issue.RecurrenceID,
			*issue.OccurrenceKey, actor); err != nil {
			return Issue{}, nil, false, fmt.Errorf("materialize next recurrence: %w", err)
		}
	}
```

- [ ] **Step 4: Rewrite the two recurrence tests that drove the tx manually**

In `internal/db/store_recurrences_close_test.go`, in `TestMaterializeNext_UniqueConflict_SkipsAndAdvancesCursor`, replace lines 148-156:

```go
	tx, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	out, err := d.MaterializeNext(ctx, tx, rec.ID, "2026-05-11", "tester")
	require.NoError(t, err)
	assert.True(t, out.Skipped, "should report skipped on UNIQUE conflict")
	assert.Equal(t, "2026-05-18", out.OccurrenceKey)
	assert.Equal(t, existingUID, out.NewIssueUID, "out.NewIssueUID should reflect the existing row")
	require.NoError(t, tx.Commit())
```

with:

```go
	out, err := d.MaterializeNext(ctx, rec.ID, "2026-05-11", "tester")
	require.NoError(t, err)
	assert.True(t, out.Skipped, "should report skipped on UNIQUE conflict")
	assert.Equal(t, "2026-05-18", out.OccurrenceKey)
	assert.Equal(t, existingUID, out.NewIssueUID, "out.NewIssueUID should reflect the existing row")
```

In `TestMaterializeNext_AfterConflict_NoRegressionOnReplay`, replace lines 188-201:

```go
	// First call: hits the conflict, advances cursor to 2026-05-25.
	tx1, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	out1, err := d.MaterializeNext(ctx, tx1, rec.ID, "2026-05-11", "tester")
	require.NoError(t, err)
	assert.True(t, out1.Skipped)
	require.NoError(t, tx1.Commit())

	// Second call walks from afterKey=2026-05-18, finds 2026-05-25 — cleanly materializes.
	tx2, err := d.BeginTx(ctx, nil)
	require.NoError(t, err)
	out2, err := d.MaterializeNext(ctx, tx2, rec.ID, "2026-05-18", "tester")
	require.NoError(t, err)
	require.NoError(t, tx2.Commit())
```

with:

```go
	// First call: hits the conflict, advances cursor to 2026-05-25.
	out1, err := d.MaterializeNext(ctx, rec.ID, "2026-05-11", "tester")
	require.NoError(t, err)
	assert.True(t, out1.Skipped)

	// Second call walks from afterKey=2026-05-18, finds 2026-05-25 — cleanly materializes.
	out2, err := d.MaterializeNext(ctx, rec.ID, "2026-05-18", "tester")
	require.NoError(t, err)
```

- [ ] **Step 5: Confirm no other caller passes a tx to `MaterializeNext`**

Run: `rg -n "MaterializeNext\(" internal/ cmd/`
Expected: the public-signature call sites only — `internal/db/store_recurrences.go` (definition + nothing else), the two rewritten tests, and no remaining `MaterializeNext(ctx, tx,` anywhere. `CloseIssue` now calls `materializeNextTx`.

Run: `rg -n "materializeNextTx\(" internal/db/`
Expected: the definition in `store_recurrences.go` and the one call in `queries.go` (`CloseIssue`).

- [ ] **Step 6: Build and test**

Run: `go build ./... && go test ./internal/db/ ./internal/daemon/ ./internal/jsonl/`
Expected: build OK; all tests PASS (recurrence materialization behavior is unchanged; `CloseIssue` still materializes atomically).

- [ ] **Step 7: Commit**

```bash
git add internal/db/store_recurrences.go internal/db/queries.go internal/db/store_recurrences_close_test.go
git commit -m "refactor(db): make MaterializeNext own its transaction"
```

---

### Task 6: Federation event-id readers — eliminate two `events`-MAX escape hatches

Federation added two raw `SELECT MAX(id) FROM events …` reads in `handlers_federation.go`: the `maxLocalOriginEventID` helper at line 440 (called from `enableReplicaPush`) and an inline read in `projectFederationBody` at line 746. Both lift into `*DB` methods so the daemon stops touching `events` directly.

**Files:**
- Modify: `internal/db/queries_events.go` (add two methods)
- Modify: `internal/daemon/handlers_federation.go` (replace the helper body at `:440-454`, replace the inline read at `:744-758`)
- Test: `internal/db/queries_events_test.go` (add `TestMaxLocalOriginEventID`, `TestMaxFederationBaselineEventID`)

- [ ] **Step 1: Write the failing tests**

Add to `internal/db/queries_events_test.go`:

```go
func TestMaxLocalOriginEventID(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	proj, err := d.CreateProject(ctx, "p")
	require.NoError(t, err)

	got, err := d.MaxLocalOriginEventID(ctx, proj.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), got, "no events yet")

	_, _, err = d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "t", Author: "a",
	})
	require.NoError(t, err)

	got, err = d.MaxLocalOriginEventID(ctx, proj.ID)
	require.NoError(t, err)
	require.Greater(t, got, int64(0), "issue creation produced a local-origin event")
}

func TestMaxFederationBaselineEventID(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	proj, err := d.CreateProject(ctx, "p")
	require.NoError(t, err)

	// No baseline snapshots yet — MAX() over an empty set is NULL, surfaced as 0.
	got, err := d.MaxFederationBaselineEventID(ctx, proj.ID, 0)
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/db/ -run 'TestMaxLocalOriginEventID|TestMaxFederationBaselineEventID' -v`
Expected: FAIL — `d.MaxLocalOriginEventID undefined`, `d.MaxFederationBaselineEventID undefined`.

- [ ] **Step 3: Implement the methods**

Add to `internal/db/queries_events.go` (the SQL bodies are moved verbatim from `handlers_federation.go`):

```go
// MaxLocalOriginEventID returns the largest events.id row whose project_id
// matches and whose origin_instance_uid is this database's instance. Federation
// uses it to seed the push cursor at "everything we authored so far". Returns 0
// when no matching rows exist.
func (d *DB) MaxLocalOriginEventID(ctx context.Context, projectID int64) (int64, error) {
	var n sql.NullInt64
	if err := d.QueryRowContext(ctx, `
		SELECT MAX(id)
		  FROM events
		 WHERE project_id = ?
		   AND origin_instance_uid = ?`,
		projectID, d.InstanceUID()).Scan(&n); err != nil {
		return 0, fmt.Errorf("max local-origin event id: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

// MaxFederationBaselineEventID returns the largest events.id row of type
// 'issue.snapshot' whose id is at least sinceEventID, scoped to projectID.
// Federation's status report uses this to declare "baseline materialized
// through" the highest snapshot at or above the replay horizon. Returns 0 when
// no matching snapshot exists.
func (d *DB) MaxFederationBaselineEventID(ctx context.Context, projectID, sinceEventID int64) (int64, error) {
	var n sql.NullInt64
	if err := d.QueryRowContext(ctx, `
		SELECT MAX(id)
		  FROM events
		 WHERE project_id = ?
		   AND type = 'issue.snapshot'
		   AND id >= ?`,
		projectID, sinceEventID).Scan(&n); err != nil {
		return 0, fmt.Errorf("max federation baseline event id: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}
```

Add `database/sql` to `queries_events.go`'s import block if it's not already imported (the file already uses `fmt`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/db/ -run 'TestMaxLocalOriginEventID|TestMaxFederationBaselineEventID' -v`
Expected: PASS.

- [ ] **Step 5: Switch the daemon call sites**

In `internal/daemon/handlers_federation.go`, replace the `maxLocalOriginEventID` helper (lines 440-454) with the thin delegator:

```go
func maxLocalOriginEventID(ctx context.Context, store *db.DB, projectID int64) (int64, error) {
	return store.MaxLocalOriginEventID(ctx, projectID)
}
```

In `projectFederationBody`, replace the inline `QueryRowContext` block (lines 744-758) with:

```go
through := binding.ReplayHorizonEventID
maxSnapshot, err := store.MaxFederationBaselineEventID(ctx, projectID, binding.ReplayHorizonEventID)
if err != nil {
	return api.ProjectFederationBody{}, api.NewError(500, "internal", err.Error(), "", nil)
}
if maxSnapshot > 0 {
	through = maxSnapshot
}
```

Run `goimports -w internal/daemon/handlers_federation.go` (or let the build tell you) so the `database/sql` import drops if no other code in the file uses it.

- [ ] **Step 6: Build, test, commit**

Run: `go build ./... && go test ./internal/db/ ./internal/daemon/ ./internal/federation/`
Expected: build OK; tests PASS. (Federation push/pull tests exercise `MaxLocalOriginEventID` through `enableReplicaPush`; federation status tests exercise `MaxFederationBaselineEventID` through `projectFederationBody`.)

```bash
git add internal/db/queries_events.go internal/db/queries_events_test.go internal/daemon/handlers_federation.go
git commit -m "refactor(db): add federation events-MAX methods on *DB"
```

---

### Task 7: Federation status counts — eliminate three COUNT-escape hatches

Federation added three raw `SELECT COUNT(*)` reads in `handlers_federation.go`: `federationEnrollmentCount` (line 667, called from `federationProjectStatus`), `federationLiveClaimCount` (line 683), and `federationPendingClaimCount` (line 697). Each lifts into a `*DB` method; the daemon-side helpers stay as thin guards/wrappers so call sites are untouched.

**Files:**
- Modify: `internal/db/federation.go` (add `CountActiveFederationEnrollments`)
- Modify: `internal/db/claims.go` (add `CountLiveClaims`, `CountPendingClaims`)
- Modify: `internal/daemon/handlers_federation.go:667-709` (helpers become thin)
- Test: `internal/db/federation_test.go` and `internal/db/claims_test.go` (add corresponding tests)

- [ ] **Step 1: Write the failing tests**

Add to `internal/db/federation_test.go`:

```go
func TestCountActiveFederationEnrollments(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	proj, err := d.CreateProject(ctx, "hub")
	require.NoError(t, err)

	// Zero case proves the path.
	got, err := d.CountActiveFederationEnrollments(ctx, proj.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), got)

	// Positive case: seed an active project-scoped enrollment + an active
	// global (project_id IS NULL) enrollment + a revoked one, prove the COUNT
	// returns 2 (active project + active global, revoked excluded). Use
	// d.CreateFederationEnrollment + d.RevokeFederationEnrollment from
	// federation_enrollments.go — see existing federation_enrollments_test.go
	// for parameter shapes (CreateFederationEnrollmentParams takes
	// SpokeInstanceUID, optional *int64 ProjectID, and Capabilities). After
	// seeding, assert CountActiveFederationEnrollments returns 2.
}
```

Add to `internal/db/claims_test.go`:

```go
func TestCountLiveClaims(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	proj, err := d.CreateProject(ctx, "p")
	require.NoError(t, err)

	// Zero case proves the path.
	got, err := d.CountLiveClaims(ctx, proj.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), got, "no claims yet")

	// Positive case: seed via d.AcquireClaim for a hard claim and a soft claim
	// with future expires_at; release one; assert count returns 2 (hard + soft
	// in force, released excluded). See claims_test.go for AcquireClaimParams
	// shape (ProjectID, IssueRef, Principal ClaimPrincipal, ClaimKind, Lease
	// time.Duration, Now time.Time).
}

func TestCountPendingClaims(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	proj, err := d.CreateProject(ctx, "p")
	require.NoError(t, err)

	// Zero case proves the path.
	got, err := d.CountPendingClaims(ctx, proj.ID)
	require.NoError(t, err)
	require.Equal(t, int64(0), got, "no pending requests yet")

	// Positive case: enqueue three pending claims via d.EnqueuePendingClaim,
	// then resolve one and reject another; assert count returns 1 (the
	// remaining open request). See claims_test.go for PendingClaimParams
	// shape.
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/db/ -run 'TestCountActiveFederationEnrollments|TestCountLiveClaims|TestCountPendingClaims' -v`
Expected: FAIL — three `undefined` errors.

- [ ] **Step 3: Implement the methods**

Add to `internal/db/federation.go` (the SQL is moved verbatim from `federationEnrollmentCount`):

```go
// CountActiveFederationEnrollments returns the number of non-revoked
// federation_enrollments rows visible to projectID: project-specific
// enrollments (project_id = projectID) plus globally-scoped ones (project_id
// IS NULL). The Hub-role guard stays in the caller — this method is a raw
// count.
func (d *DB) CountActiveFederationEnrollments(ctx context.Context, projectID int64) (int64, error) {
	var count int64
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM federation_enrollments
		 WHERE revoked_at IS NULL
		   AND (project_id = ? OR project_id IS NULL)`,
		projectID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count federation enrollments: %w", err)
	}
	return count, nil
}
```

Add to `internal/db/claims.go` (SQL moved verbatim from `federationLiveClaimCount` and `federationPendingClaimCount`):

```go
// CountLiveClaims returns the number of unreleased issue_claims rows for
// projectID that are still in force — hard claims and soft claims whose
// expires_at is in the future. "Now" is computed by SQLite's strftime so the
// semantic stays identical to the original helper.
func (d *DB) CountLiveClaims(ctx context.Context, projectID int64) (int64, error) {
	var count int64
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM issue_claims
		 WHERE project_id = ?
		   AND released_at IS NULL
		   AND (claim_kind = 'hard' OR expires_at > strftime('%Y-%m-%dT%H:%M:%fZ','now'))`,
		projectID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count live claims: %w", err)
	}
	return count, nil
}

// CountPendingClaims returns the number of pending_claim_requests rows for
// projectID that are still open — neither rejected nor resolved.
func (d *DB) CountPendingClaims(ctx context.Context, projectID int64) (int64, error) {
	var count int64
	if err := d.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM pending_claim_requests
		 WHERE project_id = ?
		   AND rejected_at IS NULL
		   AND resolved_at IS NULL`,
		projectID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count pending claims: %w", err)
	}
	return count, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/db/ -run 'TestCountActiveFederationEnrollments|TestCountLiveClaims|TestCountPendingClaims' -v`
Expected: PASS.

- [ ] **Step 5: Switch the daemon helpers to thin delegators**

In `internal/daemon/handlers_federation.go`, replace the `federationEnrollmentCount` body (lines 667-681) with:

```go
func federationEnrollmentCount(ctx context.Context, store *db.DB, binding db.FederationBinding) (int64, error) {
	if binding.Role != db.FederationRoleHub {
		return 0, nil
	}
	return store.CountActiveFederationEnrollments(ctx, binding.ProjectID)
}
```

Replace `federationLiveClaimCount` (lines 683-695) with:

```go
func federationLiveClaimCount(ctx context.Context, store *db.DB, projectID int64) (int64, error) {
	return store.CountLiveClaims(ctx, projectID)
}
```

Replace `federationPendingClaimCount` (lines 697-709) with:

```go
func federationPendingClaimCount(ctx context.Context, store *db.DB, projectID int64) (int64, error) {
	return store.CountPendingClaims(ctx, projectID)
}
```

- [ ] **Step 6: Build, test, commit**

Run: `rg -n "SELECT COUNT\\(\\*\\)" internal/daemon/`
Expected: no hits.

Run: `go build ./... && go test ./internal/db/ ./internal/daemon/ ./internal/federation/`
Expected: build OK; tests PASS.

```bash
git add internal/db/federation.go internal/db/claims.go internal/db/federation_test.go internal/db/claims_test.go internal/daemon/handlers_federation.go
git commit -m "refactor(db): add federation status COUNT methods on *DB"
```

---

### Task 8: Final verification — no raw-SQL escape hatches or tx leaks remain outside internal/db

**Files:** none (verification only)

- [ ] **Step 1: Confirm no exported `*DB` method leaks a transaction**

Run: `rg -n "func \(d \*DB\) [A-Z].*\*sql\.Tx" internal/db/`
Expected: no hits. (Every `*sql.Tx`-taking method is now unexported.)

- [ ] **Step 2: Confirm the daemon no longer runs raw SQL on the handle**

Run: `rg -n "\.QueryContext|\.QueryRowContext|\.ExecContext" internal/daemon/ -g '!*_test.go'`
Expected: no hits. (All eleven escape hatches — health and instance schema_version reads, listComments, the two project deletes, the alias update, plus the two federation event-id reads and three federation-status COUNTs — now go through `*db.DB` methods.)

Run: `rg -n "\.QueryContext|\.QueryRowContext|\.ExecContext" internal/federation/ -g '!*_test.go'`
Expected: no hits. (`internal/federation/` only touched raw SQL in tests, but verify nothing slipped into production.)

Note: `internal/jsonl` (export/import/preflight/fkmeta) still uses raw SQL on the handle by design; porting it into `sqlitestore` methods is plan 1c, not this plan.

- [ ] **Step 3: Full build, test, and lint**

Run: `go build ./... && go test ./...`
Expected: build OK; all tests PASS.

Run: `nix run 'nixpkgs#golangci-lint' -- run`
Expected: zero issues. (Fix any unused-import or unused-variable warnings introduced by the refactors, e.g. a now-unused `strconv` in a daemon handler, or a now-unused `database/sql` in `handlers_federation.go`.)

- [ ] **Step 4: No commit** (verification task; nothing changed).

---

## What this plan deliberately leaves for later phases

- **Plan 1b** — Define the `db.Storage` interface (the ~160 public `*DB` methods on federation main, now all transaction-free) in `internal/db/storage.go`, add `var _ Storage = (*DB)(nil)`, and switch the clean callers (`daemon.ServerConfig.DB`, the resolver, the `handlers_*.go`, `internal/federation/federation_sync.go`, `internal/federation/federation_client.go`, `cmd/kata`, `internal/testenv`) from `*db.DB` to `db.Storage`. Federation's merge moved `daemon.ServerConfig.DB` back to `*db.DB`; this is where the interface holder is reinstated.
- **Plan 1c** — Port the `internal/jsonl` export/import SQL (the `BeginTx`, per-entity SELECTs/INSERTs, `sqlite_sequence` reconciliation, `PRAGMA integrity_check`/`foreign_key_check`/`table_info`, and the `RefreshInstanceUID` dance) into `Store.ExportJSONL`/`Store.ImportJSONL` methods, including federation's new `federation_bindings` / `claim_log` entities; shrink `internal/jsonl` to the wire format + envelope shaping. Rewrite the jsonl tests (e.g. `fixtures_test.go`'s raw `*sql.DB` schema-shape helpers, `export_test.go`'s `d.Conn`).
- **Plan 1d** — Physically move the SQLite implementation into `internal/db/sqlitestore`; make `db.Open` return `db.Storage` via a DSN selector; keep `db.CurrentSchemaVersion()` in the neutral package; lift the `lock_retry.go` backoff into a backend-neutral `internal/db/retry.go` as `RetryTransient(ctx, isTransient, op)`. Update every `db.Open`/`db.OpenReadOnly` test call site and any test reaching the embedded `*sql.DB`.
