package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// An empty push negotiates features but changes nothing, so it must not wait
// behind another transaction that holds the hub project's write lock, even
// under the enrollment fence the ingest route applies.
func TestEmptyFederationPushDoesNotWaitForProjectLock(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{Schema: "empty_push", SchemaMode: SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	spokeUID, err := uid.New()
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spokeUID, ProjectID: &project.ID, Capabilities: "push", Actor: "worker"})
	require.NoError(t, err)
	// The ingest route runs under the enrollment's transaction fence.
	fenced := db.WithTransactionFence(ctx, store.FederationEnrollmentTransactionFence(enrollment.Enrollment, project.ID, "push"))

	holder, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.ExecContext(ctx, `SELECT id FROM projects WHERE id=$1 FOR UPDATE`, project.ID)
	require.NoError(t, err)

	pushCtx, cancel := context.WithTimeout(fenced, 3*time.Second)
	defer cancel()
	_, err = store.IngestFederationEvents(pushCtx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: project.ID, SpokeInstanceUID: spokeUID, BoundActor: "worker"})
	require.NoError(t, err, "an empty push completes while the project row is locked")
}
