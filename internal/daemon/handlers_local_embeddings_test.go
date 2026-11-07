package daemon

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

type localEmbeddingsFixture struct {
	calls int
	busy  bool
}

func (f *localEmbeddingsFixture) Status(context.Context) (api.LocalEmbeddingsStatus, error) {
	return api.LocalEmbeddingsStatus{Provider: "none", CanChange: true, DownloadBytes: 290_000_000, Job: &api.LocalEmbeddingsJob{State: "failed", Error: "setup provider detail"}}, nil
}
func (f *localEmbeddingsFixture) TurnOn(context.Context) error {
	if f.busy {
		return ErrLocalEmbeddingsBusy
	}
	f.calls++
	return nil
}

func TestLocalEmbeddingsAdministration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     PrincipalKind
		readonly bool
		allowed  bool
	}{
		{"owner local", PrincipalWebLocal, false, true}, {"bootstrap", PrincipalBootstrap, false, true}, {"static token", PrincipalStaticToken, false, true}, {"member", PrincipalDBToken, false, false}, {"readonly", PrincipalWebLocal, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &localEmbeddingsFixture{}
			mux := http.NewServeMux()
			cfg := ServerConfig{LocalEmbeddings: fixture, InsecureReadonly: tc.readonly, ReconcilerHealth: func() ReconcilerHealth { return ReconcilerHealth{Configured: true, Backlog: 2} }}
			registerLocalEmbeddingsHandlers(humago.New(mux, huma.DefaultConfig("test", "1")), cfg)
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				req := httptest.NewRequestWithContext(WithPrincipal(t.Context(), Principal{Kind: tc.kind}), method, "/api/v1/embeddings/local", nil)
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, req)
				if method == http.MethodPost && !tc.allowed {
					require.Equal(t, 403, response.Code)
					require.Zero(t, fixture.calls)
					continue
				}
				want := 200
				if method == http.MethodPost {
					want = 202
				}
				require.Equal(t, want, response.Code, response.Body.String())
				var got api.LocalEmbeddingsStatus
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
				require.Equal(t, tc.allowed, got.CanChange)
				require.NotNil(t, got.Indexing)
				if tc.allowed {
					require.Equal(t, int64(2), got.Indexing.Total)
					require.Equal(t, "setup provider detail", got.Job.Error)
				} else {
					require.Zero(t, got.Indexing.Total)
					require.NotContains(t, got.Job.Error, "setup provider detail")
				}
				if method == http.MethodPost {
					require.Equal(t, 1, fixture.calls)
				}
			}
		})
	}
	t.Run("read-only sessions", func(t *testing.T) {
		manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "readonly")
		manager.writable = false
		mux := http.NewServeMux()
		fixture := &localEmbeddingsFixture{}
		registerLocalEmbeddingsHandlers(humago.New(mux, huma.DefaultConfig("test", "1")), ServerConfig{LocalEmbeddings: fixture, WebSessions: manager})
		for _, method := range []string{"GET", "POST"} {
			request := httptest.NewRequestWithContext(WithPrincipal(t.Context(), Principal{Kind: PrincipalWebLocal}), method, "/api/v1/embeddings/local", nil)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if method == "POST" {
				require.Equal(t, 403, response.Code)
			} else {
				var got api.LocalEmbeddingsStatus
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
				require.False(t, got.CanChange)
			}
		}
		require.Zero(t, fixture.calls)
	})
	t.Run("unavailable", func(t *testing.T) {
		mux := http.NewServeMux()
		registerLocalEmbeddingsHandlers(humago.New(mux, huma.DefaultConfig("test", "1")), ServerConfig{})
		for _, method := range []string{"GET", "POST"} {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/api/v1/embeddings/local", nil))
			require.Equal(t, 404, rec.Code)
		}
	})
	t.Run("busy", func(t *testing.T) {
		mux := http.NewServeMux()
		registerLocalEmbeddingsHandlers(humago.New(mux, huma.DefaultConfig("test", "1")), ServerConfig{LocalEmbeddings: &localEmbeddingsFixture{busy: true}})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(WithPrincipal(t.Context(), Principal{Kind: PrincipalWebLocal}), "POST", "/api/v1/embeddings/local", nil))
		require.Equal(t, 409, rec.Code)
	})
}

func TestLocalEmbeddingsInitialIndexingProgress(t *testing.T) {
	health := ReconcilerHealth{Configured: true}
	mux := http.NewServeMux()
	registerLocalEmbeddingsHandlers(humago.New(mux, huma.DefaultConfig("test", "1")), ServerConfig{LocalEmbeddings: &localEmbeddingsFixture{}, ReconcilerHealth: func() ReconcilerHealth { return health }})
	for _, tc := range []struct {
		name        string
		health      ReconcilerHealth
		done, total int64
	}{
		{name: "coverage not read", health: ReconcilerHealth{Configured: true}},
		{name: "backfill", health: ReconcilerHealth{Configured: true, Embedded: 40, Backlog: 172}, done: 40, total: 212},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health = tc.health
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequestWithContext(WithPrincipal(t.Context(), Principal{Kind: PrincipalWebLocal}), "GET", "/api/v1/embeddings/local", nil))
			require.Equal(t, 200, response.Code)
			var status api.LocalEmbeddingsStatus
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
			require.NotNil(t, status.Indexing)
			require.Equal(t, tc.done, status.Indexing.Done)
			require.Equal(t, tc.total, status.Indexing.Total)
		})
	}
}

func TestLocalEmbeddingsFailedFirstIndex(t *testing.T) {
	ctx := WithPrincipal(t.Context(), Principal{Kind: PrincipalBootstrap})
	store := newReconcilerTestStore(t)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "example issue", Author: "tester"})
	require.NoError(t, err)
	var failing atomic.Bool
	failing.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			http.Error(w, "provider reflected example issue text", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	t.Cleanup(server.Close)
	embedder, err := embedding.New(embedding.Config{BaseURL: server.URL, Model: "example-model", Dims: 2})
	require.NoError(t, err)
	reconciler := NewReconciler(store, openTestVectorIndex(t), embedder, ReconcilerConfig{})
	mux := http.NewServeMux()
	registerLocalEmbeddingsHandlers(humago.New(mux, huma.DefaultConfig("test", "1")), ServerConfig{LocalEmbeddings: &localEmbeddingsFixture{}, ReconcilerHealth: reconciler.Health})
	for _, failed := range []bool{true, false} {
		failing.Store(failed)
		err := reconciler.reconcileOnce(ctx)
		if failed {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequestWithContext(WithPrincipal(ctx, Principal{Kind: PrincipalWebLocal}), "GET", "/api/v1/embeddings/local", nil))
		require.Equal(t, http.StatusOK, response.Code)
		require.NotContains(t, response.Body.String(), "provider reflected")
		var status api.LocalEmbeddingsStatus
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
		if failed {
			require.NotNil(t, status.Indexing)
			require.True(t, status.Indexing.Failed)
			require.Equal(t, int64(1), status.Indexing.Total)
		} else {
			require.Nil(t, status.Indexing)
		}
	}
}
