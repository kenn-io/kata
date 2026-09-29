package daemon_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/notionsync"
)

func TestGitHubStatusSyncEnablePreservesMode(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	body := map[string]any{"config": map[string]any{"owner": "example-owner", "repo": "example-repo"}, "status_sync": "two-way"}
	resp, raw := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out issueSyncResponseBody
	decodeJSON(t, raw, &out)
	require.Equal(t, "two-way", out.Binding.Config["status_sync"])
	delete(body, "status_sync")
	resp, raw = postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	decodeJSON(t, raw, &out)
	require.Equal(t, "two-way", out.Binding.Config["status_sync"])
	body["status_sync"] = "one-way"
	resp, raw = postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	decodeJSON(t, raw, &out)
	require.Equal(t, "one-way", out.Binding.Config["status_sync"])
}

func TestNotionStatusSyncGroupsAndOverrides(t *testing.T) {
	h := newNotionLifecycleHarness(t, nil)
	h.fetcher.source.Properties[1].Options = append(h.fetcher.source.Properties[1].Options, notionsync.Option{ID: "todo", Name: "Ready"})
	h.fetcher.source.Properties[1].Groups = []notionsync.Group{{ID: "complete", Name: "Complete", OptionIDs: []string{"done", "closed"}}, {ID: "todo-group", Name: "To-do", OptionIDs: []string{"todo"}}}
	out := h.enable(t, map[string]any{"status_sync": "two-way", "config": map[string]any{"data_source_id": notionSourceID, "closed_status": "Closed"}}, 200)
	require.Equal(t, "two-way", out.Binding.Config["status_sync"])
	require.Equal(t, "complete", out.Binding.Config["complete_group_id"])
	require.Equal(t, "closed", out.Binding.Config["closed_status_id"])
	h.fetcher.source.Properties[1].Groups[0].Name = "Renamed"
	out = h.enable(t, map[string]any{}, 200)
	require.Equal(t, "closed", out.Binding.Config["closed_status_id"])
	out = h.enable(t, map[string]any{"config": map[string]any{"closed_status": ""}}, 200)
	require.NotContains(t, out.Binding.Config, "closed_status_id")
	h.enable(t, map[string]any{"config": map[string]any{"done_statuses": []string{"Delivered"}}}, 400)
	stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	cfg, err := notionsync.DecodeConfig(stored.Config)
	require.NoError(t, err)
	require.Equal(t, "two-way", cfg.StatusSync)
}

func TestIssueStatusSyncCapabilityAndPrivateConfig(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	resp, err := h.server.Client().Get(h.server.URL + "/api/v1/instance")
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var instance map[string]any
	decodeJSON(t, raw, &instance)
	require.Equal(t, true, instance["issue_status_sync"])
	resp, raw = postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{"config": map[string]any{"owner": "example-owner", "repo": "example-repo"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out map[string]any
	decodeJSON(t, raw, &out)
	require.Equal(t, "one-way", out["status"].(map[string]any)["status_sync"])
	require.Equal(t, float64(0), out["status"].(map[string]any)["pending_count"])
}

func TestIssueStatusSyncInvalidModesDoNotEnable(t *testing.T) {
	for _, mode := range []any{"", "invalid", true, 17} {
		h := newGitHubSyncHandlerHarness(t)
		resp, raw := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{"status_sync": mode, "config": map[string]any{"owner": "example-owner", "repo": "example-repo"}})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	}
}

func TestIssueStatusSyncConfigDoesNotExposeScanCheckpoint(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	resp, raw := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{"config": map[string]any{"owner": "example-owner", "repo": "example-repo"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	binding, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	claimAt := time.Now().UTC().Truncate(time.Millisecond)
	_, ok, err := h.store.ClaimIssueSyncBinding(context.Background(), binding.ID, "github", claimAt, claimAt.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	scanner := h.store.(db.IssueStatusScanStore)
	_, err = scanner.UpdateIssueStatusScan(context.Background(), db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "github", StartedAt: claimAt}, db.IssueStatusScanState{Pending: db.IssueStatusScanCursor{Through: 7}})
	require.NoError(t, err)
	resp, err = h.server.Client().Get(h.server.URL + githubSyncEndpoint(h.project.ID, "status"))
	require.NoError(t, err)
	raw, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NotContains(t, string(raw), "_status_sync")
}

func TestIssueStatusSyncPendingCountPersistsWhilePaused(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	body := map[string]any{"status_sync": "two-way", "config": map[string]any{"owner": "example-owner", "repo": "example-repo"}}
	resp, raw := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	binding, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	issue, _, err := h.store.CreateIssue(context.Background(), db.CreateIssueParams{ProjectID: h.project.ID, Title: "Example task", Author: "worker"})
	require.NoError(t, err)
	_, err = h.store.UpsertImportMapping(context.Background(), db.ImportMappingParams{ProjectID: h.project.ID, Source: binding.SourceKey, ExternalID: "I_example", ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	_, _, _, err = h.store.CloseIssue(context.Background(), issue.ID, "done", "worker", "Completed example task", nil)
	require.NoError(t, err)
	for _, action := range []string{"disable", "enable"} {
		request := map[string]any{}
		if action == "enable" {
			request = body
		}
		resp, raw = postJSON(t, h.server, githubSyncEndpoint(h.project.ID, action), request)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		var out map[string]any
		decodeJSON(t, raw, &out)
		require.Equal(t, float64(1), out["status"].(map[string]any)["pending_count"])
	}
	body["status_sync"] = "one-way"
	resp, raw = postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out map[string]any
	decodeJSON(t, raw, &out)
	require.Equal(t, float64(0), out["status"].(map[string]any)["pending_count"])
}

type statusSyncEnableRaceFetcher struct {
	*fakeGitHubSyncFetcher
	beforeRepository func()
}

func (f statusSyncEnableRaceFetcher) Repository(ctx context.Context, host, owner, repo string) (githubsync.Repository, error) {
	f.beforeRepository()
	return f.fakeGitHubSyncFetcher.Repository(ctx, host, owner, repo)
}

func TestGitHubStatusSyncEnableRejectsConcurrentModeChange(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	body := map[string]any{"status_sync": "two-way", "config": map[string]any{"owner": "example-owner", "repo": "example-repo"}}
	resp, raw := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	binding, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	fetcher := statusSyncEnableRaceFetcher{fakeGitHubSyncFetcher: h.fetcher, beforeRepository: func() {
		cfg, err := githubsync.DecodeConfig(binding.Config)
		require.NoError(t, err)
		cfg.StatusSync = "one-way"
		config, err := githubsync.EncodeConfig(cfg)
		require.NoError(t, err)
		_, err = h.store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: h.project.ID, Provider: binding.Provider, SourceKey: binding.SourceKey, RemoteID: binding.RemoteID, DisplayName: binding.DisplayName, Config: config, IntervalSeconds: binding.IntervalSeconds})
		require.NoError(t, err)
	}}
	server := startTestServer(t, daemon.ServerConfig{DB: h.store, GitHubSyncFetcher: fetcher})
	delete(body, "status_sync")
	resp, raw = postJSON(t, server, githubSyncEndpoint(h.project.ID, "enable"), body)
	require.Equal(t, http.StatusConflict, resp.StatusCode, string(raw))
	stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	cfg, err := githubsync.DecodeConfig(stored.Config)
	require.NoError(t, err)
	require.Equal(t, "one-way", cfg.StatusSync)
}

func TestNotionStatusSyncEmptyGroupRejected(t *testing.T) {
	for _, key := range []string{"complete_group", "todo_group"} {
		h := newNotionLifecycleHarness(t, nil)
		h.fetcher.source.Properties[1].Groups = []notionsync.Group{{ID: "complete", Name: "Complete", OptionIDs: []string{"done"}}, {ID: "todo-group", Name: "To-do", OptionIDs: []string{"closed"}}}
		h.enable(t, map[string]any{"config": map[string]any{"data_source_id": notionSourceID, key: ""}}, 400)
	}
}
