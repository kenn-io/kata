package federation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type heldAnswerBeforeStart struct{ db.Storage }

func (s heldAnswerBeforeStart) RecordFederationSyncPullStarted(ctx context.Context, projectID int64, at time.Time) error {
	if err := s.RecordFederationHeldPullSuccess(ctx, projectID, at.Add(time.Second)); err != nil {
		return err
	}
	return s.Storage.RecordFederationSyncPullStarted(ctx, projectID, at)
}

func openPullRaceStore(t *testing.T, backend string) (db.Storage, db.Project) {
	t.Helper()
	ctx := t.Context()
	var store db.Storage
	if backend == "sqlite" {
		d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "spoke.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		store = d
	} else {
		dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
		t.Cleanup(cleanup)
		d, err := pgstore.Open(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		store = d
	}
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", Enabled: true})
	require.NoError(t, err)
	return store, project
}

type leaveAfterStart struct{ db.Storage }

func (s leaveAfterStart) RecordFederationSyncPullStarted(ctx context.Context, projectID int64, at time.Time) error {
	if err := s.Storage.RecordFederationSyncPullStarted(ctx, projectID, at); err != nil {
		return err
	}
	_, err := s.LeaveFederationReplica(ctx, projectID)
	return err
}

func TestPullStartAfterConcurrentLeave(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			store, project := openPullRaceStore(t, backend)
			_, _, err := startFederationPull(t.Context(), leaveAfterStart{store}, project.ID)
			require.NoError(t, err, "a binding left mid-start must not fail the start")
		})
	}
}

func TestPullRecoveryAfterConcurrentHeldAnswer(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			store, project := openPullRaceStore(t, backend)
			ctx := t.Context()
			_, _, err := startFederationPull(ctx, heldAnswerBeforeStart{store}, project.ID)
			require.NoError(t, err)
			resume, _, err := startFederationPull(ctx, store, project.ID)
			require.NoError(t, err)
			require.True(t, resume)
		})
	}
}
