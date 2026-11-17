package kata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/vector"
)

func TestServiceRejectsEmbeddingIndexError(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	indexErr := errors.New("index setup failed")
	service, err := newService(ctx, Config{DSN: dsn, Auth: AuthConfig{TrustCallerAuthentication: true}, Embeddings: EmbeddingsConfig{BaseURL: "http://127.0.0.1:1234", Model: "test", Dims: 2}}, serviceDeps{openEmbeddingIndex: func(context.Context, *sql.DB) (*vector.Index, error) { return nil, indexErr }})
	require.Nil(t, service)
	require.ErrorIs(t, err, indexErr)
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
