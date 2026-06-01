# Kata Storage Phase 8 — DSN, config-file wiring — design

Status: draft for review
Date: 2026-05-29
Parent spec: `docs/superpowers/specs/2026-05-26-kata-postgres-backend.md` §6

## 0. Context

Phase 1d landed DSN scheme dispatch in `internal/db/storeopen/storeopen.go`,
the credential-free `config.CanonicalDSNIdentity` / `config.RedactDSN` helpers,
and the `config.DBHash` postgres branch. Phase 2 landed the migration runner.
Phases 3-7 (the actual postgres backend, conformance suite, `migrate-backend`)
are still pending — postgres DSNs today reach `storeopen.Open` and return
`postgres backend not yet available: <redacted>`.

Today, `cmd/kata`, `internal/daemon/namespace.go`, and `cmd/kata/daemon_logs.go`
all reach for `config.KataDB()`, which only honors `$KATA_DB` (existing) or
falls back to `<KATA_HOME>/kata.db`. There is no path from `$KATA_DSN`, no path
from a TOML config file, and no validation of obviously-wrong DSNs.

## 1. Goal

Plumb the DSN through user-facing configuration so when the pgstore backend
lands (Phases 3-7), users can opt in via existing, well-known mechanisms:

1. A new `$KATA_DSN` env var carries a full DSN (sqlite path, `sqlite://`,
   `postgres://`, `postgresql://`). Highest precedence.
2. A new `[storage].dsn` key in `<KATA_HOME>/config.toml` carries the same.
   Lower precedence than env.
3. Precedence resolution: `KATA_DSN` > `KATA_DB` > `[storage].dsn` > default
   `<KATA_HOME>/kata.db`.
4. Validation rejects obvious mismatches (PG-only query params on a sqlite DSN,
   unknown schemes) with a clear, credential-free error.
5. **Zero behavior change for existing users.** A laptop with only `$KATA_DB`
   set, or with neither set, lands at exactly the same path and exactly the
   same default as before.

Out of scope (already in Phase 1d):
- `DBHash` for postgres DSNs.
- Credential redaction primitives (`RedactDSN`, `CanonicalDSNIdentity`).

Out of scope (later phases):
- The pgstore backend itself (Phases 3-5).
- Cross-backend conformance + `migrate-backend` (Phases 6-7).

## 2. Architecture

A new resolver function lives in `internal/config/`:

```go
// KataDSN returns the effective database DSN honoring (in precedence order)
// $KATA_DSN, $KATA_DB, [storage].dsn from <KATA_HOME>/config.toml, and the
// <KATA_HOME>/kata.db default. The returned string is whatever the user
// supplied: a bare path, sqlite:// DSN, or postgres:// DSN. Callers pass it
// directly to storeopen.Open. Validation rejects obvious shape errors (unknown
// scheme, PG-only query params on a sqlite/bare DSN) with credential-free
// errors.
func KataDSN() (string, error)
```

`config.KataDB()` stays put as a deprecation-free intermediary: it now
delegates to `KataDSN()` and returns the result as-is (the old return is a
"path" only when no scheme is present, which matches today's behavior on
default and `$KATA_DB`-set setups). The five current callers of `KataDB()`
(`cmd/kata/daemon_cmd.go`, `cmd/kata/migrate.go`, `cmd/kata/export.go`,
`cmd/kata/daemon_logs.go`, `internal/daemon/namespace.go`) flip to `KataDSN()`
verbatim — the parameter they pass to `storeopen.Open` is already DSN-shaped
and `storeopen.Open` already accepts bare paths. The local variable name
(`dbPath`) stays put because `daemon.RuntimeRecord.DBPath` is a JSON-tagged
field already and changing the variable name does not change the schema; a
follow-up rename can rationalize the naming once postgres lands.

The `[storage]` table is added to `config.DaemonConfig`:

```go
type DaemonConfig struct {
    // existing fields ...
    Storage StorageConfig `toml:"storage"`
}

type StorageConfig struct {
    // DSN is the kata database DSN. A bare path or sqlite://path is the
    // SQLite backend; postgres:// or postgresql:// is the Postgres backend.
    // Empty value means "no override from the file" — env or default wins.
    DSN string `toml:"dsn"`
}
```

Validation lives in `KataDSN()` so a bad DSN from any source (env, file,
default) fails the same way at the same call site.

### 2.1 Precedence resolver

```go
func KataDSN() (string, error) {
    if v := strings.TrimSpace(os.Getenv("KATA_DSN")); v != "" {
        if err := validateDSN(v); err != nil {
            return "", err
        }
        return v, nil
    }
    if v := strings.TrimSpace(os.Getenv("KATA_DB")); v != "" {
        if err := validateDSN(v); err != nil {
            return "", err
        }
        return v, nil
    }
    cfg, err := ReadDaemonConfig()
    if err != nil {
        return "", err
    }
    if v := cfg.Storage.DSN; v != "" {
        if err := validateDSN(v); err != nil {
            return "", err
        }
        return v, nil
    }
    home, err := KataHome()
    if err != nil {
        return "", err
    }
    return filepath.Join(home, "kata.db"), nil
}
```

`cfg.Storage.DSN` is already TrimSpace'd by `ReadDaemonConfig`, so the
file-branch does not re-trim. Validation always runs against the trimmed
value so a stray trailing space doesn't leak into the error.

`ReadDaemonConfig()` already returns a zero-value `*DaemonConfig` for the
missing-file case, so the resolver works on a clean system. The env paths
short-circuit *before* the TOML read so a daemon-config typo cannot break
`$KATA_DSN` callers.

Concerns about `ReadDaemonConfig()`'s side-effect (`applyDaemonConfigEnv` reads
auth env vars and `validateAuthProxy` rejects misconfigs) are not a blocker:
the resolver runs in the same processes that already call `ReadDaemonConfig()`
on the daemon-start path. Tooling that just opens the DB (`kata export`,
`kata import`, `kata migrate`) does not currently call `ReadDaemonConfig()`,
but pulling its zero-config-fast-path through one extra TOML decode plus an
auth-block validation is bounded and matches what those commands would do
anyway if a typo existed in the file. The alternative — a separate "storage
section only" reader — duplicates the file-I/O and is rejected to keep one
TOML schema, one decoder, one validator.

The default branch returns `filepath.Join(home, "kata.db")` (a bare path), not
`"sqlite://" + filepath.Join(home, "kata.db")`. This is the **behavior-
preservation invariant**: the existing `config.DBHash(defaultPath)` and
`namespace.go` callers all consume bare paths today, and storeopen already
treats bare paths as sqlite. Adding the `sqlite://` prefix here would re-hash
every existing setup's runtime dir / socket / hooks dir.

### 2.2 Validation rules

`validateDSN(s)` returns the first hit, otherwise nil:

1. **Unknown scheme.** If `s` contains `://` and the scheme is not one of
   `sqlite`, `postgres`, `postgresql`, return
   `unsupported dsn scheme %q`. Mirrors what `storeopen.Open` already says so
   the error surface is identical whether validation fires here or there.
2. **PG-only query params on a sqlite/bare DSN.** If the scheme is sqlite or
   absent, and the input contains a `?` followed by any of `sslmode=`,
   `pool_max_conns=`, `application_name=`, `connect_timeout=`,
   `target_session_attrs=`, `password=`, `sslpassword=`, return a clear
   "sqlite DSN does not support %s; did you mean postgres://?" error. The
   list is the libpq / pgx query-param surface that has no SQLite analogue.
3. **Ambiguous credentials.** Reuse `CanonicalDSNIdentity` as a probe — if
   the DSN is `postgres://` and `CanonicalDSNIdentity` returns the
   "ambiguous credentials" error, propagate it. This catches unencoded `://`
   in passwords before storeopen.Open's downstream parse.

Validation **must not** reach out for connectivity, dial Postgres, or stat
the sqlite file. It is shape-only. Connection-time errors continue to surface
from `storeopen.Open` -> `sqlitestore.Open`/pgstore.Open at open time.

`validateDSN`'s error never echoes the password. It quotes only the scheme
and the offending query-param name. The DSN itself is redacted via
`RedactDSN` if it needs to appear at all in the error.

### 2.3 Path() / RuntimeRecord credential safety

The daemon writes `RuntimeRecord.DBPath = dbPath` directly into
`<KataHome>/runtime/<dbhash>/daemon.<pid>.json`, and `kata daemon status`
surfaces it. With a postgres DSN this leaks the password.

Phase 8 redacts at the write site: `cmd/kata/daemon_cmd.go` passes a
small redaction helper for the `DBPath` field when building
`daemon.RuntimeRecord`. `kata daemon status` reads the runtime files written
by the daemon, so a redacted `DBPath` is automatically redacted in every
status output (JSON and text). The in-process `dbPath` variable used to call
`storeopen.Open` keeps the raw DSN — only the persisted/displayed copy is
redacted.

The helper wraps `config.RedactDSN` so the bare-path and sqlite cases pass
through unchanged. The (defensive) fallback for ambiguous postgres
credentials returns the original DSN; that branch is unreachable in practice
because `KataDSN` validation already rejects the bleed shape before this
helper sees it.

`Path()` itself returns the raw path from `sqlitestore.Store` today (sqlite
paths are not secret). Postgres `Path()` redaction belongs to the pgstore
implementation in Phase 3; Phase 8 does not touch it. This is consistent
with the parent spec §6, which scopes `Path()` to the pgstore impl.

## 3. Config file format

`<KATA_HOME>/config.toml`:

```toml
# Existing keys keep working unchanged.
listen = "100.64.0.5:7777"

[tui]
mouse = true

[auth]
token = "..."

# New in Phase 8:
[storage]
dsn = "postgres://kata@db.internal/kata?sslmode=require"
```

The key is `[storage].dsn`, not `[storage].db` or `[storage].url`, because:
- It is what the parent spec §6 names.
- Postgres callers will frequently set query params, which is more naturally
  a DSN than a "path" or "url".
- Sqlite users either keep using `$KATA_DB` or write a bare path here, which
  is still a "DSN" in the canonical sense.

An empty `dsn = ""` is treated as "unset" so commenting/clearing the key in a
shared template doesn't change behavior. Whitespace is trimmed (matches
`Listen`).

A `[storage]` table with an unknown key (`[storage].foo = "bar"`) is rejected
by the existing `meta.Undecoded()` check in `ReadDaemonConfig` — no Phase 8
change needed.

## 4. Test plan summary

The implementation plan (next doc) details TDD steps. Phase 8 conformance is:

- `KataDSN()` precedence: env-DSN, env-DB, file, default.
- `KataDSN()` validation rejects unknown scheme, PG-only query params on
  sqlite DSNs, ambiguous credentials in a postgres DSN.
- `KataDSN()` validation error never echoes a postgres password.
- `KataDB()` continues to return the same string as `KataDSN()` (the rename is
  user-visible at the resolver only; `KataDB` becomes a thin alias).
- `[storage].dsn` is decoded from a real TOML fixture.
- An empty `[storage].dsn = ""` is ignored.
- A garbled `[storage].dsn` fails through `KataDSN()`.
- Existing `$KATA_DB`-only setups produce the same value before/after Phase 8.
- An end-to-end test verifies `KATA_DSN=postgres://...` reaches
  `storeopen.Open` and returns the "postgres backend not yet available"
  error without leaking the password.
- The `cmd/kata daemon status` JSON output redacts a postgres DSN.

## 5. Open questions

None blocking. The following are intentional choices:

- **Why one resolver, not two?** A separate `KataPGSensitive()` reader would
  let storage-only callers skip the auth-config validation; rejected for the
  duplication cost. One reader, one validator, one error surface.
- **Why not deprecate `$KATA_DB` now?** Parent spec §6 says no behavior change
  for existing users. Deprecation messaging belongs in a follow-up after the
  pgstore backend lands.
- **Why not validate scheme-internal shape (e.g. `postgres://` must have a
  host)?** That's `pgstore.Open`'s job. Phase 8 validation is shape-only and
  scheme-agnostic. Driving validation deeper now means re-validating once the
  pgstore lands.

## 6. Build sequence (preview for writing-plans)

One phase, one branch, one CI run. Tasks are TDD-ordered so each commit
is green:

1. Add `validateDSN` (internal) + `KataDSN()` resolver alongside `KataDB` with
   env-only precedence. New file `internal/config/dsn_resolve.go` + tests for
   env precedence, validation, credential-free errors.
2. Add `[storage].dsn` to `DaemonConfig` (decode-only, no wiring into
   `KataDSN` yet). Tests for the TOML decode path.
3. Wire `[storage].dsn` into `KataDSN` so all four precedence tiers work end
   to end.
4. Collapse `KataDB` to a thin alias delegating to `KataDSN`. New tests
   confirm the alias equivalence; existing `$KATA_DB`-only tests still pass.
5. Rename the five `config.KataDB()` callers to `config.KataDSN()` so the
   call sites match the new contract.
6. Redact `RuntimeRecord.DBPath` at the write site (`cmd/kata/daemon_cmd.go`).
   Tests confirm a postgres DSN in `$KATA_DSN` does not produce a
   credential-bearing `daemon.<pid>.json` or `kata daemon status` output.
7. End-to-end: `KATA_DSN=postgres://...` reaches `storeopen.Open` and gets
   the redacted "not yet available" error.

## 7. Success criteria

- `go build ./...` clean.
- `go test ./...` green, including new tests above.
- `nix run 'nixpkgs#golangci-lint' -- run ./...` clean.
- A clean home (`KATA_DSN`, `KATA_DB` unset, no `<KATA_HOME>/config.toml`)
  produces an identical resolved DSN as today.
- `KATA_DSN=postgres://user:SECRET@host/db` resolves through `KataDSN()`,
  reaches `storeopen.Open`, and the "not yet available" error contains
  neither `SECRET` nor the raw query string.
- `[storage].dsn = "postgres://..."` in `config.toml` reaches `storeopen.Open`
  when neither env var is set.
