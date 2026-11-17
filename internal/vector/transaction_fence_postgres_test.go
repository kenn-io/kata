package vector_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

func TestPostgresReconcilerWritesHonorTransactionFence(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first issue", Author: "user-a"})
	require.NoError(t, err)
	idx, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	release, err := idx.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, release()) })
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	generation := kitvec.Generation{Model: "example", Dimensions: 2}
	require.NoError(t, idx.EnsureBuilding(ctx, "first", generation))
	require.NoError(t, idx.CutOver(ctx, "first"))
	require.NoError(t, idx.EnsureBuilding(ctx, "second", generation))
	var revision int64
	require.NoError(t, store.DB.QueryRowContext(ctx, `SELECT content_revision FROM issue_vector_mirror WHERE issue_uid = $1`, issue.UID).Scan(&revision))
	denied := errors.New("worker authority revoked")
	fenced := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return denied })
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "second issue", Author: "user-a"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(fenced, store)
	require.ErrorIs(t, err, denied)
	require.ErrorIs(t, idx.EnsureBuilding(fenced, "third", generation), denied)
	require.ErrorIs(t, idx.EnsureBuilding(fenced, "first", generation), denied, "an active generation still checks authority before the provider runs")
	require.ErrorIs(t, idx.SaveVectorsForTest(fenced, "first", issue.UID, revision, []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}}), denied)
	require.ErrorIs(t, idx.CutOver(fenced, "second"), denied)
	var mirror, generations, chunks, stamps int
	require.NoError(t, store.DB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM issue_vector_mirror), (SELECT count(*) FROM issue_vector_generations), (SELECT count(*) FROM issue_vector_chunks), (SELECT count(*) FROM issue_vector_stamps)`).Scan(&mirror, &generations, &chunks, &stamps))
	require.Equal(t, [4]int{1, 2, 0, 0}, [4]int{mirror, generations, chunks, stamps}, "mirror, generations, chunks, stamps")
	var active string
	require.NoError(t, store.DB.QueryRowContext(ctx, `SELECT gen_key FROM issue_vector_generations WHERE state = 'active'`).Scan(&active))
	require.Equal(t, "first", active)
}
