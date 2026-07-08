package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
