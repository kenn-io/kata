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
	_, err = store.DB.ExecContext(ctx, `CREATE TABLE vector_fence_test_markers (value int)`)
	require.NoError(t, err)
	denied := errors.New("worker authority revoked")
	fenced := db.WithTransactionFence(ctx, func(ctx context.Context, tx db.Transaction) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO vector_fence_test_markers(value) VALUES (1)`); err != nil {
			return err
		}
		return denied
	})
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "second issue", Author: "user-a"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"mirror", func() error { _, err := idx.RefreshMirror(fenced, store); return err }},
		{"generation", func() error { return idx.EnsureBuilding(fenced, "third", generation) }},
		{"active generation", func() error { return idx.EnsureBuilding(fenced, "first", generation) }},
		{"save", func() error {
			return idx.SaveVectorsForTest(fenced, "first", issue.UID, revision, []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}})
		}},
		{"cutover", func() error { return idx.CutOver(fenced, "second") }},
	} {
		t.Run(tc.name, func(t *testing.T) { require.ErrorIs(t, tc.run(), denied) })
	}
	var mirror, generations, chunks, stamps int
	require.NoError(t, store.DB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM issue_vector_mirror), (SELECT count(*) FROM issue_vector_generations), (SELECT count(*) FROM issue_vector_chunks), (SELECT count(*) FROM issue_vector_stamps)`).Scan(&mirror, &generations, &chunks, &stamps))
	require.Equal(t, 1, mirror)
	require.Equal(t, 2, generations)
	require.Zero(t, chunks)
	require.Zero(t, stamps)
	var markers int
	require.NoError(t, store.DB.QueryRowContext(ctx, `SELECT count(*) FROM vector_fence_test_markers`).Scan(&markers))
	require.Zero(t, markers)
	require.NoError(t, idx.SaveVectorsForTest(ctx, "first", issue.UID, revision, []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}}))
	var active string
	require.NoError(t, store.DB.QueryRowContext(ctx, `SELECT gen_key FROM issue_vector_generations WHERE state = 'active'`).Scan(&active))
	require.Equal(t, "first", active)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	_, err = store.DB.ExecContext(ctx, `INSERT INTO issue_vector_mirror(issue_uid, project_uid, content, content_revision) VALUES($1, $2, 'stale', 1)`, "stale-uid", project.UID)
	require.NoError(t, err)
	_, err = idx.RefreshMirror(fenced, store)
	require.ErrorIs(t, err, denied)
	var stale int
	require.NoError(t, store.DB.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_mirror WHERE issue_uid = 'stale-uid'`).Scan(&stale))
	require.Equal(t, 1, stale)
}
