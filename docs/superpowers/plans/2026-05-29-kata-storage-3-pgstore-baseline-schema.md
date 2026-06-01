# Kata Storage Phase 3 — pgstore Baseline Schema Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the `pgstore` package — Postgres backend that satisfies `db.Storage` enough to compile and drive `Migrate` on a fresh PG instance, with a v10 baseline schema semantically equivalent to today's SQLite, an 0011 placeholder keeping the ladder in lockstep, a SQLite-side 0011 migration adding a UNIQUE partial index on `events.idempotency_key`, and a storeopen dispatcher that routes `postgres://` DSNs to pgstore.

**Architecture:** `internal/db/pgstore` mirrors `internal/db/sqlitestore`'s package shape (`store.go`, `open.go`, `migrate.go`, `migrations/`, test seam). Domain methods are stubbed (return "not implemented in Phase 3" errors) so `*pgstore.Store` satisfies `db.Storage` — queries arrive in Phase 4. Migration uses `pg_advisory_xact_lock` (vs sqlitestore's pinned-`Conn` `BEGIN IMMEDIATE`) and the existing embedded-ladder machinery. Testcontainers provides a real PG for tests; `testing.Short()` skips them on `-short`.

**Tech Stack:** Go 1.26, `github.com/jackc/pgx/v5/stdlib` (database/sql wrapper for pgx), `github.com/testcontainers/testcontainers-go` (PG-17 container), `embed` + `io/fs` for the migration ladder (same as sqlitestore), `github.com/stretchr/testify` for tests, nix-only tooling (`nix run 'nixpkgs#golangci-lint' -- run ./...`).

---

## File structure

**Create:**
- `internal/testenv/pgenv.go` — testcontainers helper: spins up `postgres:17-alpine`, returns the DSN string, registers cleanup.
- `internal/db/pgstore/store.go` — `Store` struct, identity/lifecycle methods (Path, Close, InstanceUID, RefreshInstanceUID, SchemaVersion, RetryTransient), `PeekSchemaVersion` (package-level helper for storeopen's no-ApplyMigrations PG branch).
- `internal/db/pgstore/open.go` — `Open(ctx, dsn, opts...)` constructor: opens via pgx stdlib, configures connection pool defaults, returns `*Store`.
- `internal/db/pgstore/migrate.go` — `Storage.Migrate(ctx)` implementation mirroring sqlitestore: advisory lock, transactional apply loop, entry guards, ladder validation, instance_uid handling. Helpers (`listPendingMigrations`, `connCurrentVersion`, `ensureInstanceUIDFromMeta`, `ensureInstanceUIDOnConn`) duplicated from sqlitestore for now (per spec — lifting is a follow-up).
- `internal/db/pgstore/stubs.go` — domain-method stubs (all 113 non-identity-lifecycle methods of `db.Storage`) returning a sentinel "not implemented in Phase 3" error so `*Store` satisfies the interface.
- `internal/db/pgstore/migrations/0010_baseline.sql` — full Postgres baseline schema port per the spec's §5 translation tables: extensions, text search config, tables in FK-dependency order, indexes (including the new `idx_events_idempotency_uniq` UNIQUE partial index), 3 PL/pgSQL trigger functions + 6 triggers, FTS via `issues_search` table + `rebuild_issue_search` function + 4 sync triggers.
- `internal/db/pgstore/migrations/0011_idempotency_unique.sql` — placeholder file. Body: a comment explaining "UNIQUE index already in 0010 for PG; this file exists only to keep the ladder lockstep with SQLite" + `SELECT 1;`.
- `internal/db/sqlitestore/migrations/0011_idempotency_unique.sql` — real SQLite migration. Adds `CREATE UNIQUE INDEX idx_events_idempotency_uniq ON events(project_id, json_extract(payload, '$.idempotency_key')) WHERE type='issue.created' AND json_extract(payload, '$.idempotency_key') IS NOT NULL;`. Header comment lists the pre-migration duplicate-detection query.
- `internal/db/pgstore/migrations_export_test.go` — `SetMigrationsSource(fs.FS) func()` and `EmbeddedMigrationsFS() fs.FS` (mirrors sqlitestore's `migrations_export_test.go`).
- `internal/db/pgstore/migrate_test.go` — pgstore migrate tests mirroring sqlitestore's: at-current noop, fresh-DB applies baseline, synthetic v12, rollback, concurrent, pre-baseline reject, newer-than-binary reject, duplicate-ladder, ladder-gap, above-binary-cap, instance_uid repair. All guarded by `testing.Short()` skip.
- `internal/db/pgstore/schema_test.go` — schema acceptance test: spin up PG, run Migrate, assert the expected tables, named triggers, idempotency UNIQUE index, FTS config + functions, and per-table FK count. Exact constraint/index name parity is deferred to the Phase 6 conformance suite.

**Modify:**
- `go.mod` / `go.sum` — add `github.com/jackc/pgx/v5` and `github.com/testcontainers/testcontainers-go` (plus their transitive deps).
- `internal/db/schema_version.go` — bump `currentSchemaVersion` initializer from `BaselineSchemaVersion` (10) to `11`.
- `internal/db/sqlitestore/migrate_test.go` — update tests affected by the bump: `TestMigrate_FreshDBAppliesBaseline` (Applied/To values change), the synthetic-version tests (use 12 instead of 11), `TestMigrate_RejectsMigrationAboveBinarySchema` (use 0012).
- `internal/db/storeopen/storeopen.go` — replace the postgres "not yet available" return with a real dispatch: `openPostgresWithMigrations` (ApplyMigrations branch) and `openPostgresRefusingStale` (no-ApplyMigrations branch); both use pgstore.
- `internal/db/storeopen/storeopen_test.go` — replace `TestOpenPostgresReturnsRedactedNotAvailableError` with a new test asserting dispatch reaches pgstore (which returns a real connection error for unreachable hosts).

---

## Task ordering rationale

Tasks land in dependency order so every commit builds + tests pass:

1. **Task 1** — deps + testcontainers helper. Brings new packages into go.mod, adds the test fixture every subsequent task uses. Builds + the helper smoke-tests itself.
2. **Task 2** — pgstore package shell. Adds Store/Open/Close/etc. methods but no domain logic. Compiles; Open against a testcontainer works.
3. **Task 3** — pgstore Migrate + 0010 baseline schema + initial fresh-DB migrate test. The big bring-up: applying 0010 produces a real v10-shaped PG schema. Domain methods aren't on Storage yet; the package compiles standalone.
4. **Task 4** — pgstore domain method stubs. Adds the 113 stubs so `*pgstore.Store` satisfies `db.Storage`. Compiles; `var _ db.Storage = (*Store)(nil)` interface assertion passes.
5. **Task 5** — pgstore schema acceptance test. Asserts the high-value structural surface (tables, named triggers, idempotency UNIQUE, FTS config + functions, per-table FK counts) against the real PG. Full constraint/index name parity lands in Phase 6 conformance.
6. **Task 6** — bump CurrentSchemaVersion 10→11, add sqlitestore 0011 + pgstore 0011 placeholder, update affected SQLite tests. The coordinated breaking change.
7. **Task 7** — storeopen dispatch for postgres. Wires pgstore into the orchestrator.
8. **Task 8** — pgstore comprehensive migrate tests (synthetic, rollback, concurrent, guards, ladder validation, instance_uid repair). Pgstore-side parity with sqlitestore.

---

## Task 1: Add dependencies and testcontainers helper

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/testenv/pgenv.go`
- Create: `internal/testenv/pgenv_test.go`

- [ ] **Step 1: Add the new dependencies via `go get`**

One runtime dependency (`pgx/v5` — drives pgstore in production) plus two test-only dependencies (`testcontainers-go` + its `modules/postgres`, used only by the test harness):

```bash
# Runtime:
go get github.com/jackc/pgx/v5@latest
# Test-only (testcontainers harness for pgstore tests + Phase 6 conformance):
go get github.com/testcontainers/testcontainers-go@latest
go get github.com/testcontainers/testcontainers-go/modules/postgres@latest
go mod tidy
```

Expected: `go.mod` gains the three lines plus any transitive deps; `go.sum` updates. No build breakage.

- [ ] **Step 2: Verify the deps are downloadable and the project still builds**

Run: `go build ./...`
Expected: clean (no errors).

- [ ] **Step 3: Write the failing test for `pgenv.NewPostgresContainer`**

Create `internal/testenv/pgenv_test.go`:

```go
package testenv_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestNewPostgresContainerReturnsUsableDSN(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var one int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	require.Equal(t, 1, one)
}
```

- [ ] **Step 4: Run the failing test to confirm `NewPostgresContainer` is undefined**

Run: `go test ./internal/testenv/... -run TestNewPostgresContainerReturnsUsableDSN -v`
Expected: FAIL — `testenv.NewPostgresContainer` undefined.

- [ ] **Step 5: Implement `internal/testenv/pgenv.go`**

Create `internal/testenv/pgenv.go`:

```go
// Package testenv extensions for Postgres testcontainers.
// pgenv.go provides NewPostgresContainer which starts postgres:17-alpine,
// returns the DSN string, and a cleanup function. The container lives for the
// test's lifetime; cleanup tears it down. Used by pgstore tests; expanded by
// Phase 4 conformance tests.

package testenv

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NewPostgresContainer starts a postgres:17-alpine container, waits for it to
// become ready, and returns the DSN string plus a cleanup function. Callers
// must register the cleanup via t.Cleanup themselves so test ordering stays
// predictable.
func NewPostgresContainer(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	container, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("kata_test"),
		postgres.WithUsername("kata"),
		postgres.WithPassword("kata"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatalf("get postgres connection string: %v", err)
	}
	cleanup := func() {
		_ = container.Terminate(context.Background())
	}
	return dsn, cleanup
}
```

- [ ] **Step 6: Run the test to confirm it passes**

Run: `go test ./internal/testenv/... -run TestNewPostgresContainerReturnsUsableDSN -v`
Expected: PASS (the container starts, `SELECT 1` returns 1).

- [ ] **Step 7: Confirm `-short` skips the test**

Run: `go test -short ./internal/testenv/... -run TestNewPostgresContainerReturnsUsableDSN -v`
Expected: SKIP.

- [ ] **Step 8: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues.

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum internal/testenv/pgenv.go internal/testenv/pgenv_test.go
git commit -m "feat(testenv): add Postgres testcontainers helper for pgstore tests"
```

---

## Task 2: pgstore package shell — Store, Open, lifecycle methods

**Files:**
- Create: `internal/db/pgstore/store.go`
- Create: `internal/db/pgstore/open.go`
- Create: `internal/db/pgstore/store_test.go`

- [ ] **Step 1: Write the failing test for `pgstore.Open`**

Create `internal/db/pgstore/store_test.go`:

```go
package pgstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestOpen_ConnectsAndReturnsStore(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Connectivity check via the Store's underlying pool.
	require.NoError(t, store.PingContext(ctx))
}

func TestPeekSchemaVersion_ReturnsZeroForFreshDB(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	v, err := pgstore.PeekSchemaVersion(ctx, dsn)
	require.NoError(t, err)
	require.Equal(t, 0, v)
}
```

- [ ] **Step 2: Run the failing test to confirm pgstore.Open is undefined**

Run: `go test ./internal/db/pgstore/... -run TestOpen -v`
Expected: FAIL — `pgstore` package undefined or `pgstore.Open` undefined.

- [ ] **Step 3: Implement `internal/db/pgstore/store.go`**

Create `internal/db/pgstore/store.go`. The interface assertion `var _ db.Storage = (*Store)(nil)` is intentionally COMMENTED OUT in this task — the domain-method stubs land in Task 4, and an uncommented assertion would block the package from compiling here. Task 4 uncomments it once the stubs exist.

```go
// Package pgstore is the Postgres-backed implementation of db.Storage.
// Open opens via pgx's database/sql wrapper, applies sensible per-connection
// runtime params, and returns a *Store. Migrate brings the schema up to
// db.CurrentSchemaVersion() via the embedded migration ladder.
//
// Domain methods are stubbed in stubs.go for Phase 3 — queries land in
// Phase 4 (see docs/superpowers/specs/2026-05-26-kata-postgres-backend.md §10).
package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx driver as "pgx"

	"go.kenn.io/kata/internal/db"
)

// NOTE: the embedded migrations FS (`//go:embed migrations/*.sql` + the
// `migrationsFS` / `migrationsSource` variables) lives in migrate.go, which
// Task 3 creates alongside the migrations/ directory. Declaring it here would
// fail Task 2's `go build` because the migrations directory doesn't exist yet.

// Store wraps a pgx-backed *sql.DB. Use Open to construct one with pool +
// runtime-param defaults applied.
type Store struct {
	*sql.DB
	dsn         string
	instanceUID string
	readOnly    bool
}

// Interface assertion is enabled in Task 4 once the domain-method stubs land.
// Leaving it on now would block compilation: *Store only has the seven
// identity/lifecycle methods so far.
// var _ db.Storage = (*Store)(nil)

// Path returns the DSN (credential-free identity, via config.CanonicalDSNIdentity).
// Mirrors sqlitestore.Store.Path which returns the filesystem path.
func (s *Store) Path() string { return s.dsn }

// InstanceUID returns the cached meta.instance_uid value, populated on Open
// when the meta table exists (post-Migrate). Empty if Open ran on a fresh DB
// before Migrate stamped instance_uid.
func (s *Store) InstanceUID() string { return s.instanceUID }

// RefreshInstanceUID re-reads meta.instance_uid into the cached field after a
// jsonl.Import has overwritten it. Mirrors sqlitestore's contract.
func (s *Store) RefreshInstanceUID(ctx context.Context) error {
	var v string
	if err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v); err != nil {
		return fmt.Errorf("refresh instance_uid: %w", err)
	}
	s.instanceUID = v
	return nil
}

// SchemaVersion reads meta.schema_version. Errors when missing/unparseable.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v string
	if err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", v, err)
	}
	return n, nil
}

// RetryTransient currently does no retries — pgstore's transient-error set is
// out of Phase 3 scope. Phase 4 plugs in SQLSTATE-based retry (per parent
// spec §4). For now the op runs once and any error propagates.
func (s *Store) RetryTransient(ctx context.Context, op func() error) error {
	return op()
}

// PeekSchemaVersion opens the DSN read-only, reads meta.schema_version (or
// returns 0 if the meta table doesn't exist), and closes the handle. Used by
// storeopen's no-ApplyMigrations PG branch.
func PeekSchemaVersion(ctx context.Context, dsn string) (int, error) {
	s, err := Open(ctx, dsn, db.ReadOnly())
	if err != nil {
		return 0, err
	}
	defer func() { _ = s.Close() }()
	return s.currentVersion(ctx)
}

// currentVersion returns 0 when the meta table doesn't exist yet (fresh DB)
// or when the schema_version row is missing.
func (s *Store) currentVersion(ctx context.Context) (int, error) {
	exists, err := s.tableExists(ctx, "meta")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v string
	err = s.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", v, err)
	}
	return n, nil
}

// tableExists checks pg_catalog for a table named `name` in the search_path.
func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := s.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = $1)`, name).Scan(&exists)
	return exists, err
}

// cacheInstanceUIDIfPresent reads meta.instance_uid into s.instanceUID when
// the meta table exists. Used by Open on already-migrated DBs.
func (s *Store) cacheInstanceUIDIfPresent(ctx context.Context) error {
	exists, err := s.tableExists(ctx, "meta")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var v string
	err = s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	s.instanceUID = v
	return nil
}
```

- [ ] **Step 4: Implement `internal/db/pgstore/open.go`**

Create `internal/db/pgstore/open.go`:

```go
package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"go.kenn.io/kata/internal/db"
)

// Default connection pool sizing. Tunable via DSN query params in a future
// phase; conservative for v1 single-daemon deployments.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxIdleTime = 5 * time.Minute
)

// Open opens a PG connection pool against dsn using pgx's database/sql wrapper.
// Per-connection runtime params (application_name, statement_timeout,
// idle_in_transaction_session_timeout, and — when db.ReadOnly() is set —
// default_transaction_read_only) are injected into the pgx config's
// RuntimeParams so EVERY pooled connection inherits them at startup. Pings to
// verify connectivity. Returns *Store; Migrate must be called explicitly to
// apply schema changes.
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (*Store, error) {
	cfg := db.ApplyOpenOptions(opts...)

	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pgx config: %w", err)
	}
	// RuntimeParams ship to every new connection via the startup packet, so
	// these GUCs are guaranteed on every pooled connection — not just the
	// one that handled an out-of-band SET ExecContext.
	connConfig.RuntimeParams["application_name"] = "kata"
	connConfig.RuntimeParams["statement_timeout"] = "30s"
	connConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60s"
	if cfg.ReadOnly {
		// Pool-wide read-only enforcement: any transaction opened from any
		// pooled connection starts read-only. Without this on RuntimeParams,
		// a one-shot SET on a single connection would leave the rest of the
		// pool able to write.
		connConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}

	connector := stdlib.GetConnector(*connConfig)
	sdb := sql.OpenDB(connector)
	sdb.SetMaxOpenConns(defaultMaxOpenConns)
	sdb.SetMaxIdleConns(defaultMaxIdleConns)
	sdb.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping pgx: %w", err)
	}
	s := &Store{DB: sdb, dsn: dsn, readOnly: cfg.ReadOnly}
	if err := s.cacheInstanceUIDIfPresent(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return s, nil
}
```

- [ ] **Step 5: Run the test to confirm it passes**

Run: `go test ./internal/db/pgstore/... -run TestOpen -v`
Expected: PASS — Open succeeds, ping works, PeekSchemaVersion returns 0 for a fresh DB (no meta table yet).

- [ ] **Step 6: Confirm `-short` skips both tests**

Run: `go test -short ./internal/db/pgstore/...`
Expected: 0 tests run (or all skipped); no failures.

- [ ] **Step 7: Build check**

Run: `go build ./...`
Expected: clean. The `var _ db.Storage = (*Store)(nil)` assertion is intentionally commented out in this task; Task 4 uncomments it after the stubs land.

- [ ] **Step 8: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues.

- [ ] **Step 9: Commit**

```bash
git add internal/db/pgstore/store.go internal/db/pgstore/open.go internal/db/pgstore/store_test.go
git commit -m "feat(pgstore): add Store shell, Open via pgx stdlib, PeekSchemaVersion"
```

---

## Task 3: pgstore.Migrate + 0010 baseline schema + fresh-DB migrate test

This is the bring-up task: implementing Migrate, embedding the baseline schema file, and verifying a fresh PG DB reaches schema_version=10 after one Migrate call.

**Files:**
- Create: `internal/db/pgstore/migrate.go`
- Create: `internal/db/pgstore/migrations/0010_baseline.sql`
- Create: `internal/db/pgstore/migrations_export_test.go`
- Create: `internal/db/pgstore/migrate_test.go`

- [ ] **Step 1: Write the failing fresh-DB migrate test**

Create `internal/db/pgstore/migrate_test.go`:

```go
package pgstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestMigrate_FreshDBAppliesBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	result, err := s.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.From)
	assert.Equal(t, db.BaselineSchemaVersion, result.To)
	assert.Equal(t, []int{db.BaselineSchemaVersion}, result.Applied)

	// meta table exists and schema_version=10.
	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.BaselineSchemaVersion, v)

	// instance_uid was stamped during Migrate.
	assert.NotEmpty(t, s.InstanceUID())
}
```

- [ ] **Step 2: Run the failing test**

Run: `go test ./internal/db/pgstore/... -run TestMigrate_FreshDBAppliesBaseline -v`
Expected: FAIL — `s.Migrate` undefined.

- [ ] **Step 3: Implement `internal/db/pgstore/migrate.go`**

Create `internal/db/pgstore/migrate.go`:

```go
package pgstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsSource is the FS the migration runner reads from. Defaults to the
// embedded migrationsFS; tests override via migrations_export_test.go to inject
// synthetic 12+ files without polluting the on-disk migrations directory.
var migrationsSource fs.FS = migrationsFS

// migrationAdvisoryLockKey is the deterministic int8 advisory-lock key kata's
// PG migration runner takes. Derived from the constant 'kata:migrate' via
// hashtextextended at SQL time; the value here is a documentation anchor only
// — the actual hashing happens in Migrate's SQL.
const migrationAdvisoryLockSeed = "kata:migrate"

// Migrate brings the PG database up to db.CurrentSchemaVersion() by applying
// every pending migration file from migrationsSource in version order. Uses
// pg_advisory_xact_lock to serialize concurrent migrators; transaction-scoped
// (released on COMMIT/ROLLBACK). No snapshot is taken — operators rely on
// pg_dump or base-backup. On any per-step error the transaction rolls back;
// the error wraps the failing file's name.
func (s *Store) Migrate(ctx context.Context) (db.MigrationResult, error) {
	if s.readOnly {
		return db.MigrationResult{}, errors.New("migrate: backend is read-only")
	}

	current, err := s.currentVersion(ctx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version: %w", err)
	}
	if current > 0 && current < db.BaselineSchemaVersion {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d predates the baseline (%d); Postgres has no JSONL-cutover path — restore from operator backup before retrying", current, db.BaselineSchemaVersion)
	}
	if current > db.CurrentSchemaVersion() {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d is newer than binary schema %d; use a newer kata binary", current, db.CurrentSchemaVersion())
	}
	pending, err := listPendingMigrations(migrationsSource, current)
	if err != nil {
		return db.MigrationResult{}, err
	}
	if len(pending) == 0 {
		if err := s.ensureInstanceUIDFromMeta(ctx); err != nil {
			return db.MigrationResult{}, err
		}
		return db.MigrationResult{From: current, To: current, Applied: nil}, nil
	}

	// Apply pending under one transaction, with an advisory lock held for the
	// transaction's lifetime. The lock + transaction together serialize
	// concurrent migrators across processes.
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("begin migration tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		migrationAdvisoryLockSeed); err != nil {
		return db.MigrationResult{}, fmt.Errorf("acquire advisory lock: %w", err)
	}

	// Re-read schema_version inside the lock — another migrator may have
	// advanced it between our peek and the lock acquisition.
	currentInTx, err := txCurrentVersion(ctx, tx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version in tx: %w", err)
	}
	pendingInTx, err := listPendingMigrations(migrationsSource, currentInTx)
	if err != nil {
		return db.MigrationResult{}, err
	}

	applied := make([]int, 0, len(pendingInTx))
	for _, m := range pendingInTx {
		sqlBytes, err := fs.ReadFile(migrationsSource, m.fileName)
		if err != nil {
			return db.MigrationResult{}, fmt.Errorf("read migration %s: %w", m.fileName, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("apply migration %s: %w", m.fileName, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES ('schema_version', $1)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
			strconv.Itoa(m.version)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("stamp schema_version %d: %w", m.version, err)
		}
		applied = append(applied, m.version)
	}

	if err := ensureInstanceUIDInTx(ctx, tx, &s.instanceUID); err != nil {
		return db.MigrationResult{}, fmt.Errorf("ensure instance_uid: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return db.MigrationResult{}, fmt.Errorf("commit migration: %w", err)
	}
	committed = true

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
	var all []pendingMigration
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
		all = append(all, pendingMigration{version: v, fileName: path.Join("migrations", name)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	for i := 1; i < len(all); i++ {
		if all[i].version != all[i-1].version+1 {
			return nil, fmt.Errorf("migration ladder: version %d missing between %d and %d", all[i-1].version+1, all[i-1].version, all[i].version)
		}
	}
	binary := db.CurrentSchemaVersion()
	for _, m := range all {
		if m.version > binary {
			return nil, fmt.Errorf("migration version %d exceeds binary schema_version %d; use a newer kata binary", m.version, binary)
		}
	}
	var pending []pendingMigration
	for _, m := range all {
		if m.version > current {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

func txCurrentVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var exists bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = 'meta')`).Scan(&exists)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// ensureInstanceUIDFromMeta caches meta.instance_uid into s.instanceUID on the
// at-current Migrate path. Repairs a missing row (rare corruption) via
// INSERT ... ON CONFLICT DO NOTHING. Mirrors sqlitestore's repair semantics.
func (s *Store) ensureInstanceUIDFromMeta(ctx context.Context) error {
	if s.instanceUID != "" {
		return nil
	}
	var v string
	err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if err == nil {
		s.instanceUID = v
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	fresh, err := katauid.New()
	if err != nil {
		return fmt.Errorf("generate instance_uid: %w", err)
	}
	if _, err := s.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('instance_uid', $1)
		 ON CONFLICT (key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	s.instanceUID = stored
	return nil
}

// ensureInstanceUIDInTx runs as Migrate's pre-commit step. Reads or seeds the
// instance_uid row inside the active transaction.
func ensureInstanceUIDInTx(ctx context.Context, tx *sql.Tx, cached *string) error {
	var v string
	err := tx.QueryRowContext(ctx,
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
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('instance_uid', $1)
		 ON CONFLICT (key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	*cached = stored
	return nil
}
```

- [ ] **Step 4: Create the test seam**

Create `internal/db/pgstore/migrations_export_test.go`:

```go
package pgstore

import "io/fs"

// SetMigrationsSource swaps the migration FS the runner reads from. EXPORTED
// FOR TESTS — production callers cannot reach this. Returns a restore closure.
func SetMigrationsSource(fsys fs.FS) func() {
	prev := migrationsSource
	migrationsSource = fsys
	return func() { migrationsSource = prev }
}

// EmbeddedMigrationsFS returns the production embedded FS so tests that build
// synthetic ladders can anchor their MapFS on the real baseline.
func EmbeddedMigrationsFS() fs.FS {
	return migrationsFS
}
```

- [ ] **Step 5: Write the pgstore 0010_baseline.sql with the full schema port**

Create `internal/db/pgstore/migrations/0010_baseline.sql`. This is the largest single file in the plan — a verbatim Postgres port of `internal/db/sqlitestore/migrations/0010_baseline.sql` per the spec's §5 translation tables. The implementer ports each table, constraint, index, and trigger per the rules:

- `INTEGER PRIMARY KEY AUTOINCREMENT` → `BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`
- `INTEGER NOT NULL` FK columns → `BIGINT NOT NULL`
- `DATETIME` with `strftime` default → `TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`
- `DATETIME` nullable → `TEXT` nullable
- `BLOB` → not used (api_tokens uses TEXT for token_hash; verify there are no BLOB columns)
- `json_valid(x) AND json_type(x)='object'` → `jsonb_typeof((x)::jsonb) = 'object'`
- `json_valid(x) AND json_type(x)='array'` → `jsonb_typeof((x)::jsonb) = 'array'`
- `json_valid(x)` → `(x)::jsonb IS NOT NULL`
- `name NOT GLOB '*#*'` → `name NOT LIKE '%#%'`
- `NOT GLOB '*[^...]*'` → `!~ '[^...]'` (POSIX regex)
- `json_extract(payload, '$.idempotency_key')` → `(payload::jsonb ->> 'idempotency_key')`
- 6 SQLite `RAISE(ABORT, ...)` triggers → 3 PL/pgSQL functions + 6 triggers (per spec)
- FTS5 virtual table + 5 triggers → `issues_search` table + `rebuild_issue_search` function + 4 PL/pgSQL triggers + 1 FK CASCADE
- All `?` placeholders not used here (this is DDL only).

The file's structure (from the spec):

```sql
-- internal/db/pgstore/migrations/0010_baseline.sql
-- Postgres baseline schema, semantically equivalent to sqlitestore's 0010_baseline.sql
-- per docs/superpowers/specs/2026-05-29-kata-storage-3-pgstore-baseline-schema.md.

CREATE EXTENSION IF NOT EXISTS unaccent;

-- Custom text-search config: unaccent over simple. Same lower-no-stem tokenization
-- as SQLite's `unicode61 remove_diacritics 2`.
DROP TEXT SEARCH CONFIGURATION IF EXISTS kata_simple_unaccent;
CREATE TEXT SEARCH CONFIGURATION kata_simple_unaccent (COPY = simple);
ALTER TEXT SEARCH CONFIGURATION kata_simple_unaccent
  ALTER MAPPING FOR hword, hword_part, word
  WITH unaccent, simple;

-- Table order: meta, projects, project_aliases, recurrences, issues, comments,
-- links, import_mappings, events, purge_log, issue_labels, issues_search, api_tokens.

CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE projects (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid        TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL DEFAULT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
  deleted_at TEXT,
  metadata   TEXT NOT NULL DEFAULT '{}'
               CHECK (jsonb_typeof((metadata)::jsonb) = 'object'),
  revision   BIGINT NOT NULL DEFAULT 1,
  CHECK (length(uid) = 26),
  CHECK (length(trim(name)) > 0),
  CHECK (name NOT LIKE '%#%')
);
CREATE INDEX idx_projects_active ON projects(id) WHERE deleted_at IS NULL;

-- [continue porting every table in dependency order from sqlitestore's 0010_baseline.sql,
-- applying the translation rules above. The implementer should keep
-- internal/db/sqlitestore/migrations/0010_baseline.sql open as the source of truth
-- and translate it line by line.]

-- After all tables: indexes (incl. the new UNIQUE partial index for idempotency).
CREATE UNIQUE INDEX idx_events_idempotency_uniq
  ON events(project_id, (payload::jsonb ->> 'idempotency_key'))
  WHERE type = 'issue.created' AND (payload::jsonb ->> 'idempotency_key') IS NOT NULL;

-- The existing non-unique idx_events_idempotency (with created_at in the key) is
-- also ported, for the lookup+ordering path.

-- Trigger functions and triggers:
-- (1) enforce_links_same_project()
-- (2) enforce_links_uid_consistency()
-- (3) enforce_uid_immutable()
-- per the spec's §5 trigger inventory.

-- FTS: issues_search table + rebuild_issue_search() + 4 sync triggers.
```

The implementer fills in every table and index by translating from the SQLite baseline. Use `git show HEAD:internal/db/sqlitestore/migrations/0010_baseline.sql` as the source.

The full PL/pgSQL trigger functions (these are NOT in the spec's prose verbatim; render them here):

```sql
CREATE OR REPLACE FUNCTION enforce_links_same_project() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  from_project BIGINT;
  to_project BIGINT;
BEGIN
  SELECT project_id INTO from_project FROM issues WHERE id = NEW.from_issue_id;
  SELECT project_id INTO to_project FROM issues WHERE id = NEW.to_issue_id;
  IF from_project IS DISTINCT FROM NEW.project_id
     OR to_project IS DISTINCT FROM NEW.project_id THEN
    RAISE EXCEPTION 'cross-project links are not allowed';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER trg_links_same_project_insert
  BEFORE INSERT ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_same_project();
CREATE TRIGGER trg_links_same_project_update
  BEFORE UPDATE ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_same_project();

CREATE OR REPLACE FUNCTION enforce_links_uid_consistency() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  from_uid TEXT;
  to_uid TEXT;
BEGIN
  SELECT uid INTO from_uid FROM issues WHERE id = NEW.from_issue_id;
  SELECT uid INTO to_uid FROM issues WHERE id = NEW.to_issue_id;
  IF NEW.from_issue_uid IS DISTINCT FROM from_uid THEN
    RAISE EXCEPTION 'from_issue_uid does not match from_issue_id';
  END IF;
  IF NEW.to_issue_uid IS DISTINCT FROM to_uid THEN
    RAISE EXCEPTION 'to_issue_uid does not match to_issue_id';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER trg_links_uid_consistency_insert
  BEFORE INSERT ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_uid_consistency();
CREATE TRIGGER trg_links_uid_consistency_update
  BEFORE UPDATE ON links
  FOR EACH ROW EXECUTE FUNCTION enforce_links_uid_consistency();

CREATE OR REPLACE FUNCTION enforce_uid_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.uid IS DISTINCT FROM OLD.uid THEN
    RAISE EXCEPTION '%.uid is immutable', TG_TABLE_NAME;
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER trg_projects_uid_immutable
  BEFORE UPDATE OF uid ON projects
  FOR EACH ROW EXECUTE FUNCTION enforce_uid_immutable();
CREATE TRIGGER trg_issues_uid_immutable
  BEFORE UPDATE OF uid ON issues
  FOR EACH ROW EXECUTE FUNCTION enforce_uid_immutable();
```

The FTS rebuild function (with cascade-safe guard):

```sql
CREATE TABLE issues_search (
  issue_id BIGINT PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
  tsv      tsvector NOT NULL
);
CREATE INDEX idx_issues_search_tsv ON issues_search USING GIN(tsv);

CREATE OR REPLACE FUNCTION rebuild_issue_search(p_issue_id BIGINT) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_title TEXT;
  v_body TEXT;
  v_comments TEXT;
BEGIN
  -- Cascade guard: if the parent issue row is already vanishing, skip the
  -- rebuild. Prevents FK failure on transient state during DELETE cascades.
  IF NOT EXISTS (SELECT 1 FROM issues WHERE id = p_issue_id) THEN
    RETURN;
  END IF;
  SELECT title, COALESCE(body, '') INTO v_title, v_body FROM issues WHERE id = p_issue_id;
  SELECT COALESCE(string_agg(body, ' ' ORDER BY id), '')
    INTO v_comments
    FROM comments WHERE issue_id = p_issue_id;
  INSERT INTO issues_search (issue_id, tsv)
    VALUES (p_issue_id,
      to_tsvector('kata_simple_unaccent', coalesce(v_title,'') || ' ' || coalesce(v_body,'') || ' ' || coalesce(v_comments,'')))
    ON CONFLICT (issue_id) DO UPDATE SET tsv = EXCLUDED.tsv;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_issue() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.id);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_comment_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(NEW.issue_id);
  RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION issues_search_trigger_on_comment_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM rebuild_issue_search(OLD.issue_id);
  RETURN NULL;
END $$;

CREATE TRIGGER issues_search_after_issue_insert
  AFTER INSERT ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();

CREATE TRIGGER issues_search_after_issue_update
  AFTER UPDATE OF title, body ON issues
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_issue();

CREATE TRIGGER issues_search_after_comment_insert
  AFTER INSERT ON comments
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_comment_insert();

CREATE TRIGGER issues_search_after_comment_delete
  AFTER DELETE ON comments
  FOR EACH ROW EXECUTE FUNCTION issues_search_trigger_on_comment_delete();
```

- [ ] **Step 6: Run the fresh-DB migrate test**

Run: `go test ./internal/db/pgstore/... -run TestMigrate_FreshDBAppliesBaseline -v`
Expected: PASS — Migrate applies 0010_baseline.sql, stamps schema_version=10, seeds instance_uid.

- [ ] **Step 7: Confirm `-short` skips the test**

Run: `go test -short ./internal/db/pgstore/...`
Expected: 0 tests run, no failures.

- [ ] **Step 8: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues.

- [ ] **Step 9: Commit**

```bash
git add internal/db/pgstore/migrate.go internal/db/pgstore/migrations/0010_baseline.sql internal/db/pgstore/migrations_export_test.go internal/db/pgstore/migrate_test.go
git commit -m "feat(pgstore): Storage.Migrate impl + 0010 baseline schema"
```

---

## Task 4: pgstore domain method stubs

The `db.Storage` interface has ~120 methods. Phase 3 implements 7 (identity/lifecycle, already done in Task 2-3); the other ~113 are stubbed so `*pgstore.Store` satisfies the interface. All stubs return a sentinel "not implemented in Phase 3" error so any accidental Phase-3 caller fails loudly.

**Files:**
- Create: `internal/db/pgstore/stubs.go`
- Modify: `internal/db/pgstore/store.go` (uncomment the `var _ db.Storage = (*Store)(nil)` assertion)

- [ ] **Step 1: Generate the stub method bodies from the Storage interface**

Use `go run` so the tool comes from the module cache rather than a global install — keeps the workflow consistent with the plan's nix-only tooling rule and avoids depending on `$GOPATH/bin` being on PATH:

```bash
go run github.com/josharian/impl@latest -dir internal/db 's *Store' go.kenn.io/kata/internal/db.Storage > /tmp/pgstore-stubs-raw.go
```

This produces all method signatures with empty bodies. The implementer then:
- Removes the seven identity/lifecycle methods already implemented (`InstanceUID`, `RefreshInstanceUID`, `SchemaVersion`, `Path`, `Close`, `RetryTransient`, `Migrate`).
- Fills every remaining method body with the sentinel return.

- [ ] **Step 2: Build the stubs file with the sentinel error**

Create `internal/db/pgstore/stubs.go`. The file starts with:

```go
// Phase 3 stubs: every db.Storage domain method returns ErrNotImplementedPhase3.
// Phase 4 replaces each entity group's stubs with real queries.

package pgstore

import (
	"context"
	"errors"
	"iter"
	"time"

	"go.kenn.io/kata/internal/db"
)

// ErrNotImplementedPhase3 is returned by every pgstore domain method during
// Phase 3. Phase 4 replaces each entity group's stubs with real queries.
var ErrNotImplementedPhase3 = errors.New("pgstore: not implemented in Phase 3 — see Phase 4 for queries")
```

Then for each domain method on `db.Storage`, add a stub that:
- Returns zero values for each non-error return type.
- Returns `ErrNotImplementedPhase3` for the error return.

The method shapes the implementer encounters:

```go
// Single-error return:
func (s *Store) HardDeleteProject(ctx context.Context, id int64) error {
	return ErrNotImplementedPhase3
}

// (Value, error) return:
func (s *Store) ProjectByID(ctx context.Context, id int64) (db.Project, error) {
	return db.Project{}, ErrNotImplementedPhase3
}

// (Value, *Event, error) return:
func (s *Store) RemoveProject(ctx context.Context, p db.RemoveProjectParams) (db.Project, *db.Event, error) {
	return db.Project{}, nil, ErrNotImplementedPhase3
}

// (Slice, error) return:
func (s *Store) ListProjects(ctx context.Context) ([]db.Project, error) {
	return nil, ErrNotImplementedPhase3
}

// (Map, error) return:
func (s *Store) BatchProjectStats(ctx context.Context) (map[int64]db.ProjectStats, error) {
	return nil, ErrNotImplementedPhase3
}

// iter.Seq2-only return — for the export iterators. NOTE: the export methods
// return a single iter.Seq2 (no outer error), so the sentinel must be yielded
// from inside the sequence. The actual type names live in internal/db/export.go
// and use the *Export suffix (db.ProjectExport, db.AliasExport, …), NOT a
// prefix:
func (s *Store) ExportProjects(ctx context.Context, f db.ExportFilter) iter.Seq2[db.ProjectExport, error] {
	return func(yield func(db.ProjectExport, error) bool) {
		yield(db.ProjectExport{}, ErrNotImplementedPhase3)
	}
}

// ExportMeta is the same shape but with no ExportFilter parameter and yielding
// db.MetaKV:
func (s *Store) ExportMeta(ctx context.Context) iter.Seq2[db.MetaKV, error] {
	return func(yield func(db.MetaKV, error) bool) {
		yield(db.MetaKV{}, ErrNotImplementedPhase3)
	}
}
```

Export-method type-name reference (from `internal/db/storage.go` lines 144–155): `MetaKV`, `ProjectExport`, `AliasExport`, `RecurrenceExport`, `IssueExport`, `CommentExport`, `IssueLabelExport`, `LinkExport`, `ImportMappingExport`, `EventExport`, `PurgeLogExport`, `SequenceExport`. All twelve follow the same `iter.Seq2`-only signature shown above.

The implementer goes through the full interface (one entry group at a time per the `// projects + aliases`, `// issues`, `// comments`, etc. comments in `internal/db/storage.go`) and writes stubs for every method.

Tip: `grep -E '^\s+[A-Z][a-zA-Z]+\s*\(' internal/db/storage.go` lists all method signatures in order; pipe through `wc -l` to confirm count (~120 total, minus the 7 already done = ~113 stubs).

- [ ] **Step 3: Uncomment the interface assertion**

Modify `internal/db/pgstore/store.go`: change the commented `// var _ db.Storage = (*Store)(nil)` line to:

```go
var _ db.Storage = (*Store)(nil)
```

- [ ] **Step 4: Run `go build` to verify the interface is satisfied**

Run: `go build ./...`
Expected: clean. The interface assertion compiles, meaning `*Store` implements every method in `db.Storage`.

If the build fails with "missing method X", the implementer adds the missing stub and re-runs.

- [ ] **Step 5: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues. Specifically: golangci-lint's `unused` linter will NOT flag the stubs because each is part of an interface implementation visible through the `var _` assertion.

- [ ] **Step 6: Quick sanity test that the sentinel actually surfaces**

Append to `internal/db/pgstore/store_test.go`:

```go
func TestStubsReturnSentinelError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Pick one stub method to assert the sentinel comes back.
	_, err = s.ProjectByID(ctx, 1)
	require.ErrorIs(t, err, pgstore.ErrNotImplementedPhase3)
}
```

Run: `go test ./internal/db/pgstore/... -run TestStubsReturnSentinelError -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/db/pgstore/stubs.go internal/db/pgstore/store.go internal/db/pgstore/store_test.go
git commit -m "feat(pgstore): add Phase-3 stubs so *Store satisfies db.Storage"
```

---

## Task 5: pgstore schema acceptance test

The acceptance test runs Migrate against a fresh testcontainer PG and asserts the high-value structural surface — every expected table, every named trigger, the new `idx_events_idempotency_uniq` UNIQUE index, the FTS config + functions, and the per-table foreign-key inventory. Exact constraint/index name parity (every CHECK, every FK constraint name, every non-PK index) is deferred to the Phase 6 cross-backend conformance suite, which is the canonical parity gate. Phase 3's job is to lock down the structural surface so an obvious omission can't slip through.

**Files:**
- Create: `internal/db/pgstore/schema_test.go`

- [ ] **Step 1: Write the schema acceptance test**

Create `internal/db/pgstore/schema_test.go`:

```go
package pgstore_test

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// expectedTables is the canonical post-Migrate table set. The order is
// declaration order from pgstore/migrations/0010_baseline.sql.
var expectedTables = []string{
	"meta",
	"projects",
	"project_aliases",
	"recurrences",
	"issues",
	"comments",
	"links",
	"import_mappings",
	"events",
	"purge_log",
	"issue_labels",
	"issues_search",
	"api_tokens",
}

// expectedTriggers maps trigger name to the table it lives on.
var expectedTriggers = map[string]string{
	"trg_links_same_project_insert":         "links",
	"trg_links_same_project_update":         "links",
	"trg_links_uid_consistency_insert":      "links",
	"trg_links_uid_consistency_update":      "links",
	"trg_projects_uid_immutable":            "projects",
	"trg_issues_uid_immutable":              "issues",
	"issues_search_after_issue_insert":      "issues",
	"issues_search_after_issue_update":      "issues",
	"issues_search_after_comment_insert":    "comments",
	"issues_search_after_comment_delete":    "comments",
}

func TestSchema_AllTablesPresentAfterMigrate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	rows, err := s.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got = append(got, name)
	}
	require.NoError(t, rows.Err())

	want := append([]string(nil), expectedTables...)
	sort.Strings(want)
	sort.Strings(got)
	assert.Equal(t, want, got, "tables present in PG must match expectedTables exactly")
}

func TestSchema_AllTriggersPresentAfterMigrate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	for trgName, tableName := range expectedTriggers {
		var actualTable string
		err := s.QueryRowContext(ctx, `
			SELECT event_object_table FROM information_schema.triggers
			WHERE trigger_name = $1 AND event_object_schema = current_schema()
			LIMIT 1`, trgName).Scan(&actualTable)
		require.NoError(t, err, "trigger %s must exist", trgName)
		assert.Equal(t, tableName, actualTable, "trigger %s must live on table %s", trgName, tableName)
	}
}

func TestSchema_IdempotencyUniqueIndexExists(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	var indexdef string
	require.NoError(t, s.QueryRowContext(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = 'idx_events_idempotency_uniq'`,
	).Scan(&indexdef))
	assert.Contains(t, indexdef, "UNIQUE", "idx_events_idempotency_uniq must be UNIQUE")
	assert.Contains(t, indexdef, "idempotency_key", "index must reference idempotency_key")
	assert.Contains(t, indexdef, "issue.created", "index must be partial on issue.created events")
}

func TestSchema_FTSConfigAndFunctionsExist(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	var configExists bool
	require.NoError(t, s.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_ts_config WHERE cfgname = 'kata_simple_unaccent')`,
	).Scan(&configExists))
	assert.True(t, configExists, "kata_simple_unaccent text search config must exist")

	for _, fn := range []string{"rebuild_issue_search", "enforce_links_same_project", "enforce_links_uid_consistency", "enforce_uid_immutable"} {
		var fnExists bool
		require.NoError(t, s.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = $1)`,
			fn).Scan(&fnExists))
		assert.True(t, fnExists, "PL/pgSQL function %s must exist", fn)
	}
}

// expectedFKCounts captures the FK count per table that the SQLite baseline
// declares. If pgstore's translation drops a FK, this test fails. The
// implementer derives these numbers from internal/db/sqlitestore/migrations/
// 0010_baseline.sql at coding time (count each `REFERENCES ...` on each
// table) and updates the map to match; the comment after each entry names
// the targets so the count is auditable.
var expectedFKCounts = map[string]int{
	// Examples from the SQLite baseline — the implementer verifies against
	// sqlitestore's 0010 and adjusts numbers and comments to match exactly:
	//   "project_aliases": 1, // -> projects(id)
	//   "issues":          1, // -> projects(id)
	//   "comments":        1, // -> issues(id)
	//   "links":           3, // -> projects(id), issues(id) x2
	//   "events":          2, // -> projects(id), issues(id) nullable
	//   "issue_labels":    1, // -> issues(id)
	//   "issues_search":   1, // -> issues(id) ON DELETE CASCADE
	//   "import_mappings":1, "purge_log": 1, "api_tokens": 1, "recurrences": 1
}

func TestSchema_ForeignKeyInventoryMatches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	if len(expectedFKCounts) == 0 {
		t.Fatal("expectedFKCounts is empty — the implementer must populate it from sqlitestore's 0010_baseline.sql before this test is meaningful")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	for table, want := range expectedFKCounts {
		var got int
		require.NoError(t, s.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.table_constraints
			WHERE table_schema = current_schema()
			  AND table_name = $1
			  AND constraint_type = 'FOREIGN KEY'`, table).Scan(&got))
		assert.Equal(t, want, got, "table %s should have %d foreign keys; PG schema has %d (sqlite/pg FK drift)", table, want, got)
	}
}
```

`expectedFKCounts` deliberately ships empty in the plan — the spec doesn't enumerate every FK and inventing a count list here would be guessing. The implementer populates the map from `internal/db/sqlitestore/migrations/0010_baseline.sql` at coding time; the `t.Fatal` guard fires if the map is left empty so the test can't silently pass by being a noop.

- [ ] **Step 2: Populate `expectedFKCounts` and run the schema tests**

Before running, populate `expectedFKCounts` in the test file from the canonical source — `internal/db/sqlitestore/migrations/0010_baseline.sql`. Count each `REFERENCES <table>(<col>)` clause per table (both inline column constraints and standalone `FOREIGN KEY` declarations) and write the totals into the map with a comment naming the targets. Leaving the map empty makes `TestSchema_ForeignKeyInventoryMatches` fail via its `t.Fatal` guard — that guard is intentional, so an empty map can't silently pass.

Then run:

```bash
go test ./internal/db/pgstore/... -run TestSchema -v
```

Expected: PASS — all five sub-tests (`AllTablesPresentAfterMigrate`, `AllTriggersPresentAfterMigrate`, `IdempotencyUniqueIndexExists`, `FTSConfigAndFunctionsExist`, `ForeignKeyInventoryMatches`) confirm the schema port is complete.

If any assertion fails, the implementer:
- For missing tables: confirms each entry in `expectedTables` has a `CREATE TABLE` statement in `0010_baseline.sql`.
- For missing triggers: confirms each entry in `expectedTriggers` has a `CREATE TRIGGER` statement.
- For the index: confirms `CREATE UNIQUE INDEX idx_events_idempotency_uniq` exists with the partial WHERE clause.
- For FTS config/functions: confirms the file-top `CREATE TEXT SEARCH CONFIGURATION` runs and each PL/pgSQL function is declared.
- For FK counts: confirms each `REFERENCES` from the SQLite baseline ported into PG with the matching target table. A drift here usually means a `FOREIGN KEY (...) REFERENCES ...` clause was dropped or renamed during translation.

- [ ] **Step 3: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues.

- [ ] **Step 4: Commit**

```bash
git add internal/db/pgstore/schema_test.go
git commit -m "test(pgstore): schema acceptance — tables, triggers, idempotency UNIQUE, FTS, FK inventory"
```

---

## Task 6: Bump CurrentSchemaVersion, add 0011 files for both backends, update SQLite tests

The coordinated breaking change. After this commit: SQLite gains a real `0011_idempotency_unique.sql` (adds the UNIQUE partial index), pgstore gains a placeholder `0011_idempotency_unique.sql` (single comment + `SELECT 1;`), `db.CurrentSchemaVersion()` returns 11, and every existing test that asserts on the old value of `CurrentSchemaVersion` is updated.

**Files:**
- Create: `internal/db/sqlitestore/migrations/0011_idempotency_unique.sql`
- Create: `internal/db/pgstore/migrations/0011_idempotency_unique.sql`
- Modify: `internal/db/schema_version.go`
- Modify: `internal/db/sqlitestore/migrate_test.go`
- Modify: any other test file asserting on the specific schema_version value

- [ ] **Step 1: Add the sqlitestore 0011 migration file**

Create `internal/db/sqlitestore/migrations/0011_idempotency_unique.sql`:

```sql
-- Phase 3 schema_version=11 migration: add a UNIQUE partial index on
-- (project_id, idempotency_key) for issue.created events. Closes the
-- check-then-insert race that today is only serialized by SQLite's
-- single-writer. The existing non-unique idx_events_idempotency (with
-- created_at in the key) stays in place for the lookup+ordering path.
--
-- Pre-migration verification: any DB with the duplicate pattern below would
-- fail this migration loudly. Operators should run the duplicate-detection
-- query to clean up before upgrading. Under kata's design no DB should have
-- duplicates — only a manual edit or a bug could create them.
--
--   SELECT project_id,
--          json_extract(payload, '$.idempotency_key') AS idem_key,
--          COUNT(*) AS dupes
--   FROM events
--   WHERE type='issue.created'
--     AND json_extract(payload, '$.idempotency_key') IS NOT NULL
--   GROUP BY 1, 2
--   HAVING COUNT(*) > 1;

CREATE UNIQUE INDEX idx_events_idempotency_uniq
  ON events(project_id, json_extract(payload, '$.idempotency_key'))
  WHERE type = 'issue.created'
    AND json_extract(payload, '$.idempotency_key') IS NOT NULL;
```

- [ ] **Step 2: Add the pgstore 0011 placeholder**

Create `internal/db/pgstore/migrations/0011_idempotency_unique.sql`:

```sql
-- Phase 3 schema_version=11 placeholder for Postgres. The UNIQUE partial
-- index on events.idempotency_key is already in pgstore's 0010_baseline.sql
-- (PG is fresh ground; no legacy schema to evolve). This file exists only so
-- the embedded migration ladder stays lockstep with sqlitestore — the runner
-- stamps schema_version=11 when this no-op applies, matching SQLite's path.
--
-- Future migrations that need a real PG-side schema change can land here as
-- 0012, 0013, ...; the file body below is just a no-op SELECT.

SELECT 1;
```

- [ ] **Step 3: Bump `currentSchemaVersion`**

Modify `internal/db/schema_version.go`. Change line:

```go
var currentSchemaVersion = BaselineSchemaVersion
```

to:

```go
var currentSchemaVersion = 11
```

Leave `BaselineSchemaVersion = 10` (the JSONL cutover boundary is immutable).

- [ ] **Step 4: Update `TestMigrate_FreshDBAppliesBaseline` in sqlitestore**

Modify `internal/db/sqlitestore/migrate_test.go`. The test currently asserts:
```go
assert.Equal(t, 0, result.From)
assert.Equal(t, db.CurrentSchemaVersion(), result.To)
assert.Equal(t, []int{db.CurrentSchemaVersion()}, result.Applied)
```

After the bump, a fresh DB applies BOTH 0010 and 0011. Change to:

```go
assert.Equal(t, 0, result.From)
assert.Equal(t, db.CurrentSchemaVersion(), result.To) // 11
assert.Equal(t, []int{db.BaselineSchemaVersion, db.CurrentSchemaVersion()}, result.Applied) // [10, 11]
```

- [ ] **Step 5: Update the synthetic-version tests in sqlitestore**

Find tests in `internal/db/sqlitestore/migrate_test.go` that use synthetic 0011 files (`TestMigrate_AppliesSyntheticVersion11`, `TestMigrate_RollsBackOnBrokenMigration`, `TestMigrate_ConcurrentMigratorsSerialize`, `TestMigrate_RejectsDuplicateLadderVersions`). These now collide with the real 0011 file in the embedded FS — except the test seam (`SetMigrationsSource`) replaces the source entirely, so the real file isn't seen.

But they bump `SetCurrentSchemaVersion(11)`, which is now redundant. More importantly, the synthetic test asserts `From == db.CurrentSchemaVersion()` and `To == 11`. After the bump:
- The test's pre-Migrate state (post-real-Migrate) lands at v11, not v10.
- The synthetic source must contain 0010 + 0011 to apply something new.

Easiest fix: bump every synthetic test that targets "one above current" to use v12. Per-test edits:

`TestMigrate_AppliesSyntheticVersion11` — rename to `TestMigrate_AppliesSyntheticVersion12`. Change:
- `SetCurrentSchemaVersion(11)` → `SetCurrentSchemaVersion(12)`.
- `0011_add_marker.sql` → `0012_add_marker.sql`.
- Add `"migrations/0011_idempotency_unique.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}` so the ladder stays contiguous (10 → 11 → 12).
- Assertions: `result.From == 11`, `result.To == 12`, `result.Applied == []int{12}`.
- Pre-Migrate snapshot path comment changes from `premigrate-v10.db` to `premigrate-v11.db` (the snapshot is taken at the in-DB version when Migrate fires, which is now 11).

`TestMigrate_RollsBackOnBrokenMigration`, `TestMigrate_ConcurrentMigratorsSerialize` — same shape: bump `SetCurrentSchemaVersion(11)` → `(12)`, rename the synthetic 0011 file to 0012, add 0011_idempotency_unique placeholder, and update any version-specific assertions (e.g., `assert.Equal(t, 11, v)` → `12` where the test reads schema_version after success, OR keep at 11 where the test asserts the failed-rollback DB unchanged at 11). Snapshot-path assertions in `TestMigrate_RollsBackOnBrokenMigration` change from `premigrate-v10.db` to `premigrate-v11.db`.

`TestMigrate_RejectsDuplicateLadderVersions` — change `SetCurrentSchemaVersion(11)` → `(12)`, MapFS `{0010_baseline, 0011_idempotency_unique, 0012_a, 0012_b}`, expected error mentioning `0012_a.sql` and `0012_b.sql`.

`TestMigrate_RejectsLadderGaps` — currently uses `SetCurrentSchemaVersion(12)` with MapFS `{0010_baseline, 0012_skip}` and asserts "version 11 missing". After the bump:
- MapFS becomes `{0010_baseline, 0011_idempotency_unique, 0013_skip}` (contiguous through 11, gap at 12).
- Expected error text changes from `"version 11 missing"` → `"version 12 missing"`.
- Change `SetCurrentSchemaVersion(12)` → `SetCurrentSchemaVersion(13)`. This is defensive: `listPendingMigrations` already runs the gap-check loop before the cap-check loop, so a binary of 11 or 12 would still surface the gap error first; keeping the binary at or above the highest synthetic file (13) insulates the test from any future reordering of those checks.

`TestMigrate_RejectsMigrationAboveBinarySchema` — currently uses `0011_future.sql` with no bump (relies on 0011 being above the 10-cap). After CurrentSchemaVersion=11, the synthetic 0011 is within range. Change the MapFS to `{0010_baseline, 0011_idempotency_unique, 0012_future}` and rename the above-cap file to `0012_future.sql`. The 0011 placeholder (`SELECT 1;`) is REQUIRED — without it, `listPendingMigrations` rejects the ladder for a missing v11 before it reaches the binary-cap check, and the assertion `"exceeds binary schema_version"` never fires. Leave `SetCurrentSchemaVersion` alone (the test relies on the default 11); assertions unchanged.

- [ ] **Step 6: Find any other test files asserting on `CurrentSchemaVersion`**

Run:

```bash
rg -n 'CurrentSchemaVersion\(\) == 10|schema_version, 10|"schema_version".*10|currentSchemaVersion == 10' --type go
```

For each match, decide:
- If the test uses `db.CurrentSchemaVersion()` dynamically (the value adapts), no change needed.
- If the test hardcodes "10" as the expected version, update to "11".
- If the test asserts on migration output (e.g., "applied schema_version 10"), confirm whether the message is the test's expected one OR `strconv.Itoa(CurrentSchemaVersion())` — adapt either way.

`internal/jsonl/import_test.go` and `internal/jsonl/cutover_test.go` likely have version assertions; check each.

`cmd/kata/migrate_test.go` uses `strconv.Itoa(db.CurrentSchemaVersion())` already — adapts automatically.

- [ ] **Step 7: Run the full sqlitestore migrate tests**

Run: `go test ./internal/db/sqlitestore/... -run TestMigrate -v -count=1`
Expected: all pass.

- [ ] **Step 8: Run the full sqlitestore test suite**

Run: `go test ./internal/db/sqlitestore/... -count=1`
Expected: all pass.

- [ ] **Step 9: Run the full repo test suite**

Run: `go test ./... -count=1`
Expected: all pass. Any failure indicates a test that wasn't updated; fix per Step 6's pattern.

- [ ] **Step 10: Run pgstore tests too (the placeholder applies cleanly)**

Run: `go test ./internal/db/pgstore/... -count=1`
Expected: all pass. Specifically, `TestMigrate_FreshDBAppliesBaseline` now expects `result.To = 11` and `result.Applied = [10, 11]` — update if needed:

```go
assert.Equal(t, 0, result.From)
assert.Equal(t, db.CurrentSchemaVersion(), result.To) // 11
assert.Equal(t, []int{db.BaselineSchemaVersion, db.CurrentSchemaVersion()}, result.Applied)
```

- [ ] **Step 11: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues.

- [ ] **Step 12: Commit**

```bash
git add internal/db/schema_version.go internal/db/sqlitestore/migrations/0011_idempotency_unique.sql internal/db/pgstore/migrations/0011_idempotency_unique.sql internal/db/sqlitestore/migrate_test.go internal/db/pgstore/migrate_test.go
# Plus any other test files updated in Step 6
git commit -m "feat(db): bump schema_version to 11; add SQLite 0011 idempotency UNIQUE; pgstore 0011 placeholder"
```

---

## Task 7: storeopen dispatch for postgres

Replace storeopen's "not yet available" return for postgres DSNs with real dispatch to pgstore. Both ApplyMigrations and no-ApplyMigrations branches.

**Files:**
- Modify: `internal/db/storeopen/storeopen.go`
- Modify: `internal/db/storeopen/storeopen_test.go`

- [ ] **Step 1: Write the failing test for the new postgres dispatch**

Modify `internal/db/storeopen/storeopen_test.go`. Replace `TestOpenPostgresReturnsRedactedNotAvailableError` (which asserts the old "not yet available" return) with:

```go
func TestOpenPostgresWithApplyMigrationsErrorsForUnreachableHost(t *testing.T) {
	// Use a postgres DSN that points at an unroutable host — this proves
	// the dispatcher reaches pgstore.Open (which returns a real connection
	// error) rather than the old "not yet available" stub. The password is
	// "SECRET" so the mutation guard at the bottom of the test would fire if
	// a future regression `%w`'d the raw DSN into the error.
	dsn := "postgres://kata:SECRET@127.0.0.1:1/kata?sslmode=disable" //nolint:gosec // fixture proves the password is redacted
	_, _, err := storeopen.Open(context.Background(), dsn, db.ApplyMigrations())
	require.Error(t, err)
	// Connection failure should appear in the error — not "not yet available".
	assert.NotContains(t, err.Error(), "not yet available")
	// And the password must never appear in the error, even from pgx's own
	// connection errors. Old test had this guard; preserve it across the
	// dispatch rewrite.
	assert.NotContains(t, err.Error(), "SECRET")
	assert.Contains(t, dsn, "SECRET") // mutation guard: ensures the assertion above can actually fail
}

func TestOpenPostgresWithoutApplyMigrationsErrorsForUnreachableHost(t *testing.T) {
	dsn := "postgres://kata:SECRET@127.0.0.1:1/kata?sslmode=disable" //nolint:gosec // fixture
	_, _, err := storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not yet available")
	assert.NotContains(t, err.Error(), "SECRET")
	assert.Contains(t, dsn, "SECRET")
}
```

- [ ] **Step 2: Run the failing tests**

Run: `go test ./internal/db/storeopen/... -run TestOpenPostgres -v`
Expected: FAIL — current code returns "not yet available".

- [ ] **Step 3: Implement the postgres dispatch in `storeopen.go`**

Modify `internal/db/storeopen/storeopen.go`. Find the postgres case (line ~47) in `Open`:

```go
case hasScheme && (scheme == "postgres" || scheme == "postgresql"):
    return nil, db.MigrationResult{}, fmt.Errorf("postgres backend not yet available: %s", config.RedactDSN(dsn))
```

Replace with dispatch logic. The new structure of `Open`:

```go
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, db.MigrationResult, error) {
	cfg := db.ApplyOpenOptions(opts...)
	if cfg.ReadOnly {
		cfg.ApplyMigrations = false
	}

	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case hasScheme && (scheme == "postgres" || scheme == "postgresql"):
		if cfg.ApplyMigrations {
			return openPostgresWithMigrations(ctx, dsn, opts)
		}
		return openPostgresRefusingStale(ctx, dsn, opts)
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
```

Add the two new postgres-side helpers (above `openSQLiteWithMigrations`):

```go
func openPostgresWithMigrations(ctx context.Context, dsn string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	// Postgres has no pre-v10 history; no JSONL cutover branch applies.
	// Open + Migrate directly. Migrate's entry guards refuse pre-baseline
	// (current > 0 && current < 10) and newer-than-binary (current > current_binary).
	ver, peekErr := pgstore.PeekSchemaVersion(ctx, dsn)
	if peekErr == nil && ver > db.CurrentSchemaVersion() {
		return nil, db.MigrationResult{}, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary", ver, config.RedactDSN(dsn), db.CurrentSchemaVersion())
	}
	if peekErr != nil {
		// Wrap with RedactDSN context so any DSN-bearing error from pgx never
		// reaches a user log unredacted. The existing "not yet available"
		// path enforced this; preserve the contract.
		return nil, db.MigrationResult{}, fmt.Errorf("peek %s: %w", config.RedactDSN(dsn), peekErr)
	}
	store, err := pgstore.Open(ctx, dsn, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, fmt.Errorf("open %s: %w", config.RedactDSN(dsn), err)
	}
	result, err := store.Migrate(ctx)
	if err != nil {
		_ = store.Close()
		return nil, db.MigrationResult{}, fmt.Errorf("migrate %s: %w", config.RedactDSN(dsn), err)
	}
	return store, result, nil
}

func openPostgresRefusingStale(ctx context.Context, dsn string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	ver, peekErr := pgstore.PeekSchemaVersion(ctx, dsn)
	if peekErr != nil {
		return nil, db.MigrationResult{}, fmt.Errorf("peek %s: %w", config.RedactDSN(dsn), peekErr)
	}
	if ver > db.CurrentSchemaVersion() {
		return nil, db.MigrationResult{}, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary", ver, config.RedactDSN(dsn), db.CurrentSchemaVersion())
	}
	if ver != db.CurrentSchemaVersion() {
		return nil, db.MigrationResult{}, fmt.Errorf("%w: schema_version %d at %s, current %d; run kata migrate", db.ErrSchemaOutOfDate, ver, config.RedactDSN(dsn), db.CurrentSchemaVersion())
	}
	store, err := pgstore.Open(ctx, dsn, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, fmt.Errorf("open %s: %w", config.RedactDSN(dsn), err)
	}
	return store, db.MigrationResult{}, nil
}
```

Add the import:

```go
"go.kenn.io/kata/internal/db/pgstore"
```

- [ ] **Step 4: Run the postgres-dispatch tests**

Run: `go test ./internal/db/storeopen/... -run TestOpenPostgres -v`
Expected: PASS — both tests confirm the dispatcher reaches pgstore (the error is from pgstore's pgx connection attempt, not "not yet available").

- [ ] **Step 5: Run the full storeopen test suite**

Run: `go test ./internal/db/storeopen/... -count=1`
Expected: all pass.

- [ ] **Step 6: Run the full repo test suite**

Run: `go test ./... -count=1`
Expected: all pass.

- [ ] **Step 7: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues.

- [ ] **Step 8: Commit**

```bash
git add internal/db/storeopen/storeopen.go internal/db/storeopen/storeopen_test.go
git commit -m "feat(storeopen): dispatch postgres DSNs to pgstore (replaces 'not yet available' stub)"
```

---

## Task 8: pgstore comprehensive migrate tests (parity with sqlitestore)

Add the remaining migrate tests mirroring sqlitestore's coverage: synthetic v12, rollback, concurrent, guards, ladder validation, instance_uid repair. All guarded by `testing.Short()` skip.

**Files:**
- Modify: `internal/db/pgstore/migrate_test.go`

- [ ] **Step 1: Add the at-current noop test**

Append to `internal/db/pgstore/migrate_test.go`:

```go
func TestMigrate_OnAtCurrentDBIsNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Bring up to current.
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	// Second Migrate sees the DB already at current and does nothing.
	result, err := s.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	assert.Nil(t, result.Applied)
}
```

Run: `go test ./internal/db/pgstore/... -run TestMigrate_OnAtCurrentDBIsNoop -v`
Expected: PASS.

- [ ] **Step 2: Add the synthetic v12 test**

Append:

```go
func TestMigrate_AppliesSyntheticVersion12(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Bring DB to v11 first (real ladder).
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	// Bump the binary's declared version so synthetic 0012 file is within cap.
	restoreVersion := db.SetCurrentSchemaVersion(12)
	t.Cleanup(restoreVersion)

	baseline, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	placeholder, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0011_idempotency_unique.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql":         &fstest.MapFile{Data: baseline},
		"migrations/0011_idempotency_unique.sql": &fstest.MapFile{Data: placeholder},
		"migrations/0012_add_marker.sql": &fstest.MapFile{Data: []byte(`
CREATE TABLE pg_migration_marker (
  id BIGINT PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  note TEXT NOT NULL
);
`)},
	}
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	result, err := s.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, 11, result.From)
	assert.Equal(t, 12, result.To)
	assert.Equal(t, []int{12}, result.Applied)

	// The marker table now exists.
	var exists bool
	require.NoError(t, s.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = 'pg_migration_marker')`).Scan(&exists))
	assert.True(t, exists)
}
```

Add imports if missing: `"io/fs"`, `"testing/fstest"`.

Run: `go test ./internal/db/pgstore/... -run TestMigrate_AppliesSyntheticVersion12 -v`
Expected: PASS.

- [ ] **Step 3: Add the rollback test**

Append:

```go
func TestMigrate_RollsBackOnBrokenMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Bring DB to v11 first.
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	restoreVersion := db.SetCurrentSchemaVersion(12)
	t.Cleanup(restoreVersion)

	baseline, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	placeholder, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0011_idempotency_unique.sql")
	require.NoError(t, err)
	// 0012 attempts to re-CREATE the meta table — fails inside the tx, forcing rollback.
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql":          &fstest.MapFile{Data: baseline},
		"migrations/0011_idempotency_unique.sql": &fstest.MapFile{Data: placeholder},
		"migrations/0012_break.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);`)},
	}
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0012_break.sql")

	// schema_version unchanged (still 11).
	v, sverr := s.SchemaVersion(ctx)
	require.NoError(t, sverr)
	assert.Equal(t, 11, v)
}
```

Run: `go test ./internal/db/pgstore/... -run TestMigrate_RollsBackOnBrokenMigration -v`
Expected: PASS.

- [ ] **Step 4: Add the concurrent migrators test**

Append:

```go
func TestMigrate_ConcurrentMigratorsSerialize(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s1, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s1.Close() })
	s2, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	// Bring DB to v11 with one handle first.
	_, err = s1.Migrate(ctx)
	require.NoError(t, err)

	restoreVersion := db.SetCurrentSchemaVersion(12)
	t.Cleanup(restoreVersion)

	baseline, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	placeholder, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0011_idempotency_unique.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql":          &fstest.MapFile{Data: baseline},
		"migrations/0011_idempotency_unique.sql": &fstest.MapFile{Data: placeholder},
		"migrations/0012_marker.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE concurrent_marker (id BIGINT PRIMARY KEY GENERATED ALWAYS AS IDENTITY);`)},
	}
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	type outcome struct {
		applied []int
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, h := range []*pgstore.Store{s1, s2} {
		wg.Add(1)
		go func(h *pgstore.Store) {
			defer wg.Done()
			r, err := h.Migrate(ctx)
			results <- outcome{applied: r.Applied, err: err}
		}(h)
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
	assert.LessOrEqual(t, failureCount, 1)
	assert.Equal(t, 1, totalApplied)

	var exists bool
	require.NoError(t, s1.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = 'concurrent_marker')`).Scan(&exists))
	assert.True(t, exists)
}
```

Add `"sync"` to the imports.

Run: `go test ./internal/db/pgstore/... -run TestMigrate_ConcurrentMigratorsSerialize -v -count=10`
Expected: PASS in all 10 iterations.

- [ ] **Step 5: Add the guard tests**

Append:

```go
func TestMigrate_RejectsPreV10(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	_, err = s.ExecContext(ctx, `UPDATE meta SET value='8' WHERE key='schema_version'`)
	require.NoError(t, err)

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates the baseline")
}

func TestMigrate_RejectsNewerThanBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	_, err = s.ExecContext(ctx, `UPDATE meta SET value='99' WHERE key='schema_version'`)
	require.NoError(t, err)

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
}

func TestMigrate_RejectsMigrationAboveBinarySchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	// CurrentSchemaVersion stays at the default 11; synthetic 0012 file
	// would advance past it. Ladder cap refuses.
	baseline, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	placeholder, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0011_idempotency_unique.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql":          &fstest.MapFile{Data: baseline},
		"migrations/0011_idempotency_unique.sql": &fstest.MapFile{Data: placeholder},
		"migrations/0012_future.sql":            &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds binary schema_version")
}
```

Run: `go test ./internal/db/pgstore/... -run "TestMigrate_Rejects" -v`
Expected: PASS for all three.

- [ ] **Step 6: Add the ladder-validation tests**

Append:

```go
func TestMigrate_RejectsDuplicateLadderVersions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	restoreVersion := db.SetCurrentSchemaVersion(12)
	t.Cleanup(restoreVersion)

	baseline, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	placeholder, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0011_idempotency_unique.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql":          &fstest.MapFile{Data: baseline},
		"migrations/0011_idempotency_unique.sql": &fstest.MapFile{Data: placeholder},
		"migrations/0012_a.sql":                 &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		"migrations/0012_b.sql":                 &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate files")
	assert.Contains(t, err.Error(), "0012_a.sql")
	assert.Contains(t, err.Error(), "0012_b.sql")
}

func TestMigrate_RejectsLadderGaps(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	restoreVersion := db.SetCurrentSchemaVersion(13)
	t.Cleanup(restoreVersion)

	baseline, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0010_baseline.sql")
	require.NoError(t, err)
	placeholder, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), "migrations/0011_idempotency_unique.sql")
	require.NoError(t, err)
	synthetic := fstest.MapFS{
		"migrations/0010_baseline.sql":          &fstest.MapFile{Data: baseline},
		"migrations/0011_idempotency_unique.sql": &fstest.MapFile{Data: placeholder},
		"migrations/0013_skip.sql":              &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version 12 missing")
}
```

Run: `go test ./internal/db/pgstore/... -run "TestMigrate_RejectsDuplicate|TestMigrate_RejectsLadder" -v`
Expected: PASS.

- [ ] **Step 7: Add the instance_uid repair test**

Append:

```go
func TestMigrate_RepairsMissingInstanceUIDOnAtCurrentPath(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, s.InstanceUID())

	// Simulate corruption: drop the instance_uid row.
	_, err = s.ExecContext(ctx, `DELETE FROM meta WHERE key='instance_uid'`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Reopen — bare handle, cache empty.
	s2, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	require.Empty(t, s2.InstanceUID())

	// At-current Migrate must repair.
	result, err := s2.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	assert.Nil(t, result.Applied)
	assert.NotEmpty(t, s2.InstanceUID())
}
```

Run: `go test ./internal/db/pgstore/... -run TestMigrate_RepairsMissingInstanceUID -v`
Expected: PASS.

- [ ] **Step 8: Run the full pgstore test suite**

Run: `go test ./internal/db/pgstore/... -count=1`
Expected: all pass.

- [ ] **Step 9: Run the full repo test suite**

Run: `go test ./... -count=1`
Expected: all pass.

- [ ] **Step 10: Lint clean**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: 0 issues.

- [ ] **Step 11: Verify the success criteria from the spec**

Run each check:

```bash
# pgstore Migrate runs end-to-end against a fresh PG.
go test ./internal/db/pgstore/... -run TestMigrate_FreshDBAppliesBaseline -v

# Schema-shape acceptance.
go test ./internal/db/pgstore/... -run TestSchema -v

# CurrentSchemaVersion == 11 for both backends.
go test ./internal/db/... -count=1

# storeopen dispatches postgres to pgstore.
go test ./internal/db/storeopen/... -count=1

# Full suite + lint.
go test ./...
nix run 'nixpkgs#golangci-lint' -- run ./...
```

All expected: PASS / 0 issues.

- [ ] **Step 12: Commit**

```bash
git add internal/db/pgstore/migrate_test.go
git commit -m "test(pgstore): full Migrate parity with sqlitestore (synthetic, rollback, concurrent, guards, ladder, repair)"
```
