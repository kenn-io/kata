# Storage Phase 2 — migration runner, ApplyMigrations policy, baseline adoption, retained pre-v10 cutover handoff

> Phase 2 of the kata Postgres-backend work (parent spec `docs/superpowers/specs/2026-05-26-kata-postgres-backend.md` §10). Phase 1d landed `db.Storage`, `internal/db/sqlitestore`, `internal/db/storeopen`, and `db.OpenOption` (currently just `ReadOnly()`). Phase 2 replaces the current "bootstrap-or-JSONL-cutover" dichotomy with a forward-only migration runner shared across backends, hidden behind an opt-in `db.ApplyMigrations()` policy that lives on the open API. Existing v10 SQLite users see **no behavior change** (the runner is a no-op for an at-current DB).

## Goal

`storeopen.Open(ctx, dsn, db.ApplyMigrations())` is the single entry point that brings any kata database up to current. The runner peeks the schema version, drives JSONL cutover for SQLite DBs below v10, opens the store, and applies every pending migration (every file whose embedded version exceeds `meta.schema_version`) in order under a backend-appropriate write lock — that includes `0010_baseline.sql` for a fresh DB, not just `11+`. Without `ApplyMigrations`, `storeopen.Open` refuses to mutate schema and returns `db.ErrSchemaOutOfDate` with a remediation message naming `kata migrate`. The daemon passes `ApplyMigrations()` on startup (preserving today's auto-cutover ergonomics); direct-open CLI paths (`kata export`/`import`) do not, so no shared DB can be silently migrated by a co-located CLI invocation. The runner abstraction is method-on-`Storage` so each backend owns its own embed.FS, locking idiom, and recovery strategy; only `sqlitestore.Store.Migrate` is implemented in this phase — `pgstore.Store.Migrate` arrives with pgstore in Phase 3 and conforms to the same contract.

## Background (from code exploration)

- After Phase 1d, `sqlitestore.Open` runs `bootstrap()` which either applies `schema.sql` to a fresh DB, no-ops on a v10 DB, or returns `db.ErrSchemaCutoverRequired` when the DB is older. The daemon (`cmd/kata/daemon_cmd.go`) currently does the pre-Open dance: `sqlitestore.PeekSchemaVersion` → if `< db.CurrentSchemaVersion()`, call `jsonl.AutoCutover` → then `storeopen.Open`. `cmd/kata/export.go` and `cmd/kata/import.go` just call `storeopen.Open` and surface whatever error comes back.
- `internal/jsonl/cutover.go` (`AutoCutover`) is load-bearing for upgrading SQLite DBs that predate `meta.schema_version`. It exports the source DB to JSONL, imports into a fresh v10 SQLite file via `sqlitestore.Open`, and renames into place after a timestamped backup. It does not touch `meta.schema_version` directly — `ImportReplay` stamps it.
- `db.CurrentSchemaVersion() == 10`, defined in `internal/db/schema_version.go`. There is no migration ladder today: every schema change since the project began is folded into `schema.sql`.
- `db.OpenOption` is a functional-option type (`internal/db/open.go`). Today only `db.ReadOnly()` exists, fed through `db.OpenConfig` → `sqlitestore.Open`. Adding `db.ApplyMigrations()` follows the same shape.
- Import direction after Phase 1d is one-way: `sqlitestore → db`, `jsonl → {db, sqlitestore}`, `storeopen → {db, sqlitestore}`. Phase 2 adds `storeopen → jsonl` so the runner can drive cutover internally. `jsonl → storeopen` is forbidden (would cycle); `jsonl` only ever opens the cutover target via `sqlitestore.Open` directly.

## Design

### Architecture: storeopen as the single orchestrator

`storeopen.Open(ctx, dsn, opts...)` becomes the only function callers use to bring a DB online. **Signature changes** to:

```go
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, db.MigrationResult, error)
```

The `MigrationResult` carries `{From, To, Applied}` describing what (if anything) the migration did. It is zero-valued when no migration ran (no `ApplyMigrations` option, or no pending work). All existing callers of the Phase 1d two-return-value `storeopen.Open` are updated to receive three values; callers that don't care about the result assign it to `_`. The CLI consumes it.

Behavior under `db.ApplyMigrations()`:

- Peek the version (SQLite: `sqlitestore.PeekSchemaVersion(ctx, path)`; Postgres in Phase 3: `pgstore.PeekSchemaVersion(ctx, dsn)` via the corresponding helper).
  - If the peek returns a "file does not exist" error (SQLite's read-only open fails on a missing file), treat as a fresh DB and skip the cutover step — `sqlitestore.Open` below will create the file and `Migrate` will apply `0010_baseline.sql`. Other peek errors (corruption, permission denied) propagate.
  - If the peek succeeds and the returned version is `< 10`: call `jsonl.AutoCutover(ctx, path)`. The cutover produces a v10 SQLite DB at the same filesystem path (its existing semantics, unchanged). Re-peek to confirm version `>= 10`. The "file exists but has no `meta` table" case returns `(0, nil)` from peek per the existing implementation and goes down this cutover branch too (cutover on an empty source produces an empty v10 DB — equivalent to a fresh create, just slower; documented as expected but rare in practice).
- Open the backend (`sqlitestore.Open(ctx, path, opts...)` — opts forwarded so `ReadOnly` still works; ApplyMigrations is consumed by storeopen, not by sqlitestore).
- Call `store.Migrate(ctx)` to apply any pending migrations (including `0010_baseline.sql` for a fresh DB where the file was just created). The result is returned through storeopen's second return value.

Behavior without `db.ApplyMigrations()`:

- Peek the version.
  - If the peek returns a "file does not exist" error, return `db.ErrSchemaOutOfDate` with the message `"database does not exist at <path>; run kata migrate"`. The orchestrator does NOT create the file in this branch — silently creating an empty DB would surprise CLI callers who expect a missing file to be an error.
  - Other peek errors (corruption, permission denied) propagate.
- If the gap is non-zero (`< 10` for SQLite, or `>= 10` with newer files embedded), return `db.ErrSchemaOutOfDate` wrapping a message like `"schema_version 8, current 13: needs JSONL cutover to v10 + migrations 11..13; run kata migrate"`. For `>= 10` the cutover phrase is omitted. `MigrationResult` is zero.
- Otherwise open and return; `MigrationResult` is zero.

Read-only opens (`storeopen.OpenReadOnly`) never migrate (it's nonsensical to mutate schema with `mode=ro`) and surface `ErrSchemaOutOfDate` on a stale DB.

Cycle check: `storeopen → {db, sqlitestore, jsonl}`; `jsonl → {db, sqlitestore}`; `sqlitestore → db`. One-way. `jsonl` continues to use `sqlitestore.Open` directly for cutover's source/target handles (not `storeopen.Open`), so `jsonl → storeopen` never exists.

### `Storage` interface addition

Two new declarations land in the neutral `db` package:

```go
// internal/db/open.go
type OpenConfig struct {
    ReadOnly         bool
    ApplyMigrations  bool  // new
}
func ApplyMigrations() OpenOption {
    return func(c *OpenConfig) { c.ApplyMigrations = true }
}

// internal/db/migrations.go (new neutral file)
type MigrationResult struct {
    From    int   // meta.schema_version before the run
    To      int   // meta.schema_version after the run (== current expected)
    Applied []int // ordered versions advanced through; nil if already current
}

// internal/db/errors.go — replace ErrSchemaCutoverRequired with:
var ErrSchemaOutOfDate = errors.New("schema out of date")

// internal/db/storage.go — add to the identity/lifecycle group:
Migrate(ctx context.Context) (MigrationResult, error)
```

`ErrSchemaCutoverRequired` is deleted; every existing reference (jsonl/cutover.go's bootstrap-error check, daemon error routing, tests) flips to `ErrSchemaOutOfDate`. The two error cases — pre-v10 SQLite vs `>= 10` with pending — collapse into one sentinel with a discriminating message; callers who need the distinction read it from the message rather than the type. This is intentional: the user-facing remedy is the same (`kata migrate`), and the runner removes the daemon's need to distinguish at all.

`Storage.Migrate(ctx) (MigrationResult, error)` is the only new interface method. It is idempotent: when no migrations are pending, it returns `MigrationResult{From: N, To: N, Applied: nil}` with no error. It is **only meaningful when called on a writable backend** — `Storage` opened with `db.ReadOnly()` returns an error if Migrate is called (backend-defined, but conventionally a wrap of `errors.New("migrate: backend is read-only")`).

### `sqlitestore` implementation

**Migration file layout.** `internal/db/sqlitestore/schema.sql` becomes `internal/db/sqlitestore/migrations/0010_baseline.sql` byte-for-byte; the existing `//go:embed schema.sql var schemaSQL string` directive is replaced by `//go:embed migrations/*.sql var migrationsFS embed.FS`. Naming convention: `<4-digit-version>_<name>.sql`. Phase 2 ships only `0010_baseline.sql`; Phase 3+ may add `0011_*.sql` and later files. The runner discovers pending migrations by listing `migrationsFS`, parsing the version prefix, and selecting files whose version exceeds `meta.schema_version`.

**Test injection seam.** `Storage.Migrate` reads from a package-level `var migrationsSource fs.FS = migrationsFS` rather than `migrationsFS` directly. The default value is the embedded `migrationsFS`; tests in the `sqlitestore` internal-test package override `migrationsSource` (e.g., to an `fstest.MapFS` carrying a synthetic `0011_test.sql`) to exercise the multi-step loop, version-bump-per-step, snapshot lifecycle, and rollback paths without needing a real 11+ migration in tree. The seam is an unexported variable so external callers cannot tamper.

**`bootstrap()` is removed; `instance_uid` handling splits between `Open` and `Migrate`.** Today's `bootstrap` either applied schema.sql to a fresh DB, no-oped on a v10 DB, or returned `ErrSchemaCutoverRequired`. After Phase 2:

- `sqlitestore.Open` opens the file, pings, and conditionally reads `meta.instance_uid` into the cached `Store.instanceUID` — but only if the `meta` table exists. If it doesn't (fresh DB), the cache is left empty and `Open` does NOT error. `Open` also does NOT run any DDL and does NOT stamp anything.
- `Migrate` is the only writer. It creates `meta` (if missing) inside the same transaction as the first DDL step (`0010_baseline.sql` already includes the `CREATE TABLE meta` statement), applies every pending file in order, stamps `meta.schema_version` per step, and as its final pre-commit step ensures `meta.instance_uid` is populated (read existing if `Open` already cached it; otherwise generate via `uid.New()` and `INSERT ... ON CONFLICT DO NOTHING`). The cached `Store.instanceUID` is updated post-commit.

For a daemon opening a v10 DB **without** `ApplyMigrations`: `Open` reads existing `instance_uid` into the cache, returns the working handle, and `Migrate` is never called. For a daemon opening a fresh DB **with** `ApplyMigrations`: `Open` finds no meta and leaves the cache empty; `storeopen` then calls `Migrate`, which creates everything and populates the cache. Either way the returned handle is operational. The handle returned from a path where `Migrate` is not called and `meta` did not exist (i.e. `Open` without `ApplyMigrations` on a fresh DB) never reaches the caller, because storeopen returns `ErrSchemaOutOfDate` before yielding the handle.

**`Migrate(ctx)` flow.** The order matters: SQLite refuses `VACUUM INTO` while a transaction is open, so the snapshot is taken **before** the migration lock is acquired.

- Peek `meta.schema_version` outside any transaction (one `SELECT` if `meta` exists, else treat as version `0`). List `migrationsFS`, parse 4-digit version prefixes, select files where `version > current`, sort ascending. If empty: return `MigrationResult{From: current, To: current, Applied: nil}` (and ensure the `instance_uid` cache is populated as described above) — no snapshot, no lock, no commit.
- Compute the snapshot path as `filepath.Join(home, "runtime", config.DBHash(store.path), "premigrate-v<from>.db")` where `home` comes from `config.KataHome()` and `store.path` is the store's own filesystem path. This intentionally **does not** use `config.RuntimeDir()` — that helper returns the runtime dir for `KataDB()`, which is not necessarily the store being migrated (e.g., test temps, cutover targets). `mkdir -p` the parent. If a stale snapshot file already exists at the target path (residue from a prior failed run), delete it first — `VACUUM INTO` refuses to overwrite.
- Take the recovery snapshot: `VACUUM INTO '<snapshot path>'`. This must run with no transaction open.
- `BEGIN IMMEDIATE` to serialize concurrent migrators. Re-read `meta.schema_version`; if another migrator advanced it between the snapshot and the lock, re-derive the pending list and proceed only with what's still pending (or commit no-op if everyone caught up). Postgres in Phase 3 substitutes `pg_advisory_xact_lock`; the snapshot step is SQLite-only.
- For each pending version in order: apply the file's SQL inside the open transaction; execute `INSERT INTO meta(key,value) VALUES('schema_version', '<version>') ON CONFLICT(key) DO UPDATE SET value=excluded.value`.
- As the last pre-commit step: ensure `meta.instance_uid` (described above).
- Commit. On commit success, delete the snapshot file. On any per-step error, roll back; the snapshot is retained and the error message names its filesystem path. The lock releases on commit/rollback.

**Snapshot semantics.** SQLite's `VACUUM INTO 'p'` produces a consistent single-file copy that includes WAL-pending state, which a raw `cp` would miss. The snapshot path is namespaced by `config.DBHash(store.path)` so concurrent kata installations don't collide, and `<from>` is encoded so a half-completed multi-version migration leaves one snapshot per failed run (not a stack). The post-success delete is best-effort logged but the migration is still considered successful — an orphan snapshot only wastes disk, not data. **Race between snapshot and lock:** if a second migrator wins `BEGIN IMMEDIATE` and advances the version before the first migrator's `BEGIN IMMEDIATE` acquires the lock, the first migrator's snapshot is harmless (it captures a pre-this-migration state); the first migrator's re-read of `meta.schema_version` inside its transaction observes the advanced version and commits a smaller-or-empty Applied list. The snapshot is deleted as usual on commit.

**Lock + transaction interaction.** `BEGIN IMMEDIATE` writes a shared rollback journal entry that prevents any concurrent writer from starting a transaction; readers continue under WAL semantics. The full migration (every pending step + every version stamp) is one transaction so a failure at any step rolls back to the prior version. The lock is released on commit/rollback, so even a process kill mid-migration leaves the DB at the prior version and the snapshot on disk.

### CLI: `kata migrate`

`cmd/kata/migrate.go` adds a single command with no flags:

```
$ kata migrate
applied schema_version 11
applied schema_version 12
applied schema_version 13
migrated from 10 to 13 (3 versions applied)
```

Behavior: resolve the DB path via `config.KataDB()`, call `storeopen.Open(ctx, dbPath, db.ApplyMigrations())`. On success print one `"applied schema_version N"` line per element of `MigrationResult.Applied`, then a summary `"migrated from <from> to <to> (N versions applied)"` (or `"already current (schema_version <to>)"` if Applied is empty). Defer `store.Close`. Exit 0 on success; exit 1 on error.

On error, the error string is the runner's wrapped message (which already names the snapshot path for SQLite recovery). No additional formatting.

The command does **not** accept `--target-version`, `--dry-run`, `--rollback`, or `--list-pending`. YAGNI for Phase 2; future phases can add these without breaking compatibility because the runtime API is stable.

### Caller flips

- `cmd/kata/daemon_cmd.go`: the pre-open `PeekSchemaVersion → AutoCutover` triplet collapses to `storeopen.Open(ctx, dbPath, db.ApplyMigrations())`. `sqlitestore.PeekSchemaVersion` and `jsonl.AutoCutover` imports go away from this file.
- `cmd/kata/export.go`, `cmd/kata/import.go`: unchanged (continue to call `storeopen.Open` without `ApplyMigrations`); the error message they get for a stale DB now points at `kata migrate`.
- `internal/jsonl/cutover.go`: continues to use `sqlitestore.PeekSchemaVersion` and `sqlitestore.Open` directly. Its existing `db.CurrentSchemaVersion()` check (`if version >= db.CurrentSchemaVersion()` skip) is preserved. The post-`Import` block stays removed (already done in Phase 1d).
- Daemon tests, jsonl tests, cmd/kata tests: any test that previously relied on `ErrSchemaCutoverRequired` flips to `ErrSchemaOutOfDate`. Tests that opened DBs at non-current versions continue to work because the runner brings them current when `ApplyMigrations` is set.

## Out of scope (later phases)

- `pgstore.Store.Migrate` and any Postgres-side migration files — Phase 3 builds `pgstore` including its `Migrate` (using `pg_advisory_xact_lock` instead of `BEGIN IMMEDIATE`, no `VACUUM INTO` snapshot — Postgres relies on operator backups).
- The first non-baseline migration `0011_*.sql`. Phase 2 ships only `0010_baseline.sql`; introducing a real `0011` is a separate content-dependent commit driven by a real schema need.
- `migrate-backend` command (cross-backend cutover via JSONL) — Phase 7.
- `KATA_DSN` / `[storage].dsn` resolution — Phase 8.
- Backward migrations (`down.sql`). Forward-only by design; rollback is "restore from snapshot or backup."
- `--dry-run`, `--target-version`, `--list-pending` flags on `kata migrate` — future phases if needed.
- Migration-runner observability (metrics, structured per-step events). The CLI's stdout lines and the daemon log are sufficient for v1.

## Testing

- `internal/db/sqlitestore/migrate_test.go` (new):
  - **Fresh DB:** opening with `ApplyMigrations` applies `0010_baseline.sql`, ends at `schema_version=10`, `MigrationResult.From=0`, `To=10`, `Applied=[10]`.
  - **At-current:** opening a v10 DB with `ApplyMigrations` is a no-op; `Applied` is nil.
  - **Synthetic 11+:** the test overrides the `migrationsSource` injection seam (line 85) with an `fstest.MapFS` carrying a synthetic `0011_test.sql` (and optionally `0012_*`) plus the real baseline. No on-disk file is created in the source tree — using `fstest.MapFS` keeps the test self-contained and avoids any risk of the `//go:embed migrations/*.sql` pattern picking up test fixtures. The test verifies the run order, the per-version `meta.schema_version` stamps, and the snapshot lifecycle (created before, deleted after success).
  - **Failure path:** with the seam still overridden, a deliberately broken synthetic `0011` (e.g., `CREATE TABLE meta (...)` which conflicts because `0010_baseline.sql` already created it) causes rollback; `meta.schema_version` stays at 10; snapshot file remains on disk; returned error contains the snapshot path string.
  - **Concurrent migrators:** two goroutines call `Migrate` against the same DB; one wins, one returns the busy/locked error (or sees no pending after the winner commits). No double-application.
- `internal/db/storeopen/storeopen_test.go` (extend):
  - **TestOpenWithoutApplyMigrationsReturnsErrSchemaOutOfDate** — fixture DB at v0 (no meta) opened without `ApplyMigrations` returns `ErrSchemaOutOfDate`; error message contains `"kata migrate"`.
  - **TestOpenWithApplyMigrationsMigratesAndOpens** — same fixture with `ApplyMigrations` opens and returns a usable `db.Storage`.
  - **TestOpenWithApplyMigrationsRunsCutoverThenMigrate** — fixture at a pre-v10 schema (use a checked-in legacy fixture or generate via existing test helpers) is upgraded to v10 via cutover and any 11+ migrations applied, all in one `Open` call.
  - **TestOpenReadOnlyOnStaleReturnsErrSchemaOutOfDate** — `storeopen.OpenReadOnly` on a stale DB returns `ErrSchemaOutOfDate` regardless of `ApplyMigrations` (a read-only handle cannot migrate).
  - **TestOpenReadOnlyOnCurrentDropsApplyMigrationsSilently** — `storeopen.OpenReadOnly(..., db.ApplyMigrations())` on a v10 DB opens normally; `MigrationResult` is zero. The option is dropped because the handle is `mode=ro`.
- `cmd/kata/migrate_test.go` (new): exec the binary against a temp DB; assert stdout matches the format (one line per applied version + the summary); assert exit 0; assert the on-disk DB reaches `db.CurrentSchemaVersion()`. Failure case: a broken DB (test fixture) → exit 1, error on stderr, no partial migration.
- `internal/jsonl/cutover_test.go`: unchanged; cutover semantics are preserved.

## Success criteria

- `go test ./...` and `nix run 'nixpkgs#golangci-lint' -- run` both clean throughout the implementation.
- `storeopen.Open(ctx, dsn, db.ApplyMigrations())` is the only call that mutates schema; `sqlitestore.Open` no longer runs any DDL on its own (the `bootstrap` step is gone).
- `db.ErrSchemaCutoverRequired` does not exist anywhere in code; `rg -n 'ErrSchemaCutoverRequired' --glob '!docs/**'` returns no hits.
- `db.ErrSchemaOutOfDate` is returned by `storeopen.Open` for any non-current DB opened without `ApplyMigrations`, and its message names `kata migrate`.
- `kata migrate` brings any kata DB (fresh, v0..v9 legacy, v10, v10+pending) to current and reports the journey on stdout.
- The daemon (`cmd/kata/daemon_cmd.go`) opens via a single `storeopen.Open(ctx, dbPath, db.ApplyMigrations())` call; the pre-open peek+cutover gate is removed.
- Snapshot files at `<KataHome>/runtime/<DBHash(store.path)>/premigrate-v<N>.db` are created before a multi-version SQLite migration runs and deleted on success; failure leaves the file with the path named in the error.
- Existing v10 SQLite users see no behavior change at startup: `Migrate` is a no-op, no snapshot is created, the daemon comes up at the same speed.

## Risks

- **Snapshot disk pressure.** `VACUUM INTO` writes a full copy of the DB. For a multi-gigabyte DB on a constrained laptop this could fail on disk-full. Mitigate: document the requirement (free space ≥ DB size in the snapshot directory's filesystem — i.e. `<KataHome>/runtime/<DBHash(store.path)>`); add an explicit `os.Stat` check on that filesystem before kicking off the VACUUM if simple to do (else trust the VACUUM's own error). No retry on snapshot failure — the migration refuses to start.
- **Concurrent CLI + daemon migration.** `BEGIN IMMEDIATE` makes whoever loses race the second invocation. The losing caller currently sees a `SQLITE_BUSY` error wrapped in the runner's "lock acquire failed" message. For the daemon this is fine (the winning side has already migrated); for an interactive `kata migrate` it surfaces as exit 1 with a "another process is migrating" hint. Document but do not retry (kata's broader retry policy already covers transient lock contention; for migration we want the surface to be explicit).
- **Embedded-file integrity.** A migration file with a malformed prefix (`xxxx_*.sql` non-numeric) silently won't be applied. Mitigate: the runner returns an error if any embedded `*.sql` fails to parse a 4-digit version prefix; the build-time guard is the test that lists `migrationsFS` and asserts every entry parses.
- **Cutover dependency.** `storeopen` now imports `jsonl`. `jsonl` already imports `sqlitestore` (Phase 1d deviation). If a future change tries to make `jsonl` use `storeopen` (e.g., for `migrate-backend` in Phase 7), it would cycle. Document the prohibition: `jsonl` never imports `storeopen`. `migrate-backend` orchestrates from `cmd/kata` instead.
- **Read-only opens with `ApplyMigrations`.** Conflicting options. Design choice (testable): `OpenReadOnly` drops `ApplyMigrations` silently; the runner is never invoked on a read-only handle. Tested explicitly.
