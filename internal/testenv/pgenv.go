package testenv

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the pgx database/sql driver for explicit CI services
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var postgresDatabaseCounter atomic.Uint64

// sharedPostgresServers holds one automatic container per image for the
// test process. Testcontainers' reaper removes them when the process exits.
var sharedPostgresServers = struct {
	sync.Mutex
	byImage map[string]sharedPostgresServer
}{byImage: map[string]sharedPostgresServer{}}

type sharedPostgresServer struct {
	dsn string
	err error
}

const postgresAutoContainerEnv = "KATA_TEST_POSTGRES_AUTO_CONTAINER"
const plainPostgresDSNEnv = "KATA_TEST_PLAIN_POSTGRES_DSN"

// NewPostgresContainer returns a DSN for an isolated database on a
// pgvector-enabled PostgreSQL 17 server, plus a cleanup function that drops
// that database. Callers must register the cleanup via t.Cleanup themselves
// so test ordering stays predictable. Used by pgstore tests and the shared
// storage conformance suite.
//
// With KATA_TEST_POSTGRES_DSN set, the databases live on that server.
// Otherwise the first call in a test process starts a container that every
// later call in the process reuses; testcontainers' reaper removes it when
// the process exits. Starting a container takes far longer than creating a
// database, so sharing the server keeps PostgreSQL suites within the default
// go test timeout.
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
	return newPostgresContainer(
		ctx, t, "pgvector/pgvector:pg17", os.Getenv("KATA_TEST_POSTGRES_DSN"),
	)
}

// NewPostgresWithPgvectorContainer starts PostgreSQL 17 and installs pgvector
// explicitly for tests of semantic storage.
//
//nolint:revive // t is the canonical first arg for testing helpers
func NewPostgresWithPgvectorContainer(t *testing.T, ctx context.Context) (string, func()) {
	dsn, cleanup := newPostgresContainer(
		ctx, t, "pgvector/pgvector:pg17", os.Getenv("KATA_TEST_POSTGRES_DSN"),
	)
	if err := installTestPgvector(ctx, dsn); err != nil {
		cleanup()
		t.Fatalf("install PostgreSQL test extension pgvector: %v", err)
	}
	return dsn, cleanup
}

// NewPlainPostgresContainer starts PostgreSQL 17 without third-party
// extensions. It is used to prove that core storage does not depend on
// optional database features.
//
//nolint:revive // t is the canonical first arg for testing helpers
func NewPlainPostgresContainer(t *testing.T, ctx context.Context) (string, func()) {
	return newPostgresContainer(ctx, t, "postgres:17", os.Getenv(plainPostgresDSNEnv))
}

func newPostgresContainer(
	ctx context.Context,
	t *testing.T,
	image string,
	baseDSN string,
) (string, func()) {
	t.Helper()
	if baseDSN != "" {
		return newIsolatedPostgresDatabase(ctx, t, baseDSN)
	}
	if skipAutomaticPostgresContainer(runtime.GOOS, baseDSN, os.Getenv(postgresAutoContainerEnv)) {
		t.Skip("automatic PostgreSQL testcontainers are disabled; use KATA_TEST_POSTGRES_DSN to run PostgreSQL tests")
	}
	server := sharedPostgresContainer(ctx, image)
	if server.err != nil {
		t.Skipf("postgres testcontainer unavailable (docker/podman not reachable?): %v", server.err)
	}
	return newIsolatedPostgresDatabase(ctx, t, server.dsn)
}

// sharedPostgresContainer starts the image's container on first use and
// returns the cached server, or the cached startup failure, afterwards.
func sharedPostgresContainer(ctx context.Context, image string) sharedPostgresServer {
	sharedPostgresServers.Lock()
	defer sharedPostgresServers.Unlock()
	if server, ok := sharedPostgresServers.byImage[image]; ok {
		return server
	}
	server := startPostgresContainer(context.WithoutCancel(ctx), image)
	sharedPostgresServers.byImage[image] = server
	return server
}

func startPostgresContainer(ctx context.Context, image string) sharedPostgresServer {
	container, err := postgres.Run(ctx,
		image,
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
		return sharedPostgresServer{err: err}
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return sharedPostgresServer{err: fmt.Errorf("get postgres connection string: %w", err)}
	}
	return sharedPostgresServer{dsn: dsn}
}

func installTestPgvector(ctx context.Context, dsn string) error {
	database, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open PostgreSQL test database: %w", err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return err
	}
	return nil
}

func skipAutomaticPostgresContainer(goos, explicitDSN, autostart string) bool {
	return explicitDSN == "" && (goos == "windows" || autostart == "0")
}

// newIsolatedPostgresDatabase turns a server DSN, from the explicit CI service
// or the shared container, into one database per test. Unlike a container
// that fails to start, an unusable server is fatal: CI must never report
// success by skipping the Postgres suite.
func newIsolatedPostgresDatabase(ctx context.Context, t *testing.T, baseDSN string) (string, func()) {
	t.Helper()
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse PostgreSQL test server DSN: %v", err)
	}
	databaseName := fmt.Sprintf("kata_test_%d_%d", os.Getpid(), postgresDatabaseCounter.Add(1))
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open PostgreSQL test server: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("connect PostgreSQL test server: %v", err)
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
