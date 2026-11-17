package kata

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestServiceEmbeddingHonorsRevokedWorkerFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	revoked := atomic.Bool{}
	requests := make(chan string, 4)
	endpoint := testenv.NewTwoDimensionalEmbeddingServer(t, func(input string) []float32 {
		requests <- input
		return []float32{1, 0}
	})
	denied := errors.New("worker authority revoked")
	service, err := New(ctx, Config{
		DSN:      dsn,
		Postgres: PostgresConfig{Schema: "kata", SchemaMode: PostgresSchemaBootstrap},
		Access:   allowWorkerFenceAccess{},
		WorkerTransactionFence: func(context.Context, Transaction) error {
			if revoked.Load() {
				return denied
			}
			return nil
		},
		Embeddings: EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test", Dims: 2, BatchSize: 1, TrustPrivateNetwork: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project, err := service.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, _, err = service.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first issue", Author: "user-a"})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	//nolint:kennlint // sleeptest: Postgres and the worker require real I/O.
	require.Eventually(t, func() bool { return service.embeddingReconciler.Health().Embedded == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, <-requests, "first issue")

	revoked.Store(true)
	_, _, err = service.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "revoked issue", Author: "user-a"})
	require.NoError(t, err)
	service.embeddingReconciler.Wake()
	require.ErrorIs(t, <-done, denied)
	require.Empty(t, requests, "no issue text reaches the provider after revocation")
}
