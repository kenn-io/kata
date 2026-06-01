# Kata Storage Phase 2 — Migration Runner Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `sqlitestore.Open`'s bootstrap-or-cutover dichotomy with a forward-only migration runner driven by `storeopen.Open(ctx, dsn, db.ApplyMigrations())`, the single entry point that brings any kata database (fresh, pre-v10, at-current, or with future pending) up to current, plus a `kata migrate` CLI for explicit migration.

**Architecture:** `storeopen` becomes the orchestrator: it peeks the schema version, drives `jsonl.AutoCutover` for SQLite DBs below v10, opens the backend, and calls a new `Storage.Migrate(ctx)` method that applies every pending file from an embedded `migrations/*.sql` ladder under `BEGIN IMMEDIATE` and a `VACUUM INTO` snapshot. Without `ApplyMigrations`, `storeopen.Open` refuses mutations and returns `db.ErrSchemaOutOfDate` pointing the user at `kata migrate`. Existing v10 users see no behavior change.

**Tech Stack:** Go 1.26, `database/sql` over `modernc.org/sqlite`, `embed`/`io/fs` for the migration ladder, `testing/fstest` for test-time injection of synthetic migrations, `github.com/stretchr/testify` for assertions, existing `internal/jsonl` for pre-v10 cutover, `github.com/spf13/cobra` for the CLI.

---

## File structure

Each file has one responsibility; together they replace the `bootstrap` path with the orchestrated runner.

**Create:**
- `internal/db/migrations.go` — backend-neutral `MigrationResult` type (no behavior).
- `internal/db/sqlitestore/migrations/0010_baseline.sql` — byte-for-byte move of today's `schema.sql`.
- `internal/db/sqlitestore/migrate.go` — `Store.Migrate(ctx)` with snapshot + lock + apply loop; `migrationsSource` test seam.
- `internal/db/sqlitestore/export_test.go` — exports the `migrationsSource` seam to the external `sqlitestore_test` package without making it public.
- `internal/db/sqlitestore/migrate_test.go` — `Migrate`-focused tests (fresh DB, at-current, synthetic 11+, rollback, concurrent).
- `cmd/kata/migrate.go` — `kata migrate` CLI command.
- `cmd/kata/migrate_test.go` — exec-based test of the CLI.

**Modify:**
- `internal/db/open.go` — add `ApplyMigrations` field + `ApplyMigrations()` option.
- `internal/db/errors.go` — add `ErrSchemaOutOfDate`; remove `ErrSchemaCutoverRequired` in Task 5.
- `internal/db/storage.go` — add `Migrate(ctx) (MigrationResult, error)` to the identity/lifecycle group.
- `internal/db/sqlitestore/store.go` — swap `//go:embed schema.sql` for `migrations/*.sql` embed.FS; remove `bootstrap()`; split `instance_uid` cache between `Open` (read existing if `meta` exists) and `Migrate` (only writer).
- `internal/db/storeopen/storeopen.go` — third return value (`db.MigrationResult`); `ApplyMigrations` orchestration (peek → cutover → open → Migrate); `OpenReadOnly` drops `ApplyMigrations`.
- `internal/db/storeopen/storeopen_test.go` — three-value signature; new tests per spec.
- `internal/db/sqlitestore/db_test.go` — drop `ErrSchemaCutoverRequired` test; rewrite `TestOpen_*` for the new contract (Open is pure handle-creation).
- `internal/db/sqlitestore/testhelpers_test.go` — `openTestDB*` helpers call `Migrate` after `Open`.
- `internal/jsonl/cutover.go` — `importCutoverTarget` calls `target.Migrate(ctx)` after Open (cutover target is empty after Open now).
- `cmd/kata/daemon_cmd.go` — collapse pre-open peek+cutover to one `storeopen.Open(ctx, dbPath, db.ApplyMigrations())`.
- `cmd/kata/export.go`, `cmd/kata/import.go` — adapt to three-value signature; assign `MigrationResult` to `_`.
- `cmd/kata/main.go` — register `newMigrateCmd()` in `subs`.
- `cmd/kata/daemon_cutover_test.go` — adapt to the daemon no longer running its own peek+cutover dance.

---

## Task ordering rationale

Each commit must build and `go test ./...` must pass before the next task starts. The order keeps these invariants by introducing additive surface first, then layering the orchestration before removing the legacy path:

- Task 1 adds types (`MigrationResult`, `ApplyMigrations` option, `ErrSchemaOutOfDate`) — unread, additive, behavior preserved.
- Task 2 rearranges the embedded SQL file but `bootstrap` still applies it the same way.
- Task 3 adds the `Storage.Migrate` method and its sqlite implementation. `bootstrap` is still present in `Open`, so callers continue to get a v10 DB out of `sqlitestore.Open`. Migrate-on-current is a no-op; the synthetic 11+ test exercises the new ladder via the seam.
- Task 4 changes `storeopen.Open`'s return shape, adds the `ApplyMigrations` orchestration branch, and updates every caller. Daemon flips to pass `ApplyMigrations()`. Behavior still equivalent for sqlite (storeopen calls bootstrap-via-Open then Migrate-no-op).
- Task 5 removes `bootstrap()` from `sqlitestore.Open` and delegates baseline application entirely to `Migrate`. `jsonl/cutover.go` and test helpers now call Migrate explicitly. `ErrSchemaCutoverRequired` is deleted as its last hold goes away.
- Task 6 adds the `kata migrate` CLI — the surface promised by the spec's error messages.

---

## Task 1: Add `MigrationResult`, `ApplyMigrations()` option, and `ErrSchemaOutOfDate` sentinel

**Files:**
- Create: `internal/db/migrations.go`
- Create: `internal/db/migrations_test.go`
- Modify: `internal/db/open.go`
- Modify: `internal/db/errors.go`

- [ ] **Step 1: Write the failing test for `db.ApplyMigrations()` and `MigrationResult`**

Create `internal/db/migrations_test.go`:

```go
package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

func TestApplyMigrationsSetsOpenConfigFlag(t *testing.T) {
	cfg := db.ApplyOpenOptions(db.ApplyMigrations())
	assert.True(t, cfg.ApplyMigrations)
	assert.False(t, cfg.ReadOnly)
}

func TestApplyMigrationsAndReadOnlyComposeIndependently(t *testing.T) {
	cfg := db.ApplyOpenOptions(db.ApplyMigrations(), db.ReadOnly())
	assert.True(t, cfg.ApplyMigrations)
	assert.True(t, cfg.ReadOnly)
}

func TestMigrationResultZeroValueIsNoop(t *testing.T) {
	var r db.MigrationResult
	assert.Equal(t, 0, r.From)
	assert.Equal(t, 0, r.To)
	assert.Nil(t, r.Applied)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/db/... -run 'TestApplyMigrations|TestMigrationResult' -v`
Expected: FAIL (`db.ApplyMigrations` undefined, `db.MigrationResult` undefined).

- [ ] **Step 3: Add `MigrationResult` type**

Create `internal/db/migrations.go`:

```go
package db

// MigrationResult describes what a Storage.Migrate call did. The zero value
// represents "no migration ran" — either because the caller did not pass
// db.ApplyMigrations() to storeopen.Open or because the database was already
// at the current schema version.
type MigrationResult struct {
	// From is the meta.schema_version observed before the run. Zero for a
	// fresh database (no meta table).
	From int
	// To is the meta.schema_version recorded after the run completes. When
	// the migration succeeds this equals db.CurrentSchemaVersion(). When the
	// run was a no-op it equals From.
	To int
	// Applied is the ordered list of versions advanced through, in ascending
	// order. nil when no work was done (Migrate was a no-op).
	Applied []int
}
```

- [ ] **Step 4: Add `ApplyMigrations` field and option**

Modify `internal/db/open.go` (replace the file contents):

```go
package db

// OpenConfig holds backend-neutral open options resolved from OpenOption values.
type OpenConfig struct {
	ReadOnly        bool
	ApplyMigrations bool
}

// OpenOption configures how a storage backend is opened.
type OpenOption func(*OpenConfig)

// ReadOnly opens the backend without bootstrapping or mutating schema state.
func ReadOnly() OpenOption { return func(c *OpenConfig) { c.ReadOnly = true } }

// ApplyMigrations authorizes the open path to bring the backend's schema up
// to db.CurrentSchemaVersion() before the handle is returned. Without this
// option a stale database surfaces db.ErrSchemaOutOfDate.
func ApplyMigrations() OpenOption {
	return func(c *OpenConfig) { c.ApplyMigrations = true }
}

// ApplyOpenOptions folds opts into an OpenConfig for a backend's Open to consume.
func ApplyOpenOptions(opts ...OpenOption) OpenConfig {
	var c OpenConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}
```

- [ ] **Step 5: Add `ErrSchemaOutOfDate` sentinel (keep `ErrSchemaCutoverRequired` for now)**

Modify `internal/db/errors.go` — locate the `ErrSchemaCutoverRequired` declaration (around line 13-15) and add `ErrSchemaOutOfDate` immediately after it:

```go
// ErrSchemaCutoverRequired is returned by Open when an existing database is
// older than the binary's schema and must be upgraded through JSONL cutover.
//
// Deprecated: replaced by ErrSchemaOutOfDate in Phase 2; deleted once the
// bootstrap path is removed in the same phase.
var ErrSchemaCutoverRequired = errors.New("schema cutover required")

// ErrSchemaOutOfDate is returned by storeopen.Open when the database's
// schema_version is not equal to db.CurrentSchemaVersion() and the caller did
// not pass db.ApplyMigrations(). The error message names `kata migrate` so the
// user has a direct next step.
var ErrSchemaOutOfDate = errors.New("schema out of date")
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/db/... -run 'TestApplyMigrations|TestMigrationResult' -v`
Expected: PASS (3 cases).

- [ ] **Step 7: Verify the full build is still green**

Run: `go build ./... && go test ./...`
Expected: PASS (no regression — `ApplyMigrations` field is unread; `ErrSchemaOutOfDate` is unused).

- [ ] **Step 8: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/db/migrations.go internal/db/migrations_test.go internal/db/open.go internal/db/errors.go
git commit -m "feat(db): add MigrationResult, ApplyMigrations() option, ErrSchemaOutOfDate"
```

---

## Task 2: Move `schema.sql` into `migrations/` and switch to `embed.FS`

The migration ladder lives in `internal/db/sqlitestore/migrations/*.sql`. Phase 2 ships only the baseline at `0010_baseline.sql` (byte-for-byte identical to today's `schema.sql`); future tasks add `0011_*.sql` etc. `bootstrap()` continues to apply the baseline — the source just moves.

**Files:**
- Move: `internal/db/sqlitestore/schema.sql` → `internal/db/sqlitestore/migrations/0010_baseline.sql`
- Modify: `internal/db/sqlitestore/store.go:22-23` (the `//go:embed` directive and the var)
- Modify: `internal/db/sqlitestore/store.go:190-225` (`bootstrap()` reads from the new FS)

- [ ] **Step 1: Verify the file move is byte-for-byte**

Run: `mkdir -p internal/db/sqlitestore/migrations`
Run: `git mv internal/db/sqlitestore/schema.sql internal/db/sqlitestore/migrations/0010_baseline.sql`
Run: `git diff --stat -M HEAD`
Expected: one rename, zero content changes.

- [ ] **Step 2: Swap the embed directive and bootstrap reader**

Modify `internal/db/sqlitestore/store.go`. Replace lines 22-23:

```go
//go:embed schema.sql
var schemaSQL string
```

with:

```go
//go:embed migrations/*.sql
var migrationsFS embed.FS
```

Add `"embed"` to the imports block at the top of the file (or replace the existing `_ "embed"` blank import — it can become a named import now).

Replace the body of `bootstrap()` (the section between the `tx, err := d.BeginTx(...)` block and `return nil`). Specifically, the line `if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {` becomes:

```go
baselineSQL, err := migrationsFS.ReadFile("migrations/0010_baseline.sql")
if err != nil {
	_ = tx.Rollback()
	return fmt.Errorf("read baseline migration: %w", err)
}
if _, err := tx.ExecContext(ctx, string(baselineSQL)); err != nil {
	_ = tx.Rollback()
	return fmt.Errorf("apply schema: %w", err)
}
```

- [ ] **Step 3: Run the build and the full test suite to verify behavior parity**

Run: `go build ./... && go test ./...`
Expected: PASS — `bootstrap` still applies the same SQL, just from a different source.

- [ ] **Step 4: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/sqlitestore/migrations/0010_baseline.sql internal/db/sqlitestore/store.go
git commit -m "refactor(sqlitestore): embed migrations/*.sql; baseline moves to 0010_baseline.sql"
```

---

## Task 3: Add `Storage.Migrate` interface method and `sqlitestore.Store.Migrate` implementation

This task introduces the runner. `bootstrap()` stays in place so existing callers behave unchanged; the new `Migrate` method works on already-bootstrapped DBs (no-op when at-current; applies pending synthetic 11+ via the test seam). The "fresh DB applies baseline" case is verified later in Task 5 once `bootstrap()` is removed.

**Files:**
- Modify: `internal/db/storage.go` (add `Migrate` to the identity/lifecycle group, around line 23 after `RetryTransient`)
- Create: `internal/db/sqlitestore/migrate.go`
- Create: `internal/db/sqlitestore/export_test.go`
- Create: `internal/db/sqlitestore/migrate_test.go`

- [ ] **Step 1: Write the failing at-current Migrate test**

Create `internal/db/sqlitestore/migrate_test.go`:

```go
package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestMigrate_OnAtCurrentDBIsNoop(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	result, err := d.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	assert.Nil(t, result.Applied)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/db/sqlitestore/... -run TestMigrate_OnAtCurrentDBIsNoop -v`
Expected: FAIL (`d.Migrate undefined`).

- [ ] **Step 3: Add `Migrate` to the `Storage` interface**

Modify `internal/db/storage.go`. Insert `Migrate(ctx context.Context) (MigrationResult, error)` immediately after `RetryTransient(ctx context.Context, op func() error) error` in the identity/lifecycle group. The resulting block:

```go
type Storage interface {
	// identity / lifecycle
	InstanceUID() string
	RefreshInstanceUID(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	Path() string
	Close() error
	RetryTransient(ctx context.Context, op func() error) error
	// Migrate brings the backend's schema up to db.CurrentSchemaVersion()
	// by applying every pending migration file in order. Idempotent: returns
	// MigrationResult{From: N, To: N, Applied: nil} when already current.
	// On a backend opened read-only, returns an error.
	Migrate(ctx context.Context) (MigrationResult, error)

	// projects + aliases
	...
}
```

- [ ] **Step 4: Add the `migrationsSource` test seam alongside the embed**

Modify `internal/db/sqlitestore/store.go`. Add a new package-level declaration immediately after the `//go:embed migrations/*.sql` block:

```go
// migrationsSource is the FS the migration runner reads from. It defaults to
// the embedded migrationsFS; tests override it via export_test.go to inject
// synthetic 11+ files without polluting the on-disk migrations directory.
var migrationsSource fs.FS = migrationsFS
```

Add `"io/fs"` to the imports.

- [ ] **Step 5: Implement `Store.Migrate`**

Create `internal/db/sqlitestore/migrate.go`:

```go
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// Migrate brings the SQLite database up to db.CurrentSchemaVersion() by
// applying every pending migration file from migrationsSource in version
// order. The snapshot is taken before BEGIN IMMEDIATE because SQLite refuses
// VACUUM INTO inside a transaction; the lock then serializes concurrent
// migrators so only one applies pending steps. On any per-step error the
// transaction rolls back and the snapshot is retained — the error message
// names the snapshot path so an operator can restore.
func (d *Store) Migrate(ctx context.Context) (db.MigrationResult, error) {
	if d.readOnly {
		return db.MigrationResult{}, errors.New("migrate: backend is read-only")
	}

	current, err := d.currentVersion(ctx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version: %w", err)
	}
	pending, err := listPendingMigrations(migrationsSource, current)
	if err != nil {
		return db.MigrationResult{}, err
	}
	if len(pending) == 0 {
		if err := d.ensureInstanceUIDFromMeta(ctx); err != nil {
			return db.MigrationResult{}, err
		}
		return db.MigrationResult{From: current, To: current, Applied: nil}, nil
	}

	snapshotPath, err := d.takeRecoverySnapshot(ctx, current)
	if err != nil {
		return db.MigrationResult{}, err
	}

	// Acquire a SQLite write lock immediately so concurrent migrators
	// serialize on the BEGIN, not on the first write. modernc.org/sqlite
	// accepts `_txlock=immediate` in the DSN to make every BEGIN an
	// IMMEDIATE lock; another working approach is to pin a *sql.Conn and
	// run `BEGIN IMMEDIATE` directly. Pick whichever fits this driver
	// version best — the contract is "the transaction holds a write lock
	// before any per-step DDL runs". The reference implementation below
	// uses a pinned connection because it keeps the rest of the Store's
	// PRAGMAs (foreign_keys, journal_mode) untouched.
	conn, err := d.Conn(ctx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("acquire migration conn: %w (snapshot retained at %s)", err, snapshotPath)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return db.MigrationResult{}, fmt.Errorf("begin immediate: %w (snapshot retained at %s)", err, snapshotPath)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	// Re-read schema_version inside the lock — another migrator may have
	// advanced it between the snapshot and the lock. Recompute pending so we
	// only apply what's still pending.
	currentInTx, err := connCurrentVersion(ctx, conn)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version in tx: %w (snapshot retained at %s)", err, snapshotPath)
	}
	pendingInTx, err := listPendingMigrations(migrationsSource, currentInTx)
	if err != nil {
		return db.MigrationResult{}, err
	}

	applied := make([]int, 0, len(pendingInTx))
	for _, m := range pendingInTx {
		sqlBytes, err := fs.ReadFile(migrationsSource, m.fileName)
		if err != nil {
			return db.MigrationResult{}, fmt.Errorf("read migration %s: %w (snapshot retained at %s)", m.fileName, err, snapshotPath)
		}
		if _, err := conn.ExecContext(ctx, string(sqlBytes)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("apply migration %s: %w (snapshot retained at %s)", m.fileName, err, snapshotPath)
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			strconv.Itoa(m.version)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("stamp schema_version %d: %w (snapshot retained at %s)", m.version, err, snapshotPath)
		}
		applied = append(applied, m.version)
	}

	if err := ensureInstanceUIDOnConn(ctx, conn, &d.instanceUID); err != nil {
		return db.MigrationResult{}, fmt.Errorf("ensure instance_uid: %w (snapshot retained at %s)", err, snapshotPath)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return db.MigrationResult{}, fmt.Errorf("commit migration: %w (snapshot retained at %s)", err, snapshotPath)
	}
	committed = true

	if rmErr := os.Remove(snapshotPath); rmErr != nil && !os.IsNotExist(rmErr) {
		// Best-effort: leaving the snapshot on disk wastes space but the
		// migration is still successful. Do not surface as an error.
		_ = rmErr
	}

	to := currentInTx
	if len(applied) > 0 {
		to = applied[len(applied)-1]
	}
	return db.MigrationResult{From: current, To: to, Applied: applied}, nil
}

type pendingMigration struct {
	version  int
	fileName string
}

func listPendingMigrations(source fs.FS, current int) ([]pendingMigration, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var pending []pendingMigration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		if len(name) < 4 {
			return nil, fmt.Errorf("migration %q: name too short to carry a version prefix", name)
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil {
			return nil, fmt.Errorf("migration %q: parse version prefix: %w", name, err)
		}
		if v > current {
			// io/fs paths are always forward-slash; filepath.Join would emit
			// "migrations\name" on Windows and break fs.ReadFile.
			pending = append(pending, pendingMigration{version: v, fileName: path.Join("migrations", name)})
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })
	return pending, nil
}

func (d *Store) takeRecoverySnapshot(ctx context.Context, fromVersion int) (string, error) {
	home, err := config.KataHome()
	if err != nil {
		return "", fmt.Errorf("resolve KATA_HOME: %w", err)
	}
	dir := filepath.Join(home, "runtime", config.DBHash(d.path))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot dir: %w", err)
	}
	snapshot := filepath.Join(dir, fmt.Sprintf("premigrate-v%d.db", fromVersion))
	if err := os.Remove(snapshot); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("clear stale snapshot %s: %w", snapshot, err)
	}
	if _, err := d.ExecContext(ctx, fmt.Sprintf("VACUUM INTO %s", sqliteQuoteLiteral(snapshot))); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", snapshot, err)
	}
	return snapshot, nil
}

// sqliteQuoteLiteral wraps s in single quotes and doubles any single quotes
// inside it. Path-based; the migration code controls the input shape, so this
// guards against future surprises (e.g. KATA_HOME on a path with apostrophes).
func sqliteQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func connCurrentVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v string
	err = conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// ensureInstanceUIDFromMeta caches meta.instance_uid into d.instanceUID when
// Migrate is a no-op. Called only on the at-current path because Open already
// caches in the bootstrap path; once bootstrap is removed in Task 5 this is
// the canonical fast-path cacher for handles that won't run any DDL.
func (d *Store) ensureInstanceUIDFromMeta(ctx context.Context) error {
	if d.instanceUID != "" {
		return nil
	}
	var v string
	err := d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	d.instanceUID = v
	return nil
}

// ensureInstanceUIDOnConn is Migrate's pre-commit step: read meta.instance_uid
// if present, otherwise generate one and INSERT ... ON CONFLICT DO NOTHING.
// Runs on the pinned connection so it joins the active transaction.
func ensureInstanceUIDOnConn(ctx context.Context, conn *sql.Conn, cached *string) error {
	var v string
	err := conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if err == nil {
		*cached = v
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	fresh, err := katauid.New()
	if err != nil {
		return fmt.Errorf("generate instance_uid: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('instance_uid', ?)
		 ON CONFLICT(key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	*cached = stored
	return nil
}
```

- [ ] **Step 6: Add the `readOnly` field to `Store` so `Migrate` can refuse it**

Modify `internal/db/sqlitestore/store.go`. Locate the `Store` struct (around line 25-30) and add a `readOnly bool` field:

```go
type Store struct {
	*sql.DB
	path        string
	instanceUID string
	readOnly    bool
}
```

In `openReadOnly` (around line 78-92), set `readOnly: true` when constructing the Store:

```go
return &Store{DB: sdb, path: path, readOnly: true}, nil
```

- [ ] **Step 7: Export the `migrationsSource` seam for external tests**

Create `internal/db/sqlitestore/export_test.go`:

```go
package sqlitestore

import "io/fs"

// MigrationsSource exposes the migration FS seam to external tests in
// sqlitestore_test. Production callers cannot reach it. Use SetMigrationsSource
// to swap; the returned func restores the original.
func SetMigrationsSource(fsys fs.FS) func() {
	prev := migrationsSource
	migrationsSource = fsys
	return func() { migrationsSource = prev }
}
```

- [ ] **Step 8: Run the at-current Migrate test to verify it passes**

Run: `go test ./internal/db/sqlitestore/... -run TestMigrate_OnAtCurrentDBIsNoop -v`
Expected: PASS.

- [ ] **Step 9: Extend the export helper to expose the embedded FS**

Modify `internal/db/sqlitestore/export_test.go` to add a second helper alongside `SetMigrationsSource`:

```go
// EmbeddedMigrationsFS returns the production embedded FS. Tests that build
// synthetic ladders use this to anchor their MapFS on the real baseline.
func EmbeddedMigrationsFS() fs.FS {
	return migrationsFS
}
```

- [ ] **Step 10: Add the synthetic 11+ test**

Append to `internal/db/sqlitestore/migrate_test.go`. First add to the existing `import` block: `"io/fs"`, `"os"`, `"testing/fstest"`, and `"go.kenn.io/kata/internal/config"` (used by this and the next two tests). Then append:

```go
func TestMigrate_AppliesSyntheticVersion11(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()

	baseline, err := fs.ReadFile(sqlitestore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql": &fstest.MapFile{Data: baseline},
		"migrations/0011_add_marker.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE migration_marker (
  id INTEGER PRIMARY KEY,
  note TEXT NOT NULL
);
`)},
	}
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	dbPath := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	result, err := d.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, 11, result.To)
	assert.Equal(t, []int{11}, result.Applied)

	// The marker table now exists.
	var n int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migration_marker'`).Scan(&n))
	assert.Equal(t, 1, n)

	// Snapshot was created and deleted on success: the namespaced path is
	// $KATA_HOME/runtime/<DBHash(dbPath)>/premigrate-v10.db.
	expectedSnap := filepath.Join(os.Getenv("KATA_HOME"), "runtime", config.DBHash(dbPath), "premigrate-v10.db")
	_, statErr := os.Stat(expectedSnap)
	assert.True(t, os.IsNotExist(statErr), "snapshot should be removed on success; got %v", statErr)
}
```

- [ ] **Step 11: Run the synthetic test to verify it passes**

Run: `go test ./internal/db/sqlitestore/... -run TestMigrate_AppliesSyntheticVersion11 -v`
Expected: PASS.

- [ ] **Step 12: Add the rollback test**

Append to `internal/db/sqlitestore/migrate_test.go`:

```go
func TestMigrate_RollsBackOnBrokenMigration(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()

	baseline, err := fs.ReadFile(sqlitestore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	// 0011 attempts to re-CREATE the meta table — the baseline already created it,
	// so the apply fails inside the transaction and Migrate must roll back.
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql": &fstest.MapFile{Data: baseline},
		"migrations/0011_break.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);`)},
	}
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	dbPath := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	// The snapshot path appears in the error so an operator can restore.
	expectedSnap := filepath.Join(os.Getenv("KATA_HOME"), "runtime", config.DBHash(dbPath), "premigrate-v10.db")
	assert.Contains(t, err.Error(), expectedSnap)

	// schema_version unchanged.
	v, sverr := d.SchemaVersion(ctx)
	require.NoError(t, sverr)
	assert.Equal(t, db.CurrentSchemaVersion(), v)

	// Snapshot file remains on disk for recovery.
	_, statErr := os.Stat(expectedSnap)
	require.NoError(t, statErr)
}
```

- [ ] **Step 13: Run the rollback test**

Run: `go test ./internal/db/sqlitestore/... -run TestMigrate_RollsBackOnBrokenMigration -v`
Expected: PASS.

- [ ] **Step 14: Add the concurrent migrators test**

Append to `internal/db/sqlitestore/migrate_test.go`. First add `"sync"` to the existing import block, then append:

```go
func TestMigrate_ConcurrentMigratorsSerialize(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()

	baseline, err := fs.ReadFile(sqlitestore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql": &fstest.MapFile{Data: baseline},
		"migrations/0011_marker.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE concurrent_marker (id INTEGER PRIMARY KEY);`)},
	}
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	dbPath := filepath.Join(t.TempDir(), "kata.db")
	d1, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d1.Close() })
	d2, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d2.Close() })

	type outcome struct {
		applied []int
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, d := range []*sqlitestore.Store{d1, d2} {
		wg.Add(1)
		go func(store *sqlitestore.Store) {
			defer wg.Done()
			r, err := store.Migrate(ctx)
			results <- outcome{applied: r.Applied, err: err}
		}(d)
	}
	wg.Wait()
	close(results)

	totalApplied := 0
	var failureCount int
	for o := range results {
		if o.err != nil {
			failureCount++
			continue
		}
		totalApplied += len(o.applied)
	}
	// At least one migrator succeeds. The other either also succeeds (with an
	// empty Applied because it observed the post-migration version in the lock)
	// or fails on busy lock contention. The marker is created exactly once.
	assert.LessOrEqual(t, failureCount, 1)
	assert.Equal(t, 1, totalApplied)

	var n int
	require.NoError(t, d1.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='concurrent_marker'`).Scan(&n))
	assert.Equal(t, 1, n)
}
```

- [ ] **Step 15: Run the concurrent test**

Run: `go test ./internal/db/sqlitestore/... -run TestMigrate_ConcurrentMigratorsSerialize -v -count=10`
Expected: PASS in all 10 iterations.

- [ ] **Step 16: Add Migrate entry guards (pre-v10 and newer-than-binary)**

Modify `internal/db/sqlitestore/migrate.go`. Between the `current, err := d.currentVersion(ctx)` block and the `listPendingMigrations` call in `Migrate`, insert:

```go
if current > 0 && current < db.CurrentSchemaVersion() {
	return db.MigrationResult{}, fmt.Errorf("schema_version %d predates the baseline (%d); use storeopen.Open with db.ApplyMigrations() so JSONL cutover runs first", current, db.CurrentSchemaVersion())
}
if current > db.CurrentSchemaVersion() {
	return db.MigrationResult{}, fmt.Errorf("schema_version %d is newer than binary schema %d; use a newer kata binary", current, db.CurrentSchemaVersion())
}
```

The two guards turn `Migrate` into a strict forward-only operator: it only accepts a fresh DB (`current == 0`) or an at-current DB (`current == CurrentSchemaVersion()`) on a writable handle. Pre-v10 DBs must be cutover'd before reaching `Migrate`; newer-than-binary DBs must be opened by a newer binary.

- [ ] **Step 17: Add ladder validation in `listPendingMigrations`**

Modify `internal/db/sqlitestore/migrate.go`'s `listPendingMigrations` to enforce two invariants on the migration ladder before returning pending files: no duplicate version prefixes, no gaps in the embedded ladder. The discovery loop builds the full list keyed by version, then a post-loop check rejects duplicates and a contiguity sweep rejects gaps. Replace the existing function body with:

```go
func listPendingMigrations(source fs.FS, current int) ([]pendingMigration, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	type migrationFile struct {
		version  int
		fileName string
	}
	var all []migrationFile
	seen := make(map[int]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		if len(name) < 4 {
			return nil, fmt.Errorf("migration %q: name too short to carry a version prefix", name)
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil {
			return nil, fmt.Errorf("migration %q: parse version prefix: %w", name, err)
		}
		if existing, dup := seen[v]; dup {
			return nil, fmt.Errorf("migration version %d: duplicate files %q and %q", v, existing, name)
		}
		seen[v] = name
		// io/fs paths are always forward-slash; filepath.Join would emit
		// "migrations\name" on Windows and break fs.ReadFile.
		all = append(all, migrationFile{version: v, fileName: path.Join("migrations", name)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	for i := 1; i < len(all); i++ {
		if all[i].version != all[i-1].version+1 {
			return nil, fmt.Errorf("migration ladder: version %d missing between %d and %d", all[i-1].version+1, all[i-1].version, all[i].version)
		}
	}
	var pending []pendingMigration
	for _, m := range all {
		if m.version > current {
			pending = append(pending, pendingMigration{version: m.version, fileName: m.fileName})
		}
	}
	return pending, nil
}
```

- [ ] **Step 18: Add tests for the entry guards and ladder validation**

Append to `internal/db/sqlitestore/migrate_test.go`:

```go
func TestMigrate_RejectsPreV10(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.ExecContext(ctx, `UPDATE meta SET value='8' WHERE key='schema_version'`)
	require.NoError(t, err)

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates the baseline")
	assert.Contains(t, err.Error(), "storeopen.Open")
}

func TestMigrate_RejectsNewerThanBinary(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.ExecContext(ctx, `UPDATE meta SET value='99' WHERE key='schema_version'`)
	require.NoError(t, err)

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
}

func TestMigrate_RejectsDuplicateLadderVersions(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	baseline, err := fs.ReadFile(sqlitestore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql": &fstest.MapFile{Data: baseline},
		"migrations/0011_a.sql":        &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		"migrations/0011_b.sql":        &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate files")
	assert.Contains(t, err.Error(), "0011_a.sql")
	assert.Contains(t, err.Error(), "0011_b.sql")
}

func TestMigrate_RejectsLadderGaps(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	baseline, err := fs.ReadFile(sqlitestore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql": &fstest.MapFile{Data: baseline},
		"migrations/0012_skip.sql":     &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version 11 missing")
}
```

- [ ] **Step 19: Run the full test suite**

Run: `go test ./...`
Expected: PASS. Existing tests still use `bootstrap` via `Open`; the new tests cover Migrate behavior; nothing else changed.

- [ ] **Step 20: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 21: Commit**

```bash
git add internal/db/storage.go internal/db/sqlitestore/store.go internal/db/sqlitestore/migrate.go internal/db/sqlitestore/export_test.go internal/db/sqlitestore/migrate_test.go
git commit -m "feat(sqlitestore): add Storage.Migrate with snapshot, lock, embed.FS ladder, test seam"
```

---

## Task 4: `storeopen.Open` orchestration, `ApplyMigrations` branch, three-value signature

The orchestrator gains its third return value and the `ApplyMigrations` behavior the spec calls out: peek → cutover-if-needed → open → Migrate. Without `ApplyMigrations`, stale DBs surface `ErrSchemaOutOfDate` instead of opening. The daemon flips to pass `ApplyMigrations()`; `kata export`/`import` continue to call `Open` without it.

**Files:**
- Modify: `internal/db/storeopen/storeopen.go`
- Modify: `internal/db/storeopen/storeopen_test.go`
- Modify: `cmd/kata/daemon_cmd.go:289-301`
- Modify: `cmd/kata/export.go:43`
- Modify: `cmd/kata/import.go:125`
- Modify: `cmd/kata/daemon_cutover_test.go`

- [ ] **Step 1: Write the failing "without ApplyMigrations on a stale DB returns ErrSchemaOutOfDate" test**

Modify `internal/db/storeopen/storeopen_test.go`. Append:

```go
func TestOpenWithoutApplyMigrationsReturnsErrSchemaOutOfDateForMissingDB(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "kata.db") // no file created
	_, _, err := storeopen.Open(context.Background(), missing)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrSchemaOutOfDate), err)
	assert.Contains(t, err.Error(), "kata migrate")
}
```

Add `"errors"` and `"go.kenn.io/kata/internal/db"` to the imports if not already present.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/db/storeopen/... -run TestOpenWithoutApplyMigrationsReturnsErrSchemaOutOfDateForMissingDB -v`
Expected: FAIL — `storeopen.Open` currently returns two values, not three; and even after the signature change it still opens any path.

- [ ] **Step 3: Change `storeopen.Open` to three-value signature with the new orchestration**

Replace the contents of `internal/db/storeopen/storeopen.go` with:

```go
// Package storeopen routes a DSN to the right db.Storage backend. Bare paths
// and sqlite:// DSNs open the SQLite backend; postgres:// DSNs return a clear
// "not yet available" error with the password redacted via config.RedactDSN.
//
// Open is the single entry point that orchestrates migration: with
// db.ApplyMigrations(), it peeks the schema, runs JSONL cutover for SQLite
// DBs below v10, opens the backend, and calls Storage.Migrate to apply
// pending steps. Without that option, a stale or missing DB surfaces
// db.ErrSchemaOutOfDate pointing the user at `kata migrate`.
package storeopen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// jsonlCutoverThreshold is the schema_version at which kata's embedded
// migration ladder starts: pre-baseline databases (versions 1..9) must be
// upgraded through jsonl.AutoCutover before the ladder can advance them; a
// peek that returns a value >= jsonlCutoverThreshold goes straight to Open +
// Migrate. Tying the threshold to a fixed value (rather than
// db.CurrentSchemaVersion()) keeps the cutover path stable as the runner
// ships future 0011_*.sql, 0012_*.sql, … files.
const jsonlCutoverThreshold = 10

// Open selects a storage backend from the DSN and returns a db.Storage along
// with the result of any migration that ran (zero-valued when nothing did).
// See the package doc for the full orchestration contract.
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, db.MigrationResult, error) {
	cfg := db.ApplyOpenOptions(opts...)
	if cfg.ReadOnly {
		// A read-only handle cannot migrate; silently drop ApplyMigrations.
		cfg.ApplyMigrations = false
	}

	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case hasScheme && (scheme == "postgres" || scheme == "postgresql"):
		return nil, db.MigrationResult{}, fmt.Errorf("postgres backend not yet available: %s", config.RedactDSN(dsn))
	case hasScheme && scheme != "sqlite":
		return nil, db.MigrationResult{}, fmt.Errorf("unsupported dsn scheme %q", scheme)
	}

	path := dsn
	if hasScheme {
		path = strings.TrimPrefix(dsn, "sqlite://")
	}

	if cfg.ApplyMigrations {
		return openSQLiteWithMigrations(ctx, path, opts)
	}
	return openSQLiteRefusingStale(ctx, path, opts)
}

// OpenReadOnly opens the backend selected by dsn read-only. Any ApplyMigrations
// option is dropped because a mode=ro handle cannot mutate schema.
func OpenReadOnly(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, db.MigrationResult, error) {
	return Open(ctx, dsn, append(opts, db.ReadOnly())...)
}

func openSQLiteWithMigrations(ctx context.Context, path string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	ver, peekErr := sqlitestore.PeekSchemaVersion(ctx, path)
	switch {
	case peekErr == nil && ver > db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary", ver, path, db.CurrentSchemaVersion())
	case peekErr == nil && ver > 0 && ver < jsonlCutoverThreshold:
		if err := jsonl.AutoCutover(ctx, path); err != nil {
			return nil, db.MigrationResult{}, err
		}
		// Re-peek to confirm cutover advanced the version.
		if _, err := sqlitestore.PeekSchemaVersion(ctx, path); err != nil {
			return nil, db.MigrationResult{}, fmt.Errorf("peek after cutover: %w", err)
		}
	case peekErr != nil && !isFileNotExist(peekErr):
		return nil, db.MigrationResult{}, peekErr
	}
	store, err := sqlitestore.Open(ctx, path, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, err
	}
	result, err := store.Migrate(ctx)
	if err != nil {
		_ = store.Close()
		return nil, db.MigrationResult{}, err
	}
	return store, result, nil
}

func openSQLiteRefusingStale(ctx context.Context, path string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	ver, peekErr := sqlitestore.PeekSchemaVersion(ctx, path)
	switch {
	case peekErr != nil && isFileNotExist(peekErr):
		return nil, db.MigrationResult{}, fmt.Errorf("%w: database does not exist at %s; run kata migrate", db.ErrSchemaOutOfDate, path)
	case peekErr != nil:
		return nil, db.MigrationResult{}, peekErr
	case ver > db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary", ver, path, db.CurrentSchemaVersion())
	case ver != db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("%w: schema_version %d, current %d; run kata migrate", db.ErrSchemaOutOfDate, ver, db.CurrentSchemaVersion())
	}
	store, err := sqlitestore.Open(ctx, path, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, err
	}
	return store, db.MigrationResult{}, nil
}

func isFileNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	// modernc.org/sqlite reports SQLITE_CANTOPEN as a string-suffix on the
	// wrapped error chain for missing files; the most portable check is
	// substring against the canonical message.
	return strings.Contains(err.Error(), "unable to open database file")
}

func splitScheme(dsn string) (scheme, rest string, hasScheme bool) {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return "", dsn, false
	}
	return dsn[:i], dsn[i+len("://"):], true
}
```

- [ ] **Step 4: Verify the missing-DB test now passes**

Run: `go test ./internal/db/storeopen/... -run TestOpenWithoutApplyMigrationsReturnsErrSchemaOutOfDateForMissingDB -v`
Expected: PASS.

- [ ] **Step 5: Update existing storeopen tests for the three-value signature**

Modify the four existing tests in `internal/db/storeopen/storeopen_test.go` (`TestOpenBarePathReturnsWorkingSQLiteStore`, `TestOpenSQLiteSchemeReturnsWorkingStore`, `TestOpenPostgresReturnsRedactedNotAvailableError`, `TestOpenUnknownSchemeErrors`) to assign three return values. Where the existing tests open a bare path expecting it to bootstrap, they now must pass `db.ApplyMigrations()`. Example:

```go
func TestOpenBarePathReturnsWorkingSQLiteStore(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	s, _, err := storeopen.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"), db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
}

func TestOpenSQLiteSchemeReturnsWorkingStore(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	s, _, err := storeopen.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "kata.db"), db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
}

func TestOpenPostgresReturnsRedactedNotAvailableError(t *testing.T) {
	dsn := "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require" //nolint:gosec // fixture proves the password is redacted in the error
	_, _, err := storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres")
	assert.Contains(t, err.Error(), "not yet available")
	assert.NotContains(t, err.Error(), "SECRET")
	assert.Contains(t, dsn, "SECRET")
}

func TestOpenUnknownSchemeErrors(t *testing.T) {
	_, _, err := storeopen.Open(context.Background(), "mysql://h/db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}
```

- [ ] **Step 6: Add the remaining spec-mandated storeopen tests**

Append to `internal/db/storeopen/storeopen_test.go`:

```go
func TestOpenWithApplyMigrationsCreatesAndMigratesFreshDB(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "kata.db")

	s, result, err := storeopen.Open(context.Background(), path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Fresh DB: created and brought to current.
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)

	// The handle is usable.
	_, err = s.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
}

func TestOpenWithApplyMigrationsRunsCutoverThenMigrate(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	// Build a pre-cutover-threshold source DB by reusing sqlitestore.Open's
	// bootstrap path, then re-stamping meta.schema_version to a value below
	// jsonlCutoverThreshold so the orchestrator routes through cutover.
	stageLegacyPreCutoverFixture(t, path, 9)

	s, _, err := storeopen.Open(ctx, path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

// stageLegacyPreCutoverFixture creates a real kata-shaped SQLite DB at path
// and rewrites meta.schema_version to a pre-cutover-threshold value so
// jsonl.AutoCutover treats it as legacy. The bootstrap path here gives us all
// the tables AutoCutover's export step expects without hand-writing ~30 table
// CREATE statements. (In Task 4's state bootstrap still runs in
// sqlitestore.Open; Task 5 reroutes this fixture through Migrate without
// changing the contract.)
func stageLegacyPreCutoverFixture(t *testing.T, path string, version int) {
	t.Helper()
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='schema_version'`, strconv.Itoa(version))
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

func TestOpenWithApplyMigrationsRejectsNewerThanBinary(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	stageNewerThanBinaryFixture(t, path)

	_, _, err := storeopen.Open(ctx, path, db.ApplyMigrations())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
	assert.NotContains(t, err.Error(), "kata migrate")
}

func TestOpenWithoutApplyMigrationsRejectsNewerThanBinary(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	stageNewerThanBinaryFixture(t, path)

	_, _, err := storeopen.Open(ctx, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
	assert.NotContains(t, err.Error(), "kata migrate")
}

// stageNewerThanBinaryFixture creates a kata-shaped DB at path and rewrites
// meta.schema_version to a value above db.CurrentSchemaVersion() — the
// "newer DB written by a newer binary" case. Both Open and OpenReadOnly must
// refuse it with a distinct error (not ErrSchemaOutOfDate / "kata migrate").
func stageNewerThanBinaryFixture(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='schema_version'`,
		strconv.Itoa(db.CurrentSchemaVersion()+1))
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

func TestOpenReadOnlyOnStaleReturnsErrSchemaOutOfDate(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "kata.db")
	stageLegacyPreCutoverFixture(t, path, 9)

	_, _, err := storeopen.OpenReadOnly(context.Background(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrSchemaOutOfDate), err)
}

func TestOpenReadOnlyOnCurrentDropsApplyMigrationsSilently(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	// Stand up a v10 DB via storeopen + ApplyMigrations.
	s, _, err := storeopen.Open(ctx, path, db.ApplyMigrations())
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Reopen read-only with ApplyMigrations — the option is silently dropped.
	ro, result, err := storeopen.OpenReadOnly(ctx, path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	assert.Equal(t, db.MigrationResult{}, result)
}
```

Imports needed for the appended tests: `"strconv"` (for the fixture helpers) plus whichever of `"context"`, `"path/filepath"`, `"errors"`, `"go.kenn.io/kata/internal/db"`, `"go.kenn.io/kata/internal/db/storeopen"`, `"go.kenn.io/kata/internal/db/sqlitestore"` aren't already in the file's import block. `"database/sql"` and `_ "modernc.org/sqlite"` are no longer needed — the fixture helpers route through `sqlitestore.Open` so the driver registration comes through the existing import chain.

- [ ] **Step 7: Update `cmd/kata/daemon_cmd.go` to collapse the pre-open dance**

Modify `cmd/kata/daemon_cmd.go`. Replace lines 293-298 (the `if ver, err := sqlitestore.PeekSchemaVersion(...)` block and the immediately following `storeopen.Open`) with:

```go
	store, _, err := storeopen.Open(ctx, dbPath, db.ApplyMigrations())
	if err != nil {
		return err
	}
```

Remove the now-unused imports from this file: `"go.kenn.io/kata/internal/db/sqlitestore"` and `"go.kenn.io/kata/internal/jsonl"`. Keep `"go.kenn.io/kata/internal/db"` (still used for `db.ApplyMigrations()` and downstream `db.Storage` references).

- [ ] **Step 8: Update `cmd/kata/export.go` and `cmd/kata/import.go` for the three-value signature**

In `cmd/kata/export.go:43`, change:

```go
d, err := storeopen.Open(ctx, dbPath)
```

to:

```go
d, _, err := storeopen.Open(ctx, dbPath)
```

`kata export` reads from an existing kata DB, so the no-`ApplyMigrations` form is correct: a missing or stale DB surfaces `db.ErrSchemaOutOfDate` and the user is told to `kata migrate` first.

In `cmd/kata/import.go:125`, change:

```go
d, err := storeopen.Open(cmd.Context(), target)
```

to:

```go
d, _, err := storeopen.Open(cmd.Context(), target, db.ApplyMigrations())
```

`kata import` is the documented way to write into a fresh target database (the path may not exist when `import --target` runs, or `--force` may have just removed it). Without `ApplyMigrations()` the new orchestrator would reject the missing/empty file with `ErrSchemaOutOfDate`; with it, the target gets created and stamped to the current schema before `jsonl.Import` runs against it. The third return value (the `MigrationResult`) is discarded — `kata import` doesn't surface migration output in its current contract.

Add `"go.kenn.io/kata/internal/db"` to `cmd/kata/import.go`'s imports if not already present.

- [ ] **Step 9: Adapt `cmd/kata/daemon_cutover_test.go`**

This test currently asserts the daemon runs its own `AutoCutover` before Open. After Task 4 the daemon's pre-open dance is gone — `storeopen.Open` runs cutover internally. Update the test's intent to assert daemon startup still upgrades a pre-v10 DB end-to-end (the cutover is now invisible to the daemon).

Modify `cmd/kata/daemon_cutover_test.go`:

- Rename the test from `TestDaemonStartRunsAutoCutoverBeforeOpen` to `TestDaemonStartUpgradesLegacyDBThroughStoreopen`.
- Remove imports of `sqlitestore` and `jsonl` if they were only used for the pre-open assertion.
- Replace the assertion that AutoCutover happens by calling `sqlitestore.PeekSchemaVersion` directly with one that asserts the post-startup DB reports `db.CurrentSchemaVersion()` via `storeopen.OpenReadOnly`. Concretely the test sets up a legacy DB at `dbPath`, runs the daemon binary briefly (existing harness), shuts it down, then opens read-only and checks `SchemaVersion`.

If the existing test was a unit test that called `daemon.run` (or equivalent) rather than spawning a subprocess, simpler: assert that after the daemon's startup function returns, peek/SchemaVersion equals `db.CurrentSchemaVersion()` and the original schema_version=1 marker is gone (cutover renamed the source to a `.bak.*` file).

- [ ] **Step 10: Run the full test suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 11: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 12: Commit**

```bash
git add internal/db/storeopen/storeopen.go internal/db/storeopen/storeopen_test.go cmd/kata/daemon_cmd.go cmd/kata/export.go cmd/kata/import.go cmd/kata/daemon_cutover_test.go
git commit -m "feat(storeopen): ApplyMigrations orchestration and 3-value Open; daemon collapses pre-open dance"
```

---

## Task 5: Remove `bootstrap()` from `sqlitestore.Open`; cutover and test helpers call `Migrate`; delete `ErrSchemaCutoverRequired`

The runner is now the only DDL path. `sqlitestore.Open` returns a handle to whatever the file is. `internal/jsonl/cutover.go` calls `target.Migrate` to bring its empty target up to v10. Test helpers in `sqlitestore_test` do the same.

**Files:**
- Modify: `internal/db/sqlitestore/store.go` (remove `bootstrap`, simplify `openReadWrite`, keep instance_uid cache fast-path)
- Modify: `internal/jsonl/cutover.go` (call `target.Migrate` after Open in `importCutoverTarget`)
- Modify: `internal/db/sqlitestore/testhelpers_test.go` (call `Migrate` after Open in `openTestDBWithPath`)
- Modify: `internal/db/sqlitestore/db_test.go` (rewrite `TestOpen_*` for the new contract; drop the `ErrSchemaCutoverRequired` test)
- Modify: `internal/db/errors.go` (delete `ErrSchemaCutoverRequired`)

- [ ] **Step 1: Write the failing fresh-DB Migrate test**

Append to `internal/db/sqlitestore/migrate_test.go`:

```go
func TestMigrate_FreshDBAppliesBaseline(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	result, err := d.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	assert.Equal(t, []int{db.CurrentSchemaVersion()}, result.Applied)

	// The baseline tables now exist.
	var n int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projects'`).Scan(&n))
	assert.Equal(t, 1, n)

	// instance_uid was stamped.
	assert.NotEmpty(t, d.InstanceUID())
}
```

- [ ] **Step 2: Run it to verify it currently FAILS because bootstrap is still active**

Run: `go test ./internal/db/sqlitestore/... -run TestMigrate_FreshDBAppliesBaseline -v`
Expected: FAIL. `sqlitestore.Open` currently calls `bootstrap()` which stamps schema_version=10 before `Migrate` runs; the assertion `From: 0` will fail.

- [ ] **Step 3: Remove `bootstrap` from `Store.Open` and split instance_uid handling**

Modify `internal/db/sqlitestore/store.go`:

1. In `openReadWrite`, remove the `d.bootstrap(ctx)` call and the surrounding error-handling block. Replace the `d.ensureInstanceUID(ctx)` call with `d.cacheInstanceUIDIfPresent(ctx)`. After edits, `openReadWrite` body looks like:

```go
func openReadWrite(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
		path,
	)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	d := &Store{DB: sdb, path: path}
	if err := d.cacheInstanceUIDIfPresent(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return d, nil
}
```

2. Delete the `bootstrap` method entirely (around lines 185-225 in the current file).

3. Replace `ensureInstanceUID` with `cacheInstanceUIDIfPresent`. The new method is a non-mutating cache fill:

```go
// cacheInstanceUIDIfPresent reads meta.instance_uid into d.instanceUID when
// the meta table exists and the row is populated. On a fresh DB (no meta
// table) or a DB whose Migrate has not yet run, the cache stays empty and
// the handle defers stamping to Migrate.
func (d *Store) cacheInstanceUIDIfPresent(ctx context.Context) error {
	exists, err := d.tableExists(ctx, "meta")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var v string
	err = d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	d.instanceUID = v
	return nil
}
```

4. Update the doc comment on `Open` to reflect the new contract — `Open` returns a raw handle; callers run `Migrate` explicitly to apply DDL.

- [ ] **Step 4: Update `jsonl/cutover.go` to call `target.Migrate` after Open**

Modify `internal/jsonl/cutover.go`'s `importCutoverTarget`. After `target, err := sqlitestore.Open(ctx, tmpDB)` and its error check, insert:

```go
if _, err := target.Migrate(ctx); err != nil {
	return fmt.Errorf("migrate cutover target: %w", err)
}
```

The deferred `target.Close()` already covers cleanup if Migrate fails.

- [ ] **Step 5: Update the sqlitestore test helper to call Migrate**

Modify `internal/db/sqlitestore/testhelpers_test.go`. In `openTestDBWithPath`, after `sqlitestore.Open` succeeds, add:

```go
	if _, err := d.Migrate(context.Background()); err != nil {
		_ = d.Close()
		t.Fatalf("migrate test db: %v", err)
	}
```

Set `KATA_HOME` at the top of each helper so the snapshot path resolves into a temp dir (otherwise tests would write snapshots under the user's real `~/.kata/runtime/`):

```go
func openTestDB(t *testing.T) *sqlitestore.Store {
	t.Helper()
	d, _ := openTestDBWithPath(t)
	return d
}

func openTestDBWithPath(t *testing.T) (*sqlitestore.Store, string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(context.Background()); err != nil {
		_ = d.Close()
		t.Fatalf("migrate test db: %v", err)
	}
	return d, path
}
```

(Calling `t.Setenv` only in `openTestDBWithPath` is sufficient — `openTestDB` delegates to it, so the env is set in the same goroutine before any sqlite work begins.)

- [ ] **Step 6: Rewrite `internal/db/sqlitestore/db_test.go` for the new contract**

Replace the file with:

```go
package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
)

func TestOpen_AppliesPragmas(t *testing.T) {
	d := openTestDB(t)
	var fk int
	require.NoError(t, d.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk)
	var mode string
	require.NoError(t, d.QueryRow("PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode)
}

func TestOpen_OnFreshDBLeavesMetaUnpopulated(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	// The contract: Open does no DDL. No meta table exists yet.
	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&n))
	assert.Equal(t, 0, n)
}

func TestOpen_IsIdempotentAfterMigrate(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	d1, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = d1.Migrate(ctx)
	require.NoError(t, err)
	require.NoError(t, d1.Close())

	d2, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d2.Close() })

	// Reopen reads the existing schema_version from meta into a cached UID.
	v, err := d2.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
	assert.NotEmpty(t, d2.InstanceUID())
}

func TestSchema_IssuesHasShortIDColumn(t *testing.T) {
	d := openTestDB(t)
	var typ string
	err := d.QueryRow(
		`SELECT type FROM pragma_table_info('issues') WHERE name='short_id'`,
	).Scan(&typ)
	require.NoError(t, err)
	assert.Equal(t, "TEXT", typ)
}

func TestSchema_IssuesNumberColumnGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('issues') WHERE name='number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_ProjectsNextIssueNumberGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name='next_issue_number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_EventsIssueNumberGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name='issue_number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_PurgeLogIssueNumberGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('purge_log') WHERE name='issue_number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_ProjectNameRejectsHash(t *testing.T) {
	d := openTestDB(t)
	_, err := d.Exec(
		`INSERT INTO projects(uid, name) VALUES('01HZNQ7VFPK1XGD8R5MABCD4EX', 'has#hash')`,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHECK")
}

func TestOpen_TimestampColumnsScanIntoTime(t *testing.T) {
	d := openTestDB(t)
	projectUID, err := uid.New()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO projects(uid, name) VALUES(?,'x')`, projectUID)
	require.NoError(t, err)
	rows, err := d.Query(`SELECT created_at FROM projects`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var ts interface{}
	require.NoError(t, rows.Scan(&ts))
	_, ok := ts.(interface{ Year() int })
	assert.True(t, ok, "expected time.Time, got %T", ts)
}

func TestSchemaVersion(t *testing.T) {
	ctx := context.Background()
	openMigrated := func(t *testing.T) *sqlitestore.Store {
		return openTestDB(t)
	}

	t.Run("at-current DB reports the current version", func(t *testing.T) {
		d := openMigrated(t)
		v, err := d.SchemaVersion(ctx)
		require.NoError(t, err)
		require.Equal(t, db.CurrentSchemaVersion(), v)
	})

	t.Run("reads the value stored in meta, not a constant", func(t *testing.T) {
		d := openMigrated(t)
		_, err := d.ExecContext(ctx, `UPDATE meta SET value = '424242' WHERE key = 'schema_version'`)
		require.NoError(t, err)
		v, err := d.SchemaVersion(ctx)
		require.NoError(t, err)
		require.Equal(t, 424242, v)
	})

	t.Run("errors when the row is absent", func(t *testing.T) {
		d := openMigrated(t)
		_, err := d.ExecContext(ctx, `DELETE FROM meta WHERE key = 'schema_version'`)
		require.NoError(t, err)
		_, err = d.SchemaVersion(ctx)
		require.Error(t, err)
	})

	t.Run("errors when the value is unparseable", func(t *testing.T) {
		d := openMigrated(t)
		_, err := d.ExecContext(ctx, `UPDATE meta SET value = 'not-a-number' WHERE key = 'schema_version'`)
		require.NoError(t, err)
		_, err = d.SchemaVersion(ctx)
		require.Error(t, err)
	})
}
```

(The old `TestOpen_AppliesPragmasAndMigrations` is split into `TestOpen_AppliesPragmas` + `TestOpen_OnFreshDBLeavesMetaUnpopulated`. `TestOpen_RecordsCurrentSchemaVersion` is dropped because `TestMigrate_FreshDBAppliesBaseline` already covers it. `TestOpen_RejectsOlderSchemaNeedingJSONLCutover` is dropped because the orchestration moved to `storeopen`.)

- [ ] **Step 7: Delete `ErrSchemaCutoverRequired`**

Modify `internal/db/errors.go`. Delete the doc comment and `var ErrSchemaCutoverRequired = errors.New("schema cutover required")` block (the entry added in Task 1's Step 5 plus the original from the pre-Phase-2 file).

- [ ] **Step 8: Verify no orphans**

Run: `rg -n 'ErrSchemaCutoverRequired' --glob '!docs/**'`
Expected: zero hits.

Run: `rg -n 'bootstrap\b' internal/db/sqlitestore/`
Expected: zero hits (or only in comments that should be removed too).

- [ ] **Step 9: Run the full test suite**

Run: `go build ./... && go test ./...`
Expected: PASS — `TestMigrate_FreshDBAppliesBaseline` now passes (bootstrap removed); cutover continues to work via `target.Migrate`; every other test that previously relied on bootstrap-via-Open now goes through `openTestDB` which calls Migrate.

- [ ] **Step 10: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/db/sqlitestore/store.go internal/db/sqlitestore/migrate_test.go internal/db/sqlitestore/testhelpers_test.go internal/db/sqlitestore/db_test.go internal/jsonl/cutover.go internal/db/errors.go
git commit -m "feat(sqlitestore): remove bootstrap; Migrate owns all DDL; delete ErrSchemaCutoverRequired"
```

---

## Task 6: `kata migrate` CLI

Surface the runner at the command line. Behavior per spec: resolves `config.KataDB()`, calls `storeopen.Open(ctx, dbPath, db.ApplyMigrations())`, prints one line per applied version plus a summary, exits 0 on success and 1 on error.

**Files:**
- Create: `cmd/kata/migrate.go`
- Create: `cmd/kata/migrate_test.go`
- Modify: `cmd/kata/main.go` (register `newMigrateCmd()` in the `subs` list)

- [ ] **Step 1: Write the failing CLI test**

Create `cmd/kata/migrate_test.go`:

```go
package main

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestMigrateCmd_BringsFreshDBToCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)

	out, code := runKata(t, "migrate")
	assert.Equal(t, 0, code, "stdout: %s", out)
	assert.Contains(t, out, "applied schema_version "+strconv.Itoa(db.CurrentSchemaVersion()))
	assert.Contains(t, out, "migrated from 0 to "+strconv.Itoa(db.CurrentSchemaVersion()))

	ctx := context.Background()
	dbPath := filepath.Join(home, "kata.db")
	v, err := sqlitestore.PeekSchemaVersion(ctx, dbPath)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

func TestMigrateCmd_OnCurrentDBReportsAlreadyCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)

	// First invocation brings the DB to current.
	_, code := runKata(t, "migrate")
	require.Equal(t, 0, code)

	// Second invocation is a no-op.
	out, code := runKata(t, "migrate")
	assert.Equal(t, 0, code, "stdout: %s", out)
	assert.Contains(t, out, "already current")
	assert.NotContains(t, out, "applied schema_version")
}
```

Add a `runKata` helper that builds the test binary once and invokes it with the given args. If the project already has such a helper (look in `cmd/kata/main_test.go`), reuse it. Otherwise stub:

```go
func runKata(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetArgs(args)
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.ExecuteContext(context.Background())
	code := 0
	if err != nil {
		code = 1
	}
	return buf.String(), code
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/kata/... -run TestMigrateCmd -v`
Expected: FAIL — `kata migrate` doesn't exist yet.

- [ ] **Step 3: Implement the command**

Create `cmd/kata/migrate.go`:

```go
package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/storeopen"
)

func newMigrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "migrate",
		Short:         "bring the kata database up to the current schema",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			dbPath, err := config.KataDB()
			if err != nil {
				return err
			}
			store, result, err := storeopen.Open(ctx, dbPath, db.ApplyMigrations())
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			out := cmd.OutOrStdout()
			for _, v := range result.Applied {
				fmt.Fprintf(out, "applied schema_version %d\n", v)
			}
			if len(result.Applied) == 0 {
				fmt.Fprintf(out, "already current (schema_version %d)\n", result.To)
			} else {
				fmt.Fprintf(out, "migrated from %d to %d (%d versions applied)\n",
					result.From, result.To, len(result.Applied))
			}
			return nil
		},
	}
	return cmd
}
```

- [ ] **Step 4: Register the command in the root**

Modify `cmd/kata/main.go`. In the `subs := []*cobra.Command{...}` slice (around line 79), insert `newMigrateCmd(),` immediately after `newImportCmd(),` (keeping the existing alphabetical/topical grouping):

```go
		newExportCmd(),
		newImportCmd(),
		newMigrateCmd(),
		newDigestCmd(),
```

- [ ] **Step 5: Run the CLI tests to verify they pass**

Run: `go test ./cmd/kata/... -run TestMigrateCmd -v`
Expected: PASS (both cases).

- [ ] **Step 6: Run the full test suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 7: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 8: Verify the spec's success criteria one last time**

Run each check:

```bash
rg -n 'ErrSchemaCutoverRequired' --glob '!docs/**'
# expect: zero hits

rg -n '\bbootstrap\b' internal/db/sqlitestore/store.go
# expect: zero hits

go test ./...
nix run 'nixpkgs#golangci-lint' -- run ./...
```

All should report success.

- [ ] **Step 9: Commit**

```bash
git add cmd/kata/migrate.go cmd/kata/migrate_test.go cmd/kata/main.go
git commit -m "feat(cli): kata migrate brings the database up to current schema"
```

---

## Verification: end-to-end

After Task 6 the spec's full surface is in place. A final sanity check the implementer can run by hand:

```bash
export KATA_HOME=$(mktemp -d)
go run ./cmd/kata migrate
# applied schema_version 10
# migrated from 0 to 10 (1 versions applied)
go run ./cmd/kata migrate
# already current (schema_version 10)
```

The snapshot directory `$KATA_HOME/runtime/<dbhash>/` should be empty after a successful run (snapshot deleted on commit).

If the user runs `kata` commands without first running `migrate`:

```bash
export KATA_HOME=$(mktemp -d)
go run ./cmd/kata export
# Error: schema out of date: database does not exist at <path>; run kata migrate
```

That message is the entire contract the spec sets up.
