package kata

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestServiceRejectsEmbeddingIndexError(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var owner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&owner))
	cfg := Config{DSN: dsn, Postgres: PostgresConfig{Schema: "kata", SchemaMode: PostgresSchemaBootstrap, SchemaOwner: owner}, Auth: AuthConfig{TrustCallerAuthentication: true}}
	service, err := New(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, service.Close())
	_, err = admin.ExecContext(ctx, `CREATE EXTENSION vector WITH SCHEMA public`)
	require.NoError(t, err)
	cfg.Postgres.SchemaMode = PostgresSchemaValidate
	cfg.Embeddings = EmbeddingsConfig{BaseURL: "http://127.0.0.1:1234", Model: "test", Dims: 2}
	service, err = New(ctx, cfg)
	require.Nil(t, service)
	require.ErrorContains(t, err, "embedding index")
	require.ErrorContains(t, err, "semantic-search tables are not installed")
}

func TestServiceLocalEmbeddingsDefaultAndMissingIndexHealth(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPlainPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	service, err := New(ctx, Config{DSN: dsn, Auth: AuthConfig{Token: "test-token"}, Embeddings: EmbeddingsConfig{Local: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/health", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp := httptest.NewRecorder()
	service.Handler().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code)
	var body struct {
		Embeddings *struct {
			Configured   bool   `json:"configured"`
			ErrorPresent bool   `json:"error_present"`
			LastError    string `json:"last_error"`
		} `json:"embeddings"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	require.NotNil(t, body.Embeddings)
	require.True(t, body.Embeddings.Configured)
	require.True(t, body.Embeddings.ErrorPresent)
	require.Contains(t, body.Embeddings.LastError, "pgvector")
}
