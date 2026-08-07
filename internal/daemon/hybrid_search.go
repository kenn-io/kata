package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
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
	Labels         []string
	ExcludeLabels  []string
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

// labelCeilingReason labels the one degrade that is not a leg failure. Label
// filters run after the KNN, so a narrow filter can consume the whole
// knnDeepLimit candidate ceiling and leave matching issues ranked beyond it.
// The results returned are real; the caller is told they may be incomplete.
const labelCeilingReason = "label filters exhausted the semantic candidate ceiling; semantic results may be incomplete"

// knnDeepLimit is the candidate depth of the single retry the vector leg makes
// when the first fetchCap-deep KNN batch comes back full but yields too few
// in-project hits (the index is daemon-global, so another project's chunks
// can crowd out the requested project). The vector layer also returns the
// score of one candidate beyond this window as metadata: an exact-size corpus
// or a probe below cosineFloor proves no relevant candidate was hidden by the
// ceiling. Hydration still considers at most knnDeepLimit hits.
const knnDeepLimit = 1000

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
			c, e := store.SearchFTS(ctx, db.SearchFTSParams{
				ProjectID: p.ProjectID, Query: p.Query, Limit: fetch, IncludeDeleted: p.IncludeDeleted,
				Labels: p.Labels, ExcludeLabels: p.ExcludeLabels,
			})
			lexical = c
			lexErrCh <- e
		}()
	} else {
		lexErrCh <- nil
	}

	// Vector leg.
	var vector []db.SearchCandidate
	var vecBounded bool
	var vecErr error
	if mode == modeHybrid || mode == modeSemantic {
		vector, vecBounded, vecErr = runVectorLeg(ctx, store, idx, emb, p, fetch)
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
	if vecBounded && strict {
		return hybridResult{}, &modeError{status: 503, msg: labelCeilingReason}
	}

	var res hybridResult
	switch mode {
	case modeLexical:
		res = hybridResult{Mode: modeLexical, Hits: truncate(lexical, p.Limit)}
	case modeSemantic:
		res = hybridResult{Mode: modeSemantic, Hits: truncate(vector, p.Limit)}
	default: // hybrid
		res = hybridResult{Mode: modeHybrid, Hits: mergeRRF(lexical, vector, p.Limit)}
	}
	// Auto mode may return real but potentially incomplete hits when label
	// filters exhaust the candidate ceiling. Explicit modes were rejected above
	// because their strict contract does not permit degraded results.
	if vecBounded {
		res.Degraded = true
		res.DegradedReason = labelCeilingReason
	}
	return res, nil
}

// runVectorLeg embeds the query, KNN-searches the active generation, rolls
// chunk hits up to issues, and hydrates them against live canonical rows. The
// index is daemon-global while search is project-scoped, so the leg fetches a
// fetchCap raw window, filters by project and liveness afterwards, and retries
// once at knnDeepLimit when that window has more candidates but filters short;
// hydrating against kata.db (not the sidecar) preserves the guarantee that
// soft-deleted or purged issues never leak, whatever the sidecar holds.
//
// The second return reports that label filters consumed the deep candidate
// ceiling, so the leg's short result is a limit of the retrieval depth rather
// than of the corpus.
func runVectorLeg(ctx context.Context, store db.Storage, idx *vector.Index, emb *embedding.Client, p hybridParams, fetch int) ([]db.SearchCandidate, bool, error) {
	if idx == nil {
		return nil, false, errors.New("vector index unavailable")
	}
	key, ok, err := idx.ActiveGeneration(ctx)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, errors.New("no active embedding generation (backfill in progress)")
	}
	// The active generation must match the configured embedder's fingerprint.
	// After a model change the old generation keeps its vectors while the new
	// one backfills; ranking a new-model query vector against old-model stored
	// vectors is meaningless (same dims) or an error (dims change), so the leg
	// is unavailable until cutover.
	if key != emb.Generation().Fingerprint() {
		return nil, false, errors.New("embedding model changed; new index is backfilling")
	}
	ectx, cancel := context.WithTimeout(ctx, queryEmbedTimeout)
	defer cancel()
	vecs, err := emb.Embed(ectx, []string{embedding.EmbedText(p.Query, "")})
	if err != nil {
		return nil, false, err
	}
	query := kitvec.Vector(vecs[0])
	labels := newLabelFilter(p.Labels, p.ExcludeLabels)
	window, err := idx.QueryWithProbe(ctx, key, query, fetchCap)
	if err != nil {
		return nil, false, err
	}
	hits := window.Hits
	out, err := hydrateVectorHits(ctx, store, hits, p, fetch, labels)
	if err != nil {
		return nil, false, err
	}
	// Bounded depth retry: fetchCap returned hits or a raw probe beyond that
	// window means more chunks may exist past the initial boundary. The probe
	// matters on SQLite, where stale vectors consume raw KNN slots before the
	// freshness join removes them. Coming up short after filtering means stale
	// rows, another project's higher-scoring chunks, or non-matching labels may
	// have crowded this project's candidates out. Re-query once at knnDeepLimit
	// and redo the rollup + filter.
	if len(out) < fetch && (len(hits) == fetchCap || window.HasProbe) {
		var boundedRelevant bool
		if labels.empty() {
			hits, err = idx.Query(ctx, key, query, knnDeepLimit)
		} else {
			var deepWindow vector.QueryWindow
			deepWindow, err = idx.QueryWithProbe(ctx, key, query, knnDeepLimit)
			hits = deepWindow.Hits
			boundedRelevant = deepWindow.HasProbe && deepWindow.ProbeScore >= cosineFloor
		}
		if err != nil {
			return nil, false, err
		}
		out, err = hydrateVectorHits(ctx, store, hits, p, fetch, labels)
		if err != nil {
			return nil, false, err
		}
		// Still short of what the caller asked for, off a deep batch the index
		// filled completely and its extra probe remained relevant: matching
		// issues may sit past knnDeepLimit. A missing or below-floor probe makes
		// the short result exact instead.
		if !labels.empty() && boundedRelevant && len(out) < p.Limit {
			return out, true, nil
		}
	}
	return out, false, nil
}

// hydrateVectorHits rolls chunk hits up to issues and hydrates them against
// live canonical rows, filtering by project and labels, stopping at the cosine
// floor or fetch collected candidates. Labels are checked before a candidate
// counts toward fetch, so a filtered leg keeps scanning the batch rather than
// stopping on rows it is about to drop. The lexical leg gets the same
// predicates in SQL; here the KNN has already run, so the check is per
// surviving issue. The vector leg serves live issues only — even
// for include_deleted searches: soft-deleted issues leave the index at the
// next mirror refresh, and this filter enforces the same contract per
// request in the window before that refresh runs (include_deleted ranks
// deleted issues through the lexical leg alone).
func hydrateVectorHits(ctx context.Context, store db.Storage, hits []kitvec.Hit[string], p hybridParams, fetch int, labels labelFilter) ([]db.SearchCandidate, error) {
	hits = kitvec.RollupByDocument(hits)
	candidates := make([]db.SearchCandidate, 0, min(fetch, len(hits)))
	var issueIDs []int64
	if !labels.empty() {
		issueIDs = make([]int64, 0, min(fetch, len(hits)))
	}
	for _, h := range hits {
		if h.Score < cosineFloor {
			break // hits are sorted by score descending
		}
		iss, err := store.IssueByUID(ctx, h.Doc, db.IncludeDeletedNo)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue // soft-deleted or purged since the last mirror refresh
			}
			return nil, err
		}
		if iss.ProjectID != p.ProjectID {
			continue
		}
		candidates = append(candidates, db.SearchCandidate{
			Issue: iss, Score: float64(h.Score), MatchedIn: []string{"semantic"},
		})
		if labels.empty() {
			if len(candidates) == fetch {
				break
			}
			continue
		}
		issueIDs = append(issueIDs, iss.ID)
	}
	if labels.empty() || len(candidates) == 0 {
		return candidates, nil
	}
	byIssue, err := store.LabelsByIssues(ctx, p.ProjectID, issueIDs)
	if err != nil {
		return nil, err
	}
	out := make([]db.SearchCandidate, 0, min(fetch, len(candidates)))
	for _, candidate := range candidates {
		if !labels.matches(byIssue[candidate.Issue.ID]) {
			continue
		}
		out = append(out, candidate)
		if len(out) == fetch {
			break
		}
	}
	return out, nil
}

// labelFilter is a request's label predicates in the canonical lowercase form
// stored labels already use. The zero value matches every issue, so the
// unfiltered vector leg pays nothing for it.
type labelFilter struct {
	require []string
	exclude []string
}

func newLabelFilter(require, exclude []string) labelFilter {
	return labelFilter{require: lowerAll(require), exclude: lowerAll(exclude)}
}

func lowerAll(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = strings.ToLower(l)
	}
	return out
}

func (f labelFilter) empty() bool { return len(f.require) == 0 && len(f.exclude) == 0 }

// matches reports whether an issue carrying the given labels survives the
// filter: every required label present (AND), no excluded label present.
func (f labelFilter) matches(labels []string) bool {
	for _, l := range f.require {
		if !slices.Contains(labels, l) {
			return false
		}
	}
	for _, l := range f.exclude {
		if slices.Contains(labels, l) {
			return false
		}
	}
	return true
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
