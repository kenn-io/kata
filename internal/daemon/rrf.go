package daemon

import (
	"fmt"
	"sort"

	"go.kenn.io/kata/internal/db"
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
func mergeRRF(lexical, vector []db.SearchCandidate, limit int) []db.SearchCandidate {
	type agg struct {
		issue   db.Issue
		score   float64
		matched map[string]bool
	}
	byID := map[int64]*agg{}
	add := func(leg []db.SearchCandidate) {
		for rank, c := range leg {
			a := byID[c.Issue.ID]
			if a == nil {
				a = &agg{issue: c.Issue, matched: map[string]bool{}}
				byID[c.Issue.ID] = a
			}
			a.score += 1.0 / float64(rrfK+rank+1)
			for _, m := range c.MatchedIn {
				a.matched[m] = true
			}
		}
	}
	add(lexical)
	add(vector)

	out := make([]db.SearchCandidate, 0, len(byID))
	for _, a := range byID {
		matched := make([]string, 0, len(a.matched))
		for m := range a.matched {
			matched = append(matched, m)
		}
		sort.Strings(matched)
		out = append(out, db.SearchCandidate{Issue: a.issue, Score: a.score, MatchedIn: matched})
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
	return out
}
