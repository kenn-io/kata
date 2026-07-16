package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// setupTestHome creates an isolated KATA_HOME for the test and returns its
// path. Using t.TempDir keeps parallel tests from colliding on a shared dir.
func setupTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	return home
}

func TestKataHome_PrefersEnvOverDefault(t *testing.T) {
	home := setupTestHome(t)

	got, err := config.KataHome()
	require.NoError(t, err)
	assert.Equal(t, home, got)
}

func TestKataHome_DefaultsToUserHomeDotKata(t *testing.T) {
	t.Setenv("KATA_HOME", "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	got, err := config.KataHome()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".kata"), got)
}

func TestKataDB_PrefersEnvOverHomeJoin(t *testing.T) {
	home := setupTestHome(t)
	t.Setenv("KATA_DB", filepath.Join(home, "custom.db"))

	got, err := config.KataDB()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "custom.db"), got)
}

func TestKataDB_DefaultsToHomeKataDB(t *testing.T) {
	home := setupTestHome(t)
	t.Setenv("KATA_DB", "")

	got, err := config.KataDB()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "kata.db"), got)
}

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

func TestDBHash_StableTwelveLowerHex(t *testing.T) {
	a := config.DBHash("/Users/foo/.kata/kata.db")
	b := config.DBHash("/Users/foo/.kata/kata.db")
	c := config.DBHash("/Users/foo/.kata/other.db")

	assert.Len(t, a, 12)
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
	assert.Equal(t, strings.ToLower(a), a)
}

func TestDBHashSQLitePathUnchanged(t *testing.T) {
	// Golden value pins the pre-1d SQLite hashing (sha256(abs(path))[:12]) so
	// the move never relocates an existing database's runtime dir/socket. The
	// hash is taken over filepath.Abs(dbPath); on Windows, "/var/lib/..." is
	// not an absolute path so Abs prepends the CWD and produces a different
	// digest. The backwards-compat property still holds on Windows (same
	// formula in production), but the Unix-shaped golden doesn't apply there.
	if runtime.GOOS == "windows" {
		t.Skip("Unix path golden value; Windows has its own path shape")
	}
	assert.Equal(t, "1f9b906d5e3f", config.DBHash("/var/lib/kata/kata.db"))
}

func TestDBHashPostgresUsesCredentialFreeCanonicalForm(t *testing.T) {
	clearPostgresRoutingEnv(t)
	full := "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require" //nolint:gosec // fixture proves the credential never reaches the hash
	got := config.DBHash(full)
	// Stable effective-target identity, independent of credentials, incidental
	// query params, and the postgres default port (5432).
	assert.Equal(t, "7d5d38a526ca", got)
	assert.Equal(t, got, config.DBHash("postgres://other:pw2@db.example.com:5432/kata?application_name=x"))
	// Explicit :5432 must hash the same as no-port (same logical DB).
	assert.Equal(t, got, config.DBHash("postgres://db.example.com/kata"))
}

func TestStorageHashPostgresIncludesNormalizedSchema(t *testing.T) {
	clearPostgresRoutingEnv(t)
	dsn := "postgres://user@db.example.com/kata?sslmode=verify-full"

	defaultSchema := config.StorageHash(dsn, "")
	assert.Equal(t, defaultSchema, config.StorageHash(dsn, " kata "))
	assert.NotEqual(t, defaultSchema, config.StorageHash(dsn, "archive"))
	assert.Equal(t, config.StorageHash(dsn, "archive"),
		config.StorageHash("postgres://other@db.example.com/kata?application_name=x", "archive"))
}

func TestStorageHashPostgresPreservesSingleTargetRuntimeNamespace(t *testing.T) {
	clearPostgresRoutingEnv(t)
	assert.Equal(t, "a396fd24cff8",
		config.StorageHash(
			"postgres://user:secret@db.example.com:5432/kata?sslmode=verify-full", //nolint:gosec // fixed credential fixture proves it cannot alter the legacy namespace
			"kata",
		),
		"ordinary single-target DSNs must keep the pre-routing-fix daemon namespace")
}

func TestStorageHashPostgresPreservesEncodedDatabaseRuntimeNamespace(t *testing.T) {
	clearPostgresRoutingEnv(t)
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "fragment delimiter",
			dsn:  "postgres://user@db.example.com/team%23blue?sslmode=verify-full",
			want: "252a2d2691a5",
		},
		{
			name: "query delimiter",
			dsn:  "postgres://user@db.example.com/team%3Fblue?sslmode=verify-full",
			want: "1f7c7b34c3e8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, config.StorageHash(tt.dsn, "kata"),
				"percent-encoded database names must keep the legacy daemon namespace")
		})
	}
}

func TestStorageHashPostgresIncludesEffectiveRoutingTargets(t *testing.T) {
	clearPostgresRoutingEnv(t)
	firstSocket := "postgres://user@/kata?host=/var/run/postgresql-a&sslmode=disable"
	secondSocket := "postgres://user@/kata?host=/var/run/postgresql-b&sslmode=disable"

	assert.NotEqual(t,
		config.StorageHash(firstSocket, "kata"),
		config.StorageHash(secondSocket, "kata"),
		"different Unix-socket targets must never share a daemon namespace")
	assert.Equal(t,
		config.StorageHash(firstSocket, "kata"),
		config.StorageHash("postgres://other:secret@/kata?host=/var/run/postgresql-a&application_name=worker&sslmode=disable", "kata"), //nolint:gosec // fixture proves credentials and incidental params do not alter target identity
		"credentials and incidental connection settings must not split one storage target")
}

func TestStorageHashPostgresIncludesFallbackTargets(t *testing.T) {
	clearPostgresRoutingEnv(t)
	firstFallback := "postgres://user@primary.example/kata?host=primary.example,standby-a.example&port=5432,5433&sslmode=verify-full"
	secondFallback := "postgres://user@primary.example/kata?host=primary.example,standby-b.example&port=5432,5433&sslmode=verify-full"

	assert.NotEqual(t,
		config.StorageHash(firstFallback, "kata"),
		config.StorageHash(secondFallback, "kata"),
		"different fallback servers must never share a daemon namespace")
}

func TestStorageHashPostgresIncludesAmbientRouting(t *testing.T) {
	tests := []struct {
		name   string
		dsn    string
		env    string
		first  string
		second string
	}{
		{
			name: "host",
			dsn:  "postgres:///kata?sslmode=disable",
			env:  "PGHOST", first: "/var/run/postgresql-a", second: "/var/run/postgresql-b",
		},
		{
			name: "host with explicit port",
			dsn:  "postgres://:5433/kata?sslmode=disable",
			env:  "PGHOST", first: "/var/run/postgresql-a", second: "/var/run/postgresql-b",
		},
		{
			name: "port",
			dsn:  "postgres://db.example.com/kata?sslmode=verify-full",
			env:  "PGPORT", first: "5433", second: "5434",
		},
		{
			name: "database",
			dsn:  "postgres://db.example.com/?sslmode=verify-full",
			env:  "PGDATABASE", first: "kata_a", second: "kata_b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearPostgresRoutingEnv(t)
			t.Setenv(tt.env, tt.first)
			first := config.StorageHash(tt.dsn, "kata")
			t.Setenv(tt.env, tt.second)
			second := config.StorageHash(tt.dsn, "kata")
			assert.NotEqual(t, first, second,
				"ambient PostgreSQL routing must identify the effective daemon target")
		})
	}
}

func TestStorageHashPostgresIgnoresAmbientRoutingOverriddenByURL(t *testing.T) {
	clearPostgresRoutingEnv(t)
	dsn := "postgres://db.example.com:5432/kata?sslmode=verify-full"
	want := config.StorageHash(dsn, "kata")

	t.Setenv("PGHOST", "other.example")
	t.Setenv("PGPORT", "5433")
	t.Setenv("PGDATABASE", "other_database")
	assert.Equal(t, want, config.StorageHash(dsn, "kata"),
		"irrelevant ambient settings must not split an explicit PostgreSQL target")
}

func clearPostgresRoutingEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PGHOST", "PGPORT", "PGDATABASE", "PGSERVICE", "PGSERVICEFILE"} {
		t.Setenv(name, "")
	}
}

func TestRuntimeDir_NamespaceIsDBHashUnderHome(t *testing.T) {
	home := setupTestHome(t)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))

	got, err := config.RuntimeDir()
	require.NoError(t, err)
	hash := config.DBHash(filepath.Join(home, "kata.db"))
	assert.Equal(t, filepath.Join(home, "runtime", hash), got)
}

const testDBHash = "abc123def456"

func TestHookConfigPath_HonorsKataHome(t *testing.T) {
	home := setupTestHome(t)

	got, err := config.HookConfigPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "hooks.toml"), got)
}

func TestHookRootDir_NamespacedByDBHash(t *testing.T) {
	home := setupTestHome(t)

	got, err := config.HookRootDir(testDBHash)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "hooks", testDBHash), got)
}

func TestHookOutputDir_UnderHookRoot(t *testing.T) {
	setupTestHome(t)

	got, err := config.HookOutputDir(testDBHash)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(got, filepath.Join("hooks", testDBHash, "output")),
		"HookOutputDir = %q, want suffix hooks/%s/output", got, testDBHash)
}

func TestHookRunsPath_UnderHookRoot(t *testing.T) {
	home := setupTestHome(t)

	got, err := config.HookRunsPath(testDBHash)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "hooks", testDBHash, "runs.jsonl"), got)
}

// TestHookRootDir_RejectsNonHash pins that path helpers refuse to join
// any string that isn't a 12-char lower-hex DBHash, so a corrupted state
// file or test typo can't escape <KataHome>/hooks via path traversal.
func TestHookRootDir_RejectsNonHash(t *testing.T) {
	setupTestHome(t)
	cases := []string{
		"",                   // empty
		"../escape",          // traversal
		"with/slash",         // separator
		"abc123def45",        // 11 chars
		"abc123def4567",      // 13 chars
		"ABC123DEF456",       // upper-case
		"abc123def45g",       // non-hex
		string([]byte{0, 1}), // control bytes
	}
	for _, c := range cases {
		_, err := config.HookRootDir(c)
		assert.Errorf(t, err, "HookRootDir(%q) should error", c)
		_, err = config.HookOutputDir(c)
		assert.Errorf(t, err, "HookOutputDir(%q) should error", c)
		_, err = config.HookRunsPath(c)
		assert.Errorf(t, err, "HookRunsPath(%q) should error", c)
	}
}
