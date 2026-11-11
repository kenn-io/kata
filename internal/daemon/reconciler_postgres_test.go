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

type handoffEmbedder struct {
	model   string
	started chan struct{}
	resume  chan struct{}
}

func (e handoffEmbedder) Generation() kitvec.Generation {
	return kitvec.Generation{Model: e.model, Dimensions: 2}
}

func (e handoffEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		if e.started != nil {
			select {
			case e.started <- struct{}{}:
			default:
			}
			select {
			case <-e.resume:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return leaseRecoveryEmbedder{}.EncodeFunc()(ctx, texts)
	}
}

func TestPostgresAutomaticReconcilerYieldsToConfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "handoff")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "handoff", Author: "tester"})
	require.NoError(t, err)
	started, resume := make(chan struct{}, 1), make(chan struct{})
	done := make(chan error, 3)
	startedCount := 0
	t.Cleanup(func() {
		cancel()
		for range startedCount {
			<-done
		}
	})
	start := func(runCtx context.Context, e handoffEmbedder, automatic bool) *daemon.Reconciler {
		idx, err := vector.OpenPostgres(ctx, store.DB)
		require.NoError(t, err)
		r := daemon.NewReconciler(store, idx, e, daemon.ReconcilerConfig{PreserveActiveGeneration: automatic, SweepEvery: time.Hour, MinBackoff: 50 * time.Millisecond})
		go func() { done <- r.Run(runCtx) }()
		startedCount++
		return r
	}
	first := start(ctx, handoffEmbedder{model: "automatic", started: started, resume: resume}, true)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic cycle did not start")
	}
	start(ctx, handoffEmbedder{model: "automatic"}, true)
	// Wait for the earlier automatic contender to enter the PostgreSQL queue.
	waiters := func(n int) bool {
		var count int
		err := store.QueryRowContext(ctx, `SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&count)
		return err == nil && count >= n
	}
	//nolint:kennlint // sleeptest: PostgreSQL exposes queued leases asynchronously.
	require.Eventually(t, func() bool { return waiters(1) }, 5*time.Second, 10*time.Millisecond)
	configuredCtx, stopConfigured := context.WithCancel(ctx)
	defer stopConfigured()
	configured := start(configuredCtx, handoffEmbedder{model: "configured"}, false)
	//nolint:kennlint // sleeptest: PostgreSQL exposes queued leases asynchronously.
	require.Eventually(t, func() bool { return waiters(2) }, 5*time.Second, 10*time.Millisecond)
	close(resume)
	want := (kitvec.Generation{Model: "configured", Dimensions: 2}).Fingerprint()
	idx, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	//nolint:kennlint // sleeptest: Reconciliation and its database writes are asynchronous.
	require.Eventually(t, func() bool {
		active, ok, err := idx.ActiveGeneration(ctx)
		return err == nil && ok && active == want && configured.Health().Embedded == 1
	}, 5*time.Second, 10*time.Millisecond)
	stopConfigured()
	first.Wake()
	//nolint:kennlint // sleeptest: The automatic reconciler must reacquire the lease.
	require.Eventually(t, func() bool {
		return first.Health().LastError == "shared embedding choice conflicts with the active generation"
	}, 5*time.Second, 10*time.Millisecond)
	active, ok, err := idx.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, active)
}

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
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
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
			WHERE `+vector.ReconcilerLockPredicateSQL+`
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

		require.Eventually(t, func() bool {
			err := store.QueryRowContext(ctx, `
				SELECT pid FROM pg_catalog.pg_locks
				WHERE `+vector.ReconcilerLockPredicateSQL+`
				LIMIT 1`).Scan(&currentPID)
			return err == nil && currentPID != previousPID
		}, 5*time.Second, 10*time.Millisecond, "cycle %d did not reacquire leadership", cycle)
		require.Eventually(t, func() bool {
			var mirrored int
			return store.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_mirror`).Scan(&mirrored) == nil && mirrored == cycle+1
		}, 5*time.Second, 10*time.Millisecond, "cycle %d did not reconcile after reacquiring leadership", cycle)
	}
}
