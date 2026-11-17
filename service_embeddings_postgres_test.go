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
	endpoint := testenv.NewTwoDimensionalEmbeddingServer(t, func(input string) []float32 {
		switch {
		case input == "database contention" || strings.Contains(input, "serialize concurrent writers"):
			return []float32{1, 0}
		case strings.Contains(input, "deadlock"):
			return []float32{0, 1}
		}
		return []float32{1, 1}
	})
	cfg := kata.Config{DSN: dsn, Auth: kata.AuthConfig{TrustCallerAuthentication: true}, Embeddings: kata.EmbeddingsConfig{BaseURL: endpoint.URL, Model: "test-model", Dims: 2, TrustPrivateNetwork: true}}
	service, err := kata.New(ctx, cfg)
	require.NoError(t, err)
	server := httptest.NewServer(service.Handler())
	projectID := createProject(t, server.URL, "embeddings").Project.ID
	createAccessIssue(t, server, projectID, "serialize concurrent writers")
	createAccessIssue(t, server, projectID, "refresh dashboard")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	topResult := func(query, mode, want string) func() bool {
		return func() bool {
			status, titles, _ := searchTitles(ctx, server, projectID, query, mode)
			return status == http.StatusOK && len(titles) > 0 && titles[0] == want
		}
	}
	//nolint:kennlint // sleeptest: Postgres and HTTP worker progress requires real I/O.
	require.Eventually(t, topResult("database contention", "semantic", "serialize concurrent writers"), 20*time.Second, 100*time.Millisecond)
	require.True(t, topResult("database contention", "hybrid", "serialize concurrent writers")())
	createAccessIssue(t, server, projectID, "fix deadlock")
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
