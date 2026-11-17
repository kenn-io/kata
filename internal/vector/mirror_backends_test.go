package vector_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// A refresh must match the live feed through edits, project moves, deletion,
// and restoration, even when its first page contains only unchanged issues.
func TestRefreshMirrorMatchesLiveFeedAcrossBackends(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			store, idx := openVectorBackend(t, backend)
			project, err := store.CreateProject(ctx, "source-project")
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "target-project")
			require.NoError(t, err)
			issues := make([]db.Issue, 506)
			for i := range issues {
				issues[i], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("task %d", i), Body: "original body", Author: "tester"})
				require.NoError(t, err)
			}
			changed, err := idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			require.Equal(t, 506, changed)
			initial, err := idx.MirrorRowsForTest(ctx)
			require.NoError(t, err)
			gen := kitvec.Generation{Model: "example", Dimensions: 2}
			key := gen.Fingerprint()
			require.NoError(t, idx.EnsureBuilding(ctx, key, gen))
			_, err = idx.Fill(ctx, key, func(_ context.Context, texts []string) ([][]float32, error) {
				vectors := make([][]float32, len(texts))
				for i := range vectors {
					vectors[i] = []float32{1, 0}
				}
				return vectors, nil
			}, 0, nil, nil)
			require.NoError(t, err)
			count, err := idx.VectorCountForTest(ctx)
			require.NoError(t, err)
			require.Equal(t, 506, count)

			_, _, _, err = store.SoftDeleteIssue(ctx, issues[504].ID, "tester")
			require.NoError(t, err)
			changed, err = idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			require.Equal(t, 1, changed)
			_, _, _, err = store.RestoreIssue(ctx, issues[504].ID, "tester")
			require.NoError(t, err)
			_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "new task", Author: "tester"})
			require.NoError(t, err)
			_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: issues[500].ID, Title: new("edited title"), Body: new("edited body"), Actor: "tester"})
			require.NoError(t, err)
			_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issues[501].ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: issues[501].Revision, Actor: "tester", Single: true})
			require.NoError(t, err)

			changed, err = idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			require.Equal(t, 4, changed)
			want := make(map[string]vector.MirrorRowForTest)
			var afterID int64
			for {
				page, err := store.ListIssueContent(ctx, afterID, 500)
				require.NoError(t, err)
				if len(page) == 0 {
					break
				}
				for _, issue := range page {
					want[issue.UID] = vector.MirrorRowForTest{ProjectUID: issue.ProjectUID, Content: embedding.EmbedText(issue.Title, issue.Body), Revision: issue.ContentRevision}
					afterID = issue.ID
				}
			}
			got, err := idx.MirrorRowsForTest(ctx)
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.Equal(t, initial[issues[501].UID].Revision, got[issues[501].UID].Revision, "a project move keeps the content revision")
			count, err = idx.VectorCountForTest(ctx)
			require.NoError(t, err)
			require.Equal(t, 505, count, "the removed issue loses its stored vector")
			changed, err = idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			require.Zero(t, changed)
		})
	}
}
