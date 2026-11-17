package kata_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	endpoint := testenv.NewTwoDimensionalEmbeddingServer(t, nil)
	cfg := kata.Config{
		DSN: dsn, Postgres: kata.PostgresConfig{Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap, SchemaOwner: owner},
		Auth:       kata.AuthConfig{TrustCallerAuthentication: true},
		Embeddings: kata.EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test", Dims: 2, TrustPrivateNetwork: true},
	}
	service, err := kata.New(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, service.Close())
	cfg.Postgres.SchemaMode = kata.PostgresSchemaValidate
	service, err = kata.New(ctx, cfg)
	require.NoError(t, err)
	server := httptest.NewServer(service.Handler())
	project := createProject(t, server.URL, "recovery")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues", server.URL, project.Project.ID), strings.NewReader(`{"title":"recover semantic search","actor":"user-a"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	query := fmt.Sprintf("/api/v1/projects/%d/search?q=recover&mode=", project.Project.ID)
	for mode, expected := range map[string]struct {
		status int
		text   string
	}{"lexical": {http.StatusOK, "recover semantic search"}, "semantic": {http.StatusServiceUnavailable, "pgvector"}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+query+mode, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, expected.status, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), expected.text)
		require.NoError(t, resp.Body.Close())
	}
	server.Close()
	require.NoError(t, service.Close())

	_, err = admin.ExecContext(ctx, `CREATE EXTENSION vector WITH SCHEMA public`)
	require.NoError(t, err)
	cfg.Postgres.SchemaMode = kata.PostgresSchemaBootstrap
	service, err = kata.New(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, service.Close())
	cfg.Postgres.SchemaMode = kata.PostgresSchemaValidate
	service, err = kata.New(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	server = httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	//nolint:kennlint // sleeptest: Postgres and the worker require real I/O.
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+query+"semantic", nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var result struct {
			Results []struct {
				Issue struct {
					Title string `json:"title"`
				} `json:"issue"`
			} `json:"results"`
		}
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&result) == nil && len(result.Results) == 1 && result.Results[0].Issue.Title == "recover semantic search"
	}, 15*time.Second, 100*time.Millisecond)
}
