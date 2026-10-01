package daemon

import (
	"fmt"
	"sort"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/rrf"
)

type searchMode string

const (
	modeLexical  searchMode = "lexical"
	modeHybrid   searchMode = "hybrid"
	modeSemantic searchMode = "semantic"
)

const rrfK = 60

// resolveMode maps a requested mode string and whether embeddings are
// configured to the effective mode. An explicit hybrid/semantic request that
// cannot be served (unconfigured) returns an error so the handler can reply
// 400; "auto"/"" silently resolves to hybrid-when-configured, else lexical.
func resolveMode(requested string, configured bool) (searchMode, error) {
	switch requested {
	case "", "auto":
		if configured {
			return modeHybrid, nil
		}
		return modeLexical, nil
	case "lexical":
		return modeLexical, nil
	case "hybrid", "semantic":
		if !configured {
			return modeLexical, fmt.Errorf("mode %q requires [search.embeddings] to be configured", requested)
		}
		return searchMode(requested), nil
	default:
		return modeLexical, fmt.Errorf("unknown mode %q (want auto|lexical|hybrid|semantic)", requested)
	}
}

// mergeRRF fuses two ranked legs with reciprocal rank fusion (k=60, equal
// weights), deduping by issue id and unioning matched_in. Ties break by RRF
// score desc, then updated_at desc, then issue id asc. The resulting Score is
// the RRF score.
func mergeRRF(lexical, vector []db.SearchCandidate, limit int) ([]db.SearchCandidate, error) {
	type agg struct {
		issue   db.Issue
		matched map[string]bool
	}
	byID := map[int64]agg{}
	add := func(leg []db.SearchCandidate) []int64 {
		keys := make([]int64, len(leg))
		for position, c := range leg {
			keys[position] = c.Issue.ID
			a, exists := byID[c.Issue.ID]
			if !exists {
				a = agg{issue: c.Issue, matched: map[string]bool{}}
				byID[c.Issue.ID] = a
			}
			for _, m := range c.MatchedIn {
				a.matched[m] = true
			}
		}
		return keys
	}
	hits, err := rrf.Fuse(rrfK, []rrf.Leg[int64]{
		{Name: "lexical", Weight: 1, Keys: add(lexical)},
		{Name: "semantic", Weight: 1, Keys: add(vector)},
	})
	if err != nil {
		return nil, err
	}

	out := make([]db.SearchCandidate, 0, len(byID))
	for _, hit := range hits {
		a := byID[hit.Key]
		matched := make([]string, 0, len(a.matched))
		for m := range a.matched {
			matched = append(matched, m)
		}
		sort.Strings(matched)
		out = append(out, db.SearchCandidate{Issue: a.issue, Score: hit.Score, MatchedIn: matched})
	}
	// Deterministic order: RRF score desc, then most-recently-updated first,
	// then issue id asc as the final tiebreak (matches the design note).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if !out[i].Issue.UpdatedAt.Equal(out[j].Issue.UpdatedAt) {
			return out[i].Issue.UpdatedAt.After(out[j].Issue.UpdatedAt)
		}
		return out[i].Issue.ID < out[j].Issue.ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
