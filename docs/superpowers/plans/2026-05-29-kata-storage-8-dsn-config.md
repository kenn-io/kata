# Kata Storage Phase 8 — DSN / config-file wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the postgres backend to users by plumbing a DSN through env, file, and default with clear precedence and shape validation, without changing behavior for existing setups. Phase 8 is the config-layer plumbing; the pgstore backend lands in later phases.

**Architecture:** A new `config.KataDSN()` resolves the effective DSN from `$KATA_DSN > $KATA_DB > [storage].dsn > default`. A new `[storage]` table in `<KATA_HOME>/config.toml` carries `dsn = "..."`. Shape validation rejects unknown schemes and PG-only query params on sqlite DSNs with credential-free errors. `config.KataDB()` becomes a thin alias delegating to `KataDSN()` so the existing four callers and their tests do not change behavior. `cmd/kata/daemon_cmd.go` redacts the DSN written to `RuntimeRecord.DBPath` so the daemon's runtime file and `kata daemon status` output do not leak a postgres password.

**Tech Stack:** Go 1.26, `github.com/BurntSushi/toml` (already in use for `DaemonConfig`), `github.com/stretchr/testify` for assertions, existing `config.RedactDSN` / `config.CanonicalDSNIdentity` for credential handling.

---

## File structure

**Create:**
- `internal/config/dsn_resolve.go` — `KataDSN()`, `validateDSN()`.
- `internal/config/dsn_resolve_test.go` — precedence + validation tests.

**Modify:**
- `internal/config/daemon_config.go` — add `StorageConfig` struct and `Storage` field on `DaemonConfig`.
- `internal/config/daemon_config_test.go` — TOML decode tests for `[storage].dsn`.
- `internal/config/paths.go` — `KataDB()` delegates to `KataDSN()`.
- `internal/config/paths_test.go` — extend existing `KataDB` tests for the new behavior (env-DSN precedence over env-DB).
- `cmd/kata/daemon_cmd.go` — flip `config.KataDB()` → `config.KataDSN()`, redact `DBPath` written into `RuntimeRecord`.
- `cmd/kata/daemon_cmd_test.go` — assert the redaction.
- `cmd/kata/migrate.go` — flip `config.KataDB()` → `config.KataDSN()`.
- `cmd/kata/export.go` — flip `config.KataDB()` → `config.KataDSN()`.
- `cmd/kata/daemon_logs.go` — flip `config.KataDB()` → `config.KataDSN()`.
- `internal/daemon/namespace.go` — flip `config.KataDB()` → `config.KataDSN()`.

---

## Task ordering rationale

Each commit must build and `go test ./...` must pass before the next task starts. The order keeps these invariants by introducing additive surface first, then layering the precedence and validation, then making `KataDB` a thin alias:

- Task 1 adds `KataDSN` and `validateDSN` with env-only precedence (no TOML yet). `KataDB` stays untouched, so all existing call sites and tests behave identically.
- Task 2 adds the `[storage]` table and its decode to `DaemonConfig`. `KataDSN` does not yet read it.
- Task 3 wires the TOML branch into `KataDSN` so all four precedence tiers work end-to-end.
- Task 4 collapses `KataDB` to a thin alias delegating to `KataDSN`. Tests that called `KataDB` still pass — its observable behavior is unchanged when only `$KATA_DB` is set or both are unset.
- Task 5 flips the five `KataDB` call sites to `KataDSN`. Local variable names stay (`dbPath`) because they wire into existing struct fields whose names match.
- Task 6 redacts `RuntimeRecord.DBPath` at the write site.
- Task 7 adds an end-to-end test confirming `KATA_DSN=postgres://...` reaches `storeopen.Open` with the "not yet available" error.

---

## Task 1: Add `KataDSN()` resolver and `validateDSN()` shape check (env-only)

This task introduces the resolver in env-only mode so `KataDSN` is exercised by tests without requiring TOML changes yet.

**Files:**
- Create: `internal/config/dsn_resolve.go`
- Create: `internal/config/dsn_resolve_test.go`

- [ ] **Step 1: Write the failing precedence tests**

Create `internal/config/dsn_resolve_test.go`:

```go
package config_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestKataDSN_PrefersKataDSNOverKataDB(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://db.example.com/kata")
	t.Setenv("KATA_DB", "/tmp/should-be-shadowed.db")

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "postgres://db.example.com/kata", got)
}

func TestKataDSN_FallsBackToKataDBWhenKataDSNUnset(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "/tmp/from-env.db")

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "/tmp/from-env.db", got)
}

func TestKataDSN_DefaultsToHomeKataDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "")

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "kata.db"), got)
}

func TestKataDSN_TrimsEnvWhitespace(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "  postgres://h/db  ")
	t.Setenv("KATA_DB", "")

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "postgres://h/db", got)
}

func TestKataDSN_RejectsUnknownScheme(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "mysql://h/db")
	t.Setenv("KATA_DB", "")

	_, err := config.KataDSN()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported dsn scheme")
	assert.Contains(t, err.Error(), "mysql")
}

func TestKataDSN_RejectsPGOnlyQueryParamsOnSQLiteDSN(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "sqlite:///tmp/k.db?sslmode=require")
	t.Setenv("KATA_DB", "")

	_, err := config.KataDSN()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sqlite")
	assert.Contains(t, err.Error(), "sslmode")
}

func TestKataDSN_RejectsPGOnlyQueryParamsOnBarePath(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "/tmp/k.db?pool_max_conns=8")
	t.Setenv("KATA_DB", "")

	_, err := config.KataDSN()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool_max_conns")
}

func TestKataDSN_RejectsAmbiguousPostgresCredentials(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://user:p://w@host/db")
	t.Setenv("KATA_DB", "")

	_, err := config.KataDSN()
	require.Error(t, err)
	// The error must NOT echo the embedded credential.
	assert.NotContains(t, err.Error(), "p://w")
}

func TestKataDSN_ValidationErrorOnPostgresOmitsPassword(t *testing.T) {
	// A bad-port postgres DSN with a password — validateDSN does not parse
	// past CanonicalDSNIdentity, so the error must not echo the input.
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://user:SECRET@host:badport/db") //nolint:gosec // fixture
	t.Setenv("KATA_DB", "")

	_, err := config.KataDSN()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET")
}

func TestKataDSN_AcceptsPostgresDSN(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://user:pw@host:5432/kata?sslmode=require")
	t.Setenv("KATA_DB", "")

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pw@host:5432/kata?sslmode=require", got)
}

func TestKataDSN_AcceptsSQLiteDSN(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "sqlite:///var/lib/kata/kata.db")
	t.Setenv("KATA_DB", "")

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "sqlite:///var/lib/kata/kata.db", got)
}

func TestKataDSN_AcceptsBarePath(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "/var/lib/kata/kata.db")
	t.Setenv("KATA_DB", "")

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/kata/kata.db", got)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/... -run TestKataDSN -v`
Expected: FAIL (`config.KataDSN` undefined).

- [ ] **Step 3: Add the resolver**

Create `internal/config/dsn_resolve.go`:

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KataDSN returns the effective database DSN honoring (in precedence order)
// $KATA_DSN, $KATA_DB, [storage].dsn from <KATA_HOME>/config.toml, and the
// <KATA_HOME>/kata.db default. The returned string is whatever the user
// supplied: a bare path, sqlite:// DSN, or postgres:// DSN. Callers pass it
// directly to storeopen.Open. Shape validation rejects unknown schemes and
// libpq query params on sqlite/bare DSNs with credential-free errors;
// validation is shape-only and never dials.
//
// Phase 8 introduces this resolver alongside the existing config.KataDB,
// which keeps working as a thin alias for callers that still spell it the
// old way.
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
	// TOML branch wires up in Task 3. For now fall through to the default.
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "kata.db"), nil
}

// validateDSN performs shape-only validation: it rejects unknown schemes and
// libpq query params on sqlite/bare DSNs, and propagates the ambiguous-
// credentials probe from CanonicalDSNIdentity for postgres DSNs. It never
// echoes the DSN itself in errors — credentials would leak through error
// logs otherwise.
func validateDSN(dsn string) error {
	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case hasScheme && scheme != "sqlite" && scheme != "postgres" && scheme != "postgresql":
		return fmt.Errorf("unsupported dsn scheme %q", scheme)
	case hasScheme && (scheme == "postgres" || scheme == "postgresql"):
		// Probe for the credential-bleed shape so the error stays
		// credential-free. CanonicalDSNIdentity already implements the probe.
		if _, err := CanonicalDSNIdentity(dsn); err != nil {
			return err
		}
		return nil
	}
	// scheme is "sqlite" or absent — reject libpq-only query params.
	if param, ok := firstPGOnlyQueryParam(dsn); ok {
		label := "sqlite DSN"
		if !hasScheme {
			label = "sqlite path"
		}
		return fmt.Errorf("%s does not support %q query param; did you mean postgres://?", label, param)
	}
	return nil
}

// pgOnlyQueryParams enumerates libpq / pgx query-parameter names that have no
// SQLite analogue. A bare or sqlite:// DSN carrying any of these is almost
// certainly a misformatted postgres DSN.
var pgOnlyQueryParams = []string{
	"sslmode=",
	"pool_max_conns=",
	"application_name=",
	"connect_timeout=",
	"target_session_attrs=",
	"password=",
	"sslpassword=",
}

// firstPGOnlyQueryParam reports whether dsn carries any pg-only query param
// after the first "?". The search is case-sensitive — libpq treats parameter
// names as case-sensitive in URL form.
func firstPGOnlyQueryParam(dsn string) (string, bool) {
	q := strings.Index(dsn, "?")
	if q < 0 {
		return "", false
	}
	query := dsn[q+1:]
	for _, p := range pgOnlyQueryParams {
		if strings.Contains(query, p) {
			// Strip the trailing "=" so the error names the parameter cleanly.
			return strings.TrimSuffix(p, "="), true
		}
	}
	return "", false
}
```

`splitScheme` is the existing private helper declared in `internal/config/dsn.go`;
both files live in the same package so this file calls it directly without
duplicating the helper.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/config/... -run TestKataDSN -v`
Expected: PASS (all 11 cases).

- [ ] **Step 5: Run the full test suite**

Run: `go test ./...`
Expected: PASS (no regression — `KataDSN` is unused outside its tests; `KataDB` and existing precedence are unchanged).

- [ ] **Step 6: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config/dsn_resolve.go internal/config/dsn_resolve_test.go
git commit -m "feat(config): add KataDSN resolver and DSN shape validation"
```

---

## Task 2: Add `[storage].dsn` to `DaemonConfig`

This task wires the TOML decode path. `KataDSN` does not yet read the value; Task 3 will hook it up.

**Files:**
- Modify: `internal/config/daemon_config.go`
- Modify: `internal/config/daemon_config_test.go`

- [ ] **Step 1: Write the failing TOML decode test**

Append to `internal/config/daemon_config_test.go`:

```go
func TestReadDaemonConfig_ReadsStorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"postgres://db/kata\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://db/kata", cfg.Storage.DSN)
}

func TestReadDaemonConfig_TrimsStorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"  postgres://db/kata  \"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://db/kata", cfg.Storage.DSN)
}

func TestReadDaemonConfig_EmptyStorageDSNIsZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.Storage.DSN)
}

func TestReadDaemonConfig_StorageRejectsUnknownKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\nfoo = \"bar\"\n"), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err, "meta.Undecoded() must catch typo'd storage keys")
	assert.Contains(t, err.Error(), "storage.foo")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/... -run TestReadDaemonConfig_.*Storage -v`
Expected: FAIL (`cfg.Storage.DSN` undefined).

- [ ] **Step 3: Add `StorageConfig` and the `Storage` field**

Modify `internal/config/daemon_config.go`. In the `DaemonConfig` struct add a new field after `Auth`:

```go
	// Auth carries the daemon's bearer-auth token, if any.
	Auth AuthConfig `toml:"auth"`
	// Storage carries DB-selection settings. Today only `dsn` is honored;
	// see config.KataDSN for the full precedence (env > file > default).
	Storage StorageConfig `toml:"storage"`
}
```

Add the new struct after `AuthConfig`:

```go
// StorageConfig is the [storage] block of <KATA_HOME>/config.toml. An
// empty DSN means "no override from the file" — env (KATA_DSN, KATA_DB)
// or the default `<KATA_HOME>/kata.db` wins. See config.KataDSN.
type StorageConfig struct {
	DSN string `toml:"dsn"`
}
```

Modify `ReadDaemonConfig` to trim `Storage.DSN` alongside the other strings. Find the block (around line 112-114):

```go
		cfg.Listen = strings.TrimSpace(cfg.Listen)
		cfg.Auth.Token = strings.TrimSpace(cfg.Auth.Token)
		cfg.Auth.Proxy.TrustedActorHeader = strings.TrimSpace(cfg.Auth.Proxy.TrustedActorHeader)
```

and append:

```go
		cfg.Storage.DSN = strings.TrimSpace(cfg.Storage.DSN)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/config/... -run TestReadDaemonConfig_.*Storage -v`
Expected: PASS (4 cases).

- [ ] **Step 5: Run the full test suite**

Run: `go test ./...`
Expected: PASS (no regression — `Storage` is additive).

- [ ] **Step 6: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config/daemon_config.go internal/config/daemon_config_test.go
git commit -m "feat(config): add [storage].dsn to DaemonConfig"
```

---

## Task 3: Wire `[storage].dsn` into `KataDSN`

`KataDSN` now reads the TOML file when both env vars are unset, before falling through to the default.

**Files:**
- Modify: `internal/config/dsn_resolve.go`
- Modify: `internal/config/dsn_resolve_test.go`

- [ ] **Step 1: Write the failing TOML-precedence tests**

Append to `internal/config/dsn_resolve_test.go`:

```go
func TestKataDSN_ReadsStorageDSNFromConfigToml(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"postgres://h/db\"\n"), 0o600))

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "postgres://h/db", got)
}

func TestKataDSN_KataDBOverridesStorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "/tmp/from-env.db")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"postgres://h/db\"\n"), 0o600))

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "/tmp/from-env.db", got)
}

func TestKataDSN_KataDSNOverridesStorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "postgres://from-env/kata")
	t.Setenv("KATA_DB", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"postgres://from-toml/kata\"\n"), 0o600))

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, "postgres://from-env/kata", got)
}

func TestKataDSN_ValidatesStorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"mysql://h/db\"\n"), 0o600))

	_, err := config.KataDSN()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported dsn scheme")
	assert.Contains(t, err.Error(), "mysql")
}

func TestKataDSN_EmptyStorageDSNFallsThroughToDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"\"\n"), 0o600))

	got, err := config.KataDSN()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "kata.db"), got)
}
```

Also ensure the test file's imports include `"os"` and `"github.com/stretchr/testify/require"`'s package path is already there (it is from Task 1).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/... -run 'TestKataDSN_(ReadsStorage|KataDB?Overrides|ValidatesStorage|EmptyStorage)' -v`
Expected: FAIL (DSN comes back as the default path because `KataDSN` does not yet read TOML).

- [ ] **Step 3: Add the TOML branch to `KataDSN`**

Modify `internal/config/dsn_resolve.go`. Replace the `KataDSN` body's "TOML branch wires up in Task 3" comment block with a real branch. The function now reads:

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

`cfg.Storage.DSN` is already TrimSpace'd by `ReadDaemonConfig` (Task 2), so the
file-branch does not re-trim. Validation always runs against the trimmed
value so a stray trailing space doesn't leak into the error.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/... -run TestKataDSN -v`
Expected: PASS (all KataDSN tests including the new TOML-precedence cases).

- [ ] **Step 5: Run the full test suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 6: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config/dsn_resolve.go internal/config/dsn_resolve_test.go
git commit -m "feat(config): KataDSN reads [storage].dsn from config.toml"
```

---

## Task 4: Make `KataDB` a thin alias delegating to `KataDSN`

`KataDB` keeps its name and its callers, but it delegates so the new `[storage].dsn` source reaches them too. Existing test fixtures that set `KATA_DB` keep working — the precedence still puts env first.

**Files:**
- Modify: `internal/config/paths.go`
- Modify: `internal/config/paths_test.go`

- [ ] **Step 1: Write the failing alias-equivalence test**

Append to `internal/config/paths_test.go`:

```go
func TestKataDB_DelegatesToKataDSN_EnvDSN(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://db/kata")
	t.Setenv("KATA_DB", "")

	got, err := config.KataDB()
	require.NoError(t, err)
	assert.Equal(t, "postgres://db/kata", got,
		"KataDB must now resolve through KataDSN so KATA_DSN reaches the same callers")
}

func TestKataDB_DelegatesToKataDSN_StorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"postgres://from-toml/kata\"\n"), 0o600))

	got, err := config.KataDB()
	require.NoError(t, err)
	assert.Equal(t, "postgres://from-toml/kata", got)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/... -run TestKataDB_DelegatesToKataDSN -v`
Expected: FAIL (today's `KataDB` only reads `$KATA_DB` and falls through to a default path; the env-DSN test gets `<KataHome>/kata.db`).

- [ ] **Step 3: Make `KataDB` delegate to `KataDSN`**

Modify `internal/config/paths.go`. Replace the `KataDB` body:

```go
// KataDB returns the effective DB DSN. This is now a thin alias for
// KataDSN, kept so existing callers and scripts that spell it as the older
// name continue to resolve through the same precedence (KATA_DSN > KATA_DB >
// [storage].dsn > default).
func KataDB() (string, error) { return KataDSN() }
```

The doc comment for `KataDB` updates to reflect that it now returns whatever the user supplied (path or DSN), not just a "path".

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/... -v`
Expected: PASS (all KataDB, KataDSN, and existing precedence tests).

- [ ] **Step 5: Run the full test suite**

Run: `go test ./...`
Expected: PASS — the four call sites of `config.KataDB()` (`cmd/kata/daemon_cmd.go`, `cmd/kata/migrate.go`, `cmd/kata/export.go`, `cmd/kata/daemon_logs.go`, `internal/daemon/namespace.go`) already accept whatever string they get and pass it to `storeopen.Open`. Tests that set only `KATA_DB` see no change because the precedence still puts env first.

- [ ] **Step 6: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config/paths.go internal/config/paths_test.go
git commit -m "refactor(config): KataDB delegates to KataDSN (alias)"
```

---

## Task 5: Flip the five `config.KataDB()` callers to `config.KataDSN()`

The aliasing in Task 4 already routes the right value to every caller — this task is the rename so the call sites read like they document what they hold (a DSN, not necessarily a path).

**Files:**
- Modify: `cmd/kata/daemon_cmd.go`
- Modify: `cmd/kata/migrate.go`
- Modify: `cmd/kata/export.go`
- Modify: `cmd/kata/daemon_logs.go`
- Modify: `internal/daemon/namespace.go`

- [ ] **Step 1: Rename `KataDB` calls to `KataDSN` at each site**

Find each `config.KataDB()` and update it. In `cmd/kata/daemon_cmd.go:291`, `cmd/kata/migrate.go:23`, `cmd/kata/export.go:36`, `cmd/kata/daemon_logs.go:421`, and `internal/daemon/namespace.go:27`. Local variable names stay (`dbPath`) — they wire into `daemon.RuntimeRecord.DBPath` and other existing struct fields whose names match. Renaming variables would force a JSON-tag/field rename and is out of Phase 8 scope.

After the rename, every site reads `config.KataDSN()`.

- [ ] **Step 2: Run the test suite**

Run: `go test ./...`
Expected: PASS — `KataDB` and `KataDSN` resolve to identical values today; the call-site rename is a no-op at runtime.

- [ ] **Step 3: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/kata/daemon_cmd.go cmd/kata/migrate.go cmd/kata/export.go cmd/kata/daemon_logs.go internal/daemon/namespace.go
git commit -m "refactor: callers use config.KataDSN over the KataDB alias"
```

---

## Task 6: Redact `RuntimeRecord.DBPath` at the write site

The daemon writes `dbPath` directly into `daemon.<pid>.json` and emits it via `kata daemon status`. With a postgres DSN this is a credential leak. Redact at the write site.

**Files:**
- Modify: `cmd/kata/daemon_cmd.go`
- Modify: `cmd/kata/daemon_cmd_test.go`

- [ ] **Step 1: Write the failing redaction test**

Append to `cmd/kata/daemon_cmd_test.go` (any test file under `cmd/kata`; placing it alongside other daemon tests keeps discovery natural):

```go
func TestRuntimeRecordRedactsPostgresDSN(t *testing.T) {
	// Build the runtime record the way the daemon does and assert the
	// DBPath field hides the password. Direct unit test on the assembly
	// function avoids spinning up the daemon.
	dsn := "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require" //nolint:gosec // fixture
	got := redactRuntimeDSN(dsn)
	assert.NotContains(t, got, "SECRET")
	assert.Contains(t, got, "db.example.com")
	// Mutation guard: the raw DSN really does contain the secret.
	assert.Contains(t, dsn, "SECRET")
}

func TestRuntimeRecordKeepsSQLitePath(t *testing.T) {
	got := redactRuntimeDSN("/var/lib/kata/kata.db")
	assert.Equal(t, "/var/lib/kata/kata.db", got)
}
```

The test references a helper `redactRuntimeDSN` that does not exist yet — that's the failing assertion. The helper exists so the test can pin behavior without booting a daemon.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/kata/... -run TestRuntimeRecord -v`
Expected: FAIL (`redactRuntimeDSN` undefined).

- [ ] **Step 3: Add the helper and use it at the write site**

Modify `cmd/kata/daemon_cmd.go`. Add a small helper above `runDaemonStart` (or wherever fits the file's structure):

```go
// redactRuntimeDSN returns dsn safe for inclusion in the runtime file and the
// `kata daemon status` output. Bare paths and sqlite DSNs are returned as-is;
// postgres DSNs have their password stripped via config.RedactDSN.
func redactRuntimeDSN(dsn string) string {
	if r := config.RedactDSN(dsn); r != "" {
		return r
	}
	return dsn
}
```

`config.RedactDSN` returns `""` for ambiguous postgres credentials (the bleed shape); the fallback to `dsn` keeps the daemon usable even in that pathological case — but the runtime-DSN value would have already failed `KataDSN` validation upstream, so this branch is defensive.

Update the `RuntimeRecord` literal at `cmd/kata/daemon_cmd.go:325-331`:

```go
	rec := daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Address:   endpoint.Address(),
		DBPath:    redactRuntimeDSN(dbPath),
		Version:   version.Version,
		StartedAt: time.Now().UTC(),
	}
```

The in-process `dbPath` variable used to call `storeopen.Open` keeps the raw DSN; only the persisted/displayed copy is redacted.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/kata/... -run TestRuntimeRecord -v`
Expected: PASS.

- [ ] **Step 5: Run the full test suite**

Run: `go test ./...`
Expected: PASS — sqlite paths pass through `redactRuntimeDSN` unchanged.

- [ ] **Step 6: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/kata/daemon_cmd.go cmd/kata/daemon_cmd_test.go
git commit -m "fix(daemon): redact postgres DSN in RuntimeRecord.DBPath"
```

---

## Task 7: End-to-end test for `$KATA_DSN=postgres://...` reaching `storeopen.Open`

The final test exercises the precedence + redaction + storeopen path together: `KATA_DSN=postgres://...` resolves through `KataDSN`, reaches `storeopen.Open`, and produces the "postgres backend not yet available" error with the password redacted. This is the proof the Phase 8 plumbing actually delivers on the parent spec's goal.

**Files:**
- Modify: `internal/db/storeopen/storeopen_test.go` (the test goes here because `cmd/kata` can't import `storeopen` without driving the cobra command — keeping the end-to-end at the storeopen seam is more direct and follows the file's existing testing pattern).

- [ ] **Step 1: Write the failing end-to-end test**

Append to `internal/db/storeopen/storeopen_test.go`:

```go
func TestOpenResolvedFromKataDSNEnvKeepsPasswordOutOfError(t *testing.T) {
	// End-to-end proof that Phase 8's KATA_DSN plumbing actually reaches
	// storeopen.Open and that the redacted "not yet available" error never
	// echoes the password.
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require") //nolint:gosec // fixture
	t.Setenv("KATA_DB", "")

	dsn, err := config.KataDSN()
	require.NoError(t, err)
	require.Equal(t, "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require", dsn)

	_, _, err = storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet available")
	assert.NotContains(t, err.Error(), "SECRET")
}
```

Add `"go.kenn.io/kata/internal/config"` to the test file's imports.

- [ ] **Step 2: Run the test to verify it passes**

Run: `go test ./internal/db/storeopen/... -run TestOpenResolvedFromKataDSNEnv -v`
Expected: PASS — `KataDSN` returns the raw DSN, `storeopen.Open` redacts it for the error, and the test asserts both.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 4: Lint**

Run: `nix run 'nixpkgs#golangci-lint' -- run ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/db/storeopen/storeopen_test.go
git commit -m "test(storeopen): KATA_DSN postgres path resolves and redacts"
```

---

## Verification checklist

- [ ] `go build ./...` clean.
- [ ] `go test ./...` green.
- [ ] `nix run 'nixpkgs#golangci-lint' -- run ./...` clean.
- [ ] Manual sanity: with all kata env vars unset and no `<KATA_HOME>/config.toml`, `KataDSN()` returns `<KATA_HOME>/kata.db` (existing behavior).
- [ ] Manual sanity: with `KATA_DSN=postgres://...`, `kata daemon start` fails with the "not yet available" error and no password appears in stderr or the runtime file.
