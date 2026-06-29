package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func TestHybridSearchLexicalWhenUnconfigured(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a"}); err != nil {
		t.Fatal(err)
	}

	res, err := hybridSearch(ctx, store, nil /*embedder*/, hybridParams{
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
	_, err := hybridSearch(ctx, store, nil, hybridParams{ProjectID: proj.ID, Query: "x", Limit: 10, Requested: "hybrid"})
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

func TestHybridSearchAutoDegradesOnVectorFailure(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "login race", Body: "x", Author: "a"}); err != nil {
		t.Fatal(err)
	}

	res, err := hybridSearch(ctx, store, failingEmbedClient(t, http.StatusServiceUnavailable), hybridParams{
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

	_, err := hybridSearch(ctx, store, failingEmbedClient(t, http.StatusServiceUnavailable), hybridParams{
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
