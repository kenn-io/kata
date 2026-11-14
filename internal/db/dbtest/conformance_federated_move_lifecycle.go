package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMoveSourceReset(t *testing.T, store db.Storage) error {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint(guarded), func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, move := federatedRehomeFixture(t, store, fmt.Sprintf("source-reset-%t", guarded))
			prepareRehomePull(t, store, source, target, "pull")
			replayFederatedRehome(t, store, target, move, "pull")
			moved, err := store.IssueByID(ctx, issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, moved.ProjectID)
			commentUID, err := uid.New()
			require.NoError(t, err)
			at := time.UnixMilli(move.HLCPhysicalMS + 1000).UTC()
			raw, err := json.Marshal(map[string]string{"comment_uid": commentUID, "author": "worker", "body": "delayed source comment", "created_at": at.Format(time.RFC3339Nano)})
			require.NoError(t, err)
			comment := newRemoteEvent(t, source, &issue.UID, "issue.commented", "worker", move.OriginInstanceUID, at.UnixMilli(), jsontext.Value(raw))
			_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{comment})
			require.NoError(t, err)
			require.NoError(t, store.MaterializeFederatedProject(ctx, target.ID))
			before, err := store.CommentsByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Len(t, before, 1)
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.NoError(t, store.AdvanceFederationPushCursor(ctx, source.ID, cursor))
			if guarded {
				require.NoError(t, store.ResetFederatedProjectIfNoPendingPush(ctx, source.ID, cursor+1, cursor, store.InstanceUID(), cursor))
			} else {
				require.NoError(t, store.ResetFederatedProject(ctx, source.ID, cursor+1, cursor))
			}
			require.NoError(t, store.MaterializeFederatedProject(ctx, target.ID))
			after, err := store.CommentsByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Equal(t, before, after, "resetting a former source must retain history used by its moved issues")
		})
	}
	return nil
}

func checkFederatedMoveDetachedDestination(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, target, issue, move := federatedRehomeFixture(t, store, "detached-destination")
	prepareRehomePull(t, store, source, target, "pull")
	replayFederatedRehome(t, store, target, move, "pull")
	_, err := store.LeaveFederationReplica(ctx, target.ID)
	require.NoError(t, err)
	body := "standalone destination edit"
	title := "standalone title"
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Body: &body, Actor: "worker"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "standalone comment"})
	require.NoError(t, err)
	_, err = store.AddLabel(ctx, issue.ID, "standalone", "worker")
	require.NoError(t, err)
	before, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "a detached destination must not prevent the joined source from reconciling")
	after, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "source replay must preserve standalone destination state")
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "standalone comment", comments[0].Body)
	labels, err := store.LabelsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, "standalone", labels[0].Label)
	other, err := store.CreateProject(ctx, "detached-later-target")
	require.NoError(t, err)
	winner := newRemoteEvent(t, other, &issue.UID, "issue.moved", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+1000, []byte(strings.ReplaceAll(string(move.Payload), target.UID, other.UID)))
	_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{winner})
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID))
	after, err = store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "a later move must not take an issue from a detached project")
	_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, issue.ID, "worker", nil)
	require.NoError(t, err)
	// The source may still owe a hub page from before the standalone purge.
	_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{move})
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "late source history must not reconcile a purged standalone destination")
	_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkFederatedMovePurgeFormerStreams(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	t.Run("unrelated destination purge preserves source move", func(t *testing.T) {
		source, target, issue, _ := federatedRehomeFixture(t, store, "unrelated-destination-purge")
		beforeMove, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
			IfMatchRev: issue.Revision, Actor: "worker",
		})
		require.NoError(t, err)
		require.NotNil(t, moved.Event)
		unrelated, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: target.ID, Title: "unrelated destination issue", Author: "worker",
		})
		require.NoError(t, err)
		_, _, _, err = store.SoftDeleteIssue(ctx, unrelated.ID, "worker")
		require.NoError(t, err)
		_, err = store.PurgeIssue(ctx, unrelated.ID, "worker", nil)
		require.NoError(t, err)

		reset, err := store.ReadFederation(ctx, db.FederationReadParams{
			ProjectID: target.ID, AfterID: beforeMove, Limit: 100,
			Features: db.SupportedEventFeatures,
		})
		require.NoError(t, err)
		require.Greater(t, reset.ResetAfterID, beforeMove,
			"the unrelated destination purge should force an ordinary baseline refresh")
		metadata, err := store.ReadFederation(ctx, db.FederationReadParams{
			ProjectID: target.ID, Metadata: true, Features: db.SupportedEventFeatures,
		})
		require.NoError(t, err)
		require.Greater(t, metadata.Binding.ReplayHorizonEventID, moved.Event.ID)

		sourcePage, err := store.ReadFederation(ctx, db.FederationReadParams{
			ProjectID: source.ID, AfterID: beforeMove, Limit: 100,
			Features: db.SupportedEventFeatures,
		})
		require.NoError(t, err)
		foundMove := false
		for _, event := range sourcePage.Events {
			if event.UID == moved.Event.UID {
				foundMove = true
				break
			}
		}
		require.True(t, foundMove,
			"ordinary destination refreshes must not cut active move history from the stale source feed")
	})

	source, target, issue, _ := federatedRehomeFixture(t, store, "purge-streams")
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, issue.ID, "worker", nil)
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		read, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: before, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		require.Greater(t, read.ResetAfterID, before, "purging moved issue history must invalidate every affected project stream")
		metadata, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		require.Greater(t, metadata.Binding.ReplayHorizonEventID, read.ResetAfterID)
	}
	// Late history arrives in streams that hold no local row for the issue,
	// so only the header issue UID ties it to the purged issue.
	t.Run("late history without a local issue row", func(t *testing.T) {
		ctx := t.Context()
		source, target, issue, _ := federatedRehomeFixture(t, store, "purge-late-history")
		lateStream, err := store.CreateProject(ctx, "rehome-late-stream")
		require.NoError(t, err)
		moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
			IfMatchRev: issue.Revision, Actor: "worker",
		})
		require.NoError(t, err)
		origin, err := uid.New()
		require.NoError(t, err)
		lateSource := newRemoteEvent(t, source, &issue.UID, "issue.updated", "worker", origin,
			time.Now().Add(time.Hour).UnixMilli(), jsontext.Value(`{"title":"late source edit"}`))
		_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{lateSource})
		require.NoError(t, err)
		lateOther := newRemoteEvent(t, lateStream, &issue.UID, "issue.updated", "worker", origin,
			time.Now().Add(2*time.Hour).UnixMilli(), jsontext.Value(`{"title":"late stream edit"}`))
		_, err = store.InsertRemoteEvents(ctx, lateStream.ID, []db.RemoteEvent{lateOther})
		require.NoError(t, err)
		stored, err := store.EventsByUIDs(ctx, source.ID, []string{lateSource.EventUID})
		require.NoError(t, err)
		require.Len(t, stored, 1)
		require.Nil(t, stored[0].IssueID, "late source history has no local issue row")
		require.Equal(t, &issue.UID, stored[0].IssueUID)
		stored, err = store.EventsByUIDs(ctx, lateStream.ID, []string{lateOther.EventUID})
		require.NoError(t, err)
		require.Len(t, stored, 1)
		require.Nil(t, stored[0].IssueID)
		require.Equal(t, &issue.UID, stored[0].IssueUID)

		_, _, _, err = store.SoftDeleteIssue(ctx, moved.Issue.ID, "worker")
		require.NoError(t, err)
		purged, err := store.PurgeIssue(ctx, moved.Issue.ID, "worker", nil)
		require.NoError(t, err)
		require.GreaterOrEqual(t, purged.EventCount, int64(6), "late issue history belongs to the purge count")
		require.Contains(t, purged.ResetProjectIDs, source.ID,
			"purging late source history must invalidate its project stream")
		require.Contains(t, purged.ResetProjectIDs, lateStream.ID,
			"late issue history must invalidate its own project stream")
		for _, event := range []struct {
			projectID int64
			uid       string
		}{{source.ID, lateSource.EventUID}, {lateStream.ID, lateOther.EventUID}} {
			remaining, err := store.EventsByUIDs(ctx, event.projectID, []string{event.uid})
			require.ErrorIs(t, err, db.ErrNotFound)
			require.Empty(t, remaining, "purge must remove late issue history")
		}

		grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			SpokeInstanceUID: origin, Capabilities: "push,pull", Actor: "worker",
		})
		require.NoError(t, err)
		unrelatedUID, err := uid.New()
		require.NoError(t, err)
		unrelated := newRemoteEvent(t, source, &unrelatedUID, "issue.created", "worker", origin,
			time.Now().Add(2*time.Hour).UnixMilli(), IssueDocument(t, unrelatedUID, jsontext.Value(`{"uid":"`+unrelatedUID+`","title":"unrelated issue","author":"worker","status":"open","metadata":{}}`)))
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: source.ID, SpokeInstanceUID: origin, BoundActor: "worker",
			FederationEnrollmentID: grant.Enrollment.ID, EventFeatures: db.SupportedEventFeatures,
			Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: unrelated}},
		})
		require.NoError(t, err)
		_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound,
			"an unrelated source push must not replay and recreate the purged issue")
	})
	return nil
}

func checkFederatedMoveResetRetainedLink(t *testing.T, store db.Storage) error {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint(guarded), func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, move := federatedRehomeFixture(t, store, fmt.Sprintf("retained-link-%t", guarded))
			prepareRehomePull(t, store, source, target, "pull")
			peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "source peer remains", Author: "worker"})
			require.NoError(t, err)
			_, linkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "worker"})
			require.NoError(t, err)
			require.Equal(t, &peer.ID, linkEvent.RelatedIssueID)
			replayFederatedRehome(t, store, target, move, "pull")
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.NoError(t, store.AdvanceFederationPushCursor(ctx, source.ID, cursor))
			if guarded {
				err = store.ResetFederatedProjectIfNoPendingPush(ctx, source.ID, cursor+1, cursor, store.InstanceUID(), cursor)
			} else {
				err = store.ResetFederatedProject(ctx, source.ID, cursor+1, cursor)
			}
			require.NoError(t, err, "retained event numeric references must not block source projection reset")
			retained, err := store.EventsByUIDs(ctx, source.ID, []string{linkEvent.UID})
			require.NoError(t, err)
			require.Len(t, retained, 1)
			require.Nil(t, retained[0].RelatedIssueID)
			require.Equal(t, linkEvent.RelatedIssueUID, retained[0].RelatedIssueUID)
			require.Equal(t, linkEvent.Payload, retained[0].Payload)
			require.Equal(t, linkEvent.ContentHash, retained[0].ContentHash)
			require.NoError(t, store.MaterializeFederatedProject(ctx, target.ID))
			moved, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, target.ID, moved.ProjectID)
		})
	}
	return nil
}
