package vector

import (
	"context"
	"database/sql"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
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
	if ix.pg != nil {
		return ix.pg.QueryGeneration(ctx, key, query, limit)
	}
	if limit <= 0 {
		return nil, nil
	}
	candidates, err := ix.store.BuildCandidateQuery(ctx, ix.db, key, query, sqlitevec.CandidateQuery{CandidateLimit: limit})
	if err != nil {
		return nil, fmt.Errorf("vector: query generation %s: %w", key, err)
	}
	return candidates.All(ctx, ix.db, func(rows *sql.Rows) (kitvec.Hit[string], error) {
		var hit kitvec.Hit[string]
		var distance float64
		if err := rows.Scan(&hit.Doc, &hit.ChunkIndex, &hit.Revision, &distance); err != nil {
			return hit, err
		}
		score, err := sqlitevec.ScoreFromDistance(distance)
		if err != nil {
			return hit, err
		}
		hit.Score = score
		return hit, nil
	})
}

// QueryWithProbe returns at most limit hits and, when the candidate window is
// not exhausted, the score immediately beyond its raw KNN boundary.
// PostgreSQL applies freshness joins before LIMIT, so one extra ordinary hit
// is the probe. SQLite's vec0 KNN must apply LIMIT before those joins;
// Kit reads the raw slot directly so filtered stale rows cannot
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

	window, err := ix.store.QueryGenerationWindow(ctx, key, query, limit)
	if err != nil {
		return QueryWindow{}, err
	}
	return QueryWindow{Hits: window.Hits, ProbeScore: window.ProbeScore, HasProbe: window.HasProbe}, nil
}
