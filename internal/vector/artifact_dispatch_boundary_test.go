package vector

import (
	"context"

	kitvec "go.kenn.io/kit/vector"
)

func SetArtifactArrivalBoundaryForTest(ix *Index, arrive func()) {
	ix.flowStore = artifactArrivalStore{Store: ix.flowStore, arrive: arrive}
}

// A deterministic incoming-artifact transaction at the real pending-read
// boundary; provider requests and both native indexes remain real.
type artifactArrivalStore struct {
	kitvec.Store[string, string]
	arrive func()
}

func (s artifactArrivalStore) PendingForGeneration(ctx context.Context, key string, limit int) ([]kitvec.Pending[string], error) {
	pending, err := s.Store.PendingForGeneration(ctx, key, limit)
	if err == nil && len(pending) > 0 {
		s.arrive()
	}
	return pending, err
}
