package vector_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// One elected reconciler per database/schema mutates derived state. An index
// that never acquired the lease must not be able to write at all: without
// this, two daemons in the unleased state would both write to the same
// pgvector tables.
func TestPostgresMutatorsRequireTheReconcilerLease(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	idx, err := vector.OpenPostgres(ctx, store.DB)
	require.NoError(t, err)

	mutators := []struct {
		name string
		call func() error
	}{
		{"EnsureBuilding", func() error {
			return idx.EnsureBuilding(ctx, "unleased",
				kitvec.Generation{Model: "example", Dimensions: 2})
		}},
		{"CutOver", func() error {
			return idx.CutOver(ctx, "unleased")
		}},
		{"RefreshMirror", func() error {
			_, err := idx.RefreshMirror(ctx, store)
			return err
		}},
		{"SaveVectors", func() error {
			return idx.SaveVectorsForTest(ctx, "unleased", "01KSD7P0000000000000000001", int64(1),
				[]kitvec.ChunkVector{{ChunkIndex: 0, Vector: kitvec.Vector{1, 0}}})
		}},
	}
	for _, mutator := range mutators {
		t.Run(mutator.name, func(t *testing.T) {
			require.ErrorIs(t, mutator.call(), vector.ErrReconcilerLeaseNotHeldForTest)
		})
	}

	var generations, mirrorRows int
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT count(*) FROM issue_vector_generations`).Scan(&generations))
	assert.Zero(t, generations, "an unleased index must not have registered a generation")
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT count(*) FROM issue_vector_mirror`).Scan(&mirrorRows))
	assert.Zero(t, mirrorRows, "an unleased index must not have written mirror rows")
}
