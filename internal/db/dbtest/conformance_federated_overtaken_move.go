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
	t.Run("held cursor lifecycle/rejoin", func(t *testing.T) {
		ctx := t.Context()
		hub, spoke, remote, local := overtakenMoveSpoke(t, 2, "cursor-source", "cursor-destination", "cursor-absent")
		issue, creation, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: remote[0].ID, Title: "cursor lifecycle", Author: "worker"})
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(creation)})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
		localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		move, err := spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: local[0].ID, ToProjectID: local[1].ID, IfMatchRev: localIssue.Revision, Actor: "worker"})
		require.NoError(t, err)
		away, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[0].ID, ToProjectID: remote[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		awayEvent := portableNativeEvent(*away.Event)
		awayEvent.HLCPhysicalMS, awayEvent.HLCCounter = move.Event.HLCPhysicalMS, move.Event.HLCCounter+1
		awayEvent.ContentHash, err = db.RemoteEventContentHash(awayEvent)
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{awayEvent})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
		binding, err := spoke.FederationBindingByProject(ctx, local[0].ID)
		require.NoError(t, err)
		_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
		require.NoError(t, err)
		pending, err := spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
		require.NoError(t, err)
		require.Empty(t, pending)
		_, err = spoke.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		pending, err = spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, move.Event.UID, pending[0].UID)
	})
	enroll := func(t *testing.T, hub, spoke db.Storage, project db.Project) int64 {
		grant, err := hub.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
		require.NoError(t, err)
		return grant.Enrollment.ID
	}
	for _, scenario := range []string{"older", "newer", "hub-only"} {
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
			}
			remoteEvent.ContentHash, err = db.RemoteEventContentHash(remoteEvent)
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{remoteEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			superseded := scenario == "newer"
			if scenario != "hub-only" {
				pending, err := spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
				require.NoError(t, err)
				count, _, err := spoke.PendingFederationPushStats(ctx, local[1].ID, spoke.InstanceUID(), 0)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.Equal(t, localMove.Event.UID, pending[0].UID)
				require.EqualValues(t, 1, count)
				_, err = spoke.ReplaceFederationBoundActorWhenSettled(ctx, local[1].ID, spoke.InstanceUID(), "worker", "other-worker")
				require.ErrorIs(t, err, db.ErrFederationPushPending)
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
			binding, err := spoke.FederationBindingByProject(ctx, local[1].ID)
			require.NoError(t, err)
			require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, local[1].ID, 0, 0, spoke.InstanceUID(), binding.PushCursorEventID))
			parked, err := spoke.ProjectByUID(ctx, remote[2].UID)
			require.NoError(t, err)
			_, _, _, err = spoke.RestoreProject(ctx, parked.ID, "worker")
			require.NoError(t, err)
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: parked.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: remote[2].ID, HubProjectUID: remote[2].UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, parked.ID, 0, 0, spoke.InstanceUID(), 0))
			requireHeld()
			_, err = spoke.InsertRemoteEvents(ctx, parked.ID, []db.RemoteEvent{portableNativeEvent(creation), remoteEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, parked.ID))
			require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, parked.ID, 0, 0, spoke.InstanceUID(), 0))
			requireHeld()
		})
	}
	t.Run("returned issue syncs after its source leaves", func(t *testing.T) {
		ctx := t.Context()
		hub, spoke, remote, local := overtakenMoveSpoke(t, 2, "return-source", "return-joined", "return-absent")
		issue, creation, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: remote[0].ID, Title: "returned", Author: "worker"})
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(creation)})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
		localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localMove, err := spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: local[0].ID, ToProjectID: local[1].ID, IfMatchRev: localIssue.Revision, Actor: "worker"})
		require.NoError(t, err)
		away, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[0].ID, ToProjectID: remote[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		awayEvent := portableNativeEvent(*away.Event)
		awayEvent.HLCPhysicalMS = localMove.Event.HLCPhysicalMS
		awayEvent.HLCCounter = localMove.Event.HLCCounter + 1
		awayEvent.ContentHash, err = db.RemoteEventContentHash(awayEvent)
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{awayEvent})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
		_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
		require.NoError(t, err)
		back, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[2].ID, ToProjectID: remote[1].ID, IfMatchRev: away.Issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		backEvent := portableNativeEvent(*back.Event)
		backEvent.HLCPhysicalMS = awayEvent.HLCPhysicalMS
		backEvent.HLCCounter = awayEvent.HLCCounter + 1
		backEvent.ContentHash, err = db.RemoteEventContentHash(backEvent)
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, local[1].ID, []db.RemoteEvent{backEvent})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[1].ID))
		returned, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, local[1].ID, returned.ProjectID)
		_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: returned.ID, Author: "worker", Body: "after return"})
		require.NoError(t, err)
		pending, err := spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, comment.UID, pending[0].UID)
	})
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
	return nil
}
