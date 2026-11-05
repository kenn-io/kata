package vector

import (
	"context"

	kitvec "go.kenn.io/kit/vector"
)

// This file is compiled only into the test binary. It gives the external
// vector_test package access to package internals. It must not import
// internal/testenv or internal/daemon: those import internal/vector, and an
// in-package test file may not close that cycle.

// ErrReconcilerLeaseNotHeldForTest exposes the sentinel every derived-state
// mutator returns when this index holds no reconciler lease.
var ErrReconcilerLeaseNotHeldForTest = errReconcilerLeaseNotHeld

func (ix *Index) PendingForGenerationForTest(ctx context.Context, gen string, after *string, limit int) ([]kitvec.Pending[string], error) {
	return ix.flowStore.PendingForGeneration(ctx, gen, after, limit)
}

// SaveVectorsForTest reaches the one derived-state mutator that has no
// exported Index method: kit's fill drives it through the flow store.
func (ix *Index) SaveVectorsForTest(
	ctx context.Context,
	gen, doc string,
	revision any,
	vectors []kitvec.ChunkVector,
) error {
	return ix.flowStore.SaveVectors(ctx, gen, doc, revision, vectors)
}

// ClearReconcilerLeaseForTest forgets the recorded lease session without
// releasing its advisory lock or closing its connection, so a test can assert
// that fencing comes from the representation rather than from a terminated
// connection erroring.
func (ix *Index) ClearReconcilerLeaseForTest() {
	conn, err := ix.pg.leaseSession()
	if err != nil {
		return
	}
	ix.pg.releaseLease(conn)
}

// MirrorRowForTest contains the persisted embedding input and its change key.
type MirrorRowForTest struct {
	ProjectUID string
	Content    string
	Revision   int64
}

func (ix *Index) MirrorRowsForTest(ctx context.Context) (map[string]MirrorRowForTest, error) {
	table := "issue_mirror"
	if ix.pg != nil {
		table = "issue_vector_mirror"
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT issue_uid, project_uid, content, content_revision FROM `+table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]MirrorRowForTest)
	for rows.Next() {
		var uid string
		var row MirrorRowForTest
		if err := rows.Scan(&uid, &row.ProjectUID, &row.Content, &row.Revision); err != nil {
			return nil, err
		}
		out[uid] = row
	}
	return out, rows.Err()
}

func (ix *Index) VectorCountForTest(ctx context.Context) (int, error) {
	table := "issue_vectors_chunks"
	if ix.pg != nil {
		table = "issue_vector_chunks"
	}
	var count int
	err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count)
	return count, err
}
