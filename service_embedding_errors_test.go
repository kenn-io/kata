package kata

import (
	"context"
	"database/sql"
	"errors"
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
