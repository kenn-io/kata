package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Accepted pushes above a held cursor stay out of later batches, and the
// accepted prefix stops at the first event the hub has not accepted.
func checkFederationPushAccepted(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "push-accepted")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	_, base, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "base", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, base.ID))
	ids := make([]int64, 3)
	for i := range ids {
		_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "accepted", Author: "worker"})
		require.NoError(t, err)
		ids[i] = event.ID
	}
	origin := store.InstanceUID()

	require.NoError(t, store.RecordFederationPushAccepted(ctx, project.ID, []int64{ids[0], ids[2]}))
	through, err := store.FederationPushAcceptedThrough(ctx, project.ID, origin, base.ID)
	require.NoError(t, err)
	require.Equal(t, ids[0], through, "the rejected middle event holds the accepted prefix")
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, origin, base.ID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, ids[1], pending[0].ID, "accepted events are not republished")

	require.NoError(t, store.RecordFederationPushAccepted(ctx, project.ID, []int64{ids[1]}))
	through, err = store.FederationPushAcceptedThrough(ctx, project.ID, origin, base.ID)
	require.NoError(t, err)
	require.Equal(t, ids[2], through)

	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, ids[2]))
	require.NoError(t, store.RecordFederationPushAccepted(ctx, project.ID, []int64{ids[1]}))
	through, err = store.FederationPushAcceptedThrough(ctx, project.ID, origin, base.ID)
	require.NoError(t, err)
	require.Zero(t, through, "acknowledgments at or below the cursor are not retained")

	unbound, err := store.CreateProject(ctx, "push-accepted-unbound")
	require.NoError(t, err)
	require.ErrorIs(t, store.RecordFederationPushAccepted(ctx, unbound.ID, []int64{ids[0]}), db.ErrNotFound)
	return nil
}
