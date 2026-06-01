# Storage Phase 1d — package split, DSN selector, retry lift, credential redaction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `internal/db` into the pure backend-neutral boundary: move the SQLite implementation into `internal/db/sqlitestore` (`Store` = today's `*db.DB`), add a `storeopen` DSN selector returning `db.Storage`, lift lock-retry into a generic `db.RetryTransient` plus a `Storage` method, and land credential-free DSN identity + redaction — all behavior-preserving for existing SQLite users.

**Architecture:** `internal/db` keeps every type named in the `Storage` interface (domain types, param/result structs, export row structs, import contract types), the sentinel `Err*` vars and exported error structs the daemon matches with `errors.As`, the sentinel `ErrSchemaCutoverRequired`, `CurrentSchemaVersion`, a generic transient-retry, and a functional `OpenOption`. The SQLite impl (the `Store` type + every SQL method body + `schema.sql` + the `IsTransient` predicate + the FK PRAGMA resolver) moves to `internal/db/sqlitestore`. A new `internal/db/storeopen` imports both and dispatches on DSN scheme. Import direction is one-way: `sqlitestore → db`, `storeopen → {db, sqlitestore}`, `jsonl → {db, sqlitestore}` (cutover/preflight are SQLite-only machinery). `internal/db` imports no backend package.

**Two deliberate deviations from the spec's holder-flip wording, both required for the code to compile and both consistent with the dividing line (code that needs raw SQL holds the concrete store; only true entry points use `storeopen`):**
- **`internal/testenv.Env.DB` becomes `*sqlitestore.Store`, not `db.Storage`.** Daemon integration tests call `env.DB.QueryRowContext(...)` / `env.DB.ExecContext(...)` (e.g. `internal/daemon/handlers_move_test.go:142`, `handlers_links_test.go:86`), which the interface does not expose. The harness legitimately holds the concrete store so tests can seed adversarial SQL state. `*Store` satisfies `db.Storage`, so the daemon under test still receives the interface via `cfg.DB`.
- **`internal/jsonl/cutover.go`, `cutover_export.go`, and `preflight.go` open via `sqlitestore` (concrete `*Store`), not `storeopen`.** `exportForCutover` runs version-dispatch raw SQL and the preflight FK resolver needs `QueryContext`; both need the concrete handle. Cutover is the SQLite upgrade path, so depending on `sqlitestore` there is correct.

Both deviations still satisfy the spec's success criteria: the `db.DB` type is gone, so `rg '\*db\.DB'` finds nothing in `cmd/kata`, `internal/testenv`, `internal/jsonl`.

**Tech Stack:** Go 1.26, `database/sql` over `modernc.org/sqlite`, `github.com/cenkalti/backoff/v5`, `github.com/stretchr/testify`. Lint: `nix run 'nixpkgs#golangci-lint' -- run`.

---

## File Structure

**New files:**
- `internal/config/dsn.go` — `CanonicalDSNIdentity`, `RedactDSN`, `splitScheme` (pure string helpers, no `db` types).
- `internal/config/dsn_test.go` — their tests.
- `internal/db/retry.go` — generic `RetryTransient(ctx, isTransient, op)` + neutral backoff machinery.
- `internal/db/retry_test.go` — the generic-loop tests (moved out of `lock_retry_test.go`).
- `internal/db/errors.go` — all exported sentinel `Err*` vars + exported error struct types (extracted from the impl files in Task 4).
- `internal/db/params.go` — exported param/result structs named in the `Storage` interface (extracted in Task 4).
- `internal/db/export_types.go` — the export row structs + `ExportFilter` (extracted in Task 4).
- `internal/db/import_types.go` — `ImportRecord`, `ImportOptions`, import-kind constants, `ImportRecord.validate`, and the `ImportBatch*` / `ImportMapping*` contract structs (extracted in Task 4).
- `internal/db/open.go` — `OpenConfig`, `OpenOption`, `ReadOnly()`, `ApplyOpenOptions` (Task 5).
- `internal/db/schema_version.go` — `currentSchemaVersion` const + `CurrentSchemaVersion()`, relocated from `db.go` in Task 5 (since `db.go` becomes `sqlitestore/store.go`, the neutral schema-version accessor must stay in `db`).
- `internal/db/sqlitestore/` — the relocated SQLite impl: `store.go` (was `db.go`, and still holds the openers `Open`/`PeekSchemaVersion`/`bootstrap`/`ensureInstanceUID`), `transient.go` (was the predicate half of `lock_retry.go`), all `queries_*.go` / `store_*.go` / `export.go` / `import_replay.go` / `imports.go` / `import_mappings.go` / `fkmeta.go` method bodies, `schema.sql`, and the moved `sqlitestore_test` suite (Task 5).
- `internal/db/storeopen/storeopen.go` — the DSN selector (Task 5; rich dispatch + redaction in Task 6).
- `internal/db/storeopen/storeopen_test.go` — dispatch + redaction tests (Task 6).

**Heavily modified:**
- `internal/config/paths.go` — `DBHash` becomes DSN-aware (Task 2).
- `internal/db/storage.go` — add `RetryTransient` to the interface; the `var _ Storage = (*DB)(nil)` assertion moves to `sqlitestore` in Task 5.
- `internal/db/lock_retry.go` — trimmed to the predicate in Task 3, then moved to `sqlitestore/transient.go` in Task 5.
- `internal/daemon/handlers_actions.go`, `handlers_ownership.go` — retry call sites (Task 3).
- `cmd/kata/export.go`, `cmd/kata/import.go`, `cmd/kata/daemon_cmd.go`, `internal/testenv/testenv.go`, `internal/jsonl/cutover.go`, `internal/jsonl/cutover_export.go`, `internal/jsonl/preflight.go` — open-call flips (Task 5).

---

## A note on TDD for this plan

Tasks 1, 2, 3, and 6 add or change **behavior** and are strict test-first: write the failing test, watch it fail for the right reason, write minimal code, watch it pass.

Tasks 4 and 5 are **behavior-preserving relocations** (moving declarations between files/packages and qualifying references). There is no new behavior to drive a new test, so the executable guard is the **existing suite** plus the compiler: `go build ./... && go test ./...` must stay green across the move, and `var _ db.Storage = (*sqlitestore.Store)(nil)` must compile. Do not invent tests for code whose behavior is unchanged — that would be the "pointless test" anti-pattern. The move is correct iff the suite that already exercises this behavior still passes after relocation.

---

## Task 1: `config.CanonicalDSNIdentity` + `RedactDSN`

**Files:**
- Create: `internal/config/dsn.go`
- Test: `internal/config/dsn_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/config/dsn_test.go`:

```go
package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestCanonicalDSNIdentityBarePathUnchanged(t *testing.T) {
	got, err := config.CanonicalDSNIdentity("/home/u/.kata/kata.db")
	require.NoError(t, err)
	assert.Equal(t, "/home/u/.kata/kata.db", got)
}

func TestCanonicalDSNIdentityPostgresStripsCredentialsAndParams(t *testing.T) {
	got, err := config.CanonicalDSNIdentity("postgres://user:SECRET@db.example.com:5432/kata?sslmode=require")
	require.NoError(t, err)
	assert.Equal(t, "postgres://db.example.com/kata", got)
	assert.NotContains(t, got, "SECRET")
}

func TestCanonicalDSNIdentityPostgresStripsDefaultPort(t *testing.T) {
	// 5432 is the postgres default port — strip it so the same logical DB
	// referenced with or without :5432 produces the same identity.
	withPort, err := config.CanonicalDSNIdentity("postgres://host:5432/kata")
	require.NoError(t, err)
	noPort, err := config.CanonicalDSNIdentity("postgres://host/kata")
	require.NoError(t, err)
	assert.Equal(t, noPort, withPort)
	assert.Equal(t, "postgres://host/kata", withPort)

	// A non-default port is preserved.
	custom, err := config.CanonicalDSNIdentity("postgres://host:6543/kata")
	require.NoError(t, err)
	assert.Equal(t, "postgres://host:6543/kata", custom)
}

func TestCanonicalDSNIdentityPostgresNoPortOmitsColon(t *testing.T) {
	got, err := config.CanonicalDSNIdentity("postgres://user:SECRET@db.example.com/kata")
	require.NoError(t, err)
	assert.Equal(t, "postgres://db.example.com/kata", got)
}

func TestCanonicalDSNIdentityUnknownSchemeErrors(t *testing.T) {
	_, err := config.CanonicalDSNIdentity("mysql://h/db")
	require.Error(t, err)
}

func TestRedactDSNRemovesPassword(t *testing.T) {
	got := config.RedactDSN("postgres://user:SECRET@db.example.com:5432/kata?sslmode=require")
	assert.NotContains(t, got, "SECRET")
	assert.Contains(t, got, "user")
	assert.Contains(t, got, "db.example.com")
	// Mutation guard: the raw DSN really does contain the secret, so the
	// NotContains assertion above is non-vacuous.
	assert.Contains(t, "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require", "SECRET")
}

func TestRedactDSNBarePathUnchanged(t *testing.T) {
	assert.Equal(t, "/home/u/.kata/kata.db", config.RedactDSN("/home/u/.kata/kata.db"))
}

func TestRedactDSNStripsCredentialsInQueryString(t *testing.T) {
	// Postgres URLs can carry credentials in the query (libpq accepts
	// ?password=...&sslpassword=...) — RedactDSN drops the whole query for
	// display so the leak surface stays bounded regardless of which key
	// carries the secret.
	dsn := "postgres://db.example.com/kata?password=SECRET&sslmode=require" //nolint:gosec // fixture proves the query-string credential is dropped
	got := config.RedactDSN(dsn)
	assert.NotContains(t, got, "SECRET")
	assert.NotContains(t, got, "password=")
	assert.NotContains(t, got, "?")
	// Mutation guard: the raw DSN really does contain the secret.
	assert.Contains(t, dsn, "SECRET")
}

func TestCanonicalDSNIdentityRejectsAmbiguousCredentials(t *testing.T) {
	// A password containing unencoded "://" confuses url.Parse (u.User comes
	// back nil, the credential ends up in u.Host/u.Path). Defensive: refuse to
	// canonicalize rather than emit an identity that embeds the credential.
	_, err := config.CanonicalDSNIdentity("postgres://user:p://w@host/db")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "p://w")
	// Mutation guard: the raw input really does contain the credential chars.
	assert.Contains(t, "postgres://user:p://w@host/db", "p://w")
}

func TestRedactDSNRejectsAmbiguousCredentials(t *testing.T) {
	// Same defensive behavior for redaction: an unencoded "://" in the password
	// makes url.Parse populate User=nil; without a defense, the input would be
	// echoed unchanged. Returning "" is safe.
	got := config.RedactDSN("postgres://user:p://w@host/db")
	assert.Equal(t, "", got)
	// Mutation guard.
	assert.Contains(t, "postgres://user:p://w@host/db", "p://w")
}

func TestCanonicalDSNIdentityErrorOmitsCredentials(t *testing.T) {
	// url.Parse returns a *url.Error whose Error() includes the raw input;
	// wrapping with %w would leak the password through error logs.
	_, err := config.CanonicalDSNIdentity("postgres://user:SECRET@host:badport/db")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET")
	// Mutation guard.
	assert.Contains(t, "postgres://user:SECRET@host:badport/db", "SECRET")
}

func TestCanonicalDSNIdentityPreservesAtInDatabasePath(t *testing.T) {
	// A legitimate "@" in the database-path segment (e.g. pgbouncer-style
	// dbname@tenant) yields u.Path == "/db@tenant", which does NOT match the
	// credential-bleed shape (the bleed produces a path starting with "//"
	// because the misparsed "://" leaves a residual slash). Must canonicalize
	// normally.
	got, err := config.CanonicalDSNIdentity("postgres://host/db@tenant")
	require.NoError(t, err)
	assert.Equal(t, "postgres://host/db@tenant", got)
}

func TestRedactDSNPreservesAtInDatabasePath(t *testing.T) {
	// Same as above for redaction: a path-segment "@" must round-trip cleanly.
	got := config.RedactDSN("postgres://host/db@tenant")
	assert.Equal(t, "postgres://host/db@tenant", got)
}

func TestCanonicalDSNIdentityBracketsIPv6Host(t *testing.T) {
	// IPv6 hosts must be bracketed in the canonical form so the result is a
	// valid URL and two semantically distinct DSNs cannot collide.
	got, err := config.CanonicalDSNIdentity("postgres://user:SECRET@[::1]:5432/kata")
	require.NoError(t, err)
	// Default port (5432) is stripped; brackets stay.
	assert.Equal(t, "postgres://[::1]/kata", got)
	assert.NotContains(t, got, "SECRET")

	got2, err := config.CanonicalDSNIdentity("postgres://[2001:db8::1]:6543/kata")
	require.NoError(t, err)
	assert.Equal(t, "postgres://[2001:db8::1]:6543/kata", got2)

	// Mutation guard: confirm the input had the secret + a non-default port.
	assert.Contains(t, "postgres://user:SECRET@[::1]:5432/kata", "SECRET")
}
```

The last four tests defend credential-leak edge cases the original spec missed: passwords containing literal `://` that fool `url.Parse` into setting `u.User == nil` and embedding the credential in the host/path; `url.Parse` errors whose default `%w` wrap would echo the raw DSN (including the password) into logs; and IPv6 hosts that must be bracketed in the canonical form so the result is a valid URL and two semantically distinct DSNs cannot collide.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Test(CanonicalDSN|RedactDSN)' -v`
Expected: FAIL — `undefined: config.CanonicalDSNIdentity` / `config.RedactDSN`.

- [ ] **Step 3: Write the implementation**

Create `internal/config/dsn.go`:

```go
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CanonicalDSNIdentity returns a stable, credential-free identity for a database
// DSN, used to namespace per-database runtime state. A bare filesystem path or
// sqlite:// DSN returns the path (the SQLite identity has always been its path).
// A postgres:// DSN returns scheme://host[:port]/db with userinfo and every
// query parameter stripped, so the identity never carries a password and does
// not vary with incidental connection options. The postgres default port (5432)
// is normalized to no-port so the same logical DB referenced with or without
// :5432 produces the same identity. IPv6 hosts are emitted bracketed so the
// result is a valid URL. Malformed DSNs (where url.Parse produces an ambiguous
// host that may embed unencoded credentials) yield a credential-free error.
func CanonicalDSNIdentity(dsn string) (string, error) {
	scheme, rest, hasScheme := splitScheme(dsn)
	if !hasScheme {
		return dsn, nil
	}
	switch scheme {
	case "sqlite":
		return strings.TrimPrefix(rest, "//"), nil
	case "postgres", "postgresql":
		u, err := url.Parse(dsn)
		if err != nil {
			// url.Parse wraps the raw input in its error message — never
			// propagate it; the input may carry credentials.
			return "", errors.New("parse postgres dsn: invalid url")
		}
		if ambiguousUserinfo(u) {
			return "", errors.New("parse postgres dsn: ambiguous credentials (require percent-encoding)")
		}
		host := u.Hostname()
		dbName := strings.TrimPrefix(u.Path, "/")
		port := u.Port()
		if port == "5432" {
			// Postgres default — normalize to no-port so the same logical DB
			// referenced with or without :5432 produces the same identity.
			port = ""
		}
		return "postgres://" + hostPortString(host, port) + "/" + dbName, nil
	default:
		return "", fmt.Errorf("unsupported dsn scheme %q", scheme)
	}
}

// RedactDSN returns dsn with any password removed, safe for errors and logs.
// A scheme-less input (no "://") is treated as a filesystem path and returned
// unchanged; libpq key=value DSNs are not supported and should not be passed
// here. An unparseable or ambiguous DSN returns "" so a malformed string can
// never echo embedded credentials. The query string is dropped entirely —
// postgres URLs can carry credentials there too (e.g. ?password=SECRET,
// ?sslpassword=...), and keeping a maintained allowlist is fragile, so the
// safer default is to redact the whole query for display.
func RedactDSN(dsn string) string {
	if _, _, hasScheme := splitScheme(dsn); !hasScheme {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	if ambiguousUserinfo(u) {
		return ""
	}
	if u.User != nil {
		if _, hasPwd := u.User.Password(); hasPwd {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	u.RawQuery = ""
	return u.String()
}

// ambiguousUserinfo reports whether url.Parse produced the credential-bleed
// shape: u.User is nil but the residual structure shows that an unencoded
// "://" in the password confused the parser. The two shapes:
//   - "@" in u.Host: the userinfo fell into the host segment.
//   - u.Path begins with "//" AND contains "@": the misparsed "://" left a
//     residual leading slash and the credential leaked into the path.
//
// A legitimate "@" in a database path (e.g. "postgres://host/db@tenant")
// yields path "/db@tenant" — single leading slash — which is NOT this shape
// and must canonicalize/redact normally. Treating only the bleed shape as an
// error closes the credential-leak path without rejecting valid @-bearing
// database paths.
func ambiguousUserinfo(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	if strings.Contains(u.Host, "@") {
		return true
	}
	return strings.HasPrefix(u.Path, "//") && strings.Contains(u.Path, "@")
}

// hostPortString emits a postgres canonical host[:port] segment. IPv6 hosts
// are bracketed unconditionally so the output is a valid URL: "[::1]" with
// no port, "[::1]:6543" with a non-default port. IPv4/hostname forms emit
// without brackets.
func hostPortString(host, port string) string {
	if strings.Contains(host, ":") {
		// IPv6: always bracket.
		if port == "" {
			return "[" + host + "]"
		}
		return "[" + host + "]:" + port
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// splitScheme splits "scheme://rest". Reports hasScheme=false for inputs with
// no "://" (bare filesystem paths, including Windows drive paths).
func splitScheme(dsn string) (scheme, rest string, hasScheme bool) {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return "", dsn, false
	}
	return dsn[:i], dsn[i+len("://"):], true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS (all `TestCanonicalDSN*` and `TestRedactDSN*`).

- [ ] **Step 5: Commit**

```bash
git add internal/config/dsn.go internal/config/dsn_test.go
git commit -m "feat(config): add credential-free DSN identity and redaction"
```

---

## Task 2: DSN-aware `config.DBHash`

`DBHash` must keep the SQLite/bare-path hash byte-identical (existing sockets, runtime dirs, and hook dirs must not move) and only route a `postgres://` DSN through the credential-free canonical identity (no `filepath.Abs`, which would mangle a `scheme://host/db` string).

**Files:**
- Modify: `internal/config/paths.go:41-48`
- Test: `internal/config/paths_test.go` (create if absent)

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/paths_test.go` (create the file with `package config_test` and the testify imports if it does not exist):

```go
func TestDBHashSQLitePathUnchanged(t *testing.T) {
	// Golden value pins the pre-1d SQLite hashing (sha256(abs(path))[:12]) so
	// the move never relocates an existing database's runtime dir/socket.
	assert.Equal(t, "1f9b906d5e3f", config.DBHash("/var/lib/kata/kata.db"))
}

func TestDBHashPostgresUsesCredentialFreeCanonicalForm(t *testing.T) {
	full := "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require" //nolint:gosec // fixture proves the credential never reaches the hash
	got := config.DBHash(full)
	// Stable canonical identity, independent of credentials, query params, and
	// the postgres default port (5432).
	assert.Equal(t, "7d5d38a526ca", got)
	assert.Equal(t, got, config.DBHash("postgres://other:pw2@db.example.com:5432/kata?application_name=x"))
	// Explicit :5432 must hash the same as no-port (same logical DB).
	assert.Equal(t, got, config.DBHash("postgres://db.example.com/kata"))
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Test(DBHash|CanonicalDSN)' -v`
Expected: FAIL — `TestDBHashPostgres...` returns a `filepath.Abs`-based hash of the raw DSN, not `7d5d38a526ca`. (`TestDBHashSQLitePathUnchanged` passes already — it pins current behavior; the `TestCanonicalDSN*` tests already pass from Task 1, so running them here just guards against regression at this checkpoint.)

- [ ] **Step 3: Modify `DBHash`**

In `internal/config/paths.go`, replace the body of `DBHash` (lines 41-48) with:

```go
func DBHash(dbPath string) string {
	if strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://") {
		identity, err := CanonicalDSNIdentity(dbPath)
		if err != nil {
			// Never hash a raw postgres DSN — it may carry credentials. Fall
			// back to the redacted form so a malformed DSN still produces a
			// stable, credential-free hash.
			identity = RedactDSN(dbPath)
		}
		// A postgres identity is credential-free and is not a filesystem path,
		// so it must not pass through filepath.Abs.
		sum := sha256.Sum256([]byte(identity))
		return hex.EncodeToString(sum[:])[:12]
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:12]
}
```

Add `"strings"` to the import block in `internal/config/paths.go`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS. Then run bare `go test ./...` for the authoritative exit status — expect 0 (the SQLite branch is unchanged, so nothing downstream moves). To skim only failures, you can pipe through `rg -v "^ok "`, but trust the bare command's status, not the pipe's.

- [ ] **Step 5: Commit**

```bash
git add internal/config/paths.go internal/config/paths_test.go
git commit -m "feat(config): hash postgres DSNs by credential-free identity"
```

---

## Task 3: Lift lock-retry into a generic helper + `Storage.RetryTransient`

Split the SQLite-keyed retry into a backend-neutral loop (`db.RetryTransient(ctx, isTransient, op)`) and the SQLite predicate (`IsLockContention`, kept in `db` for now — it moves to `sqlitestore` in Task 5). Add `RetryTransient(ctx, op) error` to `Storage`, implement it on `*DB`, flip the three daemon handlers, and delete the now-unused public `RetryLockContention`.

**Files:**
- Create: `internal/db/retry.go`, `internal/db/retry_test.go`
- Modify: `internal/db/lock_retry.go`, `internal/db/lock_retry_test.go`, `internal/db/storage.go`, `internal/db/db.go`
- Modify: `internal/daemon/handlers_actions.go:132`, `:181`; `internal/daemon/handlers_ownership.go:91`

- [ ] **Step 1: Write the failing generic-loop tests**

Create `internal/db/retry_test.go` (move the loop/backoff tests here from `lock_retry_test.go`, retargeted at the generic API; the `sequenceBackOff` helper moves with them):

```go
package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func alwaysTransient(error) bool { return true }

func TestRetryTransientRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	err := retryTransient(context.Background(), retryConfig{
		maxElapsed: time.Second,
		newBackOff: func() backoff.BackOff { return &backoff.ZeroBackOff{} },
	}, alwaysTransient, func() error {
		attempts++
		if attempts < 4 {
			return fmt.Errorf("retry: %w", errors.New("busy"))
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 4, attempts)
}

func TestRetryTransientDoesNotRetryPermanentErrors(t *testing.T) {
	wantErr := errors.New("permanent")
	attempts := 0
	err := retryTransient(context.Background(), retryConfig{
		maxElapsed: time.Second,
		newBackOff: func() backoff.BackOff { return &backoff.ZeroBackOff{} },
	}, func(error) bool { return false }, func() error {
		attempts++
		return wantErr
	})
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, attempts)
}

func TestNewRetryBackOffUsesSmallJitteredPolicy(t *testing.T) {
	got, ok := newRetryBackOff().(*retryBackOff)
	require.True(t, ok)
	inner, ok := got.inner.(*backoff.ExponentialBackOff)
	require.True(t, ok)
	assert.Equal(t, time.Millisecond, inner.InitialInterval)
	assert.Equal(t, time.Second, inner.MaxInterval)
	assert.Equal(t, 2.0, inner.Multiplier)
	assert.Equal(t, 0.5, inner.RandomizationFactor)
	assert.Equal(t, time.Second, got.max)
}

func TestRetryBackOffCapsRandomizedDelayAtOneSecond(t *testing.T) {
	bo := &retryBackOff{
		inner: &sequenceBackOff{next: []time.Duration{2 * time.Second}},
		max:   time.Second,
	}
	assert.Equal(t, time.Second, bo.NextBackOff())
}

type sequenceBackOff struct{ next []time.Duration }

func (b *sequenceBackOff) NextBackOff() time.Duration {
	if len(b.next) == 0 {
		return backoff.Stop
	}
	d := b.next[0]
	b.next = b.next[1:]
	return d
}
func (b *sequenceBackOff) Reset() {}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/db/ -run 'TestRetryTransient|TestNewRetryBackOff|TestRetryBackOff' -v`
Expected: FAIL — `undefined: retryTransient`, `retryConfig`, `newRetryBackOff`, `retryBackOff`.

- [ ] **Step 3: Write the generic retry**

Create `internal/db/retry.go`:

```go
package db

import (
	"context"
	"time"

	backoff "github.com/cenkalti/backoff/v5"
)

const (
	defaultInitialRetryBackoff = time.Millisecond
	defaultMaxRetryBackoff     = time.Second
	defaultMaxRetryElapsed     = 30 * time.Second
)

type retryConfig struct {
	maxElapsed time.Duration
	newBackOff func() backoff.BackOff
}

// RetryTransient retries op while isTransient reports its error as a transient
// condition that may clear on a whole-operation retry. op must be safe to re-run
// in full; callers must not wrap a single statement inside an already-open
// transaction. The backend supplies isTransient (SQLite busy/locked today;
// Postgres serialization/deadlock later), so this loop stays backend-neutral.
func RetryTransient(ctx context.Context, isTransient func(error) bool, op func() error) error {
	return retryTransient(ctx, retryConfig{
		maxElapsed: defaultMaxRetryElapsed,
		newBackOff: newRetryBackOff,
	}, isTransient, op)
}

func retryTransient(ctx context.Context, cfg retryConfig, isTransient func(error) bool, op func() error) error {
	cfg = normalizedRetryConfig(cfg)
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := op()
		if err == nil {
			return struct{}{}, nil
		}
		if !isTransient(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(cfg.newBackOff()), backoff.WithMaxElapsedTime(cfg.maxElapsed))
	return err
}

func normalizedRetryConfig(cfg retryConfig) retryConfig {
	if cfg.maxElapsed <= 0 {
		cfg.maxElapsed = defaultMaxRetryElapsed
	}
	if cfg.newBackOff == nil {
		cfg.newBackOff = newRetryBackOff
	}
	return cfg
}

type retryBackOff struct {
	inner backoff.BackOff
	max   time.Duration
}

func newRetryBackOff() backoff.BackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = defaultInitialRetryBackoff
	bo.MaxInterval = defaultMaxRetryBackoff
	bo.Multiplier = 2
	bo.RandomizationFactor = 0.5
	bo.Reset()
	return &retryBackOff{inner: bo, max: defaultMaxRetryBackoff}
}

func (b *retryBackOff) NextBackOff() time.Duration {
	d := b.inner.NextBackOff()
	if d > b.max {
		return b.max
	}
	return d
}

func (b *retryBackOff) Reset() { b.inner.Reset() }
```

- [ ] **Step 4: Trim `lock_retry.go` to the SQLite predicate**

Replace the entire contents of `internal/db/lock_retry.go` with:

```go
package db

import (
	"errors"

	sqlite3 "modernc.org/sqlite/lib"
)

const sqlitePrimaryCodeMask = 0xff

type sqliteCodeError interface {
	Code() int
}

// IsLockContention reports whether err is a SQLite busy/locked condition that
// may clear if the whole mutation is retried after a short delay.
func IsLockContention(err error) bool {
	var coded sqliteCodeError
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.Code() & sqlitePrimaryCodeMask {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	default:
		return false
	}
}
```

In `internal/db/lock_retry_test.go`, delete every test except `TestIsLockContentionRecognizesSQLiteBusyAndLocked` and remove the now-unused `sequenceBackOff` type and the `backoff`/`time`/`context` imports it no longer needs (they moved to `retry_test.go`). Keep only the `codedSQLiteErr` helper and the predicate test.

- [ ] **Step 5: Add `RetryTransient` to the interface and implement it on `*DB`**

In `internal/db/storage.go`, add to the `// identity / lifecycle` group of the `Storage` interface:

```go
	RetryTransient(ctx context.Context, op func() error) error
```

In `internal/db/db.go`, add (after `Path`):

```go
// RetryTransient retries op while it returns a SQLite lock-contention error.
func (d *DB) RetryTransient(ctx context.Context, op func() error) error {
	return RetryTransient(ctx, IsLockContention, op)
}
```

- [ ] **Step 6: Flip the daemon handlers**

In `internal/daemon/handlers_actions.go` change line 132 and line 181 from `err = db.RetryLockContention(ctx, func() error {` to `err = cfg.DB.RetryTransient(ctx, func() error {`. In `internal/daemon/handlers_ownership.go` change line 91 the same way. The closures and everything else are unchanged (`cfg.DB` is the `db.Storage` handle in scope at all three sites).

- [ ] **Step 7: Verify it compiles, then prove `RetryLockContention` is dead and remove it**

Run: `go build ./... && rg -n 'RetryLockContention' --glob '!docs/**'`
Expected: build succeeds; `rg` finds **no** references (the public wrapper no longer exists — it was removed when `lock_retry.go` was rewritten in Step 4; this step confirms nothing else referenced it).

- [ ] **Step 8: Run the suite**

Run: `go test ./internal/db/ ./internal/daemon/ -v 2>&1 | rg -v '^=== |^--- PASS|^    '` then `go test ./...`
Expected: all green. The handlers exercise `RetryTransient` through the existing action/ownership tests.

- [ ] **Step 9: Lint + commit**

Run: `nix run 'nixpkgs#golangci-lint' -- run`
Expected: 0 issues.

```bash
git add internal/db/retry.go internal/db/retry_test.go internal/db/lock_retry.go internal/db/lock_retry_test.go internal/db/storage.go internal/db/db.go internal/daemon/handlers_actions.go internal/daemon/handlers_ownership.go
git commit -m "refactor(db): lift lock-retry into generic RetryTransient on Storage"
```

---

## Task 4: Extract the neutral contract into dedicated `db` files

This is the cycle prerequisite: the `Storage` interface (in `db`) names structs that currently live inside the impl files (`queries_*.go`, `store_*.go`, `export.go`, `import_replay.go`, `imports.go`, `import_mappings.go`). Those files move to `sqlitestore` in Task 5, but the interface cannot reference types that live in `sqlitestore` without `db → sqlitestore` becoming a cycle. So every **contract** declaration must first move into a `db`-resident file. This is a pure in-package reorganization: declarations move between files of the same package, so the compiler verifies completeness and the existing suite verifies behavior is unchanged.

**The partition rule.** A declaration is **contract** (extract into a `db` file now) iff it is referenced by name from outside `internal/db`, OR named in a `Storage` interface signature, OR transitively referenced by a field of another contract type. That last clause matters: if an extracted struct has a field whose type currently lives in an impl file (e.g. `CreateRecurrenceIn.Template` of type `RecurrenceTemplate`), that field type must also be extracted — otherwise after Task 5 a `db` file would reference a `sqlitestore` type and `go build ./...` fails with `undefined: T` in `db`. The compiler enforces the closure: any `undefined: T` reported in a `db` file during Task 5 means `T` was missed here. Everything else (the `DB` struct, all methods, unexported helpers like `searchFTSReq`, `linkPeerRef`, `createdLinkTarget`, `importIssueState`, `fkColumnQuerier`) stays put and moves with the impl in Task 5.

**Files:**
- Create: `internal/db/errors.go`, `internal/db/params.go`, `internal/db/export_types.go`, `internal/db/import_types.go`
- Modify (remove the extracted declarations, leave method bodies): `internal/db/queries.go`, `queries_edit_atomic.go`, `queries_events.go`, `queries_labels.go`, `queries_links.go`, `queries_move.go`, `queries_projects_merge.go`, `queries_projects_remove.go`, `queries_hooks.go`, `store_metadata.go`, `store_recurrences.go`, `export.go`, `import_replay.go`, `imports.go`, `import_mappings.go`, `db.go`

- [ ] **Step 1: Enumerate the externally-referenced `db` surface**

Run:
```bash
rg -ohN 'db\.[A-Z][A-Za-z0-9]*' --glob '!internal/db/**' --glob '!docs/**' | sort -u
```
This prints every `db.Identifier` referenced from other packages — the authoritative contract list (types, sentinel vars, error structs). Cross-check against the `Storage` interface in `internal/db/storage.go` for any type named only there (e.g. result structs returned but never referenced by callers). Together these are exactly what must end up in a `db` file after Task 5.

- [ ] **Step 2: Move the sentinel `Err*` vars and exported error structs into `errors.go`**

Create `internal/db/errors.go` with `package db` and move into it (cut from their current files, do not copy):

- The sentinel vars: `ErrSchemaCutoverRequired` (from `db.go:30`), `ErrNotFound`/`ErrOpenChildren`/`ErrNoFields`/`ErrInitialLinkTargetNotFound`/`ErrInitialLinkInvalidType`/`ErrAlreadyClaimed` (from `queries.go`), `ErrParentMismatch`/`ErrParentCycle` (`queries_edit_atomic.go`), `ErrLinkExists`/`ErrParentAlreadySet`/`ErrSelfLink`/`ErrCrossProjectLink` (`queries_links.go`), `ErrLabelExists`/`ErrLabelInvalid` (`queries_labels.go`), `ErrInvalidRecurrence` (`store_recurrences.go`), `ErrProjectAlreadyArchived`/`ErrProjectHasOpenIssues`/`ErrAliasIsLast` (`queries_projects_remove.go`), the `ErrProjectMerge*` group (`queries_projects_merge.go`), `ErrImportValidation` (`imports.go`), and any others Step 1 surfaced (e.g. `ErrParentAlreadySet`, `ErrParentMismatch`, plus the `Err*` matched in `handlers_metadata.go`/`handlers_move.go`/`handlers_recurrences.go`).
- The exported error **struct types** matched by `errors.As` in the daemon: `LinkTargetNotFoundError` (`queries_edit_atomic.go:97`), `ProjectHasOpenIssuesError` and `ProjectMergeImportMappingCollisionError` + its `ProjectMergeImportMappingCollision` payload struct, plus `RevisionConflictError`, `CrossProjectLinksError`, `RecurrencePinnedError` (grep their definitions: `rg -n 'type (RevisionConflictError|CrossProjectLinksError|RecurrencePinnedError) ' internal/db`). Move each type **with its `Error()`/`Is()`/`Unwrap()` methods** — those methods are pure value logic, not SQL.

Keep `ErrSchemaCutoverRequired`'s doc comment. Group the bare `errors.New` sentinels with a single `import "errors"`.

- [ ] **Step 3: Move param/result structs into `params.go`**

Create `internal/db/params.go` (`package db`) and move the exported structs named in the `Storage` signatures that are **not** already in `types.go`: `CreateIssueParams`, `ListIssuesParams`, `ListAllIssuesParams`, `CreateCommentParams`, `InitialLink`, `EditIssueParams` (grep it), `EditIssueAtomicParams`, `AtomicEditChanges`, `EditIssueAtomicResult`, `ReadyIssuesFilter` (grep), `CreateLinkParams`, `LinkEventParams`, `LabelEventParams`, `EventsAfterParams`, `EventsInWindowParams`, `CloseThrottledPayload` (grep), `RemoveProjectParams`, `DetachAliasParams`, `MergeProjectsParams`, `ProjectMergeResult`, `ShortIDExtension`, `MoveIssueProjectIn`/`MoveIssueProjectOut` (grep), `PatchProjectMetadataIn`/`Out`, `PatchIssueMetadataIn`/`Out`, `CreateRecurrenceIn`, `PatchRecurrenceIn`/`Out`, `MaterializeNextOut`, `AliasRow`, `ClaimResult` (grep). Apply the transitive-closure clause: if `CreateRecurrenceIn` (or any extracted struct) has a field typed by another exported impl-file struct such as `RecurrenceTemplate`, extract that struct too. The `ImportBatchParams`-family is handled in the next step. Leave unexported helper structs (`searchFTSReq`, `linkPeerRef`, `createdLinkTarget`) in place.

Use `rg -n '^type <Name> ' internal/db` to find each definition's current file, then cut it into `params.go`.

- [ ] **Step 4: Move export + import contract types**

Create `internal/db/export_types.go` (`package db`) and move from `export.go`: `ExportFilter`, `MetaKV`, `ProjectExport`, `IssueExport`, `EventExport`, `RecurrenceExport`, `LinkExport`, `PurgeLogExport`, `AliasExport`, `CommentExport`, `IssueLabelExport`, `ImportMappingExport`, `SequenceExport` (the struct definitions and their JSON tags only — leave the `Export*` methods in `export.go`).

Create `internal/db/import_types.go` (`package db`) and move: from `import_replay.go` the `ImportOptions` and `ImportRecord` structs, the import-kind constants, and the `ImportRecord` validator method (pure validation, no SQL — exported to `Validate`, see below); from `imports.go` the `ImportBatchParams`/`ImportItem`/`ImportComment`/`ImportLink`/`ImportBatchResult`/`ImportItemResult` structs; from `import_mappings.go` the `ImportMapping`/`ImportMappingParams` structs. Leave `importIssueState` (unexported) and every method body in their current files **except `ImportRecord.Validate`** — that one method moves to `import_types.go` with its receiver type and stays in `package db`. (A method on the contract type `ImportRecord` cannot be defined in `sqlitestore` once `import_replay.go` moves there in Task 5, so `Validate` must travel with `ImportRecord`; the SQL method bodies — `ImportReplay`, `importRecord`, the per-entity insert helpers — stay put and move to `sqlitestore`.)

**Export the cross-split identifiers while moving them.** `sqlitestore.ImportReplay` (Task 5) both calls `r.validate()` (`import_replay.go:122`) and reads the `importKind*` constants in its `importRecord`/`wrapImportErr` switch, but `sqlitestore` cannot reach unexported `db` members — so export both here:
- the validator: `func (r ImportRecord) validate()` → `func (r ImportRecord) Validate()`.
- the kind constants: `importKindMeta` → `ImportKindMeta`, … `importKindSQLiteSequence` → `ImportKindSQLiteSequence`.

Do this in-package (everything is still `package db` here) and update the in-package call sites: `ImportReplay`'s `r.validate()` → `r.Validate()`, and the `importKind*` mention in `import_replay_test.go`'s doc comment. Exporting `Validate` makes the `ValidateImportRecordForTest` shim obsolete — **delete `import_replay_internal_test.go`** and change the two neutral tests (`TestImportKindsMatchJSONLKinds`, `TestImportRecordValidate`) to call `rec.Validate()` directly instead of `db.ValidateImportRecordForTest(rec)`. The `importRecord`/`wrapImportErr` switch sites stay bare for now; they pick up the `db.` qualifier in Task 5 when `import_replay.go` moves. **General rule:** any unexported `db`-resident identifier that code moving to `sqlitestore` references must be exported in this task — `validate` and the import-kind constants are the known cases.

- [ ] **Step 5: Verify the reorganization is behavior-neutral**

Run: `go build ./... && go vet ./internal/db/`
Expected: builds clean (every moved declaration still resolves within `package db`; the compiler flags any duplicate or dangling reference).

Run: `go test ./...`
Expected: all green — no behavior changed; this only relocated declarations within one package.

Run: `nix run 'nixpkgs#golangci-lint' -- run`
Expected: 0 issues.

- [ ] **Step 6: Confirm every impl file is now method-only**

Run:
```bash
rg -n '^type [A-Z]' internal/db/queries*.go internal/db/store_*.go internal/db/export.go internal/db/import_replay.go internal/db/imports.go internal/db/import_mappings.go
```
Expected: no **exported** struct/type definitions remain in the impl files (only unexported helpers, methods, and SQL). Exported contract types now live in `errors.go`, `params.go`, `export_types.go`, `import_types.go`, or the pre-existing `types.go`. This is what lets Task 5 relocate the impl files wholesale.

- [ ] **Step 7: Commit**

```bash
git add internal/db/errors.go internal/db/params.go internal/db/export_types.go internal/db/import_types.go internal/db/queries.go internal/db/queries_edit_atomic.go internal/db/queries_events.go internal/db/queries_labels.go internal/db/queries_links.go internal/db/queries_move.go internal/db/queries_projects_merge.go internal/db/queries_projects_remove.go internal/db/queries_hooks.go internal/db/store_metadata.go internal/db/store_recurrences.go internal/db/export.go internal/db/import_replay.go internal/db/imports.go internal/db/import_mappings.go internal/db/db.go
git commit -m "refactor(db): extract contract types into neutral files"
```

---

## Task 5: Move the SQLite impl into `sqlitestore`; add `OpenOption` + `storeopen`; flip callers

The irreducible atomic step: a Go type's methods cannot span packages, so `DB`→`Store` and every method move together. Production callers flip in the same commit because `db.Open`/`db.OpenReadOnly` cease to exist. The executable guard is the full suite + the compile assertion; behavior is unchanged.

**Files:**
- Create: `internal/db/open.go`; `internal/db/sqlitestore/` (relocated impl + tests); `internal/db/storeopen/storeopen.go`
- Modify: `internal/db/storage.go` (remove the `*DB` assertion); `cmd/kata/export.go`, `cmd/kata/import.go`, `cmd/kata/daemon_cmd.go`, `internal/testenv/testenv.go`, `internal/jsonl/cutover.go`, `internal/jsonl/cutover_export.go`, `internal/jsonl/preflight.go`
- Move: all method-only impl files + `schema.sql` + the impl test files

- [ ] **Step 1: Add the neutral open option**

Create `internal/db/open.go`:

```go
package db

// OpenConfig holds backend-neutral open options resolved from OpenOption values.
type OpenConfig struct {
	ReadOnly bool
}

// OpenOption configures how a storage backend is opened.
type OpenOption func(*OpenConfig)

// ReadOnly opens the backend without bootstrapping or mutating schema state.
func ReadOnly() OpenOption { return func(c *OpenConfig) { c.ReadOnly = true } }

// ApplyOpenOptions folds opts into an OpenConfig for a backend's Open to consume.
func ApplyOpenOptions(opts ...OpenOption) OpenConfig {
	var c OpenConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}
```

- [ ] **Step 2: Relocate the impl files into `sqlitestore`**

```bash
mkdir -p internal/db/sqlitestore
git mv internal/db/schema.sql internal/db/sqlitestore/schema.sql
git mv internal/db/db.go internal/db/sqlitestore/store.go
git mv internal/db/lock_retry.go internal/db/sqlitestore/transient.go
for f in queries.go queries_delete.go queries_edit_atomic.go queries_events.go queries_hooks.go queries_idempotency.go queries_labels.go queries_labels_by_issues.go queries_links.go queries_move.go queries_priority.go queries_projects_merge.go queries_projects_remove.go queries_search.go queries_short_id.go store_metadata.go store_recurrences.go export.go import_replay.go import_mappings.go imports.go fkmeta.go; do git mv "internal/db/$f" "internal/db/sqlitestore/$f"; done
```

The files that **stay** in `internal/db`: `storage.go`, `types.go`, `errors.go`, `params.go`, `export_types.go`, `import_types.go`, `retry.go`, `open.go`, `schema_version.go`. Confirm: `ls internal/db/*.go`.

**One exception to the wholesale `queries_idempotency.go` move:** `Fingerprint`, `FingerprintLegacy`, and the `DedupeLinks` helper they share are pure SHA-256 canonicalization (no SQL, documented cross-language contract — see the parent spec §3.6) and are called directly by `internal/daemon/handlers_issues.go`. Leaving them in `sqlitestore` would force the daemon to import `sqlitestore` for a pure helper, leaking backend coupling into handlers — the exact thing the split prevents. Lift those functions plus their pure-unit tests into a new `internal/db/fingerprint.go` + `internal/db/fingerprint_test.go` (the 5 `LookupIdempotency_*` *method* tests stay in `sqlitestore` since they exercise SQL). `sqlitestore/queries.go` then calls `db.DedupeLinks` where it used the local `dedupeLinks`.

- [ ] **Step 3: Rewrite package clauses, the type name, and the openers**

In every moved file: change `package db` → `package sqlitestore` and add `import "go.kenn.io/kata/internal/db"` (let `goimports` place it; it gets used once you qualify in Step 4).

In `store.go` (was `db.go`):
- Rename `type DB struct {...}` → `type Store struct {...}` and every receiver `func (d *DB)` → `func (d *Store)` across **all** moved files (keep the receiver identifier `d` to minimize churn). Use: `rg -l '\(d \*DB\)' internal/db/sqlitestore | xargs sd '\(d \*DB\)' '(d *Store)'` then `sd 'type DB struct' 'type Store struct' internal/db/sqlitestore/store.go`.
- Delete `const currentSchemaVersion`, `func CurrentSchemaVersion`, and `var ErrSchemaCutoverRequired` from `store.go` — these are neutral. Create `internal/db/schema_version.go` (`package db`) holding `const currentSchemaVersion = 10` and `func CurrentSchemaVersion() int { return currentSchemaVersion }`; `ErrSchemaCutoverRequired` already moved to `db/errors.go` in Task 4. In `store.go`, replace bare `currentSchemaVersion` with `db.CurrentSchemaVersion()` and bare `ErrSchemaCutoverRequired` with `db.ErrSchemaCutoverRequired`, and keep the `//go:embed schema.sql` + `var schemaSQL string` directive.
- Merge `Open` + `OpenReadOnly` into a single options-driven opener. Replace the two functions with:

```go
// Open opens (and if needed bootstraps) the SQLite database at path. With
// db.ReadOnly() it opens mode=ro and skips bootstrap/instance-uid seeding.
func Open(ctx context.Context, path string, opts ...db.OpenOption) (*Store, error) {
	cfg := db.ApplyOpenOptions(opts...)
	if cfg.ReadOnly {
		return openReadOnly(ctx, path)
	}
	return openReadWrite(ctx, path)
}
```

with `openReadWrite` = today's `Open` body (the read-write DSN, ping, `bootstrap`, `ensureInstanceUID`, returning `&Store{DB: sdb, path: path}`) and `openReadOnly` = today's `OpenReadOnly` body (the `mode=ro` DSN, ping, returning `&Store{DB: sdb, path: path}`). Update `PeekSchemaVersion` to call `Open(ctx, path, db.ReadOnly())`.

In `transient.go` (was `lock_retry.go`): rename `IsLockContention` → `IsTransient` (keep `sqliteCodeError` + `sqlitePrimaryCodeMask` + the body). Add the `Store` method:

```go
// RetryTransient retries op while it returns a SQLite lock-contention error.
func (d *Store) RetryTransient(ctx context.Context, op func() error) error {
	return db.RetryTransient(ctx, IsTransient, op)
}
```

(Remove the `(d *DB) RetryTransient` that was added to `db.go` in Task 3 — it left `db` along with the struct; this is its replacement on `*Store`.)

- [ ] **Step 4: Compiler-guided qualification**

Run: `go build ./internal/db/sqlitestore/ 2>&1 | head -40`

Each `undefined: X` names either a neutral type/var/const that stayed in `db` or a missed `DB`→`Store` rename. For a neutral symbol, prefix with `db.` (e.g. `Project` → `db.Project`, `ErrNotFound` → `db.ErrNotFound`, `CreateIssueParams` → `db.CreateIssueParams`, `MetaKV` → `db.MetaKV`, `ImportRecord` → `db.ImportRecord`, `LinkTargetNotFoundError` → `db.LinkTargetNotFoundError`). For an `undefined: DB` (a receiver or local the `sd` rename missed), change `*DB` → `*Store`. Re-run until `internal/db/sqlitestore` compiles. The embedded `*sql.DB` methods (`QueryContext`, `ExecContext`, `BeginTx`, …) and unexported helpers moved with the impl, so they need no qualification.

- [ ] **Step 5: Move the compile assertion and the impl test suite**

In `internal/db/storage.go`, delete `var _ Storage = (*DB)(nil)` (line 150) — `DB` no longer exists in `db`. Add to `internal/db/sqlitestore/store.go`:

```go
var _ db.Storage = (*Store)(nil)
```

Relocate the impl tests. Move every `internal/db/*_test.go` that exercises a moved method or opens a DB **into `sqlitestore`** and rename its package: `package db_test` → `package sqlitestore_test`, `package db` → `package sqlitestore`. Inside them, `db.Open(` → `sqlitestore.Open(` (or bare `Open(` for internal tests), `db.OpenReadOnly(ctx, p)` → `sqlitestore.Open(ctx, p, db.ReadOnly())`, `*db.DB` → `*sqlitestore.Store`, and `db.IsLockContention` → `sqlitestore.IsTransient`; keep `db.`-qualified references to neutral types. Move `internal/db/testhelpers_test.go` with them.

```bash
for f in db_test export_test import_mappings_test imports_test instance_uid_test queries_create_initial_test queries_delete_test queries_events_test queries_idempotency_test queries_issues_test queries_labels_by_issues_test queries_labels_test queries_links_test queries_move_test queries_owner_test queries_priority_test queries_projects_remove_test queries_projects_test queries_ready_test queries_search_test schema_completeness_test schema_test store_metadata_test store_recurrences_close_test store_recurrences_test testhelpers_test; do git mv "internal/db/$f.go" "internal/db/sqlitestore/$f.go"; done
```

**Stays in `internal/db`** (neutral-type tests): `types_test.go`; `retry_test.go` (from Task 3); and the neutral cases split out of `import_replay_test.go` — `TestImportKindsMatchJSONLKinds` and `TestImportRecordValidate`, which call `db.ImportRecord`'s exported `Validate()` directly (the `ValidateImportRecordForTest` shim and its `import_replay_internal_test.go` file were deleted in Task 4). Move the remaining `import_replay_test.go` cases (`TestFKColumnResolver*`, `TestImportReplay*`) into `internal/db/sqlitestore/import_replay_test.go`; keep the two neutral cases in `internal/db/import_replay_test.go` (rename it `import_types_test.go` for clarity, `package db_test`, importing `internal/jsonl` — no cycle, `db` does not import `jsonl`). The `lock_retry_test.go` predicate test moves to `sqlitestore` as `TestIsTransientRecognizesSQLiteBusyAndLocked`.

- [ ] **Step 6: Create the minimal `storeopen` selector**

Create `internal/db/storeopen/storeopen.go`:

```go
package storeopen

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Open selects a storage backend from the DSN and returns a db.Storage. A bare
// filesystem path or sqlite:// DSN opens the SQLite backend. (Postgres dispatch
// and DSN redaction land in Task 6.)
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	if isSQLiteDSN(dsn) {
		return sqlitestore.Open(ctx, sqlitePath(dsn), opts...)
	}
	return nil, fmt.Errorf("unsupported dsn")
}

// OpenReadOnly opens the backend selected by dsn read-only.
func OpenReadOnly(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	return Open(ctx, dsn, append(opts, db.ReadOnly())...)
}

func isSQLiteDSN(dsn string) bool {
	return !strings.Contains(dsn, "://") || strings.HasPrefix(dsn, "sqlite://")
}

func sqlitePath(dsn string) string { return strings.TrimPrefix(dsn, "sqlite://") }
```

- [ ] **Step 7: Flip the production entry points to `storeopen`**

- `cmd/kata/export.go:42`: `db.Open(ctx, dbPath)` → `storeopen.Open(ctx, dbPath)`. The holder is now `db.Storage`; it already feeds `jsonl.Export(db.Storage)`, `resolveExportProject(db.Storage)`, and `d.Close()`.
- `cmd/kata/import.go:125`: `db.Open(cmd.Context(), target)` → `storeopen.Open(cmd.Context(), target)`. Feeds `jsonl.ImportWithOptions(db.Storage)` and `d.Close()`.
- `cmd/kata/daemon_cmd.go`: the pre-open cutover gate at line 291 calls `db.PeekSchemaVersion(ctx, dbPath)` → `sqlitestore.PeekSchemaVersion(ctx, dbPath)` (`db.CurrentSchemaVersion()` on the same line stays — neutral); then `db.Open(ctx, dbPath)` (line 296) → `storeopen.Open(ctx, dbPath)`. The held store feeds `cfg.DB` (`db.Storage`), `setupHooks(db.Storage)`, `store.Close()`. This file ends up importing **both** `storeopen` and `sqlitestore` (plus `db` for `CurrentSchemaVersion`).

Add the `storeopen` import to each; let `goimports` drop `db` if a file no longer references it directly (most still use `db.` types).

- [ ] **Step 8: Flip `testenv` to the concrete `*sqlitestore.Store`**

In `internal/testenv/testenv.go`: change the `DB *db.DB` field (line 24) to `DB *sqlitestore.Store`; `serveDaemon(t *testing.T, d *db.DB, ...)` (line 128) to `d *sqlitestore.Store`; and both `db.Open(...)` calls (lines 82, 95) to `sqlitestore.Open(...)`. Add the `sqlitestore` import. `cfg.DB = d` still compiles (`*Store` satisfies `db.Storage`), and every `env.DB.QueryRowContext` / `env.DB.ExecContext` / domain-method call in the daemon tests keeps working on the concrete store.

- [ ] **Step 9: Flip the jsonl cutover/preflight machinery to concrete `sqlitestore`**

- `internal/jsonl/cutover.go`: `db.PeekSchemaVersion` (line 29) → `sqlitestore.PeekSchemaVersion`; `db.OpenReadOnly(ctx, sourcePath)` (line 87) → `sqlitestore.Open(ctx, sourcePath, db.ReadOnly())`; `db.Open(ctx, tmpDB)` (line 111) → `sqlitestore.Open(ctx, tmpDB)`. Keep `db.CurrentSchemaVersion()` (line 33). **Remove** the post-`Import` re-stamp block (lines 124-129, the `target.ExecContext(... INSERT INTO meta schema_version ...)`) — `ImportReplay` already stamps the current `schema_version` (proven by `TestImportV1FillsUIDsDeterministically`, which asserts `schema_version == "10"` after import), so the re-stamp is redundant and is the only non-`Storage` call on `target`. Drop the now-unused `strconv` import. Add the `sqlitestore` import.
- `internal/jsonl/cutover_export.go`: change every `*db.DB` parameter (the `exportForCutover` signature and all `exportProjects*`/`exportIssues*`/`exportEvents*`/`exportPurgeLog*`/`tableHasColumn`/`purgeProjectNameExpr`/`exportSQLiteSequence`/etc. helpers) to `*sqlitestore.Store`. Run `sd '\*db\.DB' '*sqlitestore.Store' internal/jsonl/cutover_export.go`. Add the `sqlitestore` import. The bodies call `d.QueryContext(...)` (works via the embedded `*sql.DB`) and reference `db.`-qualified types (unchanged, this is `package jsonl`).
- `internal/jsonl/preflight.go`: `db.OpenReadOnly(ctx, path)` (line 92) → `sqlitestore.Open(ctx, path, db.ReadOnly())`; `db.NewFKColumnResolver(source)` (line 127) → `sqlitestore.NewFKColumnResolver(source)`. Add the `sqlitestore` import.

- [ ] **Step 10: Flip the remaining test call sites uniformly**

Outside `internal/db`/`sqlitestore`, every test that opened a DB used the concrete `*db.DB` for raw SQL. Apply the uniform transform in `internal/jsonl/*_test.go`, `internal/daemon/*_test.go`, and `cmd/kata/*_test.go`:

```bash
rg -l 'db\.Open|db\.OpenReadOnly|db\.PeekSchemaVersion|\*db\.DB' internal/jsonl internal/daemon cmd/kata --glob '*_test.go'
```

For each file: `db.Open(` → `sqlitestore.Open(`; `db.OpenReadOnly(ctx, p)` → `sqlitestore.Open(ctx, p, db.ReadOnly())`; `db.PeekSchemaVersion(` → `sqlitestore.PeekSchemaVersion(` (the callers are `cmd/kata/daemon_cutover_test.go`, `internal/jsonl/cutover_test.go`, `internal/jsonl/fixtures_test.go`); `*db.DB` → `*sqlitestore.Store`; add the `sqlitestore` import. (Concrete `*Store` exposes both the domain methods and the embedded raw-SQL methods, so this compiles regardless of which a given test uses.) In `internal/daemon/testhelpers_test.go`, the `db *db.DB` field and `DB() *db.DB` accessor become `*sqlitestore.Store`.

- [ ] **Step 11: Build, prove the invariants, run the full suite**

Run:
```bash
go build ./...
rg -n '\*db\.DB|db\.Open\(|db\.OpenReadOnly\(|db\.PeekSchemaVersion\(|RetryLockContention' --glob '!docs/**' --glob '!**/*.md'
rg -n 'QueryContext|QueryRowContext|ExecContext|BeginTx|PRAGMA' internal/db/*.go
```
Expected: build succeeds; the first `rg` finds **no** matches (the `db.DB` type, `db.Open*`, and `db.PeekSchemaVersion` are gone); the second `rg` finds **no** SQL-execution calls in `internal/db/*.go` (they all moved to `sqlitestore`). Note: the bare token `sqlite_sequence` is intentionally *not* in the second grep — it legitimately remains in `db` as the `ImportKindSQLiteSequence` wire constant (`import_types.go`) and in `SequenceExport`'s doc comment, which are contract/data, not SQL execution. The execution-method tokens are the real proof that no SQL runs in `db`.

Run: `go test ./...`
Expected: all green. The relocated `sqlitestore` suite is the behavior guard.

Run: `nix run 'nixpkgs#golangci-lint' -- run`
Expected: 0 issues.

- [ ] **Step 12: Commit**

```bash
git add -A
git commit -m "refactor(db): move SQLite impl to sqlitestore behind storeopen selector"
```

---

## Task 6: `storeopen` DSN dispatch + credential redaction

Add the genuinely-new selector behavior test-first: a `postgres://` DSN returns a clear "not yet available" error with the **password redacted**; an unknown scheme errors; the SQLite path keeps working. This is where the credential-leak guard is proven before any Postgres code exists.

**Files:**
- Modify: `internal/db/storeopen/storeopen.go`
- Test: `internal/db/storeopen/storeopen_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/db/storeopen/storeopen_test.go`:

```go
package storeopen_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/storeopen"
)

func TestOpenBarePathReturnsWorkingSQLiteStore(t *testing.T) {
	s, err := storeopen.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
}

func TestOpenSQLiteSchemeReturnsWorkingStore(t *testing.T) {
	s, err := storeopen.Open(context.Background(), "sqlite://"+filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
}

func TestOpenPostgresReturnsRedactedNotAvailableError(t *testing.T) {
	dsn := "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require" //nolint:gosec // fixture proves the password is redacted in the error
	_, err := storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres")
	assert.Contains(t, err.Error(), "not yet available")
	assert.NotContains(t, err.Error(), "SECRET")
	// Mutation guard: a naive %w of the raw DSN would leak the secret, so the
	// NotContains assertion above bites.
	assert.Contains(t, dsn, "SECRET")
}

func TestOpenUnknownSchemeErrors(t *testing.T) {
	_, err := storeopen.Open(context.Background(), "mysql://h/db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}
```

- [ ] **Step 2: Run the tests to verify the new ones fail**

Run: `go test ./internal/db/storeopen/ -v`
Expected: `TestOpenBarePath...` and `TestOpenSQLiteScheme...` PASS (the Task 5 minimal selector already handles sqlite); `TestOpenPostgres...` FAILS (error says `"unsupported dsn"`, missing `"not yet available"`); `TestOpenUnknownScheme...` may pass on substring `"unsupported"` but the postgres case proves the redaction path is unimplemented.

- [ ] **Step 3: Implement the full dispatch + redaction**

Replace `internal/db/storeopen/storeopen.go` with:

```go
package storeopen

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Open selects a storage backend from the DSN and returns a db.Storage. A bare
// filesystem path or sqlite:// DSN opens the SQLite backend. A postgres:// DSN
// returns a clear "not yet available" error with the password redacted. Any
// other scheme is unsupported.
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case !hasScheme:
		return sqlitestore.Open(ctx, dsn, opts...)
	case scheme == "sqlite":
		return sqlitestore.Open(ctx, strings.TrimPrefix(dsn, "sqlite://"), opts...)
	case scheme == "postgres" || scheme == "postgresql":
		return nil, fmt.Errorf("postgres backend not yet available: %s", config.RedactDSN(dsn))
	default:
		return nil, fmt.Errorf("unsupported dsn scheme %q", scheme)
	}
}

// OpenReadOnly opens the backend selected by dsn read-only.
func OpenReadOnly(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	return Open(ctx, dsn, append(opts, db.ReadOnly())...)
}

func splitScheme(dsn string) (scheme, rest string, hasScheme bool) {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return "", dsn, false
	}
	return dsn[:i], dsn[i+len("://"):], true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/db/storeopen/ -v`
Expected: all four PASS. The postgres error contains `postgres … not yet available` and the redacted DSN, never `SECRET`.

- [ ] **Step 5: Full suite + lint**

Run: `go test ./... && nix run 'nixpkgs#golangci-lint' -- run`
Expected: all green; 0 lint issues.

- [ ] **Step 6: Commit**

```bash
git add internal/db/storeopen/storeopen.go internal/db/storeopen/storeopen_test.go
git commit -m "feat(storeopen): dispatch DSN scheme with credential redaction"
```

---

## Self-Review (completed during planning)

**Spec coverage** — every Design section maps to a task: contract/SQL dividing line → Tasks 4+5; `storeopen` selector → Tasks 5+6; retry lift (`db.RetryTransient` + `Storage` method + `sqlitestore.IsTransient` + handler flips) → Tasks 3+5; DSN identity & redaction (`CanonicalDSNIdentity`/`RedactDSN`/`DBHash`) → Tasks 1+2+6; holder flips (cmd/kata, daemon-bootstrap, testenv, cutover, preflight) + cutover re-stamp removal → Task 5. Out-of-scope items (migration runner, `pgstore`, `KATA_DSN` resolution) are not implemented — `postgres://` is reachable only as a redacted unsupported error (Task 6).

**Deviations flagged** — `testenv.Env.DB` and the jsonl cutover/preflight handles hold concrete `*sqlitestore.Store` rather than `db.Storage`, because they need raw `QueryContext`/`ExecContext`/version-dispatch SQL the interface does not expose. Documented in Architecture; both still satisfy "no `*db.DB` remains."

**Placeholder scan** — new code (config helpers, `retry.go`, `open.go`, `storeopen.go`, the interface/handler/cutover edits) is given in full. The two relocation tasks give exact `git mv` lists, the partition rule, a compiler-guided qualification recipe, and the suite as guard — the correct form for a behavior-preserving move (hand-pasting 17.5k relocated LOC would be noise, not signal).

**Type consistency** — `Store` (not `DB`) in `sqlitestore`; `RetryTransient(ctx, op) error` on the interface and `*Store`; the generic `RetryTransient(ctx, isTransient, op)` in `db`; `IsTransient` (renamed from `IsLockContention`) in `sqlitestore`; `OpenOption`/`ReadOnly()`/`ApplyOpenOptions` in `db`; `sqlitestore.Open(ctx, path, opts...)` and `storeopen.Open(ctx, dsn, opts...)` consistent across Tasks 5-6; `CanonicalDSNIdentity`/`RedactDSN` consistent across Tasks 1, 2, 6.

**Green at every boundary** — Tasks 1-2 additive; Task 3 ends with handlers on the new method and `RetryLockContention` removed; Task 4 is in-package reorg; Task 5 is the atomic move guarded by the relocated suite + assertion; Task 6 adds tested dispatch. Each task ends with `go build ./... && go test ./...` (and lint on the larger tasks).
