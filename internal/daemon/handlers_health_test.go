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
	ts := startTestServer(t, daemon.ServerConfig{
		DB:        d.db,
		StartedAt: d.now,
		ReconcilerHealth: func() daemon.ReconcilerHealth {
			return daemon.ReconcilerHealth{
				Configured:      true,
				LastSuccessAt:   &last,
				LastError:       "provider reflected issue body: secret project content",
				LastErrorStatus: 400,
				Backlog:         5,
			}
		},
	})

	var body struct {
		Embeddings *api.EmbeddingsHealth `json:"embeddings"`
	}
	getAndUnmarshal(t, ts, "/api/v1/health", http.StatusOK, &body)
	require.NotNil(t, body.Embeddings, "embeddings health must surface when ReconcilerHealth is wired")
	assert.True(t, body.Embeddings.Configured)
	assert.Equal(t, int64(5), body.Embeddings.Backlog)
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
