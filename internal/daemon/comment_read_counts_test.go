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
	legacyReads int
}

func (s *commentReadCountingStore) CommentsByIssue(ctx context.Context, id int64) ([]db.Comment, error) {
	s.legacyReads++
	return s.Storage.CommentsByIssue(ctx, id)
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
	if deleted {
		want = 1
	}
	require.Equal(t, want, counted.legacyReads, "live comments come from the consistent graph; only deleted-issue extras need the legacy read")
}

func TestLiveShowAvoidsLegacyCommentRead(t *testing.T) {
	checkShowCommentReadCount(t, false, false, 0)
}

func TestScopedLiveShowAvoidsLegacyCommentRead(t *testing.T) {
	checkShowCommentReadCount(t, false, true, 0)
}

func TestDeletedShowRetainsLegacyCommentExtras(t *testing.T) {
	checkShowCommentReadCount(t, true, false, 0)
}

func FuzzShowCommentReadCount(f *testing.F) {
	f.Add(false, false, uint8(0))
	f.Add(false, true, uint8(2))
	f.Add(true, false, uint8(1))
	f.Fuzz(checkShowCommentReadCount)
}
