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
