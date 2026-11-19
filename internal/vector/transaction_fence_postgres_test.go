package vector_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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
	store, idx := openVectorBackend(t, "postgres")
	pg := store.(*pgstore.Store)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first issue", Author: "user-a"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	generation := kitvec.Generation{Model: "example", Dimensions: 2}
	require.NoError(t, idx.EnsureBuilding(ctx, "first", generation))
	require.NoError(t, idx.CutOver(ctx, "first"))
	require.NoError(t, idx.EnsureBuilding(ctx, "second", generation))
	var revision int64
	require.NoError(t, pg.DB.QueryRowContext(ctx, `SELECT content_revision FROM issue_vector_mirror WHERE issue_uid = $1`, issue.UID).Scan(&revision))
	denied := errors.New("worker authority revoked")
	fenced := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { return denied })
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "second issue", Author: "user-a"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(fenced, store)
	require.ErrorIs(t, err, denied)
	require.ErrorIs(t, idx.EnsureBuilding(fenced, "first", generation), denied, "an active generation still checks authority before the provider runs")
	require.ErrorIs(t, idx.SaveVectorsForTest(fenced, "first", issue.UID, revision, []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}}), denied)
	require.ErrorIs(t, idx.CutOver(fenced, "second"), denied)
	var mirror, generations, chunks, stamps int
	require.NoError(t, pg.DB.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM issue_vector_mirror), (SELECT count(*) FROM issue_vector_generations), (SELECT count(*) FROM issue_vector_chunks), (SELECT count(*) FROM issue_vector_stamps)`).Scan(&mirror, &generations, &chunks, &stamps))
	require.Equal(t, [4]int{1, 2, 0, 0}, [4]int{mirror, generations, chunks, stamps}, "mirror, generations, chunks, stamps")
	var active string
	require.NoError(t, pg.DB.QueryRowContext(ctx, `SELECT gen_key FROM issue_vector_generations WHERE state = 'active'`).Scan(&active))
	require.Equal(t, "first", active)
	for i := range 504 {
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("task %d", i), Author: "user-a"})
		require.NoError(t, err)
	}
	var fences atomic.Int32
	counted := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { fences.Add(1); return nil })
	changed, err := idx.RefreshMirror(counted, store)
	require.NoError(t, err)
	require.Equal(t, 505, changed)
	require.EqualValues(t, 2, fences.Load(), "each upsert page is fenced")
	_, err = pg.DB.ExecContext(ctx, `INSERT INTO issue_vector_mirror(issue_uid, project_uid, content, content_revision) SELECT 'stale-' || n, $1, 'stale', 1 FROM generate_series(1, 501) n`, project.UID)
	require.NoError(t, err)
	fences.Store(0)
	changed, err = idx.RefreshMirror(counted, store)
	require.NoError(t, err)
	require.Equal(t, 501, changed)
	require.EqualValues(t, 2, fences.Load(), "each stale-delete page is fenced")
	rows, err := idx.MirrorRowsForTest(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 506)
}

func TestPostgresCutOverReclaimsRetiredGenerationOnce(t *testing.T) {
	ctx := t.Context()
	store, idx := openVectorBackend(t, "postgres")
	pg := store.(*pgstore.Store)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first issue", Author: "user-a"})
	require.NoError(t, err)
	_, err = idx.RefreshMirror(ctx, store)
	require.NoError(t, err)
	a := kitvec.Generation{Model: "model-a", Dimensions: 2}
	b := kitvec.Generation{Model: "model-b", Dimensions: 2}
	aKey, bKey := a.Fingerprint(), b.Fingerprint()
	vectors := []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}}
	require.NoError(t, idx.EnsureBuilding(ctx, aKey, a))
	require.NoError(t, idx.SaveVectorsForTest(ctx, aKey, issue.UID, issue.ContentRevision, vectors))
	require.NoError(t, idx.CutOver(ctx, aKey))
	require.NoError(t, idx.EnsureBuilding(ctx, bKey, b))
	require.NoError(t, idx.SaveVectorsForTest(ctx, bKey, issue.UID, issue.ContentRevision, vectors))

	denied := errors.New("worker authority revoked")
	var fences int
	fenced := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error {
		fences++
		if fences == 3 {
			return denied
		}
		return nil
	})
	require.ErrorIs(t, idx.CutOver(fenced, bKey), denied)
	var chunks int
	err = pg.DB.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_chunks c JOIN issue_vector_generations g ON g.id = c.generation_id WHERE g.gen_key = $1`, aKey).Scan(&chunks)
	require.NoError(t, err)
	require.Equal(t, 1, chunks)

	fences = 0
	counted := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error { fences++; return nil })
	require.NoError(t, idx.CutOver(counted, bKey))
	require.Equal(t, 3, fences, "failed reclamation must be retried")
	fences = 0
	require.NoError(t, idx.CutOver(counted, bKey))
	require.Equal(t, 1, fences, "successful reclamation must not repeat")

	require.NoError(t, idx.EnsureBuilding(ctx, aKey, a))
	require.NoError(t, idx.SaveVectorsForTest(ctx, aKey, issue.UID, issue.ContentRevision, vectors))
	require.NoError(t, idx.CutOver(ctx, aKey))
	require.NoError(t, idx.EnsureBuilding(ctx, bKey, b))
	require.NoError(t, idx.SaveVectorsForTest(ctx, bKey, issue.UID, issue.ContentRevision, vectors))
	fences = 0
	require.NoError(t, idx.CutOver(counted, bKey))
	require.Equal(t, 3, fences, "a rebuilt generation must be reclaimed again")
	err = pg.DB.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_chunks c JOIN issue_vector_generations g ON g.id = c.generation_id WHERE g.gen_key = $1`, aKey).Scan(&chunks)
	require.NoError(t, err)
	require.Zero(t, chunks)
}

func TestPostgresCutOverReclaimsAfterLeaseTakeover(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	first, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	second, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first issue", Author: "user-a"})
	require.NoError(t, err)
	release, err := first.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		if release != nil {
			_ = release()
		}
	})
	_, err = first.RefreshMirror(ctx, store)
	require.NoError(t, err)
	a := kitvec.Generation{Model: "model-a", Dimensions: 2}
	b := kitvec.Generation{Model: "model-b", Dimensions: 2}
	aKey, bKey := a.Fingerprint(), b.Fingerprint()
	vectors := []kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}}
	require.NoError(t, first.EnsureBuilding(ctx, aKey, a))
	require.NoError(t, first.SaveVectorsForTest(ctx, aKey, issue.UID, issue.ContentRevision, vectors))
	require.NoError(t, first.CutOver(ctx, aKey))
	require.NoError(t, first.EnsureBuilding(ctx, bKey, b))
	require.NoError(t, first.SaveVectorsForTest(ctx, bKey, issue.UID, issue.ContentRevision, vectors))
	require.NoError(t, first.CutOver(ctx, bKey))
	require.NoError(t, release())
	release = nil

	release, err = second.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	require.NoError(t, second.EnsureBuilding(ctx, aKey, a))
	require.NoError(t, second.SaveVectorsForTest(ctx, aKey, issue.UID, issue.ContentRevision, vectors))
	require.NoError(t, second.CutOver(ctx, aKey))
	require.NoError(t, release())
	release = nil

	release, err = first.AcquireReconcilerLease(ctx)
	require.NoError(t, err)
	require.NoError(t, first.EnsureBuilding(ctx, bKey, b))
	require.NoError(t, first.SaveVectorsForTest(ctx, bKey, issue.UID, issue.ContentRevision, vectors))
	require.NoError(t, first.CutOver(ctx, bKey))
	var chunks int
	err = store.DB.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_chunks c JOIN issue_vector_generations g ON g.id = c.generation_id WHERE g.gen_key = $1`, aKey).Scan(&chunks)
	require.NoError(t, err)
	require.Zero(t, chunks, "reacquiring the lease must clear prior cleanup results")
}
