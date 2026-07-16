package daemon_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

type leaseRecoveryEmbedder struct{}

func (leaseRecoveryEmbedder) Generation() kitvec.Generation {
	return kitvec.Generation{Model: "lease-recovery", Dimensions: 2}
}

func (leaseRecoveryEmbedder) BatchSize() int { return 64 }

func (leaseRecoveryEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i := range texts {
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}
}

func TestPostgresReconcilerReacquiresLeaseAfterSessionLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "lease-recovery")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "first",
		Body:      "first body",
		Author:    "tester",
	})
	require.NoError(t, err)

	index, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	embedder := leaseRecoveryEmbedder{}
	reconciler := daemon.NewReconciler(store, index, embedder, daemon.ReconcilerConfig{
		BatchSize:  64,
		SweepEvery: time.Hour,
		MinBackoff: 50 * time.Millisecond,
		MaxBackoff: 2 * time.Second,
	})
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-done:
			assert.ErrorIs(t, runErr, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Error("reconciler did not stop after cancellation")
		}
	})

	var currentPID int
	require.Eventually(t, func() bool {
		return store.QueryRowContext(ctx, `
			SELECT pid FROM pg_catalog.pg_locks
			WHERE locktype = 'advisory' AND granted
			  AND classid = (hashtext(current_database())::bigint & 4294967295)
			  AND objid = (hashtext('kata:vector:reconciler:' || current_schema())::bigint & 4294967295)
			LIMIT 1`).Scan(&currentPID) == nil
	}, 5*time.Second, 10*time.Millisecond, "reconciler did not acquire its initial lease")
	require.Eventually(t, func() bool {
		var mirrored int
		return store.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_mirror`).Scan(&mirrored) == nil && mirrored == 1
	}, 5*time.Second, 10*time.Millisecond, "initial vector reconciliation did not complete")

	for cycle := 1; cycle <= 5; cycle++ {
		previousPID := currentPID
		var terminated bool
		require.NoError(t, store.QueryRowContext(ctx,
			`SELECT pg_terminate_backend($1)`, previousPID).Scan(&terminated))
		require.True(t, terminated)
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID,
			Title:     fmt.Sprintf("cycle %d", cycle),
			Body:      "recovery body",
			Author:    "tester",
		})
		require.NoError(t, err)
		reconciler.Wake()

		started := time.Now()
		require.Eventually(t, func() bool {
			err := store.QueryRowContext(ctx, `
				SELECT pid FROM pg_catalog.pg_locks
				WHERE locktype = 'advisory' AND granted
				  AND classid = (hashtext(current_database())::bigint & 4294967295)
				  AND objid = (hashtext('kata:vector:reconciler:' || current_schema())::bigint & 4294967295)
				LIMIT 1`).Scan(&currentPID)
			return err == nil && currentPID != previousPID
		}, 5*time.Second, 10*time.Millisecond, "cycle %d did not reacquire leadership", cycle)
		require.Eventually(t, func() bool {
			var mirrored int
			return store.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_mirror`).Scan(&mirrored) == nil && mirrored == cycle+1
		}, 5*time.Second, 10*time.Millisecond, "cycle %d did not reconcile after reacquiring leadership", cycle)
		require.Less(t, time.Since(started), 350*time.Millisecond,
			"healthy leadership periods must reset retry backoff before the next lease loss")
	}
}
