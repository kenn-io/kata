package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

func TestResetFederatedProjectSeqScanMetaCast(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Force sequential scans on meta so PostgreSQL evaluates WHERE predicates
	// against non-numeric metadata rows (e.g. schema/version entries) just as
	// it can after statistics update under parallel conformance load.
	store.SetMaxOpenConns(1)
	_, err = store.ExecContext(ctx, `SET enable_indexscan=off; SET enable_bitmapscan=off`)
	require.NoError(t, err)

	source, err := store.CreateProject(ctx, "seqscan-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "seqscan-target")
	require.NoError(t, err)
	for _, p := range []db.Project{source, target} {
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID:     p.ID,
			Role:          db.FederationRoleSpoke,
			HubURL:        "https://hub.example",
			HubProjectID:  p.ID,
			HubProjectUID: p.UID,
			Actor:         "worker",
			Enabled:       true,
			PushEnabled:   true,
		})
		require.NoError(t, err)
	}
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Moved issue", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, source.ID, created.ID))
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID:       issue.ID,
		FromProjectID: source.ID,
		ToProjectID:   target.ID,
		IfMatchRev:    issue.Revision,
		Actor:         "worker",
	})
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, moved.Event.ID))
	_, err = store.LeaveFederationReplica(ctx, source.ID)
	require.NoError(t, err)

	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, store.ResetFederatedProject(ctx, target.ID, cursor+1, cursor))

	_, err = store.ReadFederation(ctx, db.FederationReadParams{
		ProjectID: target.ID,
		Features:  db.SupportedEventFeatures,
		AfterID:   0,
		Limit:     10,
	})
	require.NoError(t, err)
}
