package sqlitestore_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
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

// FuzzCommentGraphPurgeEvidenceOrder checks that event-backed purge evidence
// resolves a target even when earlier imported snapshot replies have no create
// events of their own.
func FuzzCommentGraphPurgeEvidenceOrder(f *testing.F) {
	f.Add(uint8(0))
	f.Add(uint8(3))
	f.Fuzz(func(t *testing.T, input uint8) {
		ctx := t.Context()
		store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })

		projectUID, err := uid.New()
		require.NoError(t, err)
		sourceUID, err := uid.New()
		require.NoError(t, err)
		targetUID, err := uid.New()
		require.NoError(t, err)
		targetCommentUID, err := uid.New()
		require.NoError(t, err)

		const importedAt = "2000-01-01T00:00:00.000Z"
		records := []db.ImportRecord{
			&db.ProjectExport{
				ID: 1, UID: projectUID, Name: "example-project", CreatedAt: importedAt,
				Metadata: []byte(`{}`), Revision: 1,
			},
			&db.IssueExport{
				ID: 1, UID: sourceUID, ProjectID: 1, ShortID: strings.ToLower(sourceUID[len(sourceUID)-4:]),
				Title: "Reply source", Status: "open", Author: "worker", CreatedAt: importedAt,
				UpdatedAt: importedAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
			},
			&db.IssueExport{
				ID: 2, UID: targetUID, ProjectID: 1, ShortID: strings.ToLower(targetUID[len(targetUID)-4:]),
				Title: "Purged target", Status: "open", Author: "worker", CreatedAt: importedAt,
				UpdatedAt: importedAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
			},
			&db.CommentExport{
				ID: 1, UID: targetCommentUID, IssueID: 2, Author: "reviewer", Body: "Finding",
				CreatedAt: importedAt,
			},
		}
		snapshotReplies := 1 + int(input%4)
		for index := range snapshotReplies {
			replyUID, err := uid.New()
			require.NoError(t, err)
			records = append(records, &db.CommentExport{
				ID: int64(index + 2), UID: replyUID, IssueID: 1, Author: "worker",
				Body: "Imported response", CreatedAt: importedAt,
				ReplyToUID: targetCommentUID, ReplyKind: "reply",
			})
		}
		require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}))

		project, err := store.ProjectByUID(ctx, projectUID)
		require.NoError(t, err)
		target, err := store.IssueByUID(ctx, targetUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		source, err := store.IssueByUID(ctx, sourceUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: source.ID, Author: "worker", Body: "Later response",
			ReplyToUID: targetCommentUID, ReplyKind: "confirm",
		})
		require.NoError(t, err)
		_, err = store.PurgeIssue(ctx, target.ID, "worker", nil)
		require.NoError(t, err)

		graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID})
		require.NoError(t, err)
		require.Equal(t, "removed", graph.Targets[targetCommentUID].Status)
	})
}
