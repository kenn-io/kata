package vector

import (
	"context"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
)

// QueryWindow is a bounded candidate page plus the score of the next raw KNN
// candidate when one exists. SQLite's probe is read before freshness joins, so
// a stale row cannot make a still-full raw window look exhausted. PostgreSQL's
// KNN query applies those joins before LIMIT, making its probe eligible by
// construction. Hits never contains more than the requested limit; the probe
// is metadata only.
type QueryWindow struct {
	Hits       []kitvec.Hit[string]
	ProbeScore float32
	HasProbe   bool
}

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

// QueryWithProbe returns at most limit hits and, when the candidate window is
// not exhausted, the score immediately beyond its raw KNN boundary.
// PostgreSQL applies freshness joins before LIMIT, so one extra ordinary hit
// is the probe. SQLite's vec0 KNN must apply LIMIT before those joins;
// querySQLiteProbe reads the raw slot directly so filtered stale rows cannot
// hide exhaustion state.
func (ix *Index) QueryWithProbe(ctx context.Context, key string, query kitvec.Vector, limit int) (QueryWindow, error) {
	if limit <= 0 {
		return QueryWindow{}, nil
	}
	if ix.pg != nil {
		hits, err := ix.Query(ctx, key, query, limit+1)
		if err != nil {
			return QueryWindow{}, err
		}
		window := QueryWindow{Hits: hits}
		if len(hits) > limit {
			window.ProbeScore = hits[limit].Score
			window.HasProbe = true
			window.Hits = hits[:limit]
		}
		return window, nil
	}

	hits, err := ix.Query(ctx, key, query, limit)
	if err != nil {
		return QueryWindow{}, err
	}
	probeScore, hasProbe, err := ix.querySQLiteProbe(ctx, key, query, limit)
	if err != nil {
		return QueryWindow{}, err
	}
	return QueryWindow{Hits: hits, ProbeScore: probeScore, HasProbe: hasProbe}, nil
}

func (ix *Index) querySQLiteProbe(ctx context.Context, key string, query kitvec.Vector, offset int) (float32, bool, error) {
	var ordinal int64
	var dimensions int
	if err := ix.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT ordinal, dimension FROM %s_generations WHERE gen_key = ?`, vectorsPrefix), key,
	).Scan(&ordinal, &dimensions); err != nil {
		return 0, false, fmt.Errorf("vector: lookup probe generation %s: %w", key, err)
	}
	if len(query) != dimensions {
		return 0, false, fmt.Errorf("vector: probe query has %d dimensions, generation expects %d", len(query), dimensions)
	}
	expr, value, err := sidecarVectorValue(query)
	if err != nil {
		return 0, false, fmt.Errorf("vector: serialize probe query: %w", err)
	}
	rows, err := ix.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT 1 - distance
		  FROM %s_v%d
		 WHERE embedding MATCH %s AND k = ?
		 ORDER BY distance`, vectorsPrefix, ordinal, expr), value, offset+1)
	if err != nil {
		return 0, false, fmt.Errorf("vector: query probe generation %s: %w", key, err)
	}
	defer func() { _ = rows.Close() }()
	for i := 0; rows.Next(); i++ {
		var score float64
		if err := rows.Scan(&score); err != nil {
			return 0, false, fmt.Errorf("vector: scan probe generation %s: %w", key, err)
		}
		if i == offset {
			return float32(score), true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("vector: read probe generation %s: %w", key, err)
	}
	return 0, false, nil
}
