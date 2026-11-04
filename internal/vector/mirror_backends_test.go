package vector_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// A refresh must match the live feed through edits, project moves, deletion,
// and restoration, even when its first page contains only unchanged issues.
func TestRefreshMirrorMatchesLiveFeedAcrossBackends(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var store db.Storage
			var idx *vector.Index
			if backend == "sqlite" {
				s, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, idx.Close()) })
			} else {
				dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
				t.Cleanup(cleanup)
				s, err := pgstore.Open(ctx, dsn)
				require.NoError(t, err)
				store = s
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				idx, err = vector.OpenPostgres(ctx, s.DB)
				require.NoError(t, err)
				release, err := idx.AcquireReconcilerLease(ctx)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, release()) })
			}
			project, err := store.CreateProject(ctx, "source-project")
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "target-project")
			require.NoError(t, err)
			doomed, err := store.CreateProject(ctx, "removed-project")
			require.NoError(t, err)
			issues := make([]db.Issue, 506)
			for i := range issues {
				issues[i], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("task %d", i), Body: "original body", Author: "tester"})
				require.NoError(t, err)
			}
			_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: doomed.ID, Title: "removed with project", Author: "tester"})
			require.NoError(t, err)
			changed, err := idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			require.Equal(t, 507, changed)
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
			require.Equal(t, 507, count)

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
			for _, i := range []int{502, 503} {
				_, _, _, err = store.SoftDeleteIssue(ctx, issues[i].ID, "tester")
				require.NoError(t, err)
			}
			_, err = store.PurgeIssue(ctx, issues[503].ID, "tester", nil)
			require.NoError(t, err)
			_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: doomed.ID, Actor: "tester", Force: true})
			require.NoError(t, err)
			_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: doomed.ID, Actor: "tester"})
			require.NoError(t, err)

			changed, err = idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			require.Equal(t, 7, changed)
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
			require.Equal(t, 503, count, "four removed issues lose their stored vectors")
			changed, err = idx.RefreshMirror(ctx, store)
			require.NoError(t, err)
			require.Zero(t, changed)
		})
	}
}
