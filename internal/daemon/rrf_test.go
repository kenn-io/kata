package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func cand(id int64, score float64, matched ...string) db.SearchCandidate {
	return db.SearchCandidate{Issue: db.Issue{ID: id}, Score: score, MatchedIn: matched}
}

func TestMergeRRFCombinesAndDedupes(t *testing.T) {
	lex := []db.SearchCandidate{cand(1, 5, "title"), cand(2, 4, "body")}
	vec := []db.SearchCandidate{cand(2, 0.9, "semantic"), cand(3, 0.8, "semantic")}
	merged := mergeRRF(lex, vec, 10)

	// Issue 2 appears in both legs → ranks first.
	if merged[0].Issue.ID != 2 {
		t.Fatalf("expected issue 2 first, got %d", merged[0].Issue.ID)
	}
	// matched_in for issue 2 is the union.
	if !contains(merged[0].MatchedIn, "body") || !contains(merged[0].MatchedIn, "semantic") {
		t.Fatalf("matched_in not unioned: %v", merged[0].MatchedIn)
	}
	if len(merged) != 3 {
		t.Fatalf("expected 3 unique issues, got %d", len(merged))
	}
}

func TestMergeRRFEmptyLegs(t *testing.T) {
	if got := mergeRRF(nil, nil, 10); len(got) != 0 {
		t.Fatalf("empty legs should yield empty, got %d", len(got))
	}
	lex := []db.SearchCandidate{cand(1, 5, "title")}
	if got := mergeRRF(lex, nil, 10); len(got) != 1 || got[0].Issue.ID != 1 {
		t.Fatalf("lexical-only passthrough failed: %#v", got)
	}
}

func TestMergeRRFRespectsLimit(t *testing.T) {
	lex := []db.SearchCandidate{cand(1, 5, "title"), cand(2, 4, "body")}
	vec := []db.SearchCandidate{cand(2, 0.9, "semantic"), cand(3, 0.8, "semantic")}
	merged := mergeRRF(lex, vec, 2)

	require.Len(t, merged, 2)
	// Issue 2 is in both legs (highest RRF), so it survives the truncation; the
	// next slot goes to the higher-ranked of the remaining singletons (issue 1
	// at lexical rank 0 outscores issue 3 at vector rank 1).
	require.Equal(t, int64(2), merged[0].Issue.ID)
	require.Equal(t, int64(1), merged[1].Issue.ID)
}

func TestMergeRRFTieBreaksByLowerIssueID(t *testing.T) {
	// Issue 7 appears only in lexical at rank 0; issue 3 only in vector at rank
	// 0. Both score 1/(60+0+1), an exact RRF tie. The deterministic tie-break
	// is ascending issue id, so issue 3 must sort first even though issue 7 was
	// added to the lexical leg first. This fails if the id tie-break is dropped
	// or reversed.
	lex := []db.SearchCandidate{cand(7, 5, "title")}
	vec := []db.SearchCandidate{cand(3, 0.9, "semantic")}
	merged := mergeRRF(lex, vec, 10)

	require.Len(t, merged, 2)
	require.InDelta(t, merged[0].Score, merged[1].Score, 1e-12, "scores must be an exact RRF tie")
	require.Equalf(t, int64(3), merged[0].Issue.ID, "tie must break to the lower issue id")
	require.Equal(t, int64(7), merged[1].Issue.ID)
}

func TestMergeRRFTieBreaksByMostRecentlyUpdated(t *testing.T) {
	// Both issues score 1/(60+0+1): older only in lexical at rank 0, newer only
	// in vector at rank 0. The design-note tiebreak is updated_at desc, so the
	// newer issue must sort first even though it has the higher id (which would
	// lose under the id-asc final tiebreak). Fails if updated_at is ignored.
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	lex := []db.SearchCandidate{{Issue: db.Issue{ID: 1, UpdatedAt: older}, MatchedIn: []string{"title"}}}
	vec := []db.SearchCandidate{{Issue: db.Issue{ID: 2, UpdatedAt: newer}, MatchedIn: []string{"semantic"}}}
	merged := mergeRRF(lex, vec, 10)

	require.Len(t, merged, 2)
	require.InDelta(t, merged[0].Score, merged[1].Score, 1e-12, "scores must be an exact RRF tie")
	require.Equalf(t, int64(2), merged[0].Issue.ID, "more-recently-updated issue must sort first")
	require.Equal(t, int64(1), merged[1].Issue.ID)
}

func TestResolveMode(t *testing.T) {
	cases := []struct {
		req        string
		configured bool
		want       searchMode
		wantErr    bool
	}{
		{"", false, modeLexical, false},
		{"", true, modeHybrid, false},
		{"auto", true, modeHybrid, false},
		{"lexical", false, modeLexical, false},
		{"hybrid", false, modeLexical, true},   // 400
		{"semantic", false, modeLexical, true}, // 400
		{"hybrid", true, modeHybrid, false},
		{"semantic", true, modeSemantic, false},
		{"bogus", true, modeLexical, true},
	}
	for _, tc := range cases {
		got, err := resolveMode(tc.req, tc.configured)
		if (err != nil) != tc.wantErr {
			t.Fatalf("resolveMode(%q,%v) err=%v wantErr=%v", tc.req, tc.configured, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Fatalf("resolveMode(%q,%v)=%v want %v", tc.req, tc.configured, got, tc.want)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
