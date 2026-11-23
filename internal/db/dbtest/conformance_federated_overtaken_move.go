package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederationHeldCursorReplay(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	zero := int64(0)
	project := &db.ProjectExport{ID: 42, UID: "01H00000000000000000000000", Name: "held-source", CreatedAt: "2026-01-01T00:00:00Z", Metadata: jsontext.Value(`{}`), Revision: 1, HeldCursor: &zero}
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{project}, db.ImportOptions{}))
	var found *db.ProjectExport
	for p, err := range store.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		if p.UID == project.UID {
			found = &p
		}
	}
	require.NotNil(t, found)
	require.NotNil(t, found.HeldCursor)
	require.Zero(t, *found.HeldCursor)
	five := int64(5)
	project.HeldCursor = &five
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{project}, db.ImportOptions{}))
	_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "worker", Enabled: true})
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	for p, err := range store.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		if p.UID == project.UID {
			require.NotNil(t, p.HeldCursor)
			require.Equal(t, five, *p.HeldCursor)
		}
	}
	negative := int64(-1)
	project.HeldCursor = &negative
	require.Error(t, store.ImportReplay(ctx, []db.ImportRecord{project}, db.ImportOptions{}))
	project.HeldCursor = nil
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{project}, db.ImportOptions{}))
	for p, err := range store.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		if p.UID == project.UID {
			require.Nil(t, p.HeldCursor)
		}
	}
	return nil
}

func checkFederatedOvertakenMoveSourceLeave(t *testing.T, _ db.Storage, backend Backend) error {
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
	for _, sourceFirst := range []bool{true, false} {
		name := "destination first"
		if sourceFirst {
			name = "source first"
		}
		t.Run("held edit survives both resets/"+name, func(t *testing.T) {
			ctx := t.Context()
			hub, spoke, remote, local := overtakenMoveSpoke(t, 2, "reset-source", "reset-destination", "reset-absent")
			issue, creation, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: remote[0].ID, Title: "held edit", Author: "worker"})
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(creation)})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localIssue.ID, Author: "worker", Body: "held comment"})
			require.NoError(t, err)
			away, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[0].ID, ToProjectID: remote[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(*away.Event)})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			back, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[2].ID, ToProjectID: remote[1].ID, IfMatchRev: away.Issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			resetSource := func() {
				require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, local[0].ID, 0, 0, spoke.InstanceUID(), 0))
			}
			resetDestination := func() {
				require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, local[1].ID, 0, 0, spoke.InstanceUID(), 0))
			}
			if sourceFirst {
				resetSource()
				resetDestination()
			} else {
				resetDestination()
				resetSource()
			}
			_, err = spoke.InsertRemoteEvents(ctx, local[1].ID, []db.RemoteEvent{portableNativeEvent(creation), portableNativeEvent(*away.Event), portableNativeEvent(*back.Event)})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[1].ID))
			replayed, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, local[1].ID, replayed.ProjectID)
			pending, err := spoke.PendingFederationPushEvents(ctx, local[0].ID, spoke.InstanceUID(), 0, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.Equal(t, comment.UID, pending[0].UID)
			require.Equal(t, comment.ContentHash, pending[0].ContentHash)
		})
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
		lifecycle := spoke.(db.RelayLifecycleStore)
		require.ErrorIs(t, lifecycle.ValidateRelayLifecycle(ctx, local[0].ID), db.ErrFederationResetBlockedByPendingPush)
		require.NoError(t, lifecycle.ValidateFederationLeave(ctx, local[0].ID))
		require.NoError(t, spoke.(db.RelayDisconnectFenceStore).FenceFederationLeave(ctx, local[0].ID))
		_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
		require.NoError(t, err)
		pending, err := spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
		require.NoError(t, err)
		require.Empty(t, pending)
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
		pending, err = spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, comment.UID, pending[0].UID)
		binding.PushEnabled = false
		_, err = spoke.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		pending, err = spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, comment.UID, pending[0].UID)
		maxID, err := spoke.MaxLocalOriginEventID(ctx, local[0].ID)
		require.NoError(t, err)
		_, err = spoke.EnableFederationPush(ctx, local[0].ID, maxID)
		require.NoError(t, err)
		pending, err = spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
		require.NoError(t, err)
		require.Len(t, pending, 2)
	})
	for _, abort := range []bool{false, true} {
		name := "prepared leave survives a later pull"
		if abort {
			name = "aborted leave restores validation"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			hub, spoke, remote, local := overtakenMoveSpoke(t, 2, "fence-source", "fence-destination", "fence-absent")
			issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: remote[0].ID, Title: "prepared leave", Author: "worker"})
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{portableNativeEvent(created)})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			localIssue, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localIssue.ID, Author: "worker", Body: "offline"})
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
			require.NoError(t, spoke.(db.RelayDisconnectFenceStore).FenceFederationLeave(ctx, local[0].ID))
			back, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: remote[2].ID, ToProjectID: remote[1].ID, IfMatchRev: away.Issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			backEvent := portableNativeEvent(*back.Event)
			backEvent.HLCPhysicalMS, backEvent.HLCCounter = awayEvent.HLCPhysicalMS, awayEvent.HLCCounter+1
			backEvent.ContentHash, err = db.RemoteEventContentHash(backEvent)
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[1].ID, []db.RemoteEvent{backEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[1].ID))
			require.NoError(t, spoke.(db.RelayLifecycleStore).ValidateFederationLeave(ctx, local[0].ID))
			require.ErrorIs(t, spoke.(db.RelayLifecycleStore).ValidateRelayLifecycle(ctx, local[0].ID), db.ErrFederationResetBlockedByPendingPush)
			if abort {
				_, err = spoke.UpsertFederationBinding(ctx, binding)
				require.NoError(t, err)
				require.ErrorIs(t, spoke.(db.RelayLifecycleStore).ValidateFederationLeave(ctx, local[0].ID), db.ErrFederationResetBlockedByPendingPush)
				return
			}
			_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
			require.NoError(t, err)
			held, err := spoke.EventsByUIDs(ctx, local[0].ID, []string{comment.UID})
			require.NoError(t, err)
			require.Len(t, held, 1)
			require.Equal(t, comment.ContentHash, held[0].ContentHash)
		})
	}
	for _, scenario := range []string{"older", "newer", "newer-held", "hub-only"} {
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
			case "newer", "newer-held":
				remoteEvent.HLCCounter++
			case "older":
				remoteEvent.HLCPhysicalMS--
			}
			remoteEvent.ContentHash, err = db.RemoteEventContentHash(remoteEvent)
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, local[0].ID, []db.RemoteEvent{remoteEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, local[0].ID))
			superseded := scenario == "newer" || scenario == "newer-held"
			if scenario != "hub-only" {
				pending, err := spoke.PendingFederationPushEvents(ctx, local[1].ID, spoke.InstanceUID(), 0, 100)
				require.NoError(t, err)
				count, _, err := spoke.PendingFederationPushStats(ctx, local[1].ID, spoke.InstanceUID(), 0)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.Equal(t, localMove.Event.UID, pending[0].UID)
				require.EqualValues(t, 1, count)
				_, err = spoke.ReplaceFederationBoundActorWhenSettled(ctx, local[1].ID, spoke.InstanceUID(), "worker", "other-worker")
				if superseded {
					require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
				} else {
					require.ErrorIs(t, err, db.ErrFederationPushPending)
				}
			}
			if scenario == "newer" {
				require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, local[0].ID, comment.ID))
				_, _, err = spoke.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: local[0].ID, Actor: "worker", Force: true})
				require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
				_, err = spoke.DisableFederationPushWhenSettled(ctx, local[0].ID, spoke.InstanceUID())
				require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
				_, err = spoke.ReplaceFederationBoundActorWhenSettled(ctx, local[0].ID, spoke.InstanceUID(), "worker", "other-worker")
				require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
				require.ErrorIs(t, spoke.(db.RelayDisconnectFenceStore).FenceRelayDisconnect(ctx, local[0].ID), db.ErrFederationResetBlockedByPendingPush)
				return
			}
			sourceBinding, err := spoke.FederationBindingByProject(ctx, local[0].ID)
			require.NoError(t, err)
			_, err = spoke.LeaveFederationReplica(ctx, local[0].ID)
			if !superseded {
				require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
				return
			}
			require.NoError(t, err)
			if scenario == "newer-held" {
				sourceBinding.PushEnabled = false
				_, err = spoke.UpsertFederationBinding(ctx, sourceBinding)
				require.NoError(t, err)
			}
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
			if scenario == "newer-held" {
				require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, local[0].ID, 0, 0, spoke.InstanceUID(), sourceBinding.PushCursorEventID))
				requireHeld()
			}
			_, err = spoke.InsertRemoteEvents(ctx, parked.ID, []db.RemoteEvent{portableNativeEvent(creation), remoteEvent})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, parked.ID))
			replayed, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, parked.ID, replayed.ProjectID)
			require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, parked.ID, 0, 0, spoke.InstanceUID(), 0))
			requireHeld()
			if scenario == "newer-held" {
				enabled, err := spoke.EnableFederationPush(ctx, local[0].ID, comment.ID)
				require.NoError(t, err)
				require.Equal(t, sourceBinding.PushCursorEventID, enabled.PushCursorEventID)
				pending, err := spoke.PendingFederationPushEvents(ctx, local[0].ID, spoke.InstanceUID(), enabled.PushCursorEventID, 100)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.Equal(t, comment.UID, pending[0].UID)
				require.Equal(t, comment.ContentHash, pending[0].ContentHash)
				result, err := hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: remote[0].ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: pending[0].ID, Event: portableNativeEvent(pending[0])}}})
				require.NoError(t, err)
				require.Equal(t, 1, result.Accepted)
			}
		})
	}
	t.Run("later local move keeps its predecessor owed", func(t *testing.T) {
		ctx := t.Context()
		hub, spoke, remote, local := overtakenMoveSpoke(t, 3, "source", "middle", "final", "hub-target")
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
	})
	return nil
}
