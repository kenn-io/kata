package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A late destination baseline must relocate an issue already held in the source,
// even when the spoke never received the intervening issue.moved event.
func checkFederatedSnapshotRelocatesHeldIssue(t *testing.T, _ db.Storage, backend Backend) error {
	for _, lifecycle := range []string{
		"active replicas", "payload-only snapshot reference", "pausing the source retains discussion", "source paused before destination joins",
		"source leave retains discussion", "destination leave preserves standalone issue",
		"destination leave waits for pending initial link", "destination leave waits for pending cron observation",
		"destination leave waits for archived source",
	} {
		t.Run(lifecycle, func(t *testing.T) {
			hub := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, hub.Close()) })
			checkFederatedSnapshotRelocationLifecycle(t, hub, backend, lifecycle)
		})
	}
	return nil
}

func checkFederatedSnapshotRelocationLifecycle(t *testing.T, hub db.Storage, backend Backend, lifecycle string) {
	t.Helper()
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "snapshot-source")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "snapshot-target")
	require.NoError(t, err)
	peer, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "linked peer", Author: "worker"})
	require.NoError(t, err)
	issue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "late destination join", Author: "worker", Labels: []string{"retained"}, Links: []db.InitialLink{{Type: "related", ToNumber: peer.ID}}})
	require.NoError(t, err)
	sourceComment, _, err := hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "source discussion"})
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	join := func(project db.Project) db.Project {
		t.Helper()
		local, err := spoke.CreateProjectWithUID(ctx, project.Name, project.UID)
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		return local
	}
	pull := func(project, local db.Project, after int64) {
		t.Helper()
		page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: after, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		var events []db.RemoteEvent
		for _, event := range page.Events {
			require.NotEqual(t, "issue.moved", event.Type, "the reporter's baseline has no retained move history")
			remote := remoteMoveEvent(event)
			if lifecycle == "payload-only snapshot reference" && project.ID == target.ID && remote.Type == "issue.snapshot" && remote.IssueUID != nil && *remote.IssueUID == issue.UID {
				remote.IssueUID = nil
				remote.ContentHash = remoteEventHash(t, remote)
			}
			events = append(events, remote)
		}
		require.NotEmpty(t, events)
		_, err = spoke.InsertRemoteEvents(ctx, local.ID, events)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local.ID), "a destination snapshot must reuse the issue held in the source")
	}
	localSource := join(source)
	pull(source, localSource, 0)
	before, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, localSource.ID, before.ProjectID)
	beforeComments, err := spoke.CommentsByIssue(ctx, before.ID)
	require.NoError(t, err)
	require.Len(t, beforeComments, 1)
	_, err = spoke.ProjectByUID(ctx, target.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "the destination is not joined when the hub moves the issue")
	edited, _, changed, err := hub.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: sourceComment.UID, Actor: "worker", Body: "revised source discussion"})
	require.NoError(t, err)
	require.True(t, changed)
	issue, err = hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.RemoveLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.unlabeled", Label: "retained", Actor: "worker"})
	require.NoError(t, err)
	_, err = hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	// Bootstrap from a fresh destination baseline, beyond the retained move.
	baseline, refreshed, err := hub.RefreshProjectFederationBaseline(ctx, target.ID, "worker")
	require.NoError(t, err)
	require.True(t, refreshed)
	if lifecycle == "destination leave waits for archived source" {
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
	}
	_, commentEvent, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: before.ID, Author: "worker", Body: "source-only newer discussion"})
	require.NoError(t, err)
	if lifecycle == "source paused before destination joins" {
		binding, err := spoke.FederationBindingByProject(ctx, localSource.ID)
		require.NoError(t, err)
		binding.Enabled = false
		_, err = spoke.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
	}
	var pending db.Event
	switch lifecycle {
	case "destination leave waits for archived source":
		archived, _, err := spoke.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: localSource.ID, Actor: "worker", Force: true})
		require.NoError(t, err, "the issue is still source-owned when the spoke archives it")
		require.NotNil(t, archived.DeletedAt)
		binding, err := spoke.FederationBindingByProject(ctx, localSource.ID)
		require.NoError(t, err, "archive retains the source enrollment")
		require.Less(t, binding.PushCursorEventID, commentEvent.ID)
	case "destination leave waits for pending initial link":
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
		var linked db.Issue
		linked, pending, err = spoke.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: localSource.ID, Title: "new issue with a link", Author: "worker",
			Links: []db.InitialLink{{Type: "blocks", ToNumber: before.ID}},
		})
		require.NoError(t, err)
		require.Equal(t, "issue.created", pending.Type)
		require.Equal(t, &linked.UID, pending.IssueUID)
		require.Nil(t, pending.RelatedIssueUID, "the initial link peer is present only in the payload")
		require.Contains(t, pending.Payload, before.UID)
	case "destination leave waits for pending cron observation":
		job, _ := nativeFederationDefinitions(t, spoke, localSource)
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
		runUID, err := uid.New()
		require.NoError(t, err)
		observed, err := spoke.ObserveCronRun(ctx, db.ObserveCronRun{
			ProjectID: localSource.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID,
			IssueUID: &before.UID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1},
		})
		require.NoError(t, err)
		require.Len(t, observed.Events, 1)
		pending = observed.Events[0]
		require.Equal(t, "cron.run.observed", pending.Type)
		require.Nil(t, pending.IssueUID, "the observation references the issue only in its payload")
		var payload db.CronRunObservation
		require.NoError(t, json.Unmarshal([]byte(pending.Payload), &payload))
		require.Equal(t, runUID, payload.UID)
		require.NotEqual(t, before.UID, payload.UID)
		require.Equal(t, &before.UID, payload.IssueUID)
	}
	localTarget := join(target)
	blocker, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localTarget.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "local short ID reservation", Author: "worker"})
	require.NoError(t, err)
	seen, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	pull(target, localTarget, baseline.ReplayHorizonEventID-1)
	switch lifecycle {
	case "destination leave waits for pending initial link", "destination leave waits for pending cron observation":
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localTarget)
		_, err = spoke.LeaveFederationReplica(ctx, localTarget.ID)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush,
			"the pending %s requires the current owner's enrollment", pending.Type)
		require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, localSource.ID, pending.ID))
		_, err = spoke.LeaveFederationReplica(ctx, localTarget.ID)
		require.NoError(t, err, "acknowledging the pending event releases the destination enrollment")
		return
	case "destination leave waits for archived source":
		_, err = spoke.LeaveFederationReplica(ctx, localTarget.ID)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush,
			"the archived source's pending comment still requires the destination enrollment")

		require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, localSource.ID, commentEvent.ID))
		_, err = spoke.LeaveFederationReplica(ctx, localTarget.ID)
		require.NoError(t, err, "acknowledging the archived source's comment releases the destination enrollment")
		archived, err := spoke.ProjectByID(ctx, localSource.ID)
		require.NoError(t, err)
		require.NotNil(t, archived.DeletedAt)
		return
	}
	for _, project := range []db.Project{localSource, localTarget} {
		reset, err := spoke.PurgeResetCheck(ctx, seen, project.ID)
		require.NoError(t, err)
		require.Greater(t, reset, seen, "both project views must refresh after snapshot relocation")
	}
	got, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, before.ID, got.ID, "relocation preserves the local issue identity")
	require.Equal(t, localTarget.ID, got.ProjectID)
	require.NotEqual(t, blocker.ShortID, got.ShortID)
	comments, err := spoke.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	require.Equal(t, beforeComments[0].ID, comments[0].ID)
	assert.Equal(t, edited.Body, comments[0].Body)
	assert.Equal(t, edited.EditedAt, comments[0].EditedAt)
	require.Equal(t, "source-only newer discussion", comments[1].Body)
	labels, err := spoke.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Empty(t, labels)
	links, err := spoke.LinksByIssue(ctx, got.ID)
	require.NoError(t, err)
	if lifecycle == "destination leave waits for pending initial link" {
		require.Len(t, links, 2)
	} else {
		require.Len(t, links, 1)
	}
	cursor, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	for range 2 {
		if lifecycle != "source paused before destination joins" && lifecycle != "destination leave waits for archived source" {
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, localSource.ID), "newer source discussion must not restore source ownership")
		}
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
	}
	stable, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, got, stable)
	after, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, after, "repeated pulls must not create synthetic moves or reset controls")
	if lifecycle == "active replicas" || lifecycle == "payload-only snapshot reference" {
		t.Run("source reference", func(t *testing.T) {
			resolved, err := spoke.IssueByShortID(ctx, localSource.ID, before.ShortID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, issue.UID, resolved.UID)
		})
		t.Run("source reference reservation", func(t *testing.T) {
			other, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localSource.ID, UID: "01HZNQ7VFPK1XGD8R5MAYXD4EX", Title: "source reference collision", Author: "worker"})
			require.NoError(t, err)
			require.NotEqual(t, before.ShortID, other.ShortID)
		})
		t.Run("merge preserves source reference", func(t *testing.T) {
			acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
			acknowledgeFederatedRehomeSourceEvents(t, spoke, localTarget)
			_, err := spoke.LeaveFederationReplica(ctx, localSource.ID)
			require.NoError(t, err)
			_, err = spoke.LeaveFederationReplica(ctx, localTarget.ID)
			require.NoError(t, err)
			incoming, err := spoke.CreateProject(ctx, "reference-merge")
			require.NoError(t, err)
			collision, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: incoming.ID, UID: "01HZNQ7VFPK1XGD8R5MAZZD4EX", Title: "merge reference collision", Author: "worker"})
			require.NoError(t, err)
			require.Equal(t, before.ShortID, collision.ShortID)
			_, err = spoke.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: incoming.ID, TargetProjectID: localSource.ID, Actor: "worker"})
			require.NoError(t, err)
			merged, err := spoke.IssueByUID(ctx, collision.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.NotEqual(t, before.ShortID, merged.ShortID)
			resolved, err := spoke.IssueByShortID(ctx, localSource.ID, before.ShortID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, issue.UID, resolved.UID)
		})
	}
	switch lifecycle {
	case "pausing the source retains discussion":
		binding, err := spoke.FederationBindingByProject(ctx, localSource.ID)
		require.NoError(t, err)
		binding.Enabled = false
		_, err = spoke.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		cursor, err := hub.MaxEventID(ctx)
		require.NoError(t, err)
		_, _, err = hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "destination discussion"})
		require.NoError(t, err)
		page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, AfterID: cursor, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		var events []db.RemoteEvent
		var uids []string
		for _, event := range page.Events {
			events = append(events, remoteMoveEvent(event))
			uids = append(uids, event.UID)
		}
		require.NotEmpty(t, events)
		_, err = spoke.InsertRemoteEvents(ctx, localTarget.ID, events)
		require.NoError(t, err)
		if scoped, ok := spoke.(interface {
			MaterializeFederatedEvents(context.Context, int64, []string) error
		}); ok {
			require.NoError(t, scoped.MaterializeFederatedEvents(ctx, localTarget.ID, uids))
		} else {
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
		}
		retained, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Len(t, retained, 3)
		require.Equal(t, comments, retained[:2], "pausing the old source must preserve relocated discussion and comment IDs")
		require.Equal(t, "destination discussion", retained[2].Body)
	case "source leave retains discussion":
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
		_, err := spoke.LeaveFederationReplica(ctx, localSource.ID)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
		retained, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Equal(t, comments, retained)
	case "destination leave preserves standalone issue":
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localTarget)
		_, err := spoke.LeaveFederationReplica(ctx, localTarget.ID)
		require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
		_, err = spoke.LeaveFederationReplica(ctx, localTarget.ID)
		require.NoError(t, err)
		body := "standalone destination edit"
		_, _, _, err = spoke.EditIssue(ctx, db.EditIssueParams{IssueID: got.ID, Body: &body, Actor: "worker"})
		require.NoError(t, err)
		before, err := spoke.IssueByID(ctx, got.ID)
		require.NoError(t, err)
		beforeComments, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		cursor, err := hub.MaxEventID(ctx)
		require.NoError(t, err)
		other, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "unrelated source update", Author: "worker"})
		require.NoError(t, err)
		pull(source, localSource, cursor)
		_, err = spoke.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
		require.NoError(t, err, "leaving the destination must not stall source pulls")
		after, err := spoke.IssueByID(ctx, got.ID)
		require.NoError(t, err)
		require.Equal(t, before, after, "source replay must preserve standalone destination state")
		afterComments, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Equal(t, beforeComments, afterComments)
	}
}

// Snapshot relocation must retain the existing same-hub and recurrence gates.
func checkFederatedSnapshotRelocationBoundaries(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"different-hub", "recurrence", "retained-move-after-source-leave"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, move := federatedRehomeFixture(t, store, "snapshot-guard-"+kind)
			if kind == "recurrence" {
				r, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "worker", Rule: "FREQ=WEEKLY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "pinned"}})
				require.NoError(t, err)
				_, err = store.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				}).ExecContext(ctx, `UPDATE issues SET recurrence_id=$1 WHERE id=$2`, r.ID, issue.ID)
				require.NoError(t, err)
			}
			prepareRehomePull(t, store, source, target, "pull")
			if kind == "different-hub" {
				binding, err := store.FederationBindingByProject(ctx, target.ID)
				require.NoError(t, err)
				binding.HubURL = "https://other-hub.example"
				_, err = store.UpsertFederationBinding(ctx, binding)
				require.NoError(t, err)
			}
			if kind == "retained-move-after-source-leave" {
				acknowledgeFederatedRehomeSourceEvents(t, store, source)
				_, err := store.LeaveFederationReplica(ctx, source.ID)
				require.NoError(t, err)
				_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{move})
				require.NoError(t, err)
			}
			var payload map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(move.Payload, &payload))
			snapshot := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+1, payload["snapshot"])
			_, err := store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{snapshot})
			require.NoError(t, err)
			before, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			err = store.MaterializeFederatedProject(ctx, target.ID)
			if kind == "retained-move-after-source-leave" {
				require.NoError(t, err, "a newer snapshot must not disable retained move replay after source leave")
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, before.ID, got.ID)
				require.Equal(t, target.ID, got.ProjectID)
				return
			}
			if kind == "recurrence" {
				var pinned *db.RecurrencePinnedError
				require.ErrorAs(t, err, &pinned)
			} else {
				require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
			}
			got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, before, got, "refused relocation must roll back all row changes")
			after, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, after)
		})
	}
	return nil
}
