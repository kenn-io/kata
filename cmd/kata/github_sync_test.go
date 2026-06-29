package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/testenv"
)

func TestGitHubSyncEnableExplicitRepoForwardsHostRepoAndInterval(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)

	out := runCLI(t, f.env, f.dir, "sync", "github", "enable",
		"--host", "github.example",
		"--repo", "example-owner/example-repo",
		"--interval", "10m",
		"--title-prefix=false")

	assert.Contains(t, out, "GitHub sync enabled")
	assert.Equal(t, []githubSyncFetcherCall{{
		host:  "github.example",
		owner: "example-owner",
		repo:  "example-repo",
	}}, f.fetcher.calls)
	binding, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	assert.True(t, binding.Enabled)
	assert.Equal(t, "github", binding.Provider)
	assert.Equal(t, "example-owner/example-repo", binding.DisplayName)
	assert.JSONEq(t, `{"host":"github.example","owner":"example-owner","repo":"example-repo","repo_id":12345,"title_prefix":false}`, string(binding.Config))
	assert.Equal(t, 600, binding.IntervalSeconds)
}

func TestGitHubSyncEnableDefaultsHostForExplicitRepo(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)

	out := runCLI(t, f.env, f.dir, "--agent", "sync", "github", "enable",
		"--repo", "example-owner/example-repo")

	assert.True(t, strings.HasPrefix(out, "OK github-sync "), out)
	assert.Equal(t, []githubSyncFetcherCall{{
		host:  "github.com",
		owner: "example-owner",
		repo:  "example-repo",
	}}, f.fetcher.calls)
}

func TestGitHubSyncEnableResolvesRepoFromProjectGitAlias(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_ALLOWED_HOSTS", "github.example")
	f := newGitHubSyncCLIFixture(t)
	_, err := f.env.DB.AttachAlias(context.Background(), f.projectID, "github.example/example-owner/example-repo", "git")
	require.NoError(t, err)

	out := runCLI(t, f.env, f.dir, "sync", "github", "enable")

	assert.Contains(t, out, "GitHub sync enabled")
	assert.Equal(t, []githubSyncFetcherCall{{
		host:  "github.example",
		owner: "example-owner",
		repo:  "example-repo",
	}}, f.fetcher.calls)
}

func TestGitHubSyncEnableFiltersInferredRepoByHost(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_ALLOWED_HOSTS", "github.example")
	f := newGitHubSyncCLIFixture(t)
	_, err := f.env.DB.AttachAlias(context.Background(), f.projectID, "github.com/example-owner/public-repo", "git")
	require.NoError(t, err)
	_, err = f.env.DB.AttachAlias(context.Background(), f.projectID, "github.example/example-owner/enterprise-repo", "git")
	require.NoError(t, err)

	out := runCLI(t, f.env, f.dir, "sync", "github", "enable", "--host", "github.example")

	assert.Contains(t, out, "GitHub sync enabled")
	assert.Equal(t, []githubSyncFetcherCall{{
		host:  "github.example",
		owner: "example-owner",
		repo:  "enterprise-repo",
	}}, f.fetcher.calls)
}

func TestGitHubSyncEnableRejectsMissingAndAmbiguousInferredRepo(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newGitHubSyncCLIFixture(t)

		_, stderr, err := runCLIWithErr(t, f.env, f.dir, "sync", "github", "enable")

		ce := requireCLIError(t, err, ExitValidation)
		assert.Contains(t, ce.Message, "could not infer GitHub repository")
		assert.Contains(t, stderr, "could not infer GitHub repository")
	})

	t.Run("ambiguous", func(t *testing.T) {
		t.Setenv("KATA_GITHUB_SYNC_ALLOWED_HOSTS", "github.example")
		f := newGitHubSyncCLIFixture(t)
		_, err := f.env.DB.AttachAlias(context.Background(), f.projectID, "github.com/example-owner/one", "git")
		require.NoError(t, err)
		_, err = f.env.DB.AttachAlias(context.Background(), f.projectID, "github.example/example-owner/two", "git")
		require.NoError(t, err)

		_, stderr, err := runCLIWithErr(t, f.env, f.dir, "sync", "github", "enable")

		ce := requireCLIError(t, err, ExitValidation)
		assert.Contains(t, ce.Message, "ambiguous GitHub repository")
		assert.Contains(t, stderr, "ambiguous GitHub repository")
	})
}

func TestIssueSyncStatusDisableAndOnceUseDaemonEndpointsAndOutputModes(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)
	runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo")
	binding, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	now := time.Now().UTC()
	_, claimed, err := f.env.DB.ClaimIssueSyncBinding(context.Background(), binding.ID, "github", now.Add(-time.Minute), now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	_, err = f.env.DB.RecordIssueSyncError(context.Background(), db.IssueSyncErrorParams{
		BindingID: binding.ID,
		StartedAt: now.Add(-time.Minute),
		At:        now,
		Error:     "GitHub API unavailable",
	})
	require.NoError(t, err)

	statusOut := runCLI(t, f.env, f.dir, "sync", "github", "status")
	assert.Contains(t, statusOut, "GitHub sync enabled")
	assert.Contains(t, statusOut, "GitHub API unavailable")

	agentOut := runCLI(t, f.env, f.dir, "--agent", "sync", "github", "status")
	assert.True(t, strings.HasPrefix(agentOut, "OK github-sync "), agentOut)
	assert.NotContains(t, agentOut, "\n", "agent output must be one line")

	jsonOut := runCLI(t, f.env, f.dir, "--json", "sync", "github", "status")
	var statusBody struct {
		Binding *struct {
			Provider    string         `json:"provider"`
			DisplayName string         `json:"display_name"`
			Config      map[string]any `json:"config"`
		} `json:"binding"`
		Status struct {
			LastError string `json:"last_error"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &statusBody))
	require.NotNil(t, statusBody.Binding)
	assert.Equal(t, "github", statusBody.Binding.Provider)
	assert.Equal(t, "example-owner/example-repo", statusBody.Binding.DisplayName)
	assert.Equal(t, "example-owner", statusBody.Binding.Config["owner"])
	assert.Equal(t, "example-repo", statusBody.Binding.Config["repo"])
	assert.Equal(t, true, statusBody.Binding.Config["title_prefix"])
	assert.Equal(t, "GitHub API unavailable", statusBody.Status.LastError)

	onceOut := runCLI(t, f.env, f.dir, "sync", "github", "once")
	assert.Contains(t, onceOut, "GitHub sync ran")
	assert.Contains(t, onceOut, "created=2")
	assert.Equal(t, int64(1), f.runner.runs)

	disableOut := runCLI(t, f.env, f.dir, "sync", "github", "disable")
	assert.Contains(t, disableOut, "GitHub sync disabled")
	disabled, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	assert.False(t, disabled.Enabled)
}

func TestGitHubSyncOnceAllowsLongRunningRequest(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)
	runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo")
	f.runner.delay = 250 * time.Millisecond
	t.Setenv("KATA_HTTP_TIMEOUT", "100ms")

	out := runCLI(t, f.env, f.dir, "sync", "github", "once")

	assert.Contains(t, out, "GitHub sync ran")
	assert.Equal(t, int64(1), f.runner.runs)
}

func TestRootRegistersSyncGitHub(t *testing.T) {
	syncCmd, ok := rootSubcommands()["sync"]
	require.True(t, ok, "root command should register sync")
	_, _, err := syncCmd.Find([]string{"github"})
	require.NoError(t, err)
}

type githubSyncCLIFixture struct {
	env       *testenv.Env
	dir       string
	projectID int64
	fetcher   *fakeGitHubSyncCLIFetcher
	runner    *fakeGitHubSyncCLIRunner
}

func newGitHubSyncCLIFixture(t *testing.T) githubSyncCLIFixture {
	t.Helper()
	fetcher := &fakeGitHubSyncCLIFetcher{
		repo: githubsync.Repository{
			NodeID:   "R_exampleNode",
			ID:       12345,
			FullName: "example-owner/example-repo",
		},
	}
	runner := &fakeGitHubSyncCLIRunner{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.GitHubSyncFetcher = fetcher
		cfg.GitHubSyncRunnerFactory = func(daemon.GitHubSyncRunnerConfig) daemon.GitHubSyncRunner {
			return runner
		}
	})
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	projectID := resolvePIDViaHTTP(t, env.URL, dir)
	return githubSyncCLIFixture{env: env, dir: dir, projectID: projectID, fetcher: fetcher, runner: runner}
}

type githubSyncFetcherCall struct {
	host  string
	owner string
	repo  string
}

type fakeGitHubSyncCLIFetcher struct {
	repo  githubsync.Repository
	calls []githubSyncFetcherCall
}

func (f *fakeGitHubSyncCLIFetcher) Repository(_ context.Context, host, owner, repo string) (githubsync.Repository, error) {
	f.calls = append(f.calls, githubSyncFetcherCall{host: host, owner: owner, repo: repo})
	return f.repo, nil
}

func (f *fakeGitHubSyncCLIFetcher) Issues(context.Context, githubsync.Binding, *time.Time) ([]githubsync.Issue, error) {
	return nil, errors.New("CLI tests should not fetch GitHub issues")
}

func (f *fakeGitHubSyncCLIFetcher) Comments(context.Context, githubsync.Binding, int) ([]githubsync.Comment, error) {
	return nil, errors.New("CLI tests should not fetch GitHub comments")
}

func (f *fakeGitHubSyncCLIFetcher) ParentData(context.Context, githubsync.Binding) (githubsync.ParentData, error) {
	return githubsync.ParentData{}, nil
}

type fakeGitHubSyncCLIRunner struct {
	runs  int64
	delay time.Duration
}

func (r *fakeGitHubSyncCLIRunner) RunOnce(ctx context.Context, bindingID int64) (githubsync.RunResult, error) {
	r.runs++
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return githubsync.RunResult{}, ctx.Err()
		}
	}
	now := time.Now().UTC()
	return githubsync.RunResult{
		Binding: db.IssueSyncBinding{
			ID:              bindingID,
			ProjectID:       1,
			Provider:        "github",
			SourceKey:       "github:R_exampleNode",
			RemoteID:        "R_exampleNode",
			DisplayName:     "example-owner/example-repo",
			Config:          mustCmdGitHubSyncConfig(nil, "github.com", "example-owner", "example-repo", 12345),
			Enabled:         true,
			IntervalSeconds: 300,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Status: db.IssueSyncStatus{
			BindingID:     bindingID,
			ProjectID:     1,
			LastSuccessAt: &now,
			LastCreated:   2,
		},
		Import: db.ImportBatchResult{Source: "github", Created: 2, Unchanged: 3, Comments: 1},
	}, ctx.Err()
}

func mustCmdGitHubSyncConfig(t testing.TB, host, owner, repo string, repoID int64) []byte {
	if t != nil {
		t.Helper()
	}
	config, err := githubsync.EncodeConfig(githubsync.Config{
		Host:   host,
		Owner:  owner,
		Repo:   repo,
		RepoID: repoID,
	})
	if t != nil {
		require.NoError(t, err)
	} else if err != nil {
		panic(err)
	}
	return config
}
