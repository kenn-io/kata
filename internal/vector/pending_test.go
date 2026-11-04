package vector_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	kitvec "go.kenn.io/kit/vector"
)

// Pending scans must seek past the cursor even while earlier rows stay pending.
func TestPendingForGenerationCursor(t *testing.T) {
	ctx := t.Context()
	store, idx := openVectorBackend(t, "postgres")
	project, err := store.CreateProject(ctx, "cursor-project")
	require.NoError(t, err)
	var issues []db.Issue
	for _, title := range []string{"first", "second", "third"} {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "tester"})
		require.NoError(t, err)
		issues = append(issues, issue)
	}
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	gen := kitvec.Generation{Model: "example", Dimensions: 2}
	key := gen.Fingerprint()
	require.NoError(t, idx.EnsureBuilding(ctx, key, gen))
	first, err := idx.PendingForGenerationForTest(ctx, key, nil, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, issues[0].UID, first[0].Doc)
	next, err := idx.PendingForGenerationForTest(ctx, key, &first[0].Doc, 1)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, issues[1].UID, next[0].Doc)
	require.NoError(t, idx.SaveVectorsForTest(ctx, key, next[0].Doc, next[0].Revision, nil))
	next, err = idx.PendingForGenerationForTest(ctx, key, &first[0].Doc, 10)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, issues[2].UID, next[0].Doc)
	end, err := idx.PendingForGenerationForTest(ctx, key, &issues[2].UID, 10)
	require.NoError(t, err)
	require.Empty(t, end)
}
