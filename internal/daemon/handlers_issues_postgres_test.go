package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestCreateConcurrentIdempotencyAcrossPostgresDaemons(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	firstStore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = firstStore.Close() })
	secondStore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = secondStore.Close() })
	project, err := firstStore.CreateProject(ctx, "idempotency-project")
	require.NoError(t, err)

	firstDaemon := daemon.NewServer(daemon.ServerConfig{
		DB: firstStore, StartedAt: time.Now().UTC(),
	})
	t.Cleanup(func() { _ = firstDaemon.Close() })
	firstServer := httptest.NewServer(firstDaemon.Handler())
	t.Cleanup(firstServer.Close)
	secondDaemon := daemon.NewServer(daemon.ServerConfig{
		DB: secondStore, StartedAt: time.Now().UTC(),
	})
	t.Cleanup(func() { _ = secondDaemon.Close() })
	secondServer := httptest.NewServer(secondDaemon.Handler())
	t.Cleanup(secondServer.Close)

	const requestCount = 16
	start := make(chan struct{})
	results := make(chan concurrentCreateResult, requestCount)
	var ready sync.WaitGroup
	ready.Add(requestCount)
	for i := range requestCount {
		baseURL := firstServer.URL
		if i%2 == 1 {
			baseURL = secondServer.URL
		}
		go func() {
			ready.Done()
			<-start
			results <- postConcurrentCreate(ctx, baseURL, project.ID)
		}()
	}
	ready.Wait()
	close(start)

	uids := make(map[string]struct{})
	changed := 0
	reused := 0
	for range requestCount {
		result := <-results
		require.NoError(t, result.err)
		require.Equalf(t, http.StatusOK, result.status, "response: %s", result.body)
		var body struct {
			Issue struct {
				UID string `json:"uid"`
			} `json:"issue"`
			Changed bool `json:"changed"`
			Reused  bool `json:"reused"`
		}
		require.NoError(t, json.Unmarshal(result.body, &body))
		require.NotEmpty(t, body.Issue.UID)
		uids[body.Issue.UID] = struct{}{}
		if body.Changed {
			changed++
		}
		if body.Reused {
			reused++
		}
	}
	assert.Len(t, uids, 1)
	assert.Equal(t, 1, changed)
	assert.Equal(t, requestCount-1, reused)

	issues, err := firstStore.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	assert.Len(t, issues, 1)
	events, err := firstStore.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	createdEvents := 0
	for _, event := range events {
		if event.Type == "issue.created" {
			createdEvents++
		}
	}
	assert.Equal(t, 1, createdEvents)
}

func TestPostgresCreateInvalidTitlesReturnBadRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "invalid-title-project")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { _ = server.Close() })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	for _, title := range []string{" \t\n ", "before\x00after"} {
		payload, err := json.Marshal(map[string]string{"actor": "worker", "title": title})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			httpServer.URL+issuesURL(project.ID), bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // httptest URL is test-controlled
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equalf(t, http.StatusBadRequest, resp.StatusCode, "response: %s", body)
	}
	count, err := store.CountOpenIssues(ctx, project.ID)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestCreateConcurrentDistinctIdempotencyKeysWithBoundedPostgresPool(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	firstStore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = firstStore.Close() })
	secondStore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = secondStore.Close() })
	project, err := firstStore.CreateProject(ctx, "bounded-pool-project")
	require.NoError(t, err)
	firstStore.SetMaxOpenConns(2)
	secondStore.SetMaxOpenConns(2)

	firstDaemon := daemon.NewServer(daemon.ServerConfig{
		DB: firstStore, StartedAt: time.Now().UTC(),
	})
	t.Cleanup(func() { _ = firstDaemon.Close() })
	firstServer := httptest.NewServer(firstDaemon.Handler())
	t.Cleanup(firstServer.Close)
	secondDaemon := daemon.NewServer(daemon.ServerConfig{
		DB: secondStore, StartedAt: time.Now().UTC(),
	})
	t.Cleanup(func() { _ = secondDaemon.Close() })
	secondServer := httptest.NewServer(secondDaemon.Handler())
	t.Cleanup(secondServer.Close)

	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	const requestCount = 16
	start := make(chan struct{})
	results := make(chan concurrentCreateResult, requestCount)
	var ready sync.WaitGroup
	ready.Add(requestCount)
	for i := range requestCount {
		baseURL := firstServer.URL
		if i%2 == 1 {
			baseURL = secondServer.URL
		}
		go func() {
			ready.Done()
			<-start
			key := fmt.Sprintf("distinct-request-%02d", i)
			results <- postConcurrentCreateWithKey(requestCtx, baseURL, project.ID, key, key)
		}()
	}
	ready.Wait()
	close(start)

	succeeded := 0
	for range requestCount {
		result := <-results
		if result.err == nil && result.status == http.StatusOK {
			succeeded++
		}
	}
	assert.Equal(t, requestCount, succeeded,
		"distinct keys must not exhaust the main query pool while holding advisory locks")
}

type concurrentCreateResult struct {
	status int
	body   []byte
	err    error
}

func postConcurrentCreate(ctx context.Context, baseURL string, projectID int64) concurrentCreateResult {
	return postConcurrentCreateWithKey(ctx, baseURL, projectID,
		"concurrent-request", "retry-safe create")
}

func postConcurrentCreateWithKey(
	ctx context.Context,
	baseURL string,
	projectID int64,
	key, title string,
) concurrentCreateResult {
	payload, err := json.Marshal(map[string]any{
		"actor": "worker", "title": title, "body": "one logical request", "force_new": true,
	})
	if err != nil {
		return concurrentCreateResult{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+issuesURL(projectID), bytes.NewReader(payload))
	if err != nil {
		return concurrentCreateResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return concurrentCreateResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return concurrentCreateResult{status: resp.StatusCode, body: body, err: err}
}
