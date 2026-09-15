package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"
)

func createSearchStatusIssue(ctx context.Context, t *testing.T, store db.Storage, projectID int64, title, status string, labels ...string) db.Issue {
	t.Helper()
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectID, Title: title, Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range labels {
		if _, err := store.AddLabel(ctx, issue.ID, label, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if status == "closed" {
		issue, _, _, err = store.CloseIssue(ctx, issue.ID, "done", "a", "", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	return issue
}

// Neither close nor reopen refreshes the sidecar. Status must come from the
// hydrated canonical issue, including when both search legs contribute.
func TestSearchStatusTracksCloseAndReopenWithoutReembedding(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	issue := createSearchStatusIssue(ctx, t, store, project.ID, "login race", "open")
	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)
	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

	for _, state := range []string{"open", "closed", "reopened"} {
		t.Run(state, func(t *testing.T) {
			current := state
			switch state {
			case "closed":
				if _, _, changed, err := store.CloseIssue(ctx, issue.ID, "done", "a", "", nil); err != nil || !changed {
					t.Fatalf("close: changed=%v, err=%v", changed, err)
				}
			case "reopened":
				current = "open"
				if _, _, changed, err := store.ReopenIssue(ctx, issue.ID, "a"); err != nil || !changed {
					t.Fatalf("reopen: changed=%v, err=%v", changed, err)
				}
			}
			for _, mode := range []string{"semantic", "hybrid", "lexical", "auto"} {
				for _, status := range []string{"open", "closed", ""} {
					res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
						ProjectID: project.ID, Query: "login race", Limit: 10, Requested: mode, Status: status,
					})
					if err != nil {
						t.Fatalf("mode=%s status=%q: %v", mode, status, err)
					}
					want := 0
					if status == "" || status == current {
						want = 1
					}
					if len(res.Hits) != want || res.Degraded {
						t.Fatalf("mode=%s status=%q: want %d exact hits, got %#v", mode, status, want, res)
					}
					if want == 1 && (res.Hits[0].Issue.UID != issue.UID || res.Hits[0].Issue.Status != current) {
						t.Fatalf("mode=%s status=%q: canonical issue = %#v", mode, status, res.Hits[0].Issue)
					}
					if want == 1 && mode == "hybrid" && (!slices.Contains(res.Hits[0].MatchedIn, "semantic") || !slices.Contains(res.Hits[0].MatchedIn, "title")) {
						t.Fatalf("both legs must contribute: matched_in=%v", res.Hits[0].MatchedIn)
					}
				}
			}
		})
	}
}

func TestSearchStatusIntersectsLabelsProjectAndDeletion(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := store.CreateProject(ctx, "hub-project")
	if err != nil {
		t.Fatal(err)
	}
	open := createSearchStatusIssue(ctx, t, store, project.ID, "login race open", "open", "bug", "urgent")
	closed := createSearchStatusIssue(ctx, t, store, project.ID, "login race closed", "closed", "bug", "urgent")
	createSearchStatusIssue(ctx, t, store, project.ID, "login race missing label", "open", "bug")
	createSearchStatusIssue(ctx, t, store, project.ID, "login race excluded label", "open", "bug", "urgent", "blocked")
	createSearchStatusIssue(ctx, t, store, sibling.ID, "login race sibling", "open", "bug", "urgent")
	deleted := createSearchStatusIssue(ctx, t, store, project.ID, "login race deleted", "open", "bug", "urgent")
	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)
	if _, _, _, err := store.SoftDeleteIssue(ctx, deleted.ID, "a"); err != nil {
		t.Fatal(err)
	}
	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})
	for _, mode := range []string{"semantic", "hybrid", "lexical", "auto"} {
		for _, tc := range []struct {
			status string
			want   []string
		}{
			{status: "open", want: []string{open.UID}},
			{status: "closed", want: []string{closed.UID}},
			{status: "", want: []string{open.UID, closed.UID}},
		} {
			res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
				ProjectID: project.ID, Query: "login race", Limit: 10, Requested: mode, Status: tc.status,
				Labels: []string{"BUG", "urgent"}, ExcludeLabels: []string{"BLOCKED"},
			})
			if err != nil {
				t.Fatalf("mode=%s status=%q: %v", mode, tc.status, err)
			}
			if len(res.Hits) != len(tc.want) || res.Degraded {
				t.Fatalf("mode=%s status=%q: want %v, got %#v", mode, tc.status, tc.want, res)
			}
			for _, hit := range res.Hits {
				if !slices.Contains(tc.want, hit.Issue.UID) {
					t.Fatalf("mode=%s status=%q: unexpected issue %q", mode, tc.status, hit.Issue.Title)
				}
			}
		}
	}
	res, err := hybridSearch(ctx, store, idx, failingEmbedClient(t, http.StatusServiceUnavailable), hybridParams{
		ProjectID: project.ID, Query: "login race", Limit: 10, Requested: "auto", Status: "open",
		Labels: []string{"bug", "urgent"}, ExcludeLabels: []string{"blocked"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != modeLexical || !res.Degraded || res.DegradedReason == "" || len(res.Hits) != 1 || res.Hits[0].Issue.UID != open.UID {
		t.Fatalf("auto fallback must preserve status and label predicates: %#v", res)
	}
}

func TestVectorLegStatusFilterRetriesPastClosedAndStaleCandidates(t *testing.T) {
	for _, staleWindow := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale_window_%v", staleWindow), func(t *testing.T) {
			ctx := context.Background()
			store := newReconcilerTestStore(t)
			project, err := store.CreateProject(ctx, "spoke-project")
			if err != nil {
				t.Fatal(err)
			}
			target := createSearchStatusIssue(ctx, t, store, project.ID, "target login race", "open")
			count := fetchCap + 1
			var stale db.Issue
			if staleWindow {
				count = fetchCap - 1
				stale = createSearchStatusIssue(ctx, t, store, project.ID, "stale candidate", "closed")
			}
			for i := range count {
				createSearchStatusIssue(ctx, t, store, project.ID, fmt.Sprintf("distractor %d", i), "closed")
			}
			idx := openTestVectorIndex(t)
			fillGeneration(ctx, t, store, idx, labelAxisEmbedClient(t))
			if staleWindow {
				updatedTitle := "edited candidate"
				if _, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: stale.ID, Title: &updatedTitle, Actor: "a"}); err != nil || !changed {
					t.Fatalf("stale fixture edit: changed=%v, err=%v", changed, err)
				}
				if _, err := idx.RefreshMirror(ctx, store); err != nil {
					t.Fatal(err)
				}
			}
			res, err := hybridSearch(ctx, store, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}), hybridParams{
				ProjectID: project.ID, Query: "semantic query", Limit: 10, Requested: "semantic", Status: "open",
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Hits) != 1 || res.Hits[0].Issue.UID != target.UID || res.Degraded {
				t.Fatalf("deep retry must recover the open issue after closed candidates: %#v", res)
			}
		})
	}
}

func TestSearchStatusCeilingRequiresRelevantProbe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		vec   []float32
	}{
		{name: "exact ceiling", count: knnDeepLimit - ceilingSurvivors, vec: []float32{0.9, 0.43589, 0, 0}},
		{name: "below floor probe", count: knnDeepLimit + 1, vec: []float32{0.2, 0.9799, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newReconcilerTestStore(t)
			project, err := store.CreateProject(ctx, "spoke-project")
			if err != nil {
				t.Fatal(err)
			}
			for i := range ceilingSurvivors {
				createSearchStatusIssue(ctx, t, store, project.ID, fmt.Sprintf("open login race %d", i), "open")
			}
			for i := range tc.count {
				createSearchStatusIssue(ctx, t, store, project.ID, fmt.Sprintf("distractor %d", i), "closed")
			}
			idx := openTestVectorIndex(t)
			fillGeneration(ctx, t, store, idx, mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
				if strings.HasPrefix(text, "distractor") {
					return tc.vec
				}
				return []float32{1, 0, 0, 0}
			}))
			for _, mode := range []string{"semantic", "hybrid", "auto"} {
				res, err := hybridSearch(ctx, store, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}), hybridParams{
					ProjectID: project.ID, Query: "login race", Limit: 10, Requested: mode, Status: "open",
				})
				if err != nil {
					t.Fatalf("%s has no relevant unseen candidate: %v", mode, err)
				}
				if res.Degraded || len(res.Hits) != ceilingSurvivors {
					t.Fatalf("%s: want %d exact open hits, got %#v", mode, ceilingSurvivors, res)
				}
			}
		})
	}
}

// The lexical-only hits fill a hybrid page. They cannot establish that the
// bounded semantic leg found enough candidates to honor an explicit mode.
func TestSearchStatusCeilingHonorsModeStrictnessWithFullLexicalPage(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	for i := range ceilingSurvivors {
		createSearchStatusIssue(ctx, t, store, project.ID, fmt.Sprintf("open login race %d", i), "open")
	}
	for i := range knnDeepLimit {
		createSearchStatusIssue(ctx, t, store, project.ID, fmt.Sprintf("distractor %d", i), "closed")
	}
	beyond := createSearchStatusIssue(ctx, t, store, project.ID, "target semantic candidate", "open")
	for i := range 10 - ceilingSurvivors {
		createSearchStatusIssue(ctx, t, store, project.ID, fmt.Sprintf("lexical login race %d", i), "open")
	}
	idx := openTestVectorIndex(t)
	fillGeneration(ctx, t, store, idx, mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
		switch {
		case strings.HasPrefix(text, "distractor"):
			return []float32{0.9, 0.43589, 0, 0}
		case strings.HasPrefix(text, "target"):
			return []float32{0.6, 0.8, 0, 0}
		case strings.HasPrefix(text, "lexical"):
			return []float32{0.2, 0.9799, 0, 0}
		default:
			return []float32{1, 0, 0, 0}
		}
	}))
	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})
	for _, mode := range []string{"semantic", "hybrid"} {
		_, err := hybridSearch(ctx, store, idx, emb, hybridParams{
			ProjectID: project.ID, Query: "login race", Limit: 10, Requested: mode, Status: "open",
		})
		var me *modeError
		if !errors.As(err, &me) || me.Status() != http.StatusServiceUnavailable {
			t.Fatalf("explicit %s with bounded status retrieval must return 503, got %v", mode, err)
		}
	}
	for _, mode := range []string{"auto", "lexical"} {
		res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
			ProjectID: project.ID, Query: "login race", Limit: 10, Requested: mode, Status: "open",
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Hits) != 10 || res.Degraded != (mode == "auto") {
			t.Fatalf("%s: want full page with degradation only for auto, got %#v", mode, res)
		}
		if mode == "auto" && (res.Mode != modeHybrid || !strings.Contains(res.DegradedReason, "semantic candidate ceiling")) {
			t.Fatalf("auto must retain hybrid mode and explain bounded retrieval: %#v", res)
		}
		for _, hit := range res.Hits {
			if hit.Issue.Status != "open" || hit.Issue.UID == beyond.UID {
				t.Fatalf("%s returned a closed or unreachable issue: %#v", mode, hit)
			}
		}
	}
	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: project.ID, Query: "login race", Limit: ceilingSurvivors - 1, Requested: "semantic", Status: "open",
	})
	if err != nil {
		t.Fatalf("a full semantic page must succeed: %v", err)
	}
	if res.Degraded || len(res.Hits) != ceilingSurvivors-1 {
		t.Fatalf("semantic survivors fill the requested page: %#v", res)
	}
	res, err = hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: project.ID, Query: "login race", Limit: 10, Requested: "semantic",
	})
	if err != nil {
		t.Fatalf("unfiltered compatibility: %v", err)
	}
	if res.Degraded || len(res.Hits) != 10 {
		t.Fatalf("omitted status must return an undegraded mixed-status page: %#v", res)
	}
	if !slices.ContainsFunc(res.Hits, func(hit db.SearchCandidate) bool { return hit.Issue.Status == "closed" }) {
		t.Fatalf("omitted status must retain closed candidates: %q", hitTitles(res.Hits))
	}
}

func TestSearchStatusCeilingProbeSurvivesStaleSQLiteVector(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	createSearchStatusIssue(ctx, t, store, project.ID, "open candidate", "open")
	for i := range knnDeepLimit {
		createSearchStatusIssue(ctx, t, store, project.ID, fmt.Sprintf("distractor %d", i), "closed")
	}
	createSearchStatusIssue(ctx, t, store, project.ID, "target candidate", "open")
	stale := createSearchStatusIssue(ctx, t, store, project.ID, "stale candidate", "closed")
	idx := openTestVectorIndex(t)
	fillGeneration(ctx, t, store, idx, mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
		switch {
		case strings.HasPrefix(text, "stale"):
			return []float32{0.85, 0.52678, 0, 0}
		case strings.HasPrefix(text, "distractor"):
			var i int
			if n, _ := fmt.Sscanf(text, "distractor %d", &i); n == 1 && i < fetchCap {
				return []float32{0.9, 0.43589, 0, 0}
			}
			return []float32{0.8, 0.6, 0, 0}
		case strings.HasPrefix(text, "target"):
			return []float32{0.6, 0.8, 0, 0}
		default:
			return []float32{1, 0, 0, 0}
		}
	}))
	updatedTitle := "edited candidate"
	if _, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: stale.ID, Title: &updatedTitle, Actor: "a"}); err != nil || !changed {
		t.Fatalf("stale fixture edit: changed=%v, err=%v", changed, err)
	}
	if _, err := idx.RefreshMirror(ctx, store); err != nil {
		t.Fatal(err)
	}
	_, err = hybridSearch(ctx, store, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}), hybridParams{
		ProjectID: project.ID, Query: "semantic query", Limit: 10, Requested: "semantic", Status: "open",
	})
	var me *modeError
	if !errors.As(err, &me) || me.Status() != http.StatusServiceUnavailable {
		t.Fatalf("stale SQLite vector must not hide the relevant raw probe: %v", err)
	}
}
