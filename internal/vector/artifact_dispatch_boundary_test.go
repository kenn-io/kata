package vector

import (
	"context"

	kitvec "go.kenn.io/kit/vector"
)

func SetArtifactArrivalBoundaryForTest(ix *Index, arrive func(), scan func(*string)) {
	ix.flowStore = artifactArrivalStore{Store: ix.flowStore, arrive: arrive, scan: scan}
}

// A deterministic incoming-artifact transaction at the real pending-read
// boundary; provider requests and both native indexes remain real.
type artifactArrivalStore struct {
	kitvec.Store[string, string]
	arrive func()
	scan   func(*string)
}

func (s artifactArrivalStore) PendingForGeneration(ctx context.Context, key string, after *string, limit int) ([]kitvec.Pending[string], error) {
	pending, err := s.Store.PendingForGeneration(ctx, key, after, limit)
	if err == nil {
		s.scan(after)
		if len(pending) > 0 {
			s.arrive()
		}
	}
	return pending, err
}
