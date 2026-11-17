package kata_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/testenv"
)

func TestServiceRejectsEmbeddingIndexWithoutTables(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var owner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&owner))
	cfg := kata.Config{DSN: dsn, Postgres: kata.PostgresConfig{Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap, SchemaOwner: owner}, Auth: kata.AuthConfig{TrustCallerAuthentication: true}}
	service, err := kata.New(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, service.Close())
	_, err = admin.ExecContext(ctx, `CREATE EXTENSION vector WITH SCHEMA public`)
	require.NoError(t, err)
	cfg.Postgres.SchemaMode = kata.PostgresSchemaValidate
	cfg.Embeddings = kata.EmbeddingsConfig{BaseURL: "http://127.0.0.1:1234", Model: "test", Dims: 2}
	service, err = kata.New(ctx, cfg)
	require.Nil(t, service)
	require.ErrorContains(t, err, "embedding index")
	require.ErrorContains(t, err, "semantic-search tables are not installed")
}
