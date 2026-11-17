package kata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/testenv"
)

func TestServicePostgresEmbeddingSearch(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	var documentCalls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i, input := range req.Input {
			v := []float32{0, 1}
			if strings.Contains(input, "serialize concurrent writers") || input == "database contention" {
				v = []float32{1, 0}
			}
			if input != "database contention" {
				documentCalls.Add(1)
			}
			data[i] = map[string]any{"embedding": v, "index": i}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(endpoint.Close)
	cfg := kata.Config{DSN: dsn, Auth: kata.AuthConfig{TrustCallerAuthentication: true}, Embeddings: kata.EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test-model", Dims: 2, TrustPrivateNetwork: true}}
	service, err := kata.New(ctx, cfg)
	require.NoError(t, err)
	server := httptest.NewServer(service.Handler())
	project := createProject(t, server.URL, "embeddings")
	for _, title := range []string{"serialize concurrent writers", "refresh dashboard"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues", server.URL, project.Project.ID), strings.NewReader(fmt.Sprintf(`{"title":%q,"actor":"user-a"}`, title)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	search := fmt.Sprintf("%s/api/v1/projects/%d/search?q=database+contention&mode=semantic", server.URL, project.Project.ID)
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, search, nil)
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
		return resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&result) == nil && len(result.Results) > 0 && result.Results[0].Issue.Title == "serialize concurrent writers"
	}, 20*time.Second, 100*time.Millisecond)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(search, "mode=semantic", "mode=hybrid", 1), nil)
	require.NoError(t, err)
	hybrid, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, hybrid.StatusCode)
	var hybridResult struct {
		Results []struct {
			Issue struct {
				Title string `json:"title"`
			} `json:"issue"`
		} `json:"results"`
	}
	require.NoError(t, json.NewDecoder(hybrid.Body).Decode(&hybridResult))
	require.NoError(t, hybrid.Body.Close())
	require.NotEmpty(t, hybridResult.Results)
	require.Equal(t, "serialize concurrent writers", hybridResult.Results[0].Issue.Title)
	cancel()
	require.NoError(t, <-done)
	server.Close()
	require.NoError(t, service.Close())
	count := documentCalls.Load()
	service, err = kata.New(ctx, cfg)
	require.NoError(t, err)
	defer func() { _ = service.Close() }()
	server = httptest.NewServer(service.Handler())
	defer server.Close()
	search = fmt.Sprintf("%s/api/v1/projects/%d/search?q=database+contention&mode=semantic", server.URL, project.Project.ID)
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, search, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		Results []struct {
			Issue struct {
				Title string `json:"title"`
			} `json:"issue"`
		} `json:"results"`
	}
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.NotEmpty(t, result.Results)
	require.Equal(t, "serialize concurrent writers", result.Results[0].Issue.Title)
	require.Equal(t, count, documentCalls.Load())
}

func TestServicePostgresWithoutPgvector(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPlainPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer endpoint.Close()
	service, err := kata.New(ctx, kata.Config{DSN: dsn, Auth: kata.AuthConfig{TrustCallerAuthentication: true}, Embeddings: kata.EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test", Dims: 2, TrustPrivateNetwork: true}})
	require.NoError(t, err)
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	project := createProject(t, server.URL, "plain-postgres")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	for _, tc := range []struct {
		mode     string
		status   int
		contains string
	}{{"semantic", 503, "pgvector"}, {"hybrid", 503, "pgvector"}, {"auto", 200, "pgvector"}, {"lexical", 200, "results"}} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/search?q=example&mode=%s", server.URL, project.Project.ID, tc.mode), nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		require.NoError(t, resp.Body.Close())
		require.Equal(t, tc.status, resp.StatusCode)
		require.Contains(t, fmt.Sprint(body), tc.contains)
	}
	require.Zero(t, calls.Load())
}

func TestServiceEmbedsIssueCreatedWhileRunning(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresWithPgvectorContainer(t, ctx)
	t.Cleanup(cleanup)
	endpoint := testenv.NewTwoDimensionalEmbeddingServer(t, nil)
	service, err := kata.New(ctx, kata.Config{DSN: dsn, Auth: kata.AuthConfig{TrustCallerAuthentication: true}, Embeddings: kata.EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test-model", Dims: 2, TrustPrivateNetwork: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	project := createProject(t, server.URL, "event-test")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	query := fmt.Sprintf("%s/api/v1/projects/%d/search?q=matching&mode=semantic", server.URL, project.Project.ID)
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, query, nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 100*time.Millisecond)
	healthReq, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/health", nil)
	require.NoError(t, err)
	healthResp, err := http.DefaultClient.Do(healthReq)
	require.NoError(t, err)
	var health struct {
		Embeddings *struct {
			Configured    bool       `json:"configured"`
			LastSuccessAt *time.Time `json:"last_success_at"`
		} `json:"embeddings"`
	}
	require.NoError(t, json.NewDecoder(healthResp.Body).Decode(&health))
	require.NoError(t, healthResp.Body.Close())
	require.NotNil(t, health.Embeddings)
	require.True(t, health.Embeddings.Configured)
	require.NotNil(t, health.Embeddings.LastSuccessAt)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues", server.URL, project.Project.ID), strings.NewReader(`{"title":"new issue after startup","actor":"user-a"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, query, nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct {
			Results []struct {
				Issue struct {
					Title string `json:"title"`
				} `json:"issue"`
			} `json:"results"`
		}
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil && len(body.Results) == 1 && body.Results[0].Issue.Title == "new issue after startup"
	}, 10*time.Second, 100*time.Millisecond)
}
