package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
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
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Body: &body, Actor: "worker"})
	require.NoError(t, err)
	before, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "a detached destination must not prevent the joined source from reconciling")
	after, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "source replay must preserve standalone destination state")
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
	return nil
}
