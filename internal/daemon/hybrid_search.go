package daemon

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

type hybridParams struct {
	ProjectID      int64
	Query          string
	Limit          int
	IncludeDeleted bool
	Requested      string // raw mode param
}

type hybridResult struct {
	Mode           searchMode
	Degraded       bool
	DegradedReason string
	Hits           []db.SearchCandidate
}

// queryEmbedTimeout bounds the single query-embed call so a hung embedder can
// never stall the request past a few seconds; the lexical leg, running
// concurrently, is unaffected.
const queryEmbedTimeout = 3 * time.Second

// fetchFloor and fetchCap bound the per-leg candidate depth. Each leg fetches
// max(limit*3, fetchFloor) rows (capped at fetchCap) so RRF has enough overlap
// to fuse before the final truncation to limit.
const (
	fetchFloor = 50
	fetchCap   = 200
)

// cosineFloor drops weak vector hits so they do not pad results. Vectors are
// L2-normalized, so the dot product is cosine similarity in [-1, 1].
const cosineFloor = 0.3

// hybridSearch runs the lexical and (when applicable) vector legs and merges
// them. The lexical leg never waits on the embedder. A vector-leg failure in
// an auto/hybrid request degrades to lexical with a reason; an explicit
// hybrid/semantic request that cannot run returns a *modeError for the handler
// to map to 400 (unconfigured) or 503 (leg failure).
func hybridSearch(ctx context.Context, store db.Storage, idx *vector.Index, emb *embedding.Client, p hybridParams) (hybridResult, error) {
	configured := emb != nil && idx != nil
	mode, err := resolveMode(p.Requested, configured)
	if err != nil {
		return hybridResult{}, &modeError{status: 400, msg: err.Error()}
	}
	// strict = the caller explicitly asked for a leg that must run; a failure
	// is 503, not a silent degrade. auto-resolved hybrid is not strict.
	strict := p.Requested == "hybrid" || p.Requested == "semantic"

	fetch := p.Limit * 3
	if fetch < fetchFloor {
		fetch = fetchFloor
	}
	if fetch > fetchCap {
		fetch = fetchCap
	}

	// Lexical leg (skip for explicit semantic). It runs in a goroutine so the
	// vector leg's embed round-trip never blocks FTS.
	var lexical []db.SearchCandidate
	lexErrCh := make(chan error, 1)
	if mode != modeSemantic {
		go func() {
			c, e := store.SearchFTS(ctx, p.ProjectID, p.Query, fetch, p.IncludeDeleted)
			lexical = c
			lexErrCh <- e
		}()
	} else {
		lexErrCh <- nil
	}

	// Vector leg.
	var vector []db.SearchCandidate
	var vecErr error
	if mode == modeHybrid || mode == modeSemantic {
		vector, vecErr = runVectorLeg(ctx, store, idx, emb, p, fetch)
	}

	if e := <-lexErrCh; e != nil {
		return hybridResult{}, &modeError{status: 500, msg: e.Error()}
	}

	// Handle vector-leg failure. Explicit hybrid/semantic → 503; auto → degrade
	// to lexical, labeled (keeps "silent-but-labeled" honest).
	if vecErr != nil {
		if strict {
			return hybridResult{}, &modeError{status: 503, msg: vecErr.Error()}
		}
		return hybridResult{
			Mode: modeLexical, Degraded: true, DegradedReason: vecErr.Error(),
			Hits: truncate(lexical, p.Limit),
		}, nil
	}

	switch mode {
	case modeLexical:
		return hybridResult{Mode: modeLexical, Hits: truncate(lexical, p.Limit)}, nil
	case modeSemantic:
		return hybridResult{Mode: modeSemantic, Hits: truncate(vector, p.Limit)}, nil
	default: // hybrid
		return hybridResult{Mode: modeHybrid, Hits: mergeRRF(lexical, vector, p.Limit)}, nil
	}
}

// runVectorLeg embeds the query, KNN-searches the active generation, rolls
// chunk hits up to issues, and hydrates them against live canonical rows. The
// index is daemon-global while search is project-scoped, so the leg fetches
// fetchCap candidates and filters by project and liveness afterwards;
// hydrating against kata.db (not the sidecar) preserves the guarantee that
// soft-deleted or purged issues never leak, whatever the sidecar holds.
func runVectorLeg(ctx context.Context, store db.Storage, idx *vector.Index, emb *embedding.Client, p hybridParams, fetch int) ([]db.SearchCandidate, error) {
	if idx == nil {
		return nil, errors.New("vector index unavailable")
	}
	key, ok, err := idx.ActiveGeneration(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("no active embedding generation (backfill in progress)")
	}
	ectx, cancel := context.WithTimeout(ctx, queryEmbedTimeout)
	defer cancel()
	vecs, err := emb.Embed(ectx, []string{embedding.EmbedText(p.Query, "")})
	if err != nil {
		return nil, err
	}
	hits, err := idx.Query(ctx, key, kitvec.Vector(vecs[0]), fetchCap)
	if err != nil {
		return nil, err
	}
	hits = kitvec.RollupByDocument(hits)

	include := db.IncludeDeletedNo
	if p.IncludeDeleted {
		include = db.IncludeDeletedYes
	}
	out := make([]db.SearchCandidate, 0, fetch)
	for _, h := range hits {
		if h.Score < cosineFloor {
			break // hits are sorted by score descending
		}
		iss, err := store.IssueByUID(ctx, h.Doc, include)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue // purged since the last mirror refresh
			}
			return nil, err
		}
		if iss.ProjectID != p.ProjectID {
			continue
		}
		if iss.DeletedAt != nil && !p.IncludeDeleted {
			continue
		}
		out = append(out, db.SearchCandidate{
			Issue: iss, Score: float64(h.Score), MatchedIn: []string{"semantic"},
		})
		if len(out) == fetch {
			break
		}
	}
	return out, nil
}

func truncate(c []db.SearchCandidate, limit int) []db.SearchCandidate {
	if limit > 0 && len(c) > limit {
		return c[:limit]
	}
	return c
}

// modeError carries an HTTP status for the handler to translate.
type modeError struct {
	status int
	msg    string
}

func (e *modeError) Error() string { return e.msg }
func (e *modeError) Status() int   { return e.status }
