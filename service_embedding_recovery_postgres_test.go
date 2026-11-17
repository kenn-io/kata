package kata_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/testenv"
)

func TestServicePgvectorRecoveryWithSchemaBootstrap(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var owner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&owner))
	endpoint := testenv.NewEmbeddingServer(t, nil)
	cfg := kata.Config{
		DSN: dsn, Postgres: kata.PostgresConfig{Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap, SchemaOwner: owner},
		Auth:       kata.AuthConfig{TrustCallerAuthentication: true},
		Embeddings: kata.EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test", Dims: 2, TrustPrivateNetwork: true},
	}
	open := func(mode kata.PostgresSchemaMode) *kata.Service {
		cfg.Postgres.SchemaMode = mode
		service, err := kata.New(ctx, cfg)
		require.NoError(t, err)
		return service
	}
	require.NoError(t, open(kata.PostgresSchemaBootstrap).Close())
	service := open(kata.PostgresSchemaValidate)
	server := httptest.NewServer(service.Handler())
	projectID := createProject(t, server.URL, "recovery").Project.ID
	createAccessIssue(t, server, projectID, "recover semantic search")
	status, titles, _ := searchTitles(ctx, server, projectID, "recover", "lexical")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"recover semantic search"}, titles)
	status, titles, body := searchTitles(ctx, server, projectID, "recover", "auto")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{"recover semantic search"}, titles)
	var auto struct {
		Mode           string `json:"mode"`
		Degraded       bool   `json:"degraded"`
		DegradedReason string `json:"degraded_reason"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &auto))
	require.Equal(t, "lexical", auto.Mode)
	require.True(t, auto.Degraded)
	require.Contains(t, auto.DegradedReason, "pgvector")
	for _, mode := range []string{"semantic", "hybrid"} {
		status, _, body := searchTitles(ctx, server, projectID, "recover", mode)
		require.Equal(t, http.StatusServiceUnavailable, status)
		require.Contains(t, body, "pgvector")
	}
	server.Close()
	require.NoError(t, service.Close())

	_, err = admin.ExecContext(ctx, `CREATE EXTENSION vector WITH SCHEMA public`)
	require.NoError(t, err)
	service, err = kata.New(ctx, cfg)
	require.Nil(t, service)
	require.ErrorContains(t, err, "semantic-search tables are not installed")
	require.NoError(t, open(kata.PostgresSchemaBootstrap).Close())
	service = open(kata.PostgresSchemaValidate)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	server = httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	//nolint:kennlint // sleeptest: Postgres and the worker require real I/O.
	require.Eventually(t, func() bool {
		status, titles, _ := searchTitles(ctx, server, projectID, "recover", "semantic")
		return status == http.StatusOK && len(titles) == 1 && titles[0] == "recover semantic search"
	}, 15*time.Second, 100*time.Millisecond)
}
