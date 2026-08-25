package kata

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type allowWorkerFenceAccess struct{}

func (allowWorkerFenceAccess) Authorize(context.Context, AccessRequest) (AccessDecision, error) {
	return AccessDecision{TransactionFence: func(context.Context, Transaction) error { return nil }}, nil
}

func TestServiceWorkerTransactionFenceRollsBackAndStopsWorkers(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "service.db")
	rejected := errors.New("worker transaction rejected")
	service, err := New(ctx, Config{
		DSN:    databasePath,
		Access: allowWorkerFenceAccess{},
		WorkerTransactionFence: func(ctx context.Context, tx Transaction) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO worker_fence_markers(attempt) VALUES(?)`, 1); err != nil {
				return err
			}
			return rejected
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	issue := seedExpiredWorkerClaim(ctx, t, service)

	inspection, err := sql.Open("sqlite", databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	_, err = inspection.ExecContext(ctx,
		`CREATE TABLE worker_fence_markers (attempt INTEGER NOT NULL)`)
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	select {
	case runErr := <-runDone:
		require.ErrorIs(t, runErr, rejected)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "worker fence rejection did not stop the service workers")
	}

	var markerCount, released int
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT count(*) FROM worker_fence_markers`).Scan(&markerCount))
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT released_at IS NOT NULL FROM issue_claims WHERE issue_uid = ?`, issue.UID).Scan(&released))
	assert.Zero(t, markerCount)
	assert.Zero(t, released)
}

func TestServiceRunTreatsCanceledInFlightWorkerFenceAsCleanShutdown(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		close bool
	}{
		{name: "caller cancellation"},
		{name: "service close", close: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			fenceStarted := make(chan struct{})
			var startOnce sync.Once
			service, err := New(ctx, Config{
				DSN:    filepath.Join(t.TempDir(), "service.db"),
				Access: allowWorkerFenceAccess{},
				WorkerTransactionFence: func(ctx context.Context, _ Transaction) error {
					startOnce.Do(func() { close(fenceStarted) })
					<-ctx.Done()
					return ctx.Err()
				},
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, service.Close()) })

			seedExpiredWorkerClaim(ctx, t, service)

			runCtx, cancelRun := context.WithCancel(ctx)
			defer cancelRun()
			runDone := make(chan error, 1)
			go func() { runDone <- service.Run(runCtx) }()
			select {
			case <-fenceStarted:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "worker did not enter transaction fence")
			}

			closeDone := make(chan error, 1)
			if testCase.close {
				go func() { closeDone <- service.Close() }()
			} else {
				cancelRun()
			}
			select {
			case runErr := <-runDone:
				require.NoError(t, runErr)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "Run did not stop after cancellation")
			}
			if testCase.close {
				require.NoError(t, <-closeDone)
			}
		})
	}
}

func TestServiceRunTreatsDeadlineInFlightWorkerFenceAsCleanShutdown(t *testing.T) {
	ctx := t.Context()
	fenceStarted := make(chan struct{})
	var startOnce sync.Once
	service, err := New(ctx, Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: allowWorkerFenceAccess{},
		WorkerTransactionFence: func(ctx context.Context, _ Transaction) error {
			startOnce.Do(func() { close(fenceStarted) })
			<-ctx.Done()
			return ctx.Err()
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	seedExpiredWorkerClaim(ctx, t, service)

	runCtx, cancelRun := context.WithTimeout(ctx, time.Second)
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()

	select {
	case <-fenceStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "worker did not enter transaction fence")
	}
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Run did not stop after deadline")
	}
}

func TestServiceRunReportsWorkerInitiatedContextCancellation(t *testing.T) {
	ctx := context.Background()
	service, err := New(ctx, Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: allowWorkerFenceAccess{},
		WorkerTransactionFence: func(context.Context, Transaction) error {
			return context.Canceled
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	seedExpiredWorkerClaim(ctx, t, service)

	runErr := service.Run(ctx)

	require.ErrorIs(t, runErr, context.Canceled)
	assert.ErrorContains(t, runErr, "worker transaction fence")
}

func seedExpiredWorkerClaim(ctx context.Context, t *testing.T, service *Service) db.Issue {
	t.Helper()
	project, err := service.store.CreateProject(ctx, "worker-project")
	require.NoError(t, err)
	_, err = service.store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	issue, _, err := service.store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "expired claim", Author: "worker",
	})
	require.NoError(t, err)
	_, err = service.store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: service.store.InstanceUID(),
			Holder:            "worker",
			ClientKind:        "test",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       time.Now().UTC().Add(-2 * time.Minute),
	})
	require.NoError(t, err)
	return issue
}

func TestServiceRunRequiresWorkerFenceWithHostAccess(t *testing.T) {
	service, err := New(context.Background(), Config{
		DSN:    filepath.Join(t.TempDir(), "service.db"),
		Access: allowWorkerFenceAccess{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	err = service.Run(context.Background())
	assert.EqualError(t, err, "kata: worker transaction fence is required with host access")
}
