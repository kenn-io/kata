package daemon

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

type localEmbeddingsFixture struct {
	calls int
	busy  bool
}

func (f *localEmbeddingsFixture) Status(context.Context) (api.LocalEmbeddingsStatus, error) {
	return api.LocalEmbeddingsStatus{Provider: "none", CanChange: !f.busy, DownloadBytes: 290_000_000, Job: &api.LocalEmbeddingsJob{State: "failed", Error: "setup provider detail"}}, nil
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
		sessions bool
		allowed  bool
	}{
		{"owner local", PrincipalWebLocal, false, false, true}, {"bootstrap", PrincipalBootstrap, false, false, true}, {"static token", PrincipalStaticToken, false, false, true}, {"member", PrincipalDBToken, false, false, false}, {"readonly", PrincipalWebLocal, true, false, false}, {"read-only sessions", PrincipalWebLocal, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &localEmbeddingsFixture{}
			mux := http.NewServeMux()
			cfg := ServerConfig{LocalEmbeddings: fixture, InsecureReadonly: tc.readonly, ReconcilerHealth: func() ReconcilerHealth { return ReconcilerHealth{Configured: true, Backlog: 2} }}
			if tc.sessions {
				manager := newDeterministicSessionManager(t, "http://127.0.0.1:27123", "readonly")
				manager.writable = false
				cfg.WebSessions = manager
			}
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
		name             string
		health           ReconcilerHealth
		done, total      int64
		failed, complete bool
	}{
		{name: "coverage not read", health: ReconcilerHealth{Configured: true}},
		{name: "backfill", health: ReconcilerHealth{Configured: true, Embedded: 40, Skipped: 3, Backlog: 172}, done: 43, total: 215},
		{name: "shared issues deferred", health: ReconcilerHealth{Configured: true, LastSuccessAt: new(time.Time), Embedded: 1, Backlog: 1}, done: 1, total: 2},
		{name: "failed first pass", health: ReconcilerHealth{Configured: true, LastError: "provider reflected example issue text", Backlog: 3}, total: 3, failed: true},
		{name: "refresh failed after success", health: ReconcilerHealth{Configured: true, LastSuccessAt: new(time.Time), LastError: "provider reflected example issue text", Embedded: 1}, done: 1, total: 1, failed: true},
		{name: "recovered", health: ReconcilerHealth{Configured: true, LastSuccessAt: new(time.Time)}, complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health = tc.health
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequestWithContext(WithPrincipal(t.Context(), Principal{Kind: PrincipalWebLocal}), "GET", "/api/v1/embeddings/local", nil))
			require.Equal(t, 200, response.Code)
			var status api.LocalEmbeddingsStatus
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
			require.NotContains(t, response.Body.String(), "provider reflected")
			if tc.complete {
				require.Nil(t, status.Indexing)
				return
			}
			require.NotNil(t, status.Indexing)
			require.Equal(t, tc.failed, status.Indexing.Failed)
			require.Equal(t, tc.done, status.Indexing.Done)
			require.Equal(t, tc.total, status.Indexing.Total)
		})
	}
}
