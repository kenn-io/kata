package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
)

func TestHybridSearchLexicalWhenUnconfigured(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a"}); err != nil {
		t.Fatal(err)
	}

	res, err := hybridSearch(ctx, store, nil /*idx*/, nil /*embedder*/, hybridParams{
		ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != modeLexical || res.Degraded {
		t.Fatalf("unconfigured should be lexical, not degraded: %#v", res)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("expected the lexical hit to survive, got %d", len(res.Hits))
	}
}

func TestHybridSearchExplicitHybridUnconfiguredErrors(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	_, err := hybridSearch(ctx, store, nil, nil, hybridParams{ProjectID: proj.ID, Query: "x", Limit: 10, Requested: "hybrid"})
	if err == nil {
		t.Fatal("expected 400-class error for explicit hybrid without embeddings")
	}
	var me *modeError
	if !errors.As(err, &me) {
		t.Fatalf("want *modeError, got %T: %v", err, err)
	}
	if me.Status() != 400 {
		t.Fatalf("unconfigured explicit mode should be 400, got %d", me.Status())
	}
}

// failingEmbedClient builds a real *embedding.Client pointed at a server that
// always errors, so the vector leg fails the way an unreachable endpoint would.
func failingEmbedClient(t *testing.T, status int) *embedding.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	if err != nil {
		t.Fatalf("new embedding client: %v", err)
	}
	return c
}

// fixedVectorEmbedClient builds a real *embedding.Client (model "m") pointed
// at a stub server that always returns vec for every input, regardless of
// text. Tests use it to make both the fill and the query embed deterministic,
// so every indexed document scores identically and the test isolates
// project/deleted filtering rather than ranking.
func fixedVectorEmbedClient(t *testing.T, vec []float32) *embedding.Client {
	t.Helper()
	return fixedVectorEmbedClientModel(t, "m", vec)
}

// fixedVectorEmbedClientModel is fixedVectorEmbedClient with a caller-chosen
// model name, for tests that need two clients whose generation fingerprints
// differ while their dimensionality matches.
func fixedVectorEmbedClientModel(t *testing.T, model string, vec []float32) *embedding.Client {
	t.Helper()
	return mappedVectorEmbedClient(t, model, len(vec), func(string) []float32 { return vec })
}

// mappedVectorEmbedClient builds a real *embedding.Client whose stub server
// returns vecFor(input) per input, so tests can craft content-dependent
// vectors (e.g. distractors on one axis, the target off-axis).
func mappedVectorEmbedClient(t *testing.T, model string, dims int, vecFor func(string) []float32) *embedding.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			data[i] = map[string]any{"embedding": vecFor(in)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	c, err := embedding.New(embedding.Config{BaseURL: srv.URL, Model: model, Dims: dims})
	if err != nil {
		t.Fatalf("new embedding client: %v", err)
	}
	return c
}

// activateFixedGeneration runs a reconciler cycle with a fixed-vector
// embedding client (model "m", the same generation fingerprint as
// fixedVectorEmbedClient) so idx has an active generation containing every
// issue currently in store. Tests that need a populated sidecar call this
// once after creating their fixtures.
func activateFixedGeneration(ctx context.Context, t *testing.T, store db.Storage, idx *vector.Index) {
	t.Helper()
	fillGeneration(ctx, t, store, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}))
}

// fillGeneration runs one reconciler cycle with emb, filling and activating
// its generation.
func fillGeneration(ctx context.Context, t *testing.T, store db.Storage, idx *vector.Index, emb embedder) {
	t.Helper()
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64})
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcile once: %v", err)
	}
}

func TestHybridSearchAutoDegradesOnVectorFailure(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a"}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)

	res, err := hybridSearch(ctx, store, idx, failingEmbedClient(t, http.StatusServiceUnavailable), hybridParams{
		ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "auto",
	})
	if err != nil {
		t.Fatalf("auto must degrade, not error: %v", err)
	}
	if res.Mode != modeLexical || !res.Degraded {
		t.Fatalf("auto with a failing embedder should degrade to labeled lexical: %#v", res)
	}
	if res.DegradedReason == "" {
		t.Fatal("degraded result must carry a reason")
	}
	if len(res.Hits) != 1 {
		t.Fatalf("degraded lexical must still return the FTS hit, got %d", len(res.Hits))
	}
}

func TestHybridSearchExplicitHybridLegFailureReturns503(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a"}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)

	_, err := hybridSearch(ctx, store, idx, failingEmbedClient(t, http.StatusServiceUnavailable), hybridParams{
		ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "hybrid",
	})
	if err == nil {
		t.Fatal("explicit hybrid with a failing vector leg must error, not degrade")
	}
	var me *modeError
	if !errors.As(err, &me) {
		t.Fatalf("want *modeError, got %T: %v", err, err)
	}
	if me.Status() != 503 {
		t.Fatalf("leg failure under an explicit mode should be 503, got %d", me.Status())
	}
}

// TestVectorLegExcludesOtherProjectsAndDeleted covers the seam runVectorLeg
// added on top of kit's generation index: the index is daemon-global (one
// sidecar, all projects' content mirrored into it), so a raw KNN hit can
// belong to any project or point at content that has since been soft-deleted.
// hydrating against live kata.db rows must strip both before they ever reach
// a caller scoped to one project.
func TestVectorLegExcludesOtherProjectsAndDeleted(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)

	projA, err := store.CreateProject(ctx, "spoke-project-a")
	if err != nil {
		t.Fatal(err)
	}
	projB, err := store.CreateProject(ctx, "spoke-project-b")
	if err != nil {
		t.Fatal(err)
	}

	const sharedTitle = "shared login race across projects"
	liveA, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projA.ID, Title: sharedTitle, Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	toDeleteA, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projA.ID, Title: sharedTitle, Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projB.ID, Title: sharedTitle, Body: "x", Author: "a",
	}); err != nil {
		t.Fatal(err)
	}

	idx := openTestVectorIndex(t)
	// Fill and activate while all three issues are still live, so all three
	// land in the sidecar's active generation.
	activateFixedGeneration(ctx, t, store, idx)

	// Soft-delete after filling: the sidecar still holds an embedding for it,
	// so only the live-row hydration in runVectorLeg can filter it out.
	if _, _, _, err := store.SoftDeleteIssue(ctx, toDeleteA.ID, "a"); err != nil {
		t.Fatal(err)
	}

	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})
	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: projA.ID, Query: "login race", Limit: 10, Requested: "semantic",
	})
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if res.Mode != modeSemantic {
		t.Fatalf("explicit semantic must run as semantic, got %q", res.Mode)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("expected exactly project A's live issue, got %d hits: %#v", len(res.Hits), res.Hits)
	}
	hit := res.Hits[0]
	if hit.Issue.UID != liveA.UID {
		t.Fatalf("hit = %q, want project A's live issue %q", hit.Issue.UID, liveA.UID)
	}
	if hit.Issue.ProjectID != projA.ID {
		t.Fatalf("hit project = %d, want %d", hit.Issue.ProjectID, projA.ID)
	}
	if len(hit.MatchedIn) != 1 || hit.MatchedIn[0] != "semantic" {
		t.Fatalf("matched_in = %v, want [\"semantic\"]", hit.MatchedIn)
	}
}

// TestVectorLegModelChangeBackfillUnavailable pins the model-change window:
// while the active generation was built under a different fingerprint than
// the configured embedder (same dims, so raw KNN would silently rank a
// new-model query vector against old-model stored vectors), the vector leg
// must refuse to serve — auto degrades labeled, explicit semantic is 503 —
// until the new generation cuts over.
func TestVectorLegModelChangeBackfillUnavailable(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a",
	}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx) // active generation under model "m"

	// Same dims, different model: the configured embedder's fingerprint no
	// longer matches the active generation.
	emb := fixedVectorEmbedClientModel(t, "m-next", []float32{1, 0, 0, 0})

	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "auto",
	})
	if err != nil {
		t.Fatalf("auto must degrade, not error: %v", err)
	}
	if res.Mode != modeLexical || !res.Degraded || res.DegradedReason == "" {
		t.Fatalf("model change mid-backfill must degrade auto to labeled lexical: %#v", res)
	}

	_, err = hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "semantic",
	})
	var me *modeError
	if !errors.As(err, &me) {
		t.Fatalf("want *modeError, got %T: %v", err, err)
	}
	if me.Status() != 503 {
		t.Fatalf("explicit semantic during model-change backfill must be 503, got %d", me.Status())
	}
}

// TestVectorLegSoftDeleteDropsFromIndexUntilRestore pins the privacy
// contract: a soft-deleted issue leaves the mirror (and loses its vectors) at
// the next refresh, so its content is never re-sent to the embedding endpoint
// while deleted and no search — not even include_deleted — ranks it
// semantically. Restoring the issue re-mirrors and re-embeds it.
func TestVectorLegSoftDeleteDropsFromIndexUntilRestore(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	iss, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}

	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)

	if _, _, _, err := store.SoftDeleteIssue(ctx, iss.ID, "a"); err != nil {
		t.Fatal(err)
	}

	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

	// Before any mirror refresh the stale vectors still exist in the index;
	// hydration must enforce the contract per request — the vector leg serves
	// live issues only, even for include_deleted searches.
	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
		IncludeDeleted: true,
	})
	if err != nil {
		t.Fatalf("hybridSearch include_deleted pre-refresh: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("vector leg must not rank a soft-deleted issue even before the mirror refresh, got %#v", res.Hits)
	}

	// Reconcile after the soft delete: the mirror row and its vectors must be
	// removed so deleted content cannot flow to the embedding endpoint on a
	// later rebuild.
	activateFixedGeneration(ctx, t, store, idx)

	res, err = hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
		IncludeDeleted: true,
	})
	if err != nil {
		t.Fatalf("hybridSearch include_deleted: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("soft-deleted issue must leave the vector index at the next refresh, got %#v", res.Hits)
	}

	// Restore: the issue rejoins the mirror and is re-embedded, so semantic
	// recall resumes.
	if _, _, _, err := store.RestoreIssue(ctx, iss.ID, "a"); err != nil {
		t.Fatal(err)
	}
	activateFixedGeneration(ctx, t, store, idx)

	res, err = hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
	})
	if err != nil {
		t.Fatalf("hybridSearch after restore: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Issue.UID != iss.UID {
		t.Fatalf("restored issue must regain semantic recall, got %#v", res.Hits)
	}
}

// TestVectorLegDeepRetryBeatsCrossProjectStarvation pins the bounded depth
// retry: the KNN index is daemon-global, so more than fetchCap higher-scoring
// chunks from another project can fill the entire first batch and starve the
// requested project. When the first batch comes back full and yields fewer
// in-project hits than wanted, one deeper query (knnDeepLimit) must recover
// the requested project's match.
func TestVectorLegDeepRetryBeatsCrossProjectStarvation(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	projA, err := store.CreateProject(ctx, "spoke-project-a")
	if err != nil {
		t.Fatal(err)
	}
	projB, err := store.CreateProject(ctx, "spoke-project-b")
	if err != nil {
		t.Fatal(err)
	}

	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projA.ID, Title: "target login race", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	// fetchCap+1 distractors in project B whose vectors score above the
	// target for the query vector, so the first KNN batch holds only them.
	for i := 0; i < fetchCap+1; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: projB.ID, Title: fmt.Sprintf("distractor %d", i), Body: "x", Author: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}

	idx := openTestVectorIndex(t)
	// Distractor content lands on the query axis (cosine 1.0); the target is
	// off-axis but above the floor (cosine 0.6).
	fill := mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
		if strings.HasPrefix(text, "distractor") {
			return []float32{1, 0, 0, 0}
		}
		return []float32{0.6, 0.8, 0, 0}
	})
	fillGeneration(ctx, t, store, idx, fill)

	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})
	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: projA.ID, Query: "login race", Limit: 10, Requested: "semantic",
	})
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Issue.UID != target.UID {
		t.Fatalf("project A's issue must survive %d higher-scoring cross-project chunks, got %#v",
			fetchCap+1, res.Hits)
	}
}

// TestVectorLegUnavailableDegradesInAutoAndFailsExplicit covers two ways the
// vector leg can be unavailable at request time. A nil idx (sidecar failed to
// open) makes hybridSearch treat semantic/hybrid as unconfigured — identical
// to a nil Embedder, per configured := emb != nil && idx != nil — so auto
// silently stays plain lexical (not degraded: there was never a leg to fail)
// and an explicit request is the existing 400 "not configured" error. An idx
// that is open but has no active generation yet (cold start mid-backfill) is
// a different case: the leg is configured and attempted, but runVectorLeg
// itself fails, so auto degrades with a reason and an explicit request gets a
// 503.
func TestVectorLegUnavailableDegradesInAutoAndFailsExplicit(t *testing.T) {
	ctx := context.Background()

	t.Run("nil index is treated as unconfigured", func(t *testing.T) {
		store := newReconcilerTestStore(t)
		proj, _ := store.CreateProject(ctx, "spoke-project")
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a",
		}); err != nil {
			t.Fatal(err)
		}
		emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

		res, err := hybridSearch(ctx, store, nil /*idx*/, emb, hybridParams{
			ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "auto",
		})
		if err != nil {
			t.Fatalf("auto must not error when idx is nil: %v", err)
		}
		if res.Mode != modeLexical || res.Degraded {
			t.Fatalf("nil idx should look unconfigured, not degraded: %#v", res)
		}

		_, err = hybridSearch(ctx, store, nil /*idx*/, emb, hybridParams{
			ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "semantic",
		})
		var me *modeError
		if !errors.As(err, &me) {
			t.Fatalf("want *modeError, got %T: %v", err, err)
		}
		if me.Status() != 400 {
			t.Fatalf("nil idx explicit semantic should be 400 (unconfigured), got %d", me.Status())
		}
	})

	t.Run("no active generation degrades auto and fails explicit", func(t *testing.T) {
		store := newReconcilerTestStore(t)
		proj, _ := store.CreateProject(ctx, "spoke-project")
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a",
		}); err != nil {
			t.Fatal(err)
		}
		// Open but never filled: ActiveGeneration reports ok=false, the cold
		// start / mid-backfill state.
		idx := openTestVectorIndex(t)
		emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

		res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
			ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "auto",
		})
		if err != nil {
			t.Fatalf("auto must degrade, not error: %v", err)
		}
		if res.Mode != modeLexical || !res.Degraded {
			t.Fatalf("no active generation should degrade to labeled lexical: %#v", res)
		}
		if res.DegradedReason == "" {
			t.Fatal("degraded result must carry a reason")
		}
		if len(res.Hits) != 1 {
			t.Fatalf("degraded lexical must still return the FTS hit, got %d", len(res.Hits))
		}

		_, err = hybridSearch(ctx, store, idx, emb, hybridParams{
			ProjectID: proj.ID, Query: "login", Limit: 10, Requested: "semantic",
		})
		var me *modeError
		if !errors.As(err, &me) {
			t.Fatalf("want *modeError, got %T: %v", err, err)
		}
		if me.Status() != 503 {
			t.Fatalf("no active generation under explicit mode should be 503, got %d", me.Status())
		}
	})
}

// hitTitles summarizes candidates for failure messages: the label-filter
// fixtures run to hundreds of issues, where dumping whole candidate structs
// buries the one fact a reader needs.
func hitTitles(hits []db.SearchCandidate) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Issue.Title)
	}
	return out
}

// TestHybridSearchLabelFilterAppliesToBothLegs pins that a label filter binds
// the whole request rather than one leg. The unlabeled issue matches the query
// lexically (same title terms) and semantically (same vector axis), so it can
// only be absent from a hybrid response if the FTS predicate and the vector
// leg's own check both applied.
func TestHybridSearchLabelFilterAppliesToBothLegs(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	labeled, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "login race in the session handler", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddLabel(ctx, labeled.ID, "bug", "a"); err != nil {
		t.Fatal(err)
	}
	unlabeled, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "login race in the cookie handler", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Labeled but off-topic: proves the filter narrows the query's matches
	// rather than widening the response to every labeled issue.
	offTopic, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "checkout rounding drift", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddLabel(ctx, offTopic.ID, "bug", "a"); err != nil {
		t.Fatal(err)
	}

	idx := openTestVectorIndex(t)
	// Login content sits on the query axis; the off-topic issue is orthogonal
	// to it and falls under the cosine floor.
	emb := mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
		if strings.Contains(text, "login") {
			return []float32{1, 0, 0, 0}
		}
		return []float32{0, 1, 0, 0}
	})
	fillGeneration(ctx, t, store, idx, emb)

	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "hybrid",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if res.Mode != modeHybrid {
		t.Fatalf("explicit hybrid must run as hybrid, got %q", res.Mode)
	}
	if res.Degraded {
		t.Fatalf("a corpus this far inside the candidate ceiling has nothing to degrade: %q", res.DegradedReason)
	}
	if len(res.Hits) != 1 || res.Hits[0].Issue.UID != labeled.UID {
		t.Fatalf("want only the labeled issue %q, got %q", labeled.Title, hitTitles(res.Hits))
	}
	// Both legs matched the surviving issue, so both legs also ran their own
	// filter against the unlabeled one.
	if !slices.Contains(res.Hits[0].MatchedIn, "semantic") {
		t.Fatalf("matched_in = %v, want the vector leg to have contributed", res.Hits[0].MatchedIn)
	}
	if !slices.Contains(res.Hits[0].MatchedIn, "title") {
		t.Fatalf("matched_in = %v, want the lexical leg to have contributed", res.Hits[0].MatchedIn)
	}
	for _, h := range res.Hits {
		if h.Issue.UID == unlabeled.UID {
			t.Fatalf("unlabeled issue %q leaked through a label-filtered search", unlabeled.Title)
		}
	}
}

// TestSemanticSearchLabelANDAndExclusion covers the vector leg's filter
// semantics: Labels is an AND across every value, ExcludeLabels removes any
// issue carrying one, and both compare case-insensitively against the stored
// canonical labels. Every fixture shares one vector, so labels are the only
// thing separating them.
func TestSemanticSearchLabelANDAndExclusion(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	create := func(title string, labels ...string) db.Issue {
		t.Helper()
		iss, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: title, Body: "x", Author: "a",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range labels {
			if _, err := store.AddLabel(ctx, iss.ID, l, "a"); err != nil {
				t.Fatal(err)
			}
		}
		return iss
	}
	both := create("login race retry storm", "bug", "urgent")
	bugOnly := create("login race in the session handler", "bug")
	create("login race on the cookie path", "urgent")

	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)
	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
		Labels: []string{"bug", "urgent"},
	})
	if err != nil {
		t.Fatalf("hybridSearch AND: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Issue.UID != both.UID {
		t.Fatalf("multiple labels must AND to the doubly-labeled issue %q, got %q", both.Title, hitTitles(res.Hits))
	}

	// Mixed case on both filters: stored labels are canonical lowercase, so
	// the request's values are what has to be folded.
	res, err = hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
		Labels: []string{"Bug"}, ExcludeLabels: []string{"URGENT"},
	})
	if err != nil {
		t.Fatalf("hybridSearch exclusion: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Issue.UID != bugOnly.UID {
		t.Fatalf("excluding urgent must leave only %q, got %q", bugOnly.Title, hitTitles(res.Hits))
	}
}

type labelsByIssuesCountingStore struct {
	db.Storage
	calls int
}

func (s *labelsByIssuesCountingStore) LabelsByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]string, error) {
	s.calls++
	return s.Storage.LabelsByIssues(ctx, projectID, issueIDs)
}

func TestSemanticSearchHydratesLabelsOncePerCandidateBatch(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		iss, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: fmt.Sprintf("login race %d", i), Body: "x", Author: "a",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddLabel(ctx, iss.ID, "bug", "a"); err != nil {
			t.Fatal(err)
		}
	}

	idx := openTestVectorIndex(t)
	activateFixedGeneration(ctx, t, store, idx)
	counting := &labelsByIssuesCountingStore{Storage: store}
	_, err = hybridSearch(ctx, counting, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}), hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if counting.calls != 1 {
		t.Fatalf("LabelsByIssues calls = %d, want one bulk hydration for the candidate batch", counting.calls)
	}
}

// TestVectorLegLabelFilterUsesDeepRetry pins that the bounded depth retry
// serves label filters too: fetchCap unlabeled chunks can fill the entire
// first KNN batch, and the one labeled match only surfaces because the leg
// re-queries at knnDeepLimit. The index holds fewer chunks than that ceiling,
// so the short result is exact and must not be flagged degraded.
func TestVectorLegLabelFilterUsesDeepRetry(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "target login race", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddLabel(ctx, target.ID, "bug", "a"); err != nil {
		t.Fatal(err)
	}
	// fetchCap+1 unlabeled issues whose vectors outscore the target, so the
	// first batch holds only issues the label filter drops.
	for i := 0; i < fetchCap+1; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: fmt.Sprintf("distractor %d", i), Body: "x", Author: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}

	idx := openTestVectorIndex(t)
	fillGeneration(ctx, t, store, idx, labelAxisEmbedClient(t))

	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})
	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Issue.UID != target.UID {
		t.Fatalf("the labeled issue must survive %d higher-scoring unlabeled chunks, got %q",
			fetchCap+1, hitTitles(res.Hits))
	}
	if res.Degraded {
		t.Fatalf("a deep batch the index could not fill means exact results, not a degrade: %q", res.DegradedReason)
	}
}

// TestVectorLegLabelFilterRetriesPastStaleInitialSQLiteWindow pins the raw
// fetchCap boundary used by SQLite vec0. A stale vector still occupies a raw
// KNN slot even though the freshness join removes it from returned hits, so
// the retry decision cannot rely on len(hits) alone.
func TestVectorLegLabelFilterRetriesPastStaleInitialSQLiteWindow(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "target login race", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddLabel(ctx, target.ID, "bug", "a"); err != nil {
		t.Fatal(err)
	}
	stale, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "stale candidate", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < fetchCap-1; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: fmt.Sprintf("distractor %d", i), Body: "x", Author: "a",
		}); err != nil {
			t.Fatal(err)
		}
	}

	idx := openTestVectorIndex(t)
	fillGeneration(ctx, t, store, idx, mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
		switch {
		case strings.HasPrefix(text, "stale"):
			return []float32{0.95, 0.31225, 0, 0}
		case strings.HasPrefix(text, "distractor"):
			return []float32{0.9, 0.43589, 0, 0}
		case strings.HasPrefix(text, "target"):
			return []float32{0.6, 0.8, 0, 0}
		default:
			return []float32{1, 0, 0, 0}
		}
	}))

	updatedTitle := "edited candidate"
	if _, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{
		IssueID: stale.ID, Title: &updatedTitle, Actor: "a",
	}); err != nil {
		t.Fatal(err)
	} else if !changed {
		t.Fatal("fixture edit must advance the stale issue's content revision")
	}
	if _, err := idx.RefreshMirror(ctx, store); err != nil {
		t.Fatal(err)
	}

	res, err := hybridSearch(ctx, store, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}), hybridParams{
		ProjectID: proj.ID, Query: "semantic query", Limit: 10, Requested: "semantic",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Issue.UID != target.UID {
		t.Fatalf("stale initial-window vector must not hide labeled issue %q beyond fetchCap, got %q",
			target.Title, hitTitles(res.Hits))
	}
}

func TestSearchLabelCeilingRequiresRelevantProbeCandidate(t *testing.T) {
	tests := []struct {
		name            string
		irrelevantCount int
		irrelevantVec   []float32
	}{
		{
			name:            "exactly the deep limit",
			irrelevantCount: knnDeepLimit - ceilingSurvivors,
			irrelevantVec:   []float32{0.9, 0.43589, 0, 0},
		},
		{
			name:            "probe falls below cosine floor",
			irrelevantCount: knnDeepLimit + 1,
			irrelevantVec:   []float32{0.2, 0.9799, 0, 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := newReconcilerTestStore(t)
			proj, err := store.CreateProject(ctx, "spoke-project")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < ceilingSurvivors; i++ {
				iss, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: proj.ID, Title: fmt.Sprintf("relevant candidate %d", i), Body: "x", Author: "a",
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.AddLabel(ctx, iss.ID, "bug", "a"); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < tt.irrelevantCount; i++ {
				if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: proj.ID, Title: fmt.Sprintf("irrelevant candidate %d", i), Body: "x", Author: "a",
				}); err != nil {
					t.Fatal(err)
				}
			}

			idx := openTestVectorIndex(t)
			fillGeneration(ctx, t, store, idx, mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
				if strings.HasPrefix(text, "irrelevant") {
					return tt.irrelevantVec
				}
				return []float32{1, 0, 0, 0}
			}))

			res, err := hybridSearch(ctx, store, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}), hybridParams{
				ProjectID: proj.ID, Query: "semantic query", Limit: 10, Requested: "semantic",
				Labels: []string{"bug"},
			})
			if err != nil {
				t.Fatalf("a deep batch with no relevant unseen candidate must be exact: %v", err)
			}
			if res.Degraded {
				t.Fatalf("exact semantic results must not degrade: %q", res.DegradedReason)
			}
			if len(res.Hits) != ceilingSurvivors {
				t.Fatalf("hits = %d, want %d relevant labeled candidates", len(res.Hits), ceilingSurvivors)
			}
		})
	}
}

// labelAxisEmbedClient ranks issues by title prefix rather than content, in
// three tiers of the query axis: "distractor" issues sit just below it (cosine
// 0.9), "target" issues far below it but still above the cosine floor (0.6),
// and everything else — the query text included — exactly on it (1.0). Fixing
// the tiers this way keeps KNN order independent of how equal scores are
// broken, which matters once a fixture runs to a thousand chunks.
func labelAxisEmbedClient(t *testing.T) *embedding.Client {
	t.Helper()
	return mappedVectorEmbedClient(t, "m", 4, func(text string) []float32 {
		switch {
		case strings.HasPrefix(text, "distractor"):
			return []float32{0.9, 0.43589, 0, 0}
		case strings.HasPrefix(text, "target"):
			return []float32{0.6, 0.8, 0, 0}
		default:
			return []float32{1, 0, 0, 0}
		}
	})
}

// ceilingSurvivors is how many labeled issues the ceiling corpus keeps inside
// the candidate ceiling: enough to fill a small limit, too few for a large one.
const ceilingSurvivors = 3

// seedLabelCeilingCorpus creates the shape a label-filtered deep retry cannot
// see past: knnDeepLimit unlabeled distractors, ceilingSurvivors labeled issues
// ranked above them, and one labeled issue ranked below them and so pushed off
// the end of the ceiling. It returns that unreachable issue. Filling the
// generation is left to the caller, which may still be seeding other projects.
func seedLabelCeilingCorpus(ctx context.Context, t *testing.T, store db.Storage, projectID int64) db.Issue {
	t.Helper()
	label := func(iss db.Issue) {
		t.Helper()
		if _, err := store.AddLabel(ctx, iss.ID, "bug", "a"); err != nil {
			t.Fatal(err)
		}
	}
	create := func(title string) db.Issue {
		t.Helper()
		iss, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: projectID, Title: title, Body: "x", Author: "a",
		})
		if err != nil {
			t.Fatal(err)
		}
		return iss
	}
	for i := 0; i < ceilingSurvivors; i++ {
		label(create(fmt.Sprintf("labeled login race %d", i)))
	}
	for i := 0; i < knnDeepLimit; i++ {
		create(fmt.Sprintf("distractor %d", i))
	}
	beyond := create("target semantic-only candidate")
	label(beyond)
	return beyond
}

// TestSearchLabelCeilingHonorsModeStrictness pins the split between explicit
// modes, which fail when the bounded semantic leg cannot prove completeness,
// and auto mode, which may return the reachable hits with a degraded signal.
func TestSearchLabelCeilingHonorsModeStrictness(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	beyond := seedLabelCeilingCorpus(ctx, t, store, proj.ID)
	idx := openTestVectorIndex(t)
	fillGeneration(ctx, t, store, idx, labelAxisEmbedClient(t))
	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

	for _, requested := range []string{"semantic", "hybrid"} {
		_, err := hybridSearch(ctx, store, idx, emb, hybridParams{
			ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: requested,
			Labels: []string{"bug"},
		})
		if err == nil {
			t.Fatalf("explicit %s must fail when label filtering exhausts the semantic candidate ceiling", requested)
		}
		var me *modeError
		if !errors.As(err, &me) {
			t.Fatalf("explicit %s error = %T, want *modeError", requested, err)
		}
		if me.Status() != http.StatusServiceUnavailable {
			t.Fatalf("explicit %s status = %d, want 503", requested, me.Status())
		}
	}

	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "auto",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("auto search must report bounded incompleteness as a degraded response: %v", err)
	}
	if res.Mode != modeHybrid {
		t.Fatalf("mode = %q, want auto-resolved hybrid", res.Mode)
	}
	if !res.Degraded {
		t.Fatalf("auto search must report filters that consume the candidate ceiling, got %d hits and no signal", len(res.Hits))
	}
	const want = "label filters exhausted the semantic candidate ceiling; semantic results may be incomplete"
	if res.DegradedReason != want {
		t.Fatalf("degraded reason = %q, want %q", res.DegradedReason, want)
	}
	// The signal reports incompleteness, not emptiness: the reachable matches
	// are returned, and the one ranked past the ceiling is what is missing.
	if len(res.Hits) != ceilingSurvivors {
		t.Fatalf("want the %d reachable labeled issues, got %q", ceilingSurvivors, hitTitles(res.Hits))
	}
	for _, h := range res.Hits {
		if h.Issue.UID == beyond.UID {
			t.Fatalf("fixture is wrong: %q must rank beyond the ceiling for this test to mean anything", beyond.Title)
		}
	}

	// Same exhausted ceiling, but the survivors already fill the limit: the
	// caller's page is complete, so there is nothing to report.
	res, err = hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: ceilingSurvivors - 1, Requested: "semantic",
		Labels: []string{"bug"},
	})
	if err != nil {
		t.Fatalf("hybridSearch at a filled limit: %v", err)
	}
	if res.Degraded {
		t.Fatalf("a limit the survivors fill is not incomplete, got %q", res.DegradedReason)
	}
	if len(res.Hits) != ceilingSurvivors-1 {
		t.Fatalf("want a full page of %d hits, got %q", ceilingSurvivors-1, hitTitles(res.Hits))
	}
}

func TestSearchLabelCeilingProbeSurvivesStaleSQLiteVector(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	seedLabelCeilingCorpus(ctx, t, store, proj.ID)
	stale, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: proj.ID, Title: "stale candidate", Body: "x", Author: "a",
	})
	if err != nil {
		t.Fatal(err)
	}

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
	if _, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{
		IssueID: stale.ID, Title: &updatedTitle, Actor: "a",
	}); err != nil {
		t.Fatal(err)
	} else if !changed {
		t.Fatal("fixture edit must advance the stale issue's content revision")
	}
	if _, err := idx.RefreshMirror(ctx, store); err != nil {
		t.Fatal(err)
	}

	_, err = hybridSearch(ctx, store, idx, fixedVectorEmbedClient(t, []float32{1, 0, 0, 0}), hybridParams{
		ProjectID: proj.ID, Query: "semantic query", Limit: 10, Requested: "semantic",
		Labels: []string{"bug"},
	})
	if err == nil {
		t.Fatal("a stale SQLite vector must not hide a relevant raw probe beyond the candidate ceiling")
	}
	var me *modeError
	if !errors.As(err, &me) || me.Status() != http.StatusServiceUnavailable {
		t.Fatalf("bounded explicit semantic error = %T %v, want 503 modeError", err, err)
	}
}

// TestUnfilteredSearchNeverSetsCeilingDegraded guards the unfiltered path over
// the very corpus that degrades a filtered search. Searching the corpus project
// fills the first batch and never retries; searching a sibling project starved
// beyond the ceiling does take the deep retry and still comes up short — the
// pre-existing cross-project miss, which stays silent because the signal is
// only ever about label filters.
func TestUnfilteredSearchNeverSetsCeilingDegraded(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, err := store.CreateProject(ctx, "spoke-project-a")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := store.CreateProject(ctx, "spoke-project-b")
	if err != nil {
		t.Fatal(err)
	}
	seedLabelCeilingCorpus(ctx, t, store, proj.ID)
	// Ranked below every distractor, so the corpus crowds it out of the deep
	// batch exactly as it crowds out the labeled issue past the ceiling.
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: sibling.ID, Title: "target login race in the sibling project", Body: "x", Author: "a",
	}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	fillGeneration(ctx, t, store, idx, labelAxisEmbedClient(t))
	emb := fixedVectorEmbedClient(t, []float32{1, 0, 0, 0})

	res, err := hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: proj.ID, Query: "login race", Limit: 10, Requested: "semantic",
	})
	if err != nil {
		t.Fatalf("hybridSearch: %v", err)
	}
	if res.Degraded || res.DegradedReason != "" {
		t.Fatalf("an unfiltered search must never carry the ceiling signal, got %q", res.DegradedReason)
	}
	if len(res.Hits) != 10 {
		t.Fatalf("unfiltered search should fill the limit, got %d hits", len(res.Hits))
	}

	res, err = hybridSearch(ctx, store, idx, emb, hybridParams{
		ProjectID: sibling.ID, Query: "login race", Limit: 10, Requested: "semantic",
	})
	if err != nil {
		t.Fatalf("hybridSearch from the starved project: %v", err)
	}
	if res.Degraded || res.DegradedReason != "" {
		t.Fatalf("cross-project starvation is not a label-filter degrade, got %q", res.DegradedReason)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("fixture is wrong: the sibling issue must rank beyond the ceiling, got %q", hitTitles(res.Hits))
	}
}
