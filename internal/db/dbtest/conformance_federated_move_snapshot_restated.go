package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A winning repeated import can be the only event that carries a comment's
// edited text. A later move snapshot with that text is proven by it, as it
// would be by a snapshot or a comment edit.
func checkFederatedMoveSnapshotCommentFromRepeatedImport(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "restated-edit-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "restated-edit-target")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "moving issue", Author: "worker"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "original discussion"})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	grants := make(map[string]int64, 2)
	for _, project := range []db.Project{source, target} {
		grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
			SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker",
		})
		require.NoError(t, err)
		grants[project.UID] = grant.Enrollment.ID
	}

	editedAt := time.Now().UTC().Add(time.Second)
	edited := map[string]any{
		"comment_uid": comment.UID, "author": comment.Author,
		"body": "edited discussion", "created_at": comment.CreatedAt.UTC().Format(time.RFC3339Nano),
		"edited_at": editedAt.Format(time.RFC3339Nano),
	}
	restatedPayload, err := json.Marshal(map[string]any{
		"title": issue.Title, "body": issue.Body, "status": issue.Status,
		"comments": []any{edited}, "updated_at": editedAt.Format(time.RFC3339Nano),
	})
	require.NoError(t, err)
	restated := newRemoteEvent(t, source, &issue.UID, db.RepeatedImportEventType, "worker", origin, editedAt.UnixMilli(), jsontext.Value(restatedPayload))
	restated.CreatedAt = editedAt
	restated.ContentHash = remoteEventHash(t, restated)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: source.ID, SpokeInstanceUID: origin, BoundActor: "worker",
		FederationEnrollmentID: grants[source.UID], EventFeatures: db.SupportedEventFeatures,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: restated}},
	})
	require.NoError(t, err)
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "edited discussion", comments[0].Body)
	// The hub edits the comment afterwards. A home that has not pulled that
	// edit still moves the issue with the text the copy gave it.
	_, _, _, err = store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "worker", Body: "newer discussion"})
	require.NoError(t, err)

	movePayload, err := json.Marshal(map[string]any{
		"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID,
		"to_project_uid": target.UID, "to_short_id": issue.ShortID,
		"snapshot": map[string]any{
			"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title,
			"body": issue.Body, "author": issue.Author, "status": issue.Status,
			"comments": []any{edited}, "links": []any{},
		},
	})
	require.NoError(t, err)
	moveAt := editedAt.Add(2 * time.Second)
	move := newRemoteEvent(t, target, &issue.UID, "issue.moved", "worker", origin, moveAt.UnixMilli(), jsontext.Value(movePayload))
	move.CreatedAt = moveAt
	move.ContentHash = remoteEventHash(t, move)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: target.ID, SpokeInstanceUID: origin, BoundActor: "worker",
		FederationEnrollmentID:  grants[target.UID],
		MoveSourceEnrollmentIDs: map[string]int64{source.UID: grants[source.UID]},
		EventFeatures:           db.SupportedEventFeatures,
		Events:                  []db.FederationIngestEvent{{SourceEventID: 2, Event: move}},
	})
	require.NoError(t, err, "the move snapshot may use a comment version a repeated import carried")
	moved, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, moved.ProjectID)
	return nil
}
