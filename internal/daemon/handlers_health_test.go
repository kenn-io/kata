package daemon_test

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

func TestHealth_ReportsSchemaAndUptime(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	var body struct {
		OK               bool   `json:"ok"`
		SchemaVersion    int    `json:"schema_version"`
		APISchemaVersion string `json:"api_schema_version"`
		Uptime           string `json:"uptime"`
		DBPath           string `json:"db_path"`
	}
	resp, raw := doReq(t, ts, http.MethodGet, "/api/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.True(t, body.OK)
	assert.Equal(t, db.CurrentSchemaVersion(), body.SchemaVersion)
	assert.Equal(t, daemon.APISchemaVersion, body.APISchemaVersion)
	assert.NotEmpty(t, body.APISchemaVersion)
	assert.NotEmpty(t, body.Uptime)
	assert.NotEmpty(t, body.DBPath)
}

func TestHealthIncludesEffectiveIdleShutdownCapability(t *testing.T) {
	d := openTestDB(t)
	deadline := time.Date(2026, 8, 17, 17, 15, 0, 0, time.UTC)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:        d.db,
		StartedAt: d.now,
		Auth:      config.AuthConfig{Token: "operator-token"},
		IdleShutdownHealth: func() daemon.IdleSnapshot {
			return daemon.IdleSnapshot{
				Timeout:  15 * time.Minute,
				State:    daemon.IdleStateArmed,
				Deadline: deadline,
			}
		},
	})

	var body struct {
		IdleShutdown *api.IdleShutdownHealth `json:"idle_shutdown"`
		LegacyIdle   jsontext.Value          `json:"idle"`
	}
	resp, raw := doReq(t, ts, http.MethodGet, "/api/v1/health", nil,
		map[string]string{"Authorization": "Bearer operator-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &body))
	require.NotNil(t, body.IdleShutdown)
	assert.Nil(t, body.LegacyIdle)
	assert.Equal(t, "15m0s", body.IdleShutdown.Timeout)
	assert.Equal(t, "armed", body.IdleShutdown.State)
	require.NotNil(t, body.IdleShutdown.Deadline)
	assert.True(t, body.IdleShutdown.Deadline.Equal(deadline))
}

func TestHealthOmitsIdleShutdownWhenIneffective(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	var body struct {
		IdleShutdown *api.IdleShutdownHealth `json:"idle_shutdown"`
	}
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
	assert.Nil(t, body.IdleShutdown)
}

func TestHealth_OmitsEmbeddingsWhenUnconfigured(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	var body struct {
		Embeddings *api.EmbeddingsHealth `json:"embeddings"`
	}
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
	assert.Nil(t, body.Embeddings, "embeddings health must be absent when no ReconcilerHealth is wired")
}

func TestHealth_IncludesEmbeddingsWhenConfigured(t *testing.T) {
	d := openTestDB(t)
	last := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	started := last.Add(-time.Minute)
	lastProgress := last.Add(-time.Second)
	rate := 2.5
	eta := int64(2)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:        d.db,
		StartedAt: d.now,
		Auth:      config.AuthConfig{Token: "operator-token"},
		ReconcilerHealth: func() daemon.ReconcilerHealth {
			return daemon.ReconcilerHealth{
				Configured:      true,
				LastSuccessAt:   &last,
				LastError:       "provider reflected issue body: secret project content",
				LastErrorStatus: 400,
				Embedded:        7,
				Skipped:         1,
				Backlog:         5,
				RatePerSecond:   &rate,
				ETASeconds:      &eta,
				StartedAt:       &started,
				LastProgressAt:  &lastProgress,
			}
		},
	})

	var body struct {
		Embeddings *api.EmbeddingsHealth `json:"embeddings"`
	}
	resp, raw := doReq(t, ts, http.MethodGet, "/api/v1/health", nil,
		map[string]string{"Authorization": "Bearer operator-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &body))
	require.NotNil(t, body.Embeddings, "embeddings health must surface when ReconcilerHealth is wired")
	assert.True(t, body.Embeddings.Configured)
	assert.Equal(t, int64(7), body.Embeddings.Embedded)
	assert.Equal(t, int64(1), body.Embeddings.Skipped)
	assert.Equal(t, int64(5), body.Embeddings.Backlog)
	require.NotNil(t, body.Embeddings.RatePerSecond)
	assert.InDelta(t, 2.5, *body.Embeddings.RatePerSecond, 0.001)
	require.NotNil(t, body.Embeddings.ETASeconds)
	assert.Equal(t, int64(2), *body.Embeddings.ETASeconds)
	require.NotNil(t, body.Embeddings.StartedAt)
	assert.True(t, body.Embeddings.StartedAt.Equal(started))
	require.NotNil(t, body.Embeddings.LastProgressAt)
	assert.True(t, body.Embeddings.LastProgressAt.Equal(lastProgress))
	assert.Equal(t, 400, body.Embeddings.LastErrorStatus)
	require.NotNil(t, body.Embeddings.LastSuccessAt)
	assert.True(t, body.Embeddings.LastSuccessAt.Equal(last))
}

func TestHealth_DoesNotExposeEmbeddingProviderDiagnostics(t *testing.T) {
	d := openTestDB(t)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:        d.db,
		StartedAt: d.now,
		ReconcilerHealth: func() daemon.ReconcilerHealth {
			return daemon.ReconcilerHealth{
				Configured:      true,
				LastError:       "embedding endpoint returned 400: reflected issue title",
				LastErrorStatus: 400,
				Backlog:         1,
			}
		},
	})

	resp, bs := doReq(t, ts, http.MethodGet, "/api/v1/health", nil,
		map[string]string{"Forwarded": "for=198.51.100.1"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	assert.NotContains(t, string(bs), "reflected issue title")

	var body map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.NotContains(t, body, "embeddings")
}

func TestHealth_KeylessDaemonRejectsExplicitInvalidAuthorizationBeforeDiagnostics(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	resp, body := doReq(t, ts, http.MethodGet, "/api/v1/health", nil,
		map[string]string{"Authorization": "Bearer unknown-token"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"token_invalid"`)
	assert.NotContains(t, string(body), "db_path")
	assert.NotContains(t, string(body), "federation")
}

func TestHealthReportsEmbeddingTransportErrorWithoutDetails(t *testing.T) {
	d := openTestDB(t)
	ts := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now,
		ReconcilerHealth: func() daemon.ReconcilerHealth {
			return daemon.ReconcilerHealth{Configured: true, LastError: "secret-token transport failure"}
		},
	})
	resp, bs := doReq(t, ts, http.MethodGet, "/api/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotContains(t, string(bs), "secret-token")
	var body struct {
		Embeddings map[string]any `json:"embeddings"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	require.Equal(t, true, body.Embeddings["error_present"])
}

func TestHealthFederationConfigOmitsBlockWhenUnconfigured(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	var body struct {
		FederationConfig *api.FederationConfigHealth `json:"federation_config"`
	}
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
	assert.Nil(t, body.FederationConfig)
}

func TestHealthFederationConfigIncludesSanitizedAggregate(t *testing.T) {
	d := openTestDB(t)
	lastAttempt := time.Date(2026, 7, 23, 4, 0, 0, 0, time.UTC)
	lastSuccess := lastAttempt.Add(-time.Second)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:        d.db,
		StartedAt: d.now,
		Auth:      config.AuthConfig{Token: "operator-token"},
		FederationConfigHealth: func() api.FederationConfigHealth {
			return api.FederationConfigHealth{
				Configured:        4,
				Reconciled:        1,
				Pending:           2,
				Conflicted:        1,
				LastAttemptAt:     &lastAttempt,
				LastSuccessAt:     &lastSuccess,
				LastErrorCategory: "binding_conflict",
				LastErrorStatus:   http.StatusConflict,
			}
		},
	})

	resp, bs := doReq(t, ts, http.MethodGet, "/api/v1/health", nil,
		map[string]string{"Authorization": "Bearer operator-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	var body struct {
		OK               bool                        `json:"ok"`
		FederationConfig *api.FederationConfigHealth `json:"federation_config"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	assert.True(t, body.OK, "pending and conflicted config must remain fail open")
	require.NotNil(t, body.FederationConfig)
	assert.Equal(t, api.FederationConfigHealth{
		Configured:        4,
		Reconciled:        1,
		Pending:           2,
		Conflicted:        1,
		LastAttemptAt:     &lastAttempt,
		LastSuccessAt:     &lastSuccess,
		LastErrorCategory: "binding_conflict",
		LastErrorStatus:   http.StatusConflict,
	}, *body.FederationConfig)

	for _, privateValue := range []string{
		"https://sensitive-hub.example",
		"sensitive-spoke",
		"sensitive-project",
		"sensitive-actor",
		"catalog-secret",
		"reflected response body",
	} {
		assert.NotContains(t, string(bs), privateValue)
	}
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(bs, &fields))
	var federationFields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(fields["federation_config"], &federationFields))
	assert.ElementsMatch(t, []string{
		"configured", "reconciled", "pending", "conflicted",
		"last_attempt_at", "last_success_at", "last_error_category", "last_error_status",
	}, mapKeys(federationFields))
}

func mapKeys(values map[string]jsontext.Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func TestHealthIncludesSafeEmbeddingCredentialWarning(t *testing.T) {
	d := openTestDB(t)
	emb, err := embedding.New(embedding.Config{BaseURL: "http://127.0.0.1:9", Model: "m", Dims: 2})
	require.NoError(t, err)
	emb.SetCredential(config.EmbeddingCredential{Source: "env:EXAMPLE_KEY", Reason: "no embedding API key (env EXAMPLE_KEY is unset)"})
	last := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	ts := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, Embedder: emb, ReconcilerHealth: func() daemon.ReconcilerHealth {
		return daemon.ReconcilerHealth{Configured: true, LastSuccessAt: &last}
	}})
	resp, raw := doReq(t, ts, http.MethodGet, "/api/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var body struct {
		OK         bool                  `json:"ok"`
		Embeddings *api.EmbeddingsHealth `json:"embeddings"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	require.True(t, body.OK)
	require.NotNil(t, body.Embeddings)
	assert.Equal(t, "missing", body.Embeddings.Credential)
	assert.Equal(t, "env:EXAMPLE_KEY", body.Embeddings.CredentialSource)
	assert.Contains(t, body.Embeddings.CredentialReason, "unset")
	require.NotNil(t, body.Embeddings.LastSuccessAt)
	assert.True(t, body.Embeddings.LastSuccessAt.Equal(last), "credential state must not replace reconciliation time")
}

func TestHealthSerializesSanitizedEmbeddingRejection(t *testing.T) {
	d := openTestDB(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("provider echoed example-key"))
	}))
	defer provider.Close()
	emb, err := embedding.New(embedding.Config{BaseURL: provider.URL, Model: "m", Dims: 2, Credential: config.EmbeddingCredential{Key: "example-key", Source: "inline"}})
	require.NoError(t, err)
	_, err = emb.Embed(context.Background(), []string{"query"})
	require.Error(t, err)
	ts := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, Embedder: emb, ReconcilerHealth: func() daemon.ReconcilerHealth {
		return daemon.ReconcilerHealth{Configured: true}
	}})
	var body struct {
		OK         bool                  `json:"ok"`
		Embeddings *api.EmbeddingsHealth `json:"embeddings"`
	}
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
	require.True(t, body.OK)
	require.NotNil(t, body.Embeddings)
	assert.Contains(t, body.Embeddings.LastError, "rejected the API key (401)")
	assert.NotContains(t, body.Embeddings.LastError, "example-key")
	assert.Equal(t, "rejected", body.Embeddings.Credential)
	require.NotNil(t, body.Embeddings.LastErrorAt)
	assert.Equal(t, http.StatusUnauthorized, body.Embeddings.LastErrorStatus)
}
