package daemon_test

import (
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
				Configured:    true,
				LastSuccessAt: &last,
				LastError:     "boom",
				Backlog:       5,
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
	assert.Equal(t, "boom", body.Embeddings.LastError)
	require.NotNil(t, body.Embeddings.LastSuccessAt)
	assert.True(t, body.Embeddings.LastSuccessAt.Equal(last))
}
