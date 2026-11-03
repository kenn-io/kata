package testenv

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
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
	return newIsolatedPostgresDatabase(ctx, t, postgresServerDSN(ctx, t, image, baseDSN), "")
}

// postgresServerDSN returns the explicit server DSN, or the shared container
// for image, skipping the test when neither is available.
func postgresServerDSN(ctx context.Context, t *testing.T, image, baseDSN string) string {
	t.Helper()
	if baseDSN != "" {
		return baseDSN
	}
	if skipAutomaticPostgresContainer(runtime.GOOS, os.Getenv(postgresAutoContainerEnv)) {
		t.Skip("automatic PostgreSQL testcontainers are disabled; use KATA_TEST_POSTGRES_DSN to run PostgreSQL tests")
	}
	server := sharedPostgresContainer(ctx, image)
	if server.err != nil {
		t.Skipf("postgres testcontainer unavailable (docker/podman not reachable?): %v", server.err)
	}
	return server.dsn
}

// postgresTemplatePrefix names template databases so any test process can
// find templates that a finished or crashed process left behind.
const postgresTemplatePrefix = "kata_template_"

// PostgresTemplate gives each test a clone of a database that prepare set up
// once per server, so tests skip repeating expensive setup such as installing
// a schema. Clones come from the same server as NewPostgresContainer.
type PostgresTemplate struct {
	prepare func(ctx context.Context, dsn string) error
	mu      sync.Mutex
	servers map[string]postgresTemplateServer
}

type postgresTemplateServer struct {
	name string
	err  error
}

// NewPostgresTemplate returns a template that prepare initializes through
// the given DSN. prepare must close its connections before returning, because
// PostgreSQL cannot clone a database that has open sessions.
func NewPostgresTemplate(prepare func(ctx context.Context, dsn string) error) *PostgresTemplate {
	return &PostgresTemplate{prepare: prepare, servers: map[string]postgresTemplateServer{}}
}

// NewDatabase returns a DSN for an isolated clone of the template and a
// cleanup function that drops it. It skips like NewPostgresContainer.
//
//nolint:revive // t is the canonical first arg for testing helpers
func (tmpl *PostgresTemplate) NewDatabase(t *testing.T, ctx context.Context) (string, func()) {
	t.Helper()
	baseDSN := postgresServerDSN(ctx, t, "pgvector/pgvector:pg17", os.Getenv("KATA_TEST_POSTGRES_DSN"))
	server := tmpl.on(ctx, baseDSN)
	if server.err != nil {
		t.Fatalf("prepare PostgreSQL test template: %v", server.err)
	}
	return newIsolatedPostgresDatabase(ctx, t, baseDSN, server.name)
}

func (tmpl *PostgresTemplate) on(ctx context.Context, baseDSN string) postgresTemplateServer {
	tmpl.mu.Lock()
	defer tmpl.mu.Unlock()
	if server, ok := tmpl.servers[baseDSN]; ok {
		return server
	}
	name, err := createPostgresTemplate(context.WithoutCancel(ctx), baseDSN, tmpl.prepare)
	server := postgresTemplateServer{name: name, err: err}
	tmpl.servers[baseDSN] = server
	return server
}

// createPostgresTemplate builds a template database. Its owner keeps a
// session named after the template open until the process exits; templates
// without such a session are abandoned and are dropped here first.
func createPostgresTemplate(ctx context.Context, baseDSN string, prepare func(context.Context, string) error) (string, error) {
	name := fmt.Sprintf("%s%d_%d", postgresTemplatePrefix, os.Getpid(), postgresDatabaseCounter.Add(1))
	ownerDSN, err := postgresDatabaseDSN(baseDSN, "")
	if err != nil {
		return "", err
	}
	query := ownerDSN.Query()
	query.Set("application_name", name)
	ownerDSN.RawQuery = query.Encode()
	pool, err := sql.Open("pgx", ownerDSN.String())
	if err != nil {
		return "", fmt.Errorf("open PostgreSQL test server: %w", err)
	}
	// The owner session is deliberately never closed.
	owner, err := pool.Conn(ctx)
	if err != nil {
		_ = pool.Close()
		return "", fmt.Errorf("connect PostgreSQL test server: %w", err)
	}
	if err := dropAbandonedPostgresTemplates(ctx, owner); err != nil {
		return "", err
	}
	if _, err := owner.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return "", fmt.Errorf("create PostgreSQL test template: %w", err)
	}
	templateDSN, err := postgresDatabaseDSN(baseDSN, name)
	if err != nil {
		return "", err
	}
	if err := prepare(ctx, templateDSN.String()); err != nil {
		return "", err
	}
	return name, nil
}

func dropAbandonedPostgresTemplates(ctx context.Context, owner *sql.Conn) error {
	rows, err := owner.QueryContext(ctx, `SELECT datname FROM pg_database
WHERE datname LIKE $1 AND datname NOT IN (
	SELECT application_name FROM pg_stat_activity WHERE application_name IS NOT NULL
)`, strings.ReplaceAll(postgresTemplatePrefix, "_", `\_`)+"%")
	if err != nil {
		return fmt.Errorf("list abandoned PostgreSQL test templates: %w", err)
	}
	var abandoned []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("list abandoned PostgreSQL test templates: %w", err)
		}
		abandoned = append(abandoned, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list abandoned PostgreSQL test templates: %w", err)
	}
	for _, name := range abandoned {
		if _, err := owner.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			return fmt.Errorf("drop abandoned PostgreSQL test template %s: %w", name, err)
		}
	}
	return nil
}

func postgresDatabaseDSN(baseDSN, database string) (*url.URL, error) {
	u, err := url.Parse(baseDSN)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL test server DSN: %w", err)
	}
	if database != "" {
		u.Path = "/" + database
		u.RawPath = ""
	}
	return u, nil
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

// startPostgresContainer runs a server that trades durability for speed:
// test databases are disposable, so the data directory lives in memory and
// commits never wait for WAL flushes. The module already disables fsync.
func startPostgresContainer(ctx context.Context, image string) sharedPostgresServer {
	container, err := postgres.Run(ctx,
		image,
		postgres.WithDatabase("kata_test"),
		postgres.WithUsername("kata"),
		postgres.WithPassword("kata"),
		testcontainers.WithTmpfs(map[string]string{"/pgdata": "rw"}),
		testcontainers.WithEnv(map[string]string{"PGDATA": "/pgdata/data"}),
		testcontainers.WithCmdArgs("-c", "synchronous_commit=off", "-c", "full_page_writes=off"),
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

func skipAutomaticPostgresContainer(goos, autostart string) bool {
	return goos == "windows" || autostart == "0"
}

// newIsolatedPostgresDatabase turns a server DSN, from the explicit CI service
// or the shared container, into one database per test. Unlike a container
// that fails to start, an unusable server is fatal: CI must never report
// success by skipping the Postgres suite.
//
// A non-empty template names the database to clone; empty creates an empty
// database.
func newIsolatedPostgresDatabase(ctx context.Context, t *testing.T, baseDSN, template string) (string, func()) {
	t.Helper()
	databaseName := fmt.Sprintf("kata_test_%d_%d", os.Getpid(), postgresDatabaseCounter.Add(1))
	u, err := postgresDatabaseDSN(baseDSN, databaseName)
	if err != nil {
		t.Fatalf("%v", err)
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open PostgreSQL test server: %v", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("connect PostgreSQL test server: %v", err)
	}
	create := `CREATE DATABASE "` + databaseName + `"` //nolint:gosec // G202: generated test database name, not input.
	if template != "" {
		create += ` TEMPLATE "` + template + `"`
	}
	if _, err := admin.ExecContext(ctx, create); err != nil {
		_ = admin.Close()
		t.Fatalf("create isolated Postgres test database: %v", err)
	}
	cleanup := func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE "`+databaseName+`" WITH (FORCE)`)
		_ = admin.Close()
	}
	return u.String(), cleanup
}
