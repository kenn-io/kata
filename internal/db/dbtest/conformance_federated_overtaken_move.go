package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederatedOvertakenMoveSourceLeave(t *testing.T, store db.Storage, backend Backend) error {
	// overtakenMoveSpoke joins the spoke to the first joined hub projects; a hub
	// move into a later one parks the issue on the spoke.
	overtakenMoveSpoke := func(t *testing.T, joined int, names ...string) (hub, spoke db.Storage, remote, local []db.Project) {
		ctx := t.Context()
		hub = backend.Open(t)
		t.Cleanup(func() { require.NoError(t, hub.Close()) })
		spoke = backend.Open(t)
		t.Cleanup(func() { require.NoError(t, spoke.Close()) })
		for i, name := range names {
			project, err := hub.CreateProject(ctx, name)
			require.NoError(t, err)
			_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
			require.NoError(t, err)
			remote = append(remote, project)
			if i >= joined {
				continue
			}
			replica, err := spoke.CreateProjectWithUID(ctx, name, project.UID)
			require.NoError(t, err)
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			local = append(local, replica)
		}
		return hub, spoke, remote, local
	}
	enroll := func(t *testing.T, hub, spoke db.Storage, project db.Project) int64 {
		grant, err := hub.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
		require.NoError(t, err)
		return grant.Enrollment.ID
	}
	for _, scenario := range []string{"older", "newer", "tie-wins", "tie-loses", "hub-only"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			hub, spoke, remote, local := overtakenMoveSpoke(t, 2, "source", "joined", "absent")
			issue, creation, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: remote[0].ID, Title: "overtaken", Author: "worker"})
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(creation)})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localIssue.ID, Author: "worker", Body: "held"})
			require.NoError(t, err)
			var localMove db.MoveIssueProjectOut
			if scenario != "hub-only" {
				localMove, err = spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: local[0].ID, ToProjectID: local[1].ID, IfMatchRev: localIssue.Revision, Actor: "worker"})
				require.NoError(t, err)
			}
			move, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[0].ID, ToProjectID: remote[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			remoteEvent := portableNativeEvent(*move.Event)
			if localMove.Event != nil {
				// Give the remote event a deterministic clock relative to the local move.
				remoteEvent.HLCPhysicalMS = localMove.Event.HLCPhysicalMS
				remoteEvent.HLCCounter = localMove.Event.HLCCounter
			}
			switch scenario {
			case "newer":
				remoteEvent.HLCCounter++
			case "older":
				remoteEvent.HLCPhysicalMS--
			case "tie-wins":
				remoteEvent.OriginInstanceUID = "7ZZZZZZZZZZZZZZZZZZZZZZZZZ"
			case "tie-loses":
				remoteEvent.OriginInstanceUID = "00000000000000000000000001"
			}
			remoteEvent.ContentHash, err = db.RemoteEventContentHash(remoteEvent)
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{remoteEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			superseded := scenario == "newer" || scenario == "tie-wins"
			if scenario != "hub-only" {
				pending, err := spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
				require.NoError(t, err)
				count, _, err := spoke.PendingFederationPushStats(ctx, local[1].ID, spoke.InstanceUID(), 0)
				require.NoError(t, err)
				if superseded {
					require.Empty(t, pending)
					require.Zero(t, count)
				} else {
					require.Len(t, pending, 1)
					require.Equal(t, localMove.Event.UID, pending[0].UID)
					require.EqualValues(t, 1, count)
				}
			}
			_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
			if !superseded {
				require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
				return
			}
			require.NoError(t, err)
			requireHeld := func() {
				t.Helper()
				retained, err := spoke.EventsByUIDs(ctx, local[0].ID, []string{comment.UID})
				require.NoError(t, err)
				require.Len(t, retained, 1)
				require.Equal(t, comment.ContentHash, retained[0].ContentHash)
			}
			binding, err := spoke.DisableFederationPushWhenSettled(ctx, local[1].ID, spoke.InstanceUID())
			require.NoError(t, err)
			require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, local[1].ID, 0, 0, spoke.InstanceUID(), binding.PushCursorEventID))
			requireHeld()
			parked, err := spoke.ProjectByUID(ctx, remote[2].UID)
			require.NoError(t, err)
			_, _, _, err = spoke.RestoreProject(ctx, parked.ID, "worker")
			require.NoError(t, err)
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: parked.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: remote[2].ID, HubProjectUID: remote[2].UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			for range 2 {
				require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, parked.ID, 0, 0, spoke.InstanceUID(), 0))
				requireHeld()
			}
			_, err = spoke.InsertRemoteEvents(ctx, parked.ID, []db.RemoteEvent{portableNativeEvent(creation), remoteEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, parked.ID))
			require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, parked.ID, 0, 0, spoke.InstanceUID(), 0))
			requireHeld()
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local[0].ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: remote[0].ID, HubProjectUID: remote[0].UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			pending, err := spoke.PendingFederationPushEvents(ctx, local[0].ID, spoke.InstanceUID(), 0, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.Equal(t, comment.UID, pending[0].UID)
			_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: remote[0].ID, FederationEnrollmentID: enroll(t, hub, spoke, remote[0]), SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, MoveSourceEnrollmentIDs: map[string]int64{remote[2].UID: enroll(t, hub, spoke, remote[2])}, Events: []db.FederationIngestEvent{{SourceEventID: pending[0].ID, Event: portableNativeEvent(pending[0])}}})
			require.NoError(t, err)
			stored, err := hub.EventsByUIDs(ctx, remote[0].ID, []string{comment.UID})
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, comment.ContentHash, stored[0].ContentHash)
		})
	}
	t.Run("later local move keeps its predecessor owed", func(t *testing.T) {
		ctx := t.Context()
		hub, spoke, remote, local := overtakenMoveSpoke(t, 4, "source", "middle", "final", "hub-target")
		issue, creation, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: remote[0].ID, Title: "chained", Author: "worker"})
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(creation)})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
		localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		first, err := spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: local[0].ID, ToProjectID: local[1].ID, IfMatchRev: localIssue.Revision, Actor: "worker"})
		require.NoError(t, err)
		hubMove, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[0].ID, ToProjectID: remote[3].ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		// Receiving the hub move before the offline B to C advances the spoke's
		// clock, so B to C is the newest move on both sides.
		_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(*hubMove.Event)})
		require.NoError(t, err)
		second, err := spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: local[1].ID, ToProjectID: local[2].ID, IfMatchRev: first.Issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
		placed, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, local[2].ID, placed.ProjectID)
		var owed [2]db.Event
		for i, project := range []db.Project{local[1], local[2]} {
			pending, err := spoke.PendingFederationPushEvents(ctx, project.ID, spoke.InstanceUID(), 0, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			owed[i] = pending[0]
		}
		require.Equal(t, first.Event.UID, owed[0].UID)
		require.Equal(t, second.Event.UID, owed[1].UID)
		_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
		for i, event := range owed {
			_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: remote[i+1].ID, FederationEnrollmentID: enroll(t, hub, spoke, remote[i+1]), SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, MoveSourceEnrollmentIDs: map[string]int64{remote[i].UID: enroll(t, hub, spoke, remote[i]), remote[3].UID: enroll(t, hub, spoke, remote[3])}, Events: []db.FederationIngestEvent{{SourceEventID: event.ID, Event: portableNativeEvent(event)}}})
			require.NoError(t, err)
		}
		reached, err := hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, remote[2].ID, reached.ProjectID)
	})
	t.Run("held comment through resets", func(t *testing.T) {
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "held-reset-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "held-reset-target")
		require.NoError(t, err)
		for _, project := range []db.Project{source, target} {
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
		}
		issue, creation, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "held comment", Author: "worker"})
		require.NoError(t, err)
		require.NoError(t, store.AdvanceFederationPushCursor(ctx, source.ID, creation.ID))
		_, comment, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "held"})
		require.NoError(t, err)
		move, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, move.EventID))
		require.NoError(t, store.ResetFederatedProject(ctx, source.ID, 0, 0))
		err = store.ResetFederatedProjectIfNoPendingPush(ctx, target.ID, 0, 0, store.InstanceUID(), move.EventID)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
		pending, err := store.PendingFederationPushEvents(ctx, source.ID, store.InstanceUID(), creation.ID, 100)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, comment.UID, pending[0].UID)
		require.Equal(t, comment.ContentHash, pending[0].ContentHash)
	})
	return nil
}
