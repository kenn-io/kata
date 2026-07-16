package testenv

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx database/sql driver for explicit CI services
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var postgresDatabaseCounter atomic.Uint64

const postgresAutoContainerEnv = "KATA_TEST_POSTGRES_AUTO_CONTAINER"

// NewPostgresContainer starts a pgvector-enabled PostgreSQL 17 container, waits for it to
// become ready, and returns the DSN string plus a cleanup function. Callers
// must register the cleanup via t.Cleanup themselves so test ordering stays
// predictable. The container lives for the test's lifetime; cleanup tears it
// down. Used by pgstore tests and the shared storage conformance suite.
//
// Skips with t.Skip when docker/podman is not reachable, so the suite runs
// cleanly in environments without a container runtime. -short callers should
// gate this helper themselves; the helper does not consult testing.Short().
//
// Signature note: ctx follows t to keep the parameter order ergonomic for
// table-driven tests; revive's context-first rule is silenced for this
// signature because t is the canonical first arg for testing helpers.
//
//nolint:revive // t is the canonical first arg for testing helpers
func NewPostgresContainer(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	baseDSN := os.Getenv("KATA_TEST_POSTGRES_DSN")
	if baseDSN != "" {
		return newIsolatedPostgresDatabase(ctx, t, baseDSN)
	}
	if skipAutomaticPostgresContainer(runtime.GOOS, baseDSN, os.Getenv(postgresAutoContainerEnv)) {
		t.Skip("automatic PostgreSQL testcontainers are disabled; use KATA_TEST_POSTGRES_DSN to run PostgreSQL tests")
	}
	container, err := postgres.Run(ctx,
		"pgvector/pgvector:pg17",
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
		t.Skipf("postgres testcontainer unavailable (docker/podman not reachable?): %v", err)
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

func skipAutomaticPostgresContainer(goos, explicitDSN, autostart string) bool {
	return explicitDSN == "" && (goos == "windows" || autostart == "0")
}

// newIsolatedPostgresDatabase turns the explicit CI service DSN into one
// database per test. Unlike the best-effort local testcontainer path, an
// invalid explicit service is fatal: CI must never report success by skipping
// the Postgres suite.
func newIsolatedPostgresDatabase(ctx context.Context, t *testing.T, baseDSN string) (string, func()) {
	t.Helper()
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse KATA_TEST_POSTGRES_DSN: %v", err)
	}
	databaseName := fmt.Sprintf("kata_ci_%d_%d", os.Getpid(), postgresDatabaseCounter.Add(1))
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open KATA_TEST_POSTGRES_DSN: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("connect KATA_TEST_POSTGRES_DSN: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+databaseName+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated Postgres test database: %v", err)
	}
	u.Path = "/" + databaseName
	u.RawPath = ""
	cleanup := func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE "`+databaseName+`" WITH (FORCE)`)
		_ = admin.Close()
	}
	return u.String(), cleanup
}
