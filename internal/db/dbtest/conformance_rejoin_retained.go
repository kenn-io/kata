package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkRejoinRetainedHistory(t *testing.T, store db.Storage) error {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint("guarded=", guarded), func(t *testing.T) {
			checkRejoinRetainedHistoryReset(t, store, guarded)
		})
	}
	return nil
}

func checkRejoinRetainedHistoryReset(t *testing.T, store db.Storage, guarded bool) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, fmt.Sprintf("spoke-project-%t", guarded))
	require.NoError(t, err)
	issue, old, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.RetainedThroughEventID, old.ID)
	snapshot, sourceID, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
	require.NoError(t, err)
	require.Equal(t, old.ID, sourceID)
	again, _, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
	require.NoError(t, err)
	require.Equal(t, snapshot.UID, again.UID)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, snapshot.ID))
	if guarded {
		err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, snapshot.ID+1, snapshot.ID, store.InstanceUID(), snapshot.ID)
	} else {
		err = store.ResetFederatedProject(ctx, project.ID, snapshot.ID+1, snapshot.ID)
	}
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{old.UID})
	require.NoError(t, err)
	require.Equal(t, old.ContentHash, events[0].ContentHash)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "old task", got.Title)
}
