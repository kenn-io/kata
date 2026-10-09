package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type commentReadCountingStore struct {
	db.Storage
	legacyReads  int
	graphQueries []db.CommentGraphQuery
}

func (s *commentReadCountingStore) CommentsByIssue(ctx context.Context, id int64) ([]db.Comment, error) {
	s.legacyReads++
	return s.Storage.CommentsByIssue(ctx, id)
}

func (s *commentReadCountingStore) ReadCommentGraph(ctx context.Context, query db.CommentGraphQuery) (db.CommentGraphData, error) {
	s.graphQueries = append(s.graphQueries, query)
	return s.Storage.ReadCommentGraph(ctx, query)
}

func checkShowCommentReadCount(t *testing.T, deleted, scoped bool, count uint8) {
	t.Helper()
	if deleted && scoped {
		return // A deleted subtree root cannot authorize a live subtree view.
	}
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue := createScopedAuthIssue(t, store, project.ID, "Finding", nil)
	n := int(count%3) + 1
	for range n {
		_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "finder", Body: "Evidence"})
		require.NoError(t, err)
	}
	ctx := t.Context()
	if deleted {
		issue, _, _, err = store.SoftDeleteIssue(ctx, issue.ID, "finder")
		require.NoError(t, err)
	} else if scoped {
		ctx = withScopedAuthorizationTestPrincipal(t, store, project, issue)
	}
	counted := &commentReadCountingStore{Storage: store}
	out, err := hydrateShowIssueResponse(ctx, ServerConfig{DB: counted, InsecureReadonly: true}, issue, deleted)
	require.NoError(t, err)
	require.Len(t, out.Body.Comments, n)
	for _, comment := range out.Body.Comments {
		require.Equal(t, "Evidence", comment.Body)
	}
	want := 0
	require.Equal(t, want, counted.legacyReads, "live and selected deleted-source comments come from the consistent graph")
	require.Len(t, counted.graphQueries, 1)
	var deletedSourceID int64
	if deleted {
		deletedSourceID = issue.ID
	}
	require.Equal(t, deletedSourceID, counted.graphQueries[0].IncludeDeletedSourceIssueID)
	require.Equal(t, issueScopeFromContext(ctx), counted.graphQueries[0].IssueScope)
}

func TestLiveShowAvoidsLegacyCommentRead(t *testing.T) {
	checkShowCommentReadCount(t, false, false, 0)
}

func TestScopedLiveShowAvoidsLegacyCommentRead(t *testing.T) {
	checkShowCommentReadCount(t, false, true, 0)
}

func TestDeletedShowUsesSelectedSourceGraphRead(t *testing.T) {
	checkShowCommentReadCount(t, true, false, 0)
}

func FuzzShowCommentReadCount(f *testing.F) {
	f.Add(true, false, uint8(1))
	f.Add(false, false, uint8(0))
	f.Add(false, true, uint8(2))
	f.Fuzz(checkShowCommentReadCount)
}
