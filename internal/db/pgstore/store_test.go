package pgstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// TestStubReturnsErrNotImplementedPhase3 proves a representative stub
// surfaces the sentinel so callers that try to drive pgstore through a
// non-Migrate path fail loudly with a clear error. The sentinel is the
// signal Phase 4 work will switch off as queries land.
func TestStubReturnsErrNotImplementedPhase3(t *testing.T) {
	store := pgstore.NewStoreForTesting("postgres://h/db")
	_, err := store.ListProjects(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, pgstore.ErrNotImplementedPhase3),
		"want errors.Is(err, ErrNotImplementedPhase3), got %v", err)
}

// TestStoreSatisfiesStorageInterface is a runtime-side mirror of the
// compile-time assertion in stubs.go. Kept so a future split of stubs
// across files (Phase 4) cannot silently break the interface.
func TestStoreSatisfiesStorageInterface(_ *testing.T) {
	var _ db.Storage = pgstore.NewStoreForTesting("postgres://h/db")
}

// TestOpen_ConnectsAndReturnsStore proves Open against a fresh PG container
// returns a usable *Store and Ping works. -short skip because testcontainers
// requires docker/podman.
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

	require.NoError(t, store.PingContext(ctx))
}

// TestPeekSchemaVersion_ReturnsZeroForFreshDB proves the package-level peek
// against a fresh DB (no meta table) returns 0, mirroring sqlitestore's
// PeekSchemaVersion contract.
func TestPeekSchemaVersion_ReturnsZeroForFreshDB(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	v, err := pgstore.PeekSchemaVersion(ctx, dsn)
	require.NoError(t, err)
	assert.Equal(t, 0, v)
}

// TestStorePath_RedactsCredentials proves Path() never leaks the password
// from the DSN. Backstop for roborev #5357 HIGH: the CanonicalDSNIdentity
// form is what runtime records persist for the identity-bearing surface,
// and Path() must agree. Uses NewStoreForTesting (no live connection) so
// the test exercises pure DSN handling without a container round-trip.
func TestStorePath_RedactsCredentials(t *testing.T) {
	const secret = "VeryS3cretP@ssword"
	const dsn = "postgres://kata:" + secret + "@db.example.com:5432/kata_test?sslmode=disable"

	store := pgstore.NewStoreForTesting(dsn)
	path := store.Path()
	assert.NotContains(t, path, secret, "Path() must not leak the password")
	assert.NotContains(t, path, "kata:"+secret, "Path() must not leak user:pass form")
	// CanonicalDSNIdentity drops userinfo entirely; the identity should
	// carry only scheme + host + db.
	assert.Equal(t, "postgres://db.example.com/kata_test", path)
}

// TestStorePath_MalformedDSNStillRedacts proves that even when
// CanonicalDSNIdentity refuses a DSN (e.g. ambiguous userinfo, where the
// password contained an unencoded ':' or '@'), Path() falls back to
// RedactDSN rather than returning the raw secret-bearing string.
func TestStorePath_MalformedDSNStillRedacts(t *testing.T) {
	// Use a DSN whose password contains an unencoded ':' so
	// CanonicalDSNIdentity errors and the fallback path runs.
	const dsn = "postgres://user:pa:ss@host/db"

	store := pgstore.NewStoreForTesting(dsn)
	path := store.Path()
	// Either canonical succeeded (no secret) or redact ran (password
	// replaced with xxxxx). Neither path leaks the raw password.
	assert.NotContains(t, path, "pa:ss", "Path() must not leak the raw password")
}
