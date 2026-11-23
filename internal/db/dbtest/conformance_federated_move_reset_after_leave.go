package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A successful leave retires its push cursor. Already acknowledged source
// history must not become pending again when the destination later resets.
func checkFederatedMoveResetAfterAcknowledgedSourceLeave(t *testing.T, store db.Storage) error {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint("guarded=", guarded), func(t *testing.T) {
			ctx := t.Context()
			source, err := store.CreateProject(ctx, fmt.Sprintf("acknowledged-source-%t", guarded))
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, fmt.Sprintf("acknowledged-target-%t", guarded))
			require.NoError(t, err)
			for _, project := range []db.Project{source, target} {
				_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
					ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
					HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true,
				})
				require.NoError(t, err)
			}
			issue, creation, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "acknowledged move", Author: "worker"})
			require.NoError(t, err)
			require.Equal(t, store.InstanceUID(), creation.OriginInstanceUID)
			require.NoError(t, store.AdvanceFederationPushCursor(ctx, source.ID, creation.ID))
			moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
				IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker",
			})
			require.NoError(t, err)
			require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, moved.EventID))
			_, err = store.LeaveFederationReplica(ctx, source.ID)
			require.NoError(t, err)
			if guarded {
				err = store.ResetFederatedProjectIfNoPendingPush(ctx, target.ID, moved.EventID+1, moved.EventID, store.InstanceUID(), moved.EventID)
			} else {
				err = store.ResetFederatedProject(ctx, target.ID, moved.EventID+1, moved.EventID)
			}
			require.NoError(t, err, "retired source history was acknowledged before leaving")
			_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
	return nil
}
