package vector

import (
	"context"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
)

// Query runs cosine KNN against a single generation and returns chunk-level
// hits (callers roll up with kitvec.RollupByDocument). It deliberately does
// not use kitvec.Search: kata serves exactly one generation (the building one
// must not answer mid-fill) and embeds the query once under a tight timeout.
func (ix *Index) Query(ctx context.Context, key string, query kitvec.Vector, limit int) ([]kitvec.Hit[string], error) {
	hits, err := ix.flowStore.QueryGeneration(ctx, key, query, limit)
	if err != nil {
		return nil, fmt.Errorf("vector: query generation %s: %w", key, err)
	}
	return hits, nil
}
