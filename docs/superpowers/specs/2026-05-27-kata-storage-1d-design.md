# Storage Phase 1d — package split, DSN selector, retry lift, credential redaction

Final lettered sub-phase of Phase 1 of the Postgres backend spec
(`docs/superpowers/specs/2026-05-26-kata-postgres-backend.md`, §1/§2/§6/§10). It
turns `internal/db` into the pure backend-neutral boundary by moving the SQLite
implementation into `internal/db/sqlitestore`, introduces a `storeopen` wiring
package as the DSN selector that returns `db.Storage`, lifts lock-retry into a
generic helper plus a `Storage` method, and lands DSN canonical-identity +
credential redaction so no DSN can leak before any Postgres code exists. No
migration runner and no `pgstore` (later phases).

## Goal

`internal/db` holds **no SQL** — only the `Storage` interface, the domain types,
the param structs, the export/import contract types, the sentinel errors, and a
generic transient-retry. The SQLite implementation lives in
`internal/db/sqlitestore` as `Store`. Opening a database goes through
`storeopen.Open`/`OpenReadOnly(ctx, dsn, opts...) (db.Storage, error)`,
dispatching on DSN scheme (sqlite / bare path now; `postgres://` → a clear,
**redacted** "not yet available" error). The remaining `db.Open` /
`db.OpenReadOnly` call sites flip to one of two openers. The true entry points
(`cmd/kata` import/export and the daemon-bootstrap open call) use
`storeopen.Open` / `OpenReadOnly` and hold `db.Storage`. The SQLite-bound
machinery (`internal/testenv` and jsonl's `cutover.go`/`preflight.go`) calls
`sqlitestore.Open` directly and holds the concrete `*sqlitestore.Store`, since
it needs raw `*sql.DB` access — daemon integration tests poke
`QueryRowContext`/`ExecContext` through `env.DB`, `exportForCutover` runs
version-dispatch SQL, and the FK preflight runs `PRAGMA foreign_key_check`.
Existing SQLite users see **no behavior change**.

## Background (from code exploration)

- `internal/db` today is a single package mixing the neutral contract and the
  SQLite impl: `storage.go` (the `Storage` interface), `types.go`,
  `db.go` (`type DB struct { *sql.DB; path string; instanceUID string }`,
  `Open(ctx, path) (*DB, error)`, `OpenReadOnly(ctx, path) (*DB, error)`,
  `Path()` → `d.path`), the `queries_*.go` / `store_*.go` query methods (with
  param/result structs intermixed), `schema.sql`, `lock_retry.go`
  (`RetryLockContention(ctx, op) error` + `IsLockContention(err) bool` keyed on
  `sqlite3.SQLITE_BUSY`/`SQLITE_LOCKED`, over a `backoff/v5` loop), and the 1c
  additions `export.go` / `import_replay.go` / `fkmeta.go`. The compile assertion
  `var _ Storage = (*DB)(nil)` lives in `storage.go`.
- 1b already moved the daemon (`daemon.Server.DB` is `db.Storage`) and the clean
  callers onto the interface; 1c removed `internal/jsonl`'s `*db.DB` dependency.
  The remaining concrete `*db.DB` holders are `cmd/kata` import/export, the
  daemon-bootstrap `db.Open` call (`daemon_cmd.go`), `internal/testenv` (the `DB`
  field + `serveDaemon`), `cutover.go` (both `db.Open` for the cutover target and
  `db.OpenReadOnly` for the source), and `preflight.go` (`db.OpenReadOnly`).
- The daemon handlers `handlers_actions.go` / `handlers_ownership.go` wrap
  **single** store writes (`CloseIssue`, `ReopenIssue`, `ClaimOwner`) in
  `db.RetryLockContention(ctx, func() error { ... })` — lock-retry is
  caller-visible, not internal to the store.
- `config.DBHash(dbPath) = sha256(filepath.Abs(dbPath))[:12]` namespaces the
  runtime dir / socket / hooks dir; `config.KataDB()` resolves `KATA_DB` or
  `<KATA_HOME>/kata.db`. `internal/config` and `internal/db` are **independent**
  (neither imports the other).

## Design

### The dividing line: contract in `db`, SQL in `sqlitestore`

The clean split is: **`internal/db` keeps every type named in the `Storage`
interface; `sqlitestore` gets every method body (the SQL).**

- **Stays in `internal/db` (neutral):** the `Storage` interface (`storage.go`);
  the domain types (`types.go` — `Project`, `Issue`, `Event`, …); the param
  structs lifted into a `params.go`; the export row structs (`MetaKV`,
  `ProjectExport`, … `SequenceExport`) and the import contract types
  (`ImportRecord`, `ImportOptions`, the import-kind constants, and
  `ImportRecord.validate`) — these are named in the `Export*` / `ImportReplay`
  interface signatures, so they are contract, not impl; the sentinel errors
  (`errors.go`); `CurrentSchemaVersion()` (a neutral accessor in
  `schema_version.go`; the backing `currentSchemaVersion = 10` const lives
  there too) — both `internal/jsonl/cutover.go` and `cmd/kata/daemon_cmd.go`
  call it without choosing a backend; a new `retry.go` (generic
  `RetryTransient`); and a new `OpenOption` type (`db.ReadOnly()`).
- **Moves to `internal/db/sqlitestore`:** the `Store` type (today's `*db.DB`:
  `type Store struct { *sql.DB; path string; instanceUID string }`); `open.go`
  (PRAGMAs, pool config, the read-only option); `transient.go` (`IsTransient` —
  today's `IsLockContention`); **all method bodies** — the `queries_*.go` /
  `store_*.go` SQL, the `export.go` `Export*` iterator bodies, the
  `import_replay.go` `ImportReplay` body + per-entity insert helpers,
  `PeekSchemaVersion` (which reads SQLite `meta.schema_version` via raw SQL),
  and `fkmeta.go`; plus `schema.sql`. The assertion becomes
  `var _ db.Storage = (*Store)(nil)`.

Concretely, `export.go` and `import_replay.go` **split**: their structs/contract
types stay in `internal/db`; their method bodies move to `sqlitestore`. The
`queries_*.go` extraction is the same shape — param/result structs to
`internal/db`, query methods to `sqlitestore`. Method bodies move with
their logic unchanged; the only edits are mechanical: the receiver
(`*DB`→`*Store`), package qualifiers (bare `Project` → `db.Project` for the
neutral types that stayed in `internal/db`), imports, the `//go:embed
schema.sql` path (which moves with `store.go`), and test package names. The
compiler enumerates the qualification work — each `undefined: X` is a neutral
symbol that needs a `db.` prefix. The existing `db`
tests move to `sqlitestore` (`package sqlitestore_test`) along with the methods
they exercise and are the behavior guard for the move; the few tests of neutral
contract types (e.g. `ImportRecord.validate`, the import-kind/JSONL-kind guard)
stay in `internal/db`.

### `storeopen` wiring package (the DSN selector)

A new package `internal/db/storeopen` imports **both** `internal/db` and
`internal/db/sqlitestore` and dispatches on DSN scheme. Nothing imports
`storeopen` except top-level entry points, so there is no cycle (`sqlitestore →
db` one way; `storeopen → {db, sqlitestore}` one way). This is why the selector
is **not** in `internal/db`: `db` must never import `sqlitestore`.

```
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error)
func OpenReadOnly(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error)
```

`OpenReadOnly` is kept as a named function (minimal caller churn); internally it
appends `db.ReadOnly()` and calls the same path. Dispatch:

- bare path (back-compat) and `sqlite:///abs/path` → `sqlitestore.Open(...)`
- `postgres://…` → a clear `"postgres backend not yet available"` error, with
  the DSN **redacted** (no password)
- unknown scheme → a clear error (Phase 1d dispatches on scheme only and does
  not validate query parameters)

**Scheme detection uses the literal `://` substring**, not `url.Parse` scheme
detection. Otherwise a Windows path like `C:\Users\you\kata.db` would parse as
scheme `c` and break the local-first path guarantee; the split-on-`://` rule
keeps every bare filesystem path (including Windows drive paths) on the SQLite
branch.

`db.OpenOption` is a functional-option type in the neutral package so both
`storeopen` and `sqlitestore` can reference it without a cycle; `sqlitestore.Open`
consumes `db.ReadOnly()` as sqlite `mode=ro`.

### Retry lift

- `internal/db/retry.go`: `RetryTransient(ctx context.Context, isTransient
  func(error) bool, op func() error) error` — the generic backoff loop (today's
  `retryLockContention` / `newLockBackOff` / `normalizedLockRetryConfig`,
  backend-neutral, over `backoff/v5`).
- `sqlitestore/transient.go`: `IsTransient(err error) bool` — today's
  `IsLockContention` (`SQLITE_BUSY` / `SQLITE_LOCKED` via the `sqliteCodeError`
  shape).
- The `Storage` interface gains one method:
  `RetryTransient(ctx context.Context, op func() error) error`. `sqlitestore.Store`
  implements it as `return db.RetryTransient(ctx, IsTransient, op)`; a future
  `pgstore` supplies its own predicate (40001 / 40P01). Each impl owns its
  transient-error knowledge; neutral callers retry arbitrary closures without
  knowing the backend.
- Callers: the daemon handlers change `db.RetryLockContention(ctx, op)` →
  `cfg.DB.RetryTransient(ctx, op)` (where `cfg.DB` is the `db.Storage` handle).
  Behavior is preserved — same closures, same backoff.

### DSN identity & credential redaction

Pure string helpers live in `internal/config` (it already owns DB identity /
resolution — `DBHash`, `KataDB` — and Phase 8's `KATA_DSN` resolution will land
there; keeping them here avoids a `config`↔`db` dependency, since the helpers
need no `db` types):

- `CanonicalDSNIdentity(dsn string) (string, error)` — for a `postgres://` DSN,
  returns `scheme://host[:port]/database` with credentials and all query params
  stripped, and with the default port `5432` normalized to no-port (so
  `postgres://host/db` and `postgres://host:5432/db` produce the same identity;
  non-default ports are preserved); for a bare path or `sqlite://` DSN, returns
  the path **exactly as given** — no `filepath.Abs`, no normalization. `DBHash`
  alone applies `filepath.Abs` to the SQLite path before hashing; the
  `postgres://` branch hashes this canonical identity directly.
- `RedactDSN(dsn string) string` — the DSN with any password removed, for safe
  display in errors and logs.

`config.DBHash` hashes the canonical identity; for a bare path / `sqlite://` it
keeps today's `sha256(filepath.Abs(path))` exactly (existing hashes / sockets /
hooks dirs unchanged), and only a `postgres://` DSN takes the credential-free
canonical form (no `filepath.Abs`, which §6 flags as the bug to avoid). If
`CanonicalDSNIdentity` returns an error (a malformed `postgres://` DSN),
`DBHash` hashes `RedactDSN(dsn)` instead of the raw DSN, so the hash stays
credential-free even when canonicalization fails.
`storeopen` uses `RedactDSN` when building the postgres-not-available error and
wraps any
`sqlitestore.Open` connection error so no raw DSN can surface. The SQLite
`Store.Path()` returns the path as today (paths are not secret). Even with no
Postgres code, this is exercised: a `postgres://user:pass@…` DSN must yield an
error and identity that never contain `pass`.

### Holder flips

`db.Open` and `db.OpenReadOnly` are **removed** — not renamed in place, and not
left as compatibility wrappers (a wrapper in `internal/db` would have to import
`sqlitestore`, violating the one-way import rule). Opening goes through one of
two doors:

- **Entry points → `storeopen` (holding `db.Storage`):** `cmd/kata`
  import/export and the daemon-bootstrap open call (`daemon_cmd.go`) call
  `storeopen.Open(path-or-dsn)`; `daemon.Server.DB` is already `db.Storage` (1b),
  so only its construction flips. `config.KataDB()` still yields the SQLite path,
  passed as a bare path → sqlite back-compat.
- **SQLite-bound machinery → `sqlitestore.Open` (holding the concrete
  `*sqlitestore.Store`):** `internal/testenv` (its `DB` field + `serveDaemon`),
  `cutover.go` (the cutover target and the export source), and `preflight.go`
  (the FK-check source) need the embedded `*sql.DB` for raw SQL — daemon
  integration tests poke `QueryRowContext`/`ExecContext` through `env.DB`,
  `exportForCutover` runs version-dispatch SQL, and the preflight resolver runs
  `PRAGMA foreign_key_list`. They call `sqlitestore.Open(path[, db.ReadOnly()])`
  and hold `*Store`, which satisfies `db.Storage` wherever the interface is
  wanted. This keeps `storeopen` for genuine backend selection while the SQLite
  cutover path stays explicitly SQLite-bound.
- `cutover.go`'s post-`Import` `target.ExecContext(... INSERT INTO meta
  schema_version ...)` re-stamp is **removed** — `ImportReplay` already stamps
  the current `schema_version`, so the re-stamp is redundant.
- If a flipped entry-point caller (one holding `db.Storage`) calls a method
  **not** on `Storage`, that is a signal it needs a domain method on the
  interface — surface it; do **not** re-expose the embedded `*sql.DB`.

## Build sequence

Implemented as small, independently green steps — each keeps `go build ./...`
and `go test ./...` passing, so mechanical moves and semantic changes land as
separate reviewable commits rather than one large refactor. The task-by-task
detail (exact files, code, and commands) lives in
`docs/superpowers/plans/2026-05-27-kata-storage-1d-package-split.md`; the order:

1. `config.CanonicalDSNIdentity` + `RedactDSN` (test-first).
2. DSN-aware `config.DBHash` — the SQLite hash stays byte-identical; a
   `postgres://` DSN uses the credential-free canonical identity.
3. Lift retry into generic `db.RetryTransient` + `Storage.RetryTransient`; flip
   the three daemon handlers (`CloseIssue`, `ReopenIssue`, `ClaimOwner`); remove
   `RetryLockContention`.
4. Extract the neutral contract — param/result structs, export row structs,
   import contract types, sentinel `Err*` vars, exported error structs — into
   `db`-resident files. Pure in-package reorganization.
5. Move the SQLite impl to `sqlitestore` (`DB`→`Store`), add `db.OpenOption` +
   `storeopen`, flip every opener, remove `db.Open`/`db.OpenReadOnly`, relocate
   the impl test suite, and drop the cutover re-stamp. This is necessarily one
   commit — a Go type's methods cannot span packages, and removing
   `db.Open`/`db.OpenReadOnly` breaks every caller at once, so no green
   intermediate separates the move from the opener flips; the plan's per-task
   steps give the reviewable granularity within that commit.
6. `storeopen` scheme dispatch + credential redaction (test-first).

Steps 1–3 are additive; 4 is an in-package reorg; 5 is the atomic move guarded
by the relocated suite plus `var _ db.Storage = (*sqlitestore.Store)(nil)`;
6 adds the redaction guard.

## Out of scope (later phases)

- The migration runner (Phase 2), `pgstore` (Phase 3+), `migrate-backend`
  (Phase 7), and the `KATA_DSN` / `[storage].dsn` config resolution (Phase 8 —
  `KATA_DB` stays honored, no behavior change). 1d only makes `Open` DSN-shaped
  and lands the redaction plumbing; `postgres://` is reachable solely by explicit
  input → a redacted unsupported error.

## Testing

- **Behavior-preserving:** `go build ./...` and `go test ./...` stay green
  throughout. The bulk of the existing `internal/db` suite moves to
  `sqlitestore` (`package sqlitestore_test`) and is the guard that the move + the
  param/struct extraction changed no behavior; method bodies move with their
  logic unchanged (mechanical edits only). (Tests of neutral contract types stay
  in `internal/db`.)
- **`storeopen` dispatch:** bare path → a SQLite `Store`; `sqlite:///path` → a
  SQLite `Store`; `postgres://…` → the "not yet available" error; unknown scheme
  → error.
- **Redaction (non-vacuous):** a `postgres://user:SECRET@host/db` DSN → the
  returned error string, `CanonicalDSNIdentity`, and `RedactDSN` never contain
  `SECRET`. Mutation-prove by confirming a naive `%w` of the raw DSN *would*
  contain it, so the assertion bites.
- **Retry:** the lock-retry tests split — `IsTransient` in `sqlitestore`, the
  generic loop in `internal/db`; add a test that `store.RetryTransient` retries a
  simulated transient error and gives up on a permanent one.
- **`DBHash` canonical:** the SQLite path hash is unchanged (pin the existing
  value); a `postgres://` canonical form is stable across working directories
  and strips credentials / incidental params.
- **Compile assertion:** `var _ db.Storage = (*sqlitestore.Store)(nil)`.

## Success criteria

- `internal/db` holds no SQL: `rg` finds no
  `QueryContext`/`ExecContext`/`BeginTx`/`PRAGMA`/`sqlite_sequence` in
  `internal/db/*.go` (only under `sqlitestore`).
- `storeopen.Open`/`OpenReadOnly` return `db.Storage`; the `db.DB` type is gone
  (renamed `sqlitestore.Store`), so no `*db.DB` references remain anywhere — the
  build enforces this, and `rg` confirms `cmd/kata`, `internal/testenv`, and
  `internal/jsonl` are clean.
- The `Storage` interface includes `RetryTransient`;
  `var _ db.Storage = (*sqlitestore.Store)(nil)` compiles; there is no
  `db`↔`sqlitestore` import cycle.
- A password-bearing `postgres://` DSN never leaks via `Path()`, errors, or
  logs; `DBHash` hashes only the credential-free canonical form (test-proven).
- `go build ./...` and `go test ./...` are green; `golangci-lint run` reports 0
  issues.
- Existing SQLite users see no behavior change: `KATA_DB` still works and the
  sqlite `DBHash` is unchanged.

## Risks

- The package move is large and the param/contract-type extraction is surgical
  (structs to `internal/db`, method bodies to `sqlitestore`). Mitigate: move
  method bodies with their logic unchanged (mechanical edits only); extract
  structs mechanically; lean on the moved
  test suite as the behavior guard; do it as small, independently-green steps.
- The import cycle is the central constraint. The `storeopen` wiring package and
  putting `OpenOption` + all interface-named types in `internal/db` are what
  break it; a stray `internal/db` → `sqlitestore` import would reintroduce it.
  Guard with the build and an explicit check that `internal/db` imports no
  backend package.
- Adding `RetryTransient` to `Storage` means `sqlitestore.Store` must implement
  it and the handler call sites must all flip (`rg` for `RetryLockContention` →
  none outside the moved retry code).
- Redaction must cover every surface (`Path`, error wrapping, logs, `DBHash`
  input); the redaction test asserts the secret substring is absent across them.
