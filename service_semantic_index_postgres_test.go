package kata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestServiceSemanticIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	endpoint := testenv.NewEmbeddingServer(t, nil)
	service, err := New(ctx, Config{
		DSN: dsn, Auth: AuthConfig{TrustCallerAuthentication: true}, Postgres: PostgresConfig{Schema: "kata", SchemaMode: PostgresSchemaBootstrap},
		Embeddings: EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test", Dims: 2, TrustPrivateNetwork: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	issue := seedExpiredWorkerClaim(ctx, t, service)
	done := make(chan error, 1)
	go func() { done <- service.RunSemanticIndex(ctx) }()
	//nolint:kennlint // sleeptest: Postgres and the worker require real I/O.
	require.Eventually(t, func() bool { return service.embeddingReconciler.Health().LastSuccessAt != nil }, 5*time.Second, 10*time.Millisecond)
	require.ErrorContains(t, service.RunSemanticIndex(ctx), "already running")
	require.ErrorContains(t, service.Run(ctx), "already running")
	inspection, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	var released bool
	require.NoError(t, inspection.QueryRowContext(ctx, `SELECT released_at IS NOT NULL FROM kata.issue_claims WHERE issue_uid = $1`, issue.UID).Scan(&released))
	require.False(t, released)
	require.NoError(t, service.Close())
	require.NoError(t, <-done)
}

func TestServiceSemanticIndexDisabled(t *testing.T) {
	service, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- service.RunSemanticIndex(ctx) }()
	//nolint:kennlint // sleeptest: wait for the lifecycle to start.
	require.Eventually(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.running
	}, time.Second, time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("index lifecycle returned before cancellation: %v", err)
	default:
	}
	cancel()
	require.NoError(t, <-done)
}
