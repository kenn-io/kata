package dbtest

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkRejoinRetainedHistory(t *testing.T, store db.Storage, backend Backend) error {
	t.Run("snapshot includes incoming local links", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "incoming-link")
		require.NoError(t, err)
		blocker, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "blocker", Author: "old-actor"})
		require.NoError(t, err)
		blocked, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "blocked", Author: "old-actor"})
		require.NoError(t, err)
		_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: blocker.ID, ToIssueID: blocked.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: blocker.ID, Actor: "old-actor", FromUID: blocker.UID, ToUID: blocked.UID, FromShortID: blocker.ShortID, ToShortID: blocked.ShortID})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		snapshot, _, _, err := store.RetainedIssueSnapshot(ctx, project.ID, blocked.UID)
		require.NoError(t, err)
		var document db.IssueDocumentPayload
		require.NoError(t, json.Unmarshal([]byte(snapshot.Payload), &document))
		require.Len(t, document.Links, 1)
		require.Equal(t, blocker.UID, document.Links[0].ToIssueUID)
		require.True(t, document.Links[0].Incoming)
	})
	for _, restored := range []bool{false, true} {
		for _, guarded := range []bool{false, true} {
			t.Run(fmt.Sprintf("reset/restored=%t/guarded=%t", restored, guarded), func(t *testing.T) {
				checkRejoinRetainedHistoryReset(t, store, backend, guarded, restored)
			})
		}
	}
	t.Run("retained snapshot source and reuse", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "pending-retained-edit")
		require.NoError(t, err)
		issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "task", Author: "old-actor"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, created.ID))
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "pending edit"})
		require.NoError(t, err)
		snapshot, sourceID, createdSnapshot, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
		require.NoError(t, err)
		require.True(t, createdSnapshot)
		require.Equal(t, created.ID, sourceID)
		reused, _, createdSnapshot, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
		require.NoError(t, err)
		require.False(t, createdSnapshot)
		require.Equal(t, snapshot.UID, reused.UID)
	})

	t.Run("move after later recovery snapshot accepted", func(t *testing.T) {
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "later-recovery-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "later-recovery-target")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "task", Author: "actor-a"})
		require.NoError(t, err)
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "actor-a", Body: "discussion"})
		require.NoError(t, err)
		binding := db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: source.UID, Actor: "actor-b", Enabled: true, PushEnabled: true, RetainHistory: true}
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		earlier, _, _, err := store.RetainedIssueSnapshot(ctx, source.ID, issue.UID)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 2, HubProjectUID: target.UID, Actor: "actor-b", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
		issue, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "actor-b", DryRun: true})
		require.ErrorIs(t, err, db.ErrRetainedSnapshotUnaccepted)
		unchanged, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, issue, unchanged)
		_, err = store.LeaveFederationReplica(ctx, source.ID)
		require.NoError(t, err)
		binding.Actor = "actor-c"
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.NoError(t, store.AdvanceFederationPushCursor(ctx, source.ID, earlier.ID))
		accepted, _, _, err := store.RetainedIssueSnapshot(ctx, source.ID, issue.UID)
		require.NoError(t, err)
		require.Greater(t, accepted.ID, earlier.ID)
		require.NoError(t, store.AcknowledgeFederationPush(ctx, source.ID, accepted.ID, nil))
		_, err = store.LeaveFederationReplica(ctx, source.ID)
		require.NoError(t, err)
		binding.Actor = "actor-d"
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 2, HubProjectUID: target.UID, Actor: "actor-d", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
		issue, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "actor-d"})
		require.NoError(t, err)
		var envelope struct {
			Snapshot db.IssueDocumentPayload `json:"snapshot"`
		}
		require.NoError(t, json.Unmarshal([]byte(moved.Event.Payload), &envelope))
		require.Equal(t, "actor-c", envelope.Snapshot.Author)
		require.Len(t, envelope.Snapshot.Comments, 1)
		require.Equal(t, "actor-c", envelope.Snapshot.Comments[0].Author)
		require.Equal(t, "actor-a", moved.Issue.Author)
		comments, err := store.CommentsByIssue(ctx, moved.Issue.ID)
		require.NoError(t, err)
		require.Equal(t, "actor-a", comments[0].Author)
	})

	t.Run("recover task moved before enrollment", func(t *testing.T) {
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "offline-move-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "offline-move-target")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "offline task", Author: "old-actor"})
		require.NoError(t, err)
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "offline discussion"})
		require.NoError(t, err)
		move, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "old-actor"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: target.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		require.NoError(t, store.ResetFederatedProject(ctx, target.ID, move.Event.ID+1, move.Event.ID))
		restored, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		comments, err := store.CommentsByIssue(ctx, restored.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, "offline discussion", comments[0].Body)
	})
	t.Run("purge removes retained event markers", func(t *testing.T) {
		ctx := t.Context()
		var projects []db.Project
		var events []db.Event
		for _, name := range []string{"purged-retained", "remaining-retained"} {
			project, err := store.CreateProject(ctx, name)
			require.NoError(t, err)
			_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
			require.NoError(t, err)
			_, err = store.LeaveFederationReplica(ctx, project.ID)
			require.NoError(t, err)
			projects, events = append(projects, project), append(events, event)
		}
		markers := func() map[string]bool {
			keys := make(map[string]bool)
			for row, err := range store.ExportMeta(ctx) {
				require.NoError(t, err)
				keys[row.Key] = true
			}
			return keys
		}
		require.True(t, markers()[db.FederationRetainedEventPrefix+events[0].UID])
		require.True(t, markers()[db.FederationRetainedEventPrefix+events[1].UID])
		_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: projects[0].ID, Actor: "worker", Force: true})
		require.NoError(t, err)
		_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: projects[0].ID, Actor: "worker"})
		require.NoError(t, err)
		require.False(t, markers()[db.FederationRetainedEventPrefix+events[0].UID])
		require.True(t, markers()[db.FederationRetainedEventPrefix+events[1].UID])
	})
	t.Run("refresh boundary on upsert", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "refresh-retained-boundary")
		require.NoError(t, err)
		binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "actor-b", Enabled: true, PushEnabled: true, RetainHistory: true}
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "new retained task", Author: "actor-a"})
		require.NoError(t, err)
		updated, err := store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, event.ID, updated.RetainedThroughEventID)
		_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "actor-b"})
		require.NoError(t, err)
		require.Greater(t, later.ID, event.ID)
		binding.RetainHistory = false
		updated, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, event.ID, updated.RetainedThroughEventID)
	})
	return nil
}

func checkRejoinRetainedHistoryReset(t *testing.T, store db.Storage, backend Backend, guarded, restored bool) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, fmt.Sprintf("spoke-project-%t-%t", guarded, restored))
	require.NoError(t, err)
	issue, old, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	other, otherEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "another task", Author: "old-actor"})
	require.NoError(t, err)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, otherEvent.ID))
	if restored {
		records, err := CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
		require.NoError(t, err)
		store = backend.Open(t)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		binding.Actor = "old-actor"
		binding.RetainHistory = true
		binding, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
	}
	if guarded {
		err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, otherEvent.ID+1, otherEvent.ID, store.InstanceUID(), otherEvent.ID)
	} else {
		err = store.ResetFederatedProject(ctx, project.ID, otherEvent.ID+1, otherEvent.ID)
	}
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{old.UID})
	require.NoError(t, err)
	require.Equal(t, old.ContentHash, events[0].ContentHash)
	require.Equal(t, old.OriginInstanceUID, events[0].OriginInstanceUID)
	events, err = store.EventsByUIDs(ctx, project.ID, []string{otherEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, otherEvent.ContentHash, events[0].ContentHash)
	_, err = store.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "old task", got.Title)
}
