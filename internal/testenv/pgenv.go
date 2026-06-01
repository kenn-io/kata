package testenv

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NewPostgresContainer starts a postgres:17-alpine container, waits for it to
// become ready, and returns the DSN string plus a cleanup function. Callers
// must register the cleanup via t.Cleanup themselves so test ordering stays
// predictable. The container lives for the test's lifetime; cleanup tears it
// down. Used by pgstore tests; Phase 4+ conformance tests build on top.
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
	container, err := postgres.Run(ctx,
		"postgres:17-alpine",
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
