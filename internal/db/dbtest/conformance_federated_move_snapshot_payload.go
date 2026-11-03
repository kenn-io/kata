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

func checkFederatedMoveSnapshotPayloadOnlyCommentEdit(t *testing.T, store db.Storage) error {
	for _, identityField := range []string{"issue_uid", "uid"} {
		t.Run(identityField, func(t *testing.T) {
			ctx := t.Context()
			source, err := store.CreateProject(ctx, "snapshot-edit-source-"+identityField)
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "snapshot-edit-target-"+identityField)
			require.NoError(t, err)
			for _, project := range []db.Project{source, target} {
				_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
				require.NoError(t, err)
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, Title: "moving issue", Author: "worker"})
			require.NoError(t, err)
			targetShortID := issue.ShortID
			comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "original discussion"})
			require.NoError(t, err)
			movedOut, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
				IssueID: issue.ID, FromProjectID: target.ID, ToProjectID: source.ID,
				IfMatchRev: issue.Revision, Actor: "worker",
			})
			require.NoError(t, err)
			issue = movedOut.Issue
			origin, err := uid.New()
			require.NoError(t, err)

			editedAt := time.Now().UTC().Add(time.Second)
			editPayload, err := json.Marshal(map[string]any{
				identityField: issue.UID, "comment_uid": comment.UID,
				"body": "edited discussion", "edited_at": editedAt.Format(time.RFC3339Nano),
			})
			require.NoError(t, err)
			edit := newRemoteEvent(t, target, nil, "issue.comment_edited", "worker", origin, editedAt.UnixMilli(), jsontext.Value(editPayload))
			edit.CreatedAt = editedAt
			edit.ContentHash = remoteEventHash(t, edit)

			snapshot := map[string]any{
				"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title,
				"body": issue.Body, "author": issue.Author, "status": issue.Status,
				"comments": []any{map[string]any{
					"comment_uid": comment.UID, "author": comment.Author,
					"body": "edited discussion", "created_at": comment.CreatedAt.UTC().Format(time.RFC3339Nano),
					"edited_at": editedAt.Format(time.RFC3339Nano),
				}},
				"links": []any{},
			}
			movePayload, err := json.Marshal(map[string]any{
				"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID,
				"to_project_uid": target.UID, "to_short_id": targetShortID, "snapshot": snapshot,
			})
			require.NoError(t, err)
			moveAt := editedAt.Add(time.Millisecond)
			move := newRemoteEvent(t, target, &issue.UID, "issue.moved", "worker", origin, moveAt.UnixMilli(), jsontext.Value(movePayload))
			move.CreatedAt = moveAt
			move.ContentHash = remoteEventHash(t, move)

			grants := make(map[string]int64, 2)
			for _, project := range []db.Project{source, target} {
				grant, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
					SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker",
				})
				require.NoError(t, err)
				grants[project.UID] = grant.Enrollment.ID
			}
			_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
				ProjectID: target.ID, SpokeInstanceUID: origin, BoundActor: "worker",
				FederationEnrollmentID:  grants[target.UID],
				MoveSourceEnrollmentIDs: map[string]int64{source.UID: grants[source.UID]},
				EventFeatures:           db.SupportedEventFeatures,
				Events: []db.FederationIngestEvent{
					{SourceEventID: 1, Event: edit},
					{SourceEventID: 2, Event: move},
				},
			})
			require.NoError(t, err, "the move snapshot may include a comment edit from this batch")
			moved, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, target.ID, moved.ProjectID)
			comments, err := store.CommentsByIssue(ctx, moved.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "edited discussion", comments[0].Body)
		})
	}
	return nil
}
