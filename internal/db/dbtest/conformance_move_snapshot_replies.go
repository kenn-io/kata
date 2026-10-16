package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkMoveSnapshotReplyProjectScope(t *testing.T, store db.Storage) error {
	for _, state := range []string{"hidden", "visible", "moved-out", "missing", "hidden-link", "visible-link"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			projects := make([]db.Project, 3)
			for i, name := range []string{"source", "target", "other"} {
				var err error
				projects[i], err = store.CreateProject(ctx, fmt.Sprintf("snapshot-%s-%s", state, name))
				require.NoError(t, err)
				_, err = store.EnableProjectFederation(ctx, projects[i].ID, "worker")
				require.NoError(t, err)
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moving issue", Author: "worker"})
			require.NoError(t, err)
			peerProject := projects[2]
			if state == "visible" || state == "visible-link" || state == "moved-out" {
				peerProject = projects[1]
			}
			peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peerProject.ID, Title: "reply target", Author: "worker"})
			require.NoError(t, err)
			hiddenUID := peer.UID
			if state == "hidden-link" || state == "visible-link" {
				_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "worker"})
				require.NoError(t, err)
			} else {
				comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "worker", Body: "target comment"})
				require.NoError(t, err)
				_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "nested reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
				require.NoError(t, err)
				hiddenUID = comment.UID
			}
			moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NotNil(t, moved.Event)
			require.Contains(t, moved.Event.Payload, hiddenUID)
			if state == "moved-out" {
				_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: peer.Revision, Actor: "worker"})
				require.NoError(t, err)
			} else if state == "missing" {
				_, err = store.PurgeIssue(ctx, peer.ID, "worker", nil)
				require.NoError(t, err)
			}
			query := db.EventsAfterParams{AfterID: moved.EventID - 1, ThroughID: moved.EventID, Limit: 1}
			scoped := db.WithExposedIssues(db.WithAuthorizedProjects(ctx, []string{projects[0].UID, projects[1].UID}))
			page, err := store.EventsAfter(scoped, query)
			require.NoError(t, err)
			require.Len(t, page, 1)
			if state == "visible" || state == "visible-link" {
				require.Equal(t, "issue.moved", page[0].Type)
				require.Contains(t, page[0].Payload, hiddenUID)
				require.Contains(t, db.ExposedIssues(scoped), peer.ID, "current reference owner participates in release revalidation")
			} else {
				require.Equal(t, db.ProjectScopeResetEventType, page[0].Type)
				require.Equal(t, moved.EventID, page[0].ID)
				require.Empty(t, page[0].Payload)
				require.Nil(t, page[0].IssueUID)
				require.Nil(t, page[0].RelatedIssueUID)
			}
			trusted, err := store.EventsAfter(db.WithFederationEventStream(scoped), query)
			require.NoError(t, err)
			require.Len(t, trusted, 1)
			require.Equal(t, "issue.moved", trusted[0].Type, "trusted federation retains immutable replay data")
			require.Contains(t, trusted[0].Payload, hiddenUID)
		})
	}
	return nil
}
