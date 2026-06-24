package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
)

func TestGitHubSyncHandlers_EnableValidatesStoresStableSourceAndWakesRunner(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	h.fetcher.repo = githubsync.Repository{
		NodeID:   "R_exampleNode",
		ID:       12345,
		FullName: "example-owner/example-repo-renamed",
	}
	sub := h.broadcaster.Subscribe(daemon.SubFilter{ProjectID: h.project.ID})
	defer sub.Unsub()

	resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{
		"config": map[string]any{
			"owner": "example-owner",
			"repo":  "example-repo",
		},
		"interval_seconds": 120,
	})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "enable: %s", string(body))

	var out issueSyncResponseBody
	decodeJSON(t, body, &out)
	require.NotNil(t, out.Binding)
	assert.Equal(t, h.project.ID, out.Binding.ProjectID)
	assert.Equal(t, "github", out.Binding.Provider)
	assert.Equal(t, "R_exampleNode", out.Binding.RemoteID)
	assert.Equal(t, "example-owner/example-repo-renamed", out.Binding.DisplayName)
	assert.Equal(t, "github:R_exampleNode", out.Binding.SourceKey)
	assert.Equal(t, map[string]any{
		"host":         "github.com",
		"owner":        "example-owner",
		"repo":         "example-repo-renamed",
		"repo_id":      float64(12345),
		"title_prefix": true,
	}, out.Binding.Config)
	assert.True(t, out.Binding.Enabled)
	assert.Equal(t, 120, out.Binding.IntervalSeconds)
	assert.Equal(t, "enabled", out.Status.State)
	assert.True(t, out.Status.Enabled)
	assert.Equal(t, out.Binding.ID, out.Status.BindingID)
	assert.Equal(t, 1, h.fetcher.repositoryCalls)
	assert.Equal(t, []fakeRepositoryCall{{
		host:  "github.com",
		owner: "example-owner",
		repo:  "example-repo",
	}}, h.fetcher.repositoryCallLog)
	assert.Equal(t, 1, h.wakeCount())

	stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	assert.Equal(t, out.Binding.SourceKey, stored.SourceKey)
	assert.Equal(t, "github", stored.Provider)
	assert.Equal(t, out.Binding.RemoteID, stored.RemoteID)
	assert.Equal(t, out.Binding.DisplayName, stored.DisplayName)
	assert.JSONEq(t, `{"host":"github.com","owner":"example-owner","repo":"example-repo-renamed","repo_id":12345,"title_prefix":true}`, string(stored.Config))
	assertNoReceive(t, sub.Ch, 50*time.Millisecond, "enable should not broadcast normal events")
	assert.Empty(t, h.hooks.snapshot(), "enable should not enqueue hook events")
}

func TestIssueSyncHandlers_EnableGitHubUsesProviderNeutralContract(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	h.fetcher.repo = githubsync.Repository{
		NodeID:   "R_exampleNode",
		ID:       12345,
		FullName: "example-owner/example-repo-renamed",
	}

	resp, body := postJSON(t, h.server, issueSyncEndpoint(h.project.ID, "github", "enable"), map[string]any{
		"config": map[string]any{
			"owner": "example-owner",
			"repo":  "example-repo",
		},
		"interval_seconds": 120,
	})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "enable: %s", string(body))

	var out issueSyncResponseBody
	decodeJSON(t, body, &out)
	require.NotNil(t, out.Binding)
	assert.Equal(t, h.project.ID, out.Binding.ProjectID)
	assert.Equal(t, "github", out.Binding.Provider)
	assert.Equal(t, "github:R_exampleNode", out.Binding.SourceKey)
	assert.Equal(t, "R_exampleNode", out.Binding.RemoteID)
	assert.Equal(t, "example-owner/example-repo-renamed", out.Binding.DisplayName)
	assert.Equal(t, map[string]any{
		"host":         "github.com",
		"owner":        "example-owner",
		"repo":         "example-repo-renamed",
		"repo_id":      float64(12345),
		"title_prefix": true,
	}, out.Binding.Config)
	assert.True(t, out.Binding.Enabled)
	assert.Equal(t, 120, out.Binding.IntervalSeconds)
	assert.Equal(t, "enabled", out.Status.State)
	assert.True(t, out.Status.Enabled)
	assert.Equal(t, out.Binding.ID, out.Status.BindingID)
	assert.NotContains(t, string(body), "repo_node_id")
}

func TestGitHubSyncHandlers_EnableValidationErrors(t *testing.T) {
	t.Run("requires owner before fetching", func(t *testing.T) {
		h := newGitHubSyncHandlerHarness(t)
		resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{
			"config": map[string]any{"repo": "example-repo"},
		})
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		assert.Equal(t, 0, h.fetcher.repositoryCalls)
		assert.Equal(t, 0, h.wakeCount())
	})

	t.Run("maps fetcher failure to redacted validation error", func(t *testing.T) {
		h := newGitHubSyncHandlerHarness(t)
		h.fetcher.repositoryErr = errors.New("gh api failed: token [redacted] cannot access repository")

		resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{
			"config": map[string]any{
				"owner": "example-owner",
				"repo":  "example-repo",
			},
		})
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		assert.Contains(t, string(body), "[redacted]")
		assert.NotContains(t, string(body), "ghp_")
		assert.Equal(t, 1, h.fetcher.repositoryCalls)
		assert.Equal(t, 0, h.wakeCount())
	})
}

func TestGitHubSyncHandlers_EnableAllowsFederationHub(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	_, err := h.store.EnableProjectFederation(context.Background(), h.project.ID, "tester")
	require.NoError(t, err)

	resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{
		"config": map[string]any{
			"owner": "example-owner",
			"repo":  "example-repo",
		},
	})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "enable: %s", string(body))

	var out issueSyncResponseBody
	decodeJSON(t, body, &out)
	require.NotNil(t, out.Binding)
	assert.Equal(t, "github", out.Binding.Provider)
	assert.True(t, out.Binding.Enabled)
	assert.Equal(t, "enabled", out.Status.State)
	assert.Equal(t, 1, h.fetcher.repositoryCalls, "enable validates repository before storing")
	assert.Equal(t, 1, h.wakeCount())
}

func TestGitHubSyncHandlers_EnableRejectsFederationSpoke(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	_, err := h.store.UpsertFederationBinding(context.Background(), db.FederationBinding{
		ProjectID:            h.project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        h.project.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{
		"config": map[string]any{
			"owner": "example-owner",
			"repo":  "example-repo",
		},
	})
	assertAPIError(t, resp.StatusCode, body, http.StatusConflict, "issue_sync_federation_conflict")
	assert.Contains(t, string(body), "federation spoke")
	assert.Contains(t, string(body), "hub project")
	assert.Equal(t, 1, h.fetcher.repositoryCalls, "enable validates repository before storing")
	assert.Equal(t, 0, h.wakeCount())
}

func TestGitHubSyncHandlers_DisableDisablesWithoutDeleting(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	binding := h.mustUpsertBinding(t, true)

	resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "disable"), map[string]any{})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "disable: %s", string(body))

	var out issueSyncResponseBody
	decodeJSON(t, body, &out)
	require.NotNil(t, out.Binding)
	assert.Equal(t, binding.ID, out.Binding.ID)
	assert.False(t, out.Binding.Enabled)
	assert.Equal(t, "disabled", out.Status.State)
	assert.False(t, out.Status.Enabled)

	stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
	require.NoError(t, err)
	assert.Equal(t, binding.ID, stored.ID)
	assert.False(t, stored.Enabled)
}

func TestGitHubSyncHandlers_StatusReturnsDisabledAndMissingStates(t *testing.T) {
	t.Run("disabled binding remains readable", func(t *testing.T) {
		h := newGitHubSyncHandlerHarness(t)
		binding := h.mustUpsertBinding(t, true)
		_, err := h.store.DisableIssueSyncBinding(context.Background(), h.project.ID)
		require.NoError(t, err)

		resp, body := getStatusBody(t, h.server, githubSyncEndpoint(h.project.ID, "status"))
		require.Equalf(t, http.StatusOK, resp.StatusCode, "status: %s", string(body))

		var out issueSyncResponseBody
		decodeJSON(t, body, &out)
		require.NotNil(t, out.Binding)
		assert.Equal(t, binding.ID, out.Binding.ID)
		assert.False(t, out.Binding.Enabled)
		assert.Equal(t, "disabled", out.Status.State)
		assert.False(t, out.Status.Enabled)
	})

	t.Run("missing binding reports not enabled", func(t *testing.T) {
		h := newGitHubSyncHandlerHarness(t)

		resp, body := getStatusBody(t, h.server, githubSyncEndpoint(h.project.ID, "status"))
		require.Equalf(t, http.StatusOK, resp.StatusCode, "status: %s", string(body))

		var out issueSyncResponseBody
		decodeJSON(t, body, &out)
		assert.Nil(t, out.Binding)
		assert.Equal(t, h.project.ID, out.Status.ProjectID)
		assert.Equal(t, int64(0), out.Status.BindingID)
		assert.Equal(t, "not_enabled", out.Status.State)
		assert.False(t, out.Status.Enabled)
	})
}

func TestGitHubSyncHandlers_OnceRunsSyncAndBroadcastsImportEvents(t *testing.T) {
	h := newGitHubSyncHandlerHarness(t)
	h.fetcher.repo = githubsync.Repository{
		NodeID:   "R_exampleNode",
		ID:       12345,
		FullName: "example-owner/example-repo",
	}
	createdAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	h.fetcher.issues = []githubsync.Issue{{
		ID:        1001,
		NodeID:    "I_exampleIssue",
		Number:    7,
		Comments:  1,
		HTMLURL:   "https://github.com/example-owner/example-repo/issues/7",
		Title:     "Imported issue",
		Body:      "issue body",
		State:     "open",
		User:      &githubsync.User{Login: "example-author"},
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
	}}
	h.fetcher.comments = map[int][]githubsync.Comment{
		7: {{
			ID:        2001,
			NodeID:    "IC_exampleComment",
			Body:      "comment body",
			User:      &githubsync.User{Login: "example-commenter"},
			CreatedAt: &updatedAt,
		}},
	}
	h.mustUpsertBinding(t, true)
	sub := h.broadcaster.Subscribe(daemon.SubFilter{ProjectID: h.project.ID})
	defer sub.Unsub()

	resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "once"), map[string]any{})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "once: %s", string(body))

	var out runIssueSyncOnceResponseBody
	decodeJSON(t, body, &out)
	require.NotNil(t, out.Binding)
	assert.Equal(t, "enabled", out.Status.State)
	assert.Equal(t, 1, out.Import.Created)
	assert.Equal(t, 1, out.Import.Comments)
	assert.Equal(t, 1, h.fetcher.issueCalls)
	assert.Equal(t, 1, h.fetcher.commentCalls)

	first := receiveMsg(t, sub.Ch, time.Second, "issue import broadcast")
	second := receiveMsg(t, sub.Ch, time.Second, "comment import broadcast")
	assert.Equal(t, []string{"issue.created", "issue.commented"}, []string{first.Event.Type, second.Event.Type})
	assert.Equal(t, []string{"github-sync", "github-sync"}, []string{first.Event.Actor, second.Event.Actor})

	hookEvents := h.hooks.snapshot()
	require.Len(t, hookEvents, 2)
	assert.Equal(t, []string{"issue.created", "issue.commented"}, []string{hookEvents[0].Type, hookEvents[1].Type})
	assert.Equal(t, []string{"github-sync", "github-sync"}, []string{hookEvents[0].Actor, hookEvents[1].Actor})
}

func TestGitHubSyncHandlers_OnceRejectsMissingAndDisabledBindings(t *testing.T) {
	t.Run("missing binding", func(t *testing.T) {
		h := newGitHubSyncHandlerHarnessWithFakeRunner(t)

		resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "once"), map[string]any{})
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		assert.Equal(t, 0, h.runnerRunCount())
	})

	t.Run("disabled binding", func(t *testing.T) {
		h := newGitHubSyncHandlerHarnessWithFakeRunner(t)
		h.mustUpsertBinding(t, false)

		resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "once"), map[string]any{})
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		assert.Equal(t, 0, h.runnerRunCount())
	})
}

// TestGitHubSyncHandlers_MutationsRejectUnattributedTrustedProxy locks the
// write-attribution guard on the three mutating routes: on a trusted listener
// that omits the actor header the principal is PrincipalTrustedProxyAbsent, so
// enable/disable/once must fail with 400 actor_header_required before touching
// the fetcher, the wake channel, or the runner.
func TestGitHubSyncHandlers_MutationsRejectUnattributedTrustedProxy(t *testing.T) {
	t.Run("enable", func(t *testing.T) {
		h := newGitHubSyncHandlerHarnessWithOpts(t, gitHubSyncHarnessOpts{trustedActorHeader: "X-Kata-Actor"})
		resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "enable"), map[string]any{
			"config": map[string]any{
				"owner": "example-owner",
				"repo":  "example-repo",
			},
		})
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "actor_header_required")
		assert.Equal(t, 0, h.fetcher.repositoryCalls)
		assert.Equal(t, 0, h.wakeCount())
	})

	t.Run("disable", func(t *testing.T) {
		h := newGitHubSyncHandlerHarnessWithOpts(t, gitHubSyncHarnessOpts{trustedActorHeader: "X-Kata-Actor"})
		binding := h.mustUpsertBinding(t, true)

		resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "disable"), map[string]any{})
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "actor_header_required")

		stored, err := h.store.IssueSyncBindingByProject(context.Background(), h.project.ID)
		require.NoError(t, err)
		assert.Equal(t, binding.ID, stored.ID)
		assert.True(t, stored.Enabled, "rejected disable must not flip the binding")
	})

	t.Run("once", func(t *testing.T) {
		h := newGitHubSyncHandlerHarnessWithOpts(t, gitHubSyncHarnessOpts{
			fakeRunner:         true,
			trustedActorHeader: "X-Kata-Actor",
		})
		h.mustUpsertBinding(t, true)

		resp, body := postJSON(t, h.server, githubSyncEndpoint(h.project.ID, "once"), map[string]any{})
		assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "actor_header_required")
		assert.Equal(t, 0, h.runnerRunCount())
	})
}

type gitHubSyncHandlerHarness struct {
	server      *httptest.Server
	store       db.Storage
	project     db.Project
	fetcher     *fakeGitHubSyncFetcher
	broadcaster *daemon.EventBroadcaster
	hooks       *recordingSink
	mu          sync.Mutex
	wakes       int
	runnerRuns  int
}

type gitHubSyncHarnessOpts struct {
	fakeRunner bool
	// trustedActorHeader, when set, runs the server in trusted-proxy mode on
	// an allowlisted loopback listener so tests can exercise the
	// write-attribution guard by omitting the actor header.
	trustedActorHeader string
}

func newGitHubSyncHandlerHarness(t *testing.T) *gitHubSyncHandlerHarness {
	t.Helper()
	return newGitHubSyncHandlerHarnessWithOpts(t, gitHubSyncHarnessOpts{})
}

func newGitHubSyncHandlerHarnessWithFakeRunner(t *testing.T) *gitHubSyncHandlerHarness {
	t.Helper()
	return newGitHubSyncHandlerHarnessWithOpts(t, gitHubSyncHarnessOpts{fakeRunner: true})
}

func newGitHubSyncHandlerHarnessWithOpts(t *testing.T, opts gitHubSyncHarnessOpts) *gitHubSyncHandlerHarness {
	t.Helper()
	d := openTestDB(t)
	project, err := d.db.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	h := &gitHubSyncHandlerHarness{
		store:       d.db,
		project:     project,
		fetcher:     &fakeGitHubSyncFetcher{},
		broadcaster: daemon.NewEventBroadcaster(),
		hooks:       &recordingSink{},
	}
	h.fetcher.repo = githubsync.Repository{
		NodeID:   "R_exampleNode",
		ID:       12345,
		FullName: "example-owner/example-repo",
	}
	cfg := daemon.ServerConfig{
		DB:                d.db,
		StartedAt:         d.now,
		Broadcaster:       h.broadcaster,
		Hooks:             h.hooks,
		GitHubSyncFetcher: h.fetcher,
		GitHubSyncWake:    h.wake,
		GitHubSyncRunnerFactory: func(cfg daemon.GitHubSyncRunnerConfig) daemon.GitHubSyncRunner {
			if opts.fakeRunner {
				return fakeGitHubSyncRunner{h: h}
			}
			return daemon.NewDefaultGitHubSyncRunner(cfg)
		},
	}
	if opts.trustedActorHeader != "" {
		h.server = startGitHubSyncTrustedProxyServer(t, cfg, opts.trustedActorHeader)
	} else {
		h.server = startTestServer(t, cfg)
	}
	return h
}

// startGitHubSyncTrustedProxyServer pre-binds a loopback listener and names it
// in the trusted-proxy allowlist so the server treats requests on it as
// trusted-proxy traffic. Requests that omit the actor header land in
// PrincipalTrustedProxyAbsent, which the write-attribution guard must reject.
func startGitHubSyncTrustedProxyServer(t *testing.T, cfg daemon.ServerConfig, headerName string) *httptest.Server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cfg.Auth = config.AuthConfig{
		Proxy: config.ProxyConfig{
			TrustedActorHeader:    headerName,
			TrustedProxyListeners: []string{l.Addr().String()},
		},
	}
	srv := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewUnstartedServer(srv.Handler())
	require.NoError(t, ts.Listener.Close())
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

func (h *gitHubSyncHandlerHarness) wake() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.wakes++
}

func (h *gitHubSyncHandlerHarness) wakeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wakes
}

func (h *gitHubSyncHandlerHarness) runnerRunCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runnerRuns
}

func (h *gitHubSyncHandlerHarness) mustUpsertBinding(t *testing.T, enabled bool) db.IssueSyncBinding {
	t.Helper()
	binding, err := h.store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{
		ProjectID:       h.project.ID,
		Provider:        "github",
		SourceKey:       "github:R_exampleNode",
		RemoteID:        "R_exampleNode",
		DisplayName:     "example-owner/example-repo",
		Config:          mustDaemonGitHubSyncConfig(t, "github.com", "example-owner", "example-repo", 12345),
		IntervalSeconds: 300,
	})
	require.NoError(t, err)
	if !enabled {
		binding, err = h.store.DisableIssueSyncBinding(context.Background(), h.project.ID)
		require.NoError(t, err)
	}
	return binding
}

func mustDaemonGitHubSyncConfig(t testing.TB, host, owner, repo string, repoID int64) []byte {
	t.Helper()
	config, err := githubsync.EncodeConfig(githubsync.Config{
		Host:   host,
		Owner:  owner,
		Repo:   repo,
		RepoID: repoID,
	})
	require.NoError(t, err)
	return config
}

func githubSyncEndpoint(projectID int64, action string) string {
	return issueSyncEndpoint(projectID, "github", action)
}

func issueSyncEndpoint(projectID int64, provider, action string) string {
	return fmt.Sprintf("/api/v1/projects/%d/issue-sync/%s/%s", projectID, provider, action)
}

func decodeJSON(t *testing.T, body []byte, target any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(body, target), string(body))
}

type issueSyncResponseBody struct {
	Binding *issueSyncBindingOut `json:"binding"`
	Status  issueSyncStatusOut   `json:"status"`
}

type issueSyncBindingOut struct {
	ID              int64          `json:"id"`
	ProjectID       int64          `json:"project_id"`
	Provider        string         `json:"provider"`
	SourceKey       string         `json:"source_key"`
	RemoteID        string         `json:"remote_id"`
	DisplayName     string         `json:"display_name"`
	Config          map[string]any `json:"config"`
	Enabled         bool           `json:"enabled"`
	IntervalSeconds int            `json:"interval_seconds"`
}

type issueSyncStatusOut struct {
	BindingID int64  `json:"binding_id"`
	ProjectID int64  `json:"project_id"`
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"`
}

type runIssueSyncOnceResponseBody struct {
	Binding *issueSyncBindingOut `json:"binding"`
	Status  issueSyncStatusOut   `json:"status"`
	Import  struct {
		Created  int `json:"created"`
		Comments int `json:"comments"`
	} `json:"import"`
}

type fakeRepositoryCall struct {
	host  string
	owner string
	repo  string
}

type fakeGitHubSyncFetcher struct {
	repo              githubsync.Repository
	repositoryErr     error
	issues            []githubsync.Issue
	comments          map[int][]githubsync.Comment
	repositoryCalls   int
	issueCalls        int
	commentCalls      int
	repositoryCallLog []fakeRepositoryCall
}

func (f *fakeGitHubSyncFetcher) Repository(_ context.Context, host, owner, repo string) (githubsync.Repository, error) {
	f.repositoryCalls++
	f.repositoryCallLog = append(f.repositoryCallLog, fakeRepositoryCall{host: host, owner: owner, repo: repo})
	if f.repositoryErr != nil {
		return githubsync.Repository{}, f.repositoryErr
	}
	return f.repo, nil
}

func (f *fakeGitHubSyncFetcher) Issues(_ context.Context, _ githubsync.Binding, _ *time.Time) ([]githubsync.Issue, error) {
	f.issueCalls++
	return f.issues, nil
}

func (f *fakeGitHubSyncFetcher) Comments(_ context.Context, _ githubsync.Binding, issueNumber int) ([]githubsync.Comment, error) {
	f.commentCalls++
	return f.comments[issueNumber], nil
}

type fakeGitHubSyncRunner struct {
	h *gitHubSyncHandlerHarness
}

func (r fakeGitHubSyncRunner) RunOnce(_ context.Context, _ int64) (githubsync.RunResult, error) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	r.h.runnerRuns++
	return githubsync.RunResult{}, errors.New("fake runner should not run")
}
