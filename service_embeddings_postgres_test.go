package kata_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	var providerCalls atomic.Int32
	var deniedTextCalls atomic.Int32
	var deniedQueryCalls atomic.Int32
	var allowDenied atomic.Bool
	var deniedUID atomic.Value
	endpoint := testenv.NewEmbeddingServer(t, func(input string) []float32 {
		if strings.Contains(input, "private indexing example") {
			deniedTextCalls.Add(1)
		}
		if input == "private semantic query" {
			deniedQueryCalls.Add(1)
		}
		switch {
		case input == "database contention" || strings.Contains(input, "serialize concurrent writers"):
			return []float32{1, 0}
		case strings.Contains(input, "deadlock"):
			return []float32{0, 1}
		}
		return []float32{1, 1}
	}, func() { providerCalls.Add(1) })
	cfg := kata.Config{DSN: dsn, Auth: kata.AuthConfig{TrustCallerAuthentication: true}, Embeddings: kata.EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test-model", Dims: 2, TrustPrivateNetwork: true}, EmbeddingProjectAllowed: func(_ context.Context, uid string) (bool, error) {
		return uid != deniedUID.Load() || allowDenied.Load(), nil
	}}
	service, err := kata.New(ctx, cfg)
	require.NoError(t, err)
	server := httptest.NewServer(service.Handler())
	projectID := createProject(t, server.URL, "embeddings").Project.ID
	denied := createProject(t, server.URL, "restricted-embeddings").Project
	deniedUID.Store(denied.UID)
	createAccessIssue(t, server, projectID, "serialize concurrent writers")
	createAccessIssue(t, server, projectID, "refresh dashboard")
	createAccessIssue(t, server, denied.ID, "private indexing example")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.RunSemanticIndex(runCtx) }()
	topResult := func(query, mode, want string) func() bool {
		return func() bool {
			status, titles, _ := searchTitles(ctx, server, projectID, query, mode)
			return status == http.StatusOK && len(titles) > 0 && titles[0] == want
		}
	}
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, topResult("database contention", "semantic", "serialize concurrent writers"), 20*time.Second, 100*time.Millisecond)
	require.Zero(t, deniedTextCalls.Load())
	status, _, body := searchTitles(ctx, server, denied.ID, "private semantic query", "semantic")
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.Zero(t, deniedQueryCalls.Load())
	allowDenied.Store(true)
	createAccessIssue(t, server, projectID, "fix deadlock")
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, func() bool { return deniedTextCalls.Load() > 0 }, 20*time.Second, 100*time.Millisecond)
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, func() bool {
		status, titles, _ := searchTitles(ctx, server, denied.ID, "private semantic query", "semantic")
		return status == http.StatusOK && len(titles) > 0 && titles[0] == "private indexing example"
	}, 20*time.Second, 100*time.Millisecond)
	require.Positive(t, deniedQueryCalls.Load())
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, topResult("deadlock", "semantic", "fix deadlock"), 10*time.Second, 100*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	server.Close()
	require.NoError(t, service.Close())
	service, err = kata.New(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	server = httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	beforeRestart := providerCalls.Load()
	runCtx, cancel = context.WithCancel(ctx)
	done = make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	//nolint:kennlint // sleeptest: the worker needs time to perform its startup pass.
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/health", nil)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var health struct {
			Embeddings *struct {
				LastSuccessAt *time.Time `json:"last_success_at"`
			} `json:"embeddings"`
		}
		return json.NewDecoder(resp.Body).Decode(&health) == nil && health.Embeddings != nil && health.Embeddings.LastSuccessAt != nil
	}, 15*time.Second, 100*time.Millisecond)
	require.Equal(t, beforeRestart, providerCalls.Load())
	require.True(t, topResult("database contention", "semantic", "serialize concurrent writers")())
}

// searchTitles reports status 0 on transport errors so it can poll inside
// require.Eventually.
func searchTitles(ctx context.Context, server *httptest.Server, projectID int64, query, mode string) (int, []string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/search?q=%s&mode=%s", server.URL, projectID, url.QueryEscape(query), mode), nil)
	if err != nil {
		return 0, nil, ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, ""
	}
	var result struct {
		Results []struct {
			Issue struct {
				Title string `json:"title"`
			} `json:"issue"`
		} `json:"results"`
	}
	_ = json.Unmarshal(body, &result)
	titles := make([]string, len(result.Results))
	for i, r := range result.Results {
		titles[i] = r.Issue.Title
	}
	return resp.StatusCode, titles, string(body)
}
