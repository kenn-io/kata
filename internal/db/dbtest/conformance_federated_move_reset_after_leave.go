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
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained/guarded=%t", guarded), func(t *testing.T) {
			ctx := t.Context()
			source, err := store.CreateProject(ctx, fmt.Sprintf("retained-source-%t", guarded))
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, fmt.Sprintf("retained-target-%t", guarded))
			require.NoError(t, err)
			issue, creation, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "retained task", Author: "old-actor"})
			require.NoError(t, err)
			for _, project := range []db.Project{source, target} {
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true, RetainHistory: project.ID == source.ID})
				require.NoError(t, err)
			}
			require.NoError(t, store.AdvanceFederationPushCursor(ctx, source.ID, creation.ID))
			moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, moved.EventID))
			_, err = store.LeaveFederationReplica(ctx, source.ID)
			require.NoError(t, err)
			if guarded {
				err = store.ResetFederatedProjectIfNoPendingPush(ctx, target.ID, moved.EventID+1, moved.EventID, store.InstanceUID(), moved.EventID)
			} else {
				err = store.ResetFederatedProject(ctx, target.ID, moved.EventID+1, moved.EventID)
			}
			require.NoError(t, err)
			events, err := store.EventsByUIDs(ctx, source.ID, []string{creation.UID})
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, creation.ContentHash, events[0].ContentHash)
			_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			require.NoError(t, store.ResetFederatedProject(ctx, source.ID, creation.ID+1, creation.ID))
			_, err = store.EventsByUIDs(ctx, source.ID, []string{creation.UID})
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}

	t.Run("source rejoins before reset", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "retained-source-rejoin")
		require.NoError(t, err)
		_, creation, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
		require.NoError(t, err)
		binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true, RetainHistory: true}
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		binding.RetainHistory = false
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.NoError(t, store.ResetFederatedProject(ctx, project.ID, creation.ID+1, creation.ID))
		_, err = store.EventsByUIDs(ctx, project.ID, []string{creation.UID})
		require.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("move after source leave", func(t *testing.T) {
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "retained-late-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "retained-late-target")
		require.NoError(t, err)
		issue, creation, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "old task", Author: "old-actor"})
		require.NoError(t, err)
		for _, project := range []db.Project{source, target} {
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true, RetainHistory: project.ID == source.ID})
			require.NoError(t, err)
		}
		_, err = store.LeaveFederationReplica(ctx, source.ID)
		require.NoError(t, err)
		_, err = store.LeaveFederationReplica(ctx, target.ID)
		require.NoError(t, err)
		moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Actor: "worker", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
		require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, moved.EventID))
		require.NoError(t, store.ResetFederatedProject(ctx, target.ID, moved.EventID+1, moved.EventID))
		events, err := store.EventsByUIDs(ctx, source.ID, []string{creation.UID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, creation.ContentHash, events[0].ContentHash)
	})

	return nil
}
