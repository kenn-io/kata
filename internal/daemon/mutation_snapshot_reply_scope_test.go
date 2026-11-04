package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestMutationSnapshotReplyProjectScope(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "scope.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	projects := make([]db.Project, 3)
	for i, name := range []string{"source-project", "target-project", "other-project"} {
		projects[i], err = store.CreateProject(t.Context(), name)
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(t.Context(), projects[i].ID, "worker")
		require.NoError(t, err)
	}
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moving issue", Author: "worker"})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[2].ID, Title: "reply target", Author: "worker"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: peer.ID, Author: "worker", Body: "target comment"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "nested reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, moved.Event)
	for _, visible := range []bool{false, true} {
		allowed := []string{projects[0].UID, projects[1].UID}
		if visible {
			allowed = append(allowed, projects[2].UID)
		}
		ctx := db.WithExposedIssues(db.WithAuthorizedProjects(t.Context(), allowed))
		projected, err := projectScopedMutationEvent(ctx, store, moved.Event)
		require.NoError(t, err)
		if visible {
			require.NotNil(t, projected)
			require.Contains(t, projected.Payload, comment.UID)
			require.Contains(t, db.ExposedIssues(ctx), peer.ID, "mutation release rechecks the nested reply owner")
		} else {
			require.Nil(t, projected, "mutation receipts cannot expose a hidden nested reply")
		}
	}
}
