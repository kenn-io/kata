package vector

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kitvec "go.kenn.io/kit/vector"
)

// A project-scoped SQLite query must stay on vec0's KNN plan with the project
// pre-filter pushed into it, not fall back to a per-row distance scan over the
// whole index or a global window filtered afterwards. Upstream assumption:
// sqlite-vec encodes its chosen plan as the first character of the virtual
// table index string, '3' for KNN and '1' for a full scan.
func TestAuthorizedQueryUsesVec0KNNWithProjectPrefilter(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "canonical.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	ix, err := Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	searched, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	other, err := source.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	target, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: searched.ID, Title: "target", Author: "a"})
	require.NoError(t, err)
	for range 3 {
		_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "closer", Author: "a"})
		require.NoError(t, err)
	}
	_, err = ix.RefreshMirror(ctx, source)
	require.NoError(t, err)
	gen := kitvec.Generation{Model: "m", Dimensions: 2}
	key := gen.Fingerprint()
	require.NoError(t, ix.EnsureBuilding(ctx, key, gen))
	_, err = ix.Fill(ctx, key, func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			if text == "target" {
				out[i] = []float32{0.8, 0.6}
			} else {
				out[i] = []float32{1, 0}
			}
		}
		return out, nil
	}, 0, nil, nil)
	require.NoError(t, err)

	scoped := db.WithAuthorizedProjects(ctx, []string{searched.UID})
	hits, err := ix.Query(scoped, key, kitvec.Vector{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, target.UID, hits[0].Doc, "three closer chunks in another project do not starve the searched project")

	statement, args := authorizedCandidateSQL(scoped, 1, "[1,0]", 1)
	rows, err := ix.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+statement, args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	joined := strings.Join(details, "\n")
	require.Contains(t, joined, "MATERIALIZE knn", "the vec0 scan must run once:\n%s", joined)
	vecScan := slices.IndexFunc(details, func(d string) bool {
		return strings.HasPrefix(d, "SCAN issue_vectors_v1 VIRTUAL TABLE INDEX 0:3")
	})
	require.GreaterOrEqual(t, vecScan, 0, "expected a vec0 KNN plan, not a full scan:\n%s", joined)
	prefilter := slices.IndexFunc(details, func(d string) bool { return strings.Contains(d, "LIST SUBQUERY") })
	require.Greater(t, prefilter, vecScan, "the project rowid pre-filter feeds the KNN:\n%s", joined)
	knnScan := slices.IndexFunc(details, func(d string) bool { return d == "SCAN knn" })
	chunkProbe := slices.IndexFunc(details, func(d string) bool {
		return strings.Contains(d, "SEARCH c USING INDEX issue_vectors_chunks_by_vector (ordinal=? AND vec_rowid=?)")
	})
	require.GreaterOrEqual(t, knnScan, 0, "expected the materialized knn to drive the join:\n%s", joined)
	require.Less(t, knnScan, chunkProbe, "knn must be the outer loop probing the chunk map:\n%s", joined)
}
