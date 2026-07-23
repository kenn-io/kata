package daemon_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
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
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
	assert.True(t, body.OK)
	assert.Equal(t, db.CurrentSchemaVersion(), body.SchemaVersion)
	assert.Equal(t, daemon.APISchemaVersion, body.APISchemaVersion)
	assert.NotEmpty(t, body.APISchemaVersion)
	assert.NotEmpty(t, body.Uptime)
	assert.NotEmpty(t, body.DBPath)
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
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
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

	resp, bs := doReq(t, ts, http.MethodGet, "/api/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	assert.NotContains(t, string(bs), "reflected issue title")

	var body struct {
		Embeddings map[string]json.RawMessage `json:"embeddings"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	_, hasLastError := body.Embeddings["last_error"]
	assert.False(t, hasLastError, "unauthenticated health must omit raw embedding provider diagnostics")
	assert.Contains(t, body.Embeddings, "last_error_status")
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

	resp, bs := doReq(t, ts, http.MethodGet, "/api/v1/health", nil, nil)
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
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(bs, &fields))
	var federationFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["federation_config"], &federationFields))
	assert.ElementsMatch(t, []string{
		"configured", "reconciled", "pending", "conflicted",
		"last_attempt_at", "last_success_at", "last_error_category", "last_error_status",
	}, mapKeys(federationFields))
}

func mapKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
