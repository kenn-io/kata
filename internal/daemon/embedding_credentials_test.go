package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func TestMissingEmbeddingCredentialPreservesBacklogAndExplainsSearch(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	idx := openTestVectorIndex(t)
	p, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "credential rotation", Author: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(401) }))
	defer srv.Close()
	c, err := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "m", Dims: 2})
	if err != nil {
		t.Fatal(err)
	}
	c.SetCredential(config.EmbeddingCredential{Source: "env:EXAMPLE_KEY", Reason: "no embedding API key (env EXAMPLE_KEY is unset)"})
	r := NewReconciler(store, idx, c, ReconcilerConfig{})
	if err := r.reconcileOnce(ctx); err == nil {
		t.Fatal("missing key must leave backfill pending")
	}
	h := r.Health()
	if h.Backlog != 1 || h.Embedded != 0 || h.Skipped != 0 || h.LastSuccessAt != nil {
		t.Fatalf("unexpected health %+v", h)
	}
	for _, mode := range []string{"auto", "semantic", "hybrid", "lexical"} {
		res, err := hybridSearch(ctx, store, idx, c, hybridParams{ProjectID: p.ID, Query: "credential", Limit: 20, Requested: mode})
		switch mode {
		case "auto":
			if err != nil || !res.Degraded || res.Mode != modeLexical || !strings.Contains(res.DegradedReason, "EXAMPLE_KEY") || len(res.Hits) != 1 {
				t.Fatalf("auto result %+v error %v", res, err)
			}
		case "lexical":
			if err != nil || res.Degraded {
				t.Fatalf("lexical result %+v error %v", res, err)
			}
		default:
			var me *modeError
			if !errors.As(err, &me) || me.Status() != 400 || !strings.Contains(err.Error(), "no embedding API key") {
				t.Fatalf("%s error %v", mode, err)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("provider called %d times", calls.Load())
	}
}

func TestEmptyReconcilerReportsSuccessfulReconciliation(t *testing.T) {
	c, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:9", Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	if err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(newReconcilerTestStore(t), openTestVectorIndex(t), c, ReconcilerConfig{})
	if err := r.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Health().LastSuccessAt == nil {
		t.Fatal("successful reconciliation must report last_success_at even without a provider call")
	}
	if c.CredentialHealth().LastSuccessAt != nil {
		t.Fatal("empty reconciliation called the provider")
	}
}

func TestQueryCredentialRejectionUpdatesHealthAndRecovers(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	idx := openTestVectorIndex(t)
	p, _ := store.CreateProject(ctx, "spoke-project")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	c, _ := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	r := NewReconciler(store, idx, c, ReconcilerConfig{})
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"semantic", "hybrid"} {
		res, err := hybridSearch(ctx, store, idx, c, hybridParams{ProjectID: p.ID, Query: "credential", Limit: 20, Requested: mode})
		if mode == "semantic" {
			var me *modeError
			if !errors.As(err, &me) || me.Status() != 400 || !strings.Contains(err.Error(), "rejected the API key") {
				t.Fatalf("error %v", err)
			}
			h := c.CredentialHealth()
			if h.Credential != "rejected" || h.LastErrorAt == nil || h.LastErrorStatus != 401 {
				t.Fatalf("unexpected health %+v", h)
			}
		} else {
			if err != nil || res.Degraded {
				t.Fatalf("recovery error %v", err)
			}
			h := c.CredentialHealth()
			if h.Credential != "ok" || h.LastError != "" || h.LastErrorAt != nil || h.LastSuccessAt == nil {
				t.Fatalf("unexpected recovery %+v", h)
			}
		}
	}
}

func TestRejectedCredentialExplainsUnavailableOldGeneration(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	idx := openTestVectorIndex(t)
	p, _ := store.CreateProject(ctx, "spoke-project")
	old, _ := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:9", Model: "old-model", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	r := NewReconciler(store, idx, old, ReconcilerConfig{})
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	current, _ := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "new-model", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	if _, err := current.Embed(ctx, []string{"query"}); err == nil {
		t.Fatal("provider must reject key")
	}
	for _, mode := range []string{"semantic", "hybrid"} {
		_, err := hybridSearch(ctx, store, idx, current, hybridParams{ProjectID: p.ID, Query: "credential", Limit: 20, Requested: mode})
		var me *modeError
		if !errors.As(err, &me) || me.Status() != 400 || !strings.Contains(err.Error(), "rejected the API key") {
			t.Fatalf("%s returned %v", mode, err)
		}
	}
}
