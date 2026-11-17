package kata

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
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
	providerEntered := make(chan struct{})
	providerRelease := make(chan struct{})
	var entered, release sync.Once
	defer release.Do(func() { close(providerRelease) })
	endpoint := testenv.NewEmbeddingServer(t, func(string) []float32 {
		entered.Do(func() { close(providerEntered) })
		<-providerRelease
		return []float32{1, 0}
	})
	service, err := New(ctx, Config{
		DSN: dsn, Auth: AuthConfig{TrustCallerAuthentication: true}, Postgres: PostgresConfig{Schema: "kata", SchemaMode: PostgresSchemaBootstrap},
		Embeddings: EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test", Dims: 2, TrustPrivateNetwork: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	issue := seedExpiredWorkerClaim(ctx, t, service)
	inspection, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	released := func() bool {
		var result bool
		err := inspection.QueryRowContext(ctx, `SELECT released_at IS NOT NULL FROM kata.issue_claims WHERE issue_uid = $1`, issue.UID).Scan(&result)
		if err != nil {
			t.Error(err)
			return true
		}
		return result
	}

	indexCtx, stopIndex := context.WithCancel(ctx)
	defer stopIndex()
	indexDone := make(chan error, 1)
	go func() { indexDone <- service.RunSemanticIndex(indexCtx) }()
	select {
	case <-providerEntered:
	case <-ctx.Done():
		t.Fatal("indexer did not reach the embedding provider")
	}
	//nolint:kennlint // sleeptest: give any wrongly started sweeper time to expire the claim.
	require.Never(t, released, 2*time.Second, 10*time.Millisecond)
	release.Do(func() { close(providerRelease) })
	stopIndex()
	require.NoError(t, <-indexDone)

	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()
	//nolint:kennlint // sleeptest: the worker and Postgres require real I/O.
	require.Eventually(t, released, 5*time.Second, 10*time.Millisecond)
	stopRun()
	require.NoError(t, <-runDone)
}

func TestServiceSemanticIndexDisabled(t *testing.T) {
	service, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- service.RunSemanticIndex(ctx) }()
	//nolint:kennlint // sleeptest: wait for the lifecycle to start.
	require.Eventually(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.running
	}, time.Second, time.Millisecond)
	require.ErrorContains(t, service.RunSemanticIndex(ctx), "already running")
	cancel()
	require.NoError(t, <-done)
}
