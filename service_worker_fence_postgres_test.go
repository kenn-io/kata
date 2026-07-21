package kata

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestServicePostgresWorkerTransactionFenceRollsBackAndStopsWorkers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	rejected := errors.New("worker transaction rejected")
	service, err := New(ctx, Config{
		DSN:      dsn,
		Postgres: PostgresConfig{Schema: "kata", SchemaMode: PostgresSchemaBootstrap},
		Access:   allowWorkerFenceAccess{},
		WorkerTransactionFence: func(ctx context.Context, tx Transaction) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO kata.worker_fence_markers(attempt) VALUES($1)`, 1); err != nil {
				return err
			}
			return rejected
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

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

	inspection, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	_, err = inspection.ExecContext(ctx,
		`CREATE TABLE kata.worker_fence_markers (attempt BIGINT NOT NULL)`)
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(ctx) }()
	select {
	case runErr := <-runDone:
		require.ErrorIs(t, runErr, rejected)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "worker fence rejection did not stop the service workers")
	}

	var markerCount, released int
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT count(*) FROM kata.worker_fence_markers`).Scan(&markerCount))
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT CASE WHEN released_at IS NOT NULL THEN 1 ELSE 0 END
		   FROM kata.issue_claims WHERE issue_uid = $1`, issue.UID).Scan(&released))
	assert.Zero(t, markerCount)
	assert.Zero(t, released)
}
