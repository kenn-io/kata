package daemon_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

type sharedChoiceEmbedder struct{}

func (sharedChoiceEmbedder) Generation() kitvec.Generation {
	return kitvec.Generation{Model: "built-in", Dimensions: 2}
}
func (sharedChoiceEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i := range vectors {
			vectors[i] = []float32{1, 0}
		}
		return vectors, nil
	}
}

func TestReconcilerPreservesActiveGenerationAfterReacquiringLease(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pgvector testcontainer")
	}
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, t.Context())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	idx, err := vector.OpenPostgres(t.Context(), store.DB)
	require.NoError(t, err)
	emb := sharedChoiceEmbedder{}
	r := daemon.NewReconciler(store, idx, emb, daemon.ReconcilerConfig{PreserveActiveGeneration: true, MinBackoff: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("reconciler did not stop")
			}
		}
	})
	require.Eventually(t, func() bool {
		key, ok, err := idx.ActiveGeneration(t.Context())
		return err == nil && ok && key == emb.Generation().Fingerprint()
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		stopped = true
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("reconciler did not stop")
	}

	other := kitvec.Generation{Model: "external", Dimensions: 2}
	otherKey := other.Fingerprint()
	release, err := idx.AcquireReconcilerLease(t.Context())
	require.NoError(t, err)
	require.NoError(t, idx.EnsureBuilding(t.Context(), otherKey, other))
	require.NoError(t, idx.CutOver(t.Context(), otherKey))
	require.NoError(t, release())
	ctx2, cancel2 := context.WithCancel(t.Context())
	done2 := make(chan error, 1)
	go func() { done2 <- r.Run(ctx2) }()
	t.Cleanup(func() {
		cancel2()
		select {
		case <-done2:
		case <-time.After(5 * time.Second):
			t.Error("reconciler did not stop")
		}
	})
	require.Never(t, func() bool {
		active, ok, err := idx.ActiveGeneration(t.Context())
		return err != nil || !ok || active != otherKey
	}, 2*time.Second, 10*time.Millisecond)
}
