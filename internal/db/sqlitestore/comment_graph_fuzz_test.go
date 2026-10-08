package sqlitestore_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Scope filtering must precede graph expansion and endpoint disclosure.
func FuzzCommentGraphScope(f *testing.F) {
	f.Add(true, false)
	f.Add(true, true)
	f.Fuzz(func(t *testing.T, sourceVisible, targetVisible bool) {
		s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		p, err := s.CreateProject(t.Context(), "example-project")
		require.NoError(t, err)
		a, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Source"})
		require.NoError(t, err)
		b, _, err := s.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Target"})
		require.NoError(t, err)
		c, _, err := s.CreateComment(t.Context(), db.CreateCommentParams{IssueID: b.ID, Author: "worker", Body: "Finding"})
		require.NoError(t, err)
		_, _, err = s.CreateComment(t.Context(), db.CreateCommentParams{IssueID: a.ID, Author: "worker", Body: "Reply", ReplyToUID: c.UID, ReplyKind: "reply"})
		require.NoError(t, err)
		ids := []int64{}
		if sourceVisible {
			ids = append(ids, a.ID)
		}
		if targetVisible {
			ids = append(ids, b.ID)
		}
		got, err := s.ReadCommentGraph(t.Context(), db.CommentGraphQuery{ProjectID: p.ID, AllowedIssueIDs: ids})
		require.NoError(t, err)
		want := 0
		if sourceVisible {
			want++
		}
		if targetVisible {
			want++
		}
		require.Len(t, got.Comments, want)
		require.Empty(t, got.CommentUIDsByProject, "scoped reads must not fetch collision candidates in an unauthorized target project")
		for _, r := range got.Comments {
			if r.Comment.IssueID == a.ID {
				require.True(t, sourceVisible)
				if !targetVisible {
					require.Empty(t, r.Comment.ReplyToUID)
					require.Empty(t, r.Comment.ReplyKind)
				}
			} else {
				require.True(t, targetVisible)
				require.Equal(t, b.ID, r.Comment.IssueID)
			}
		}
	})
}
