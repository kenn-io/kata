package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	progress  *githubsync.ProgressTracker
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
	progress := githubsync.NewProgressTracker()
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.GitHubSyncProgress = progress
		cfg.GitHubSyncFetcher = fetcher
		cfg.GitHubSyncRunnerFactory = func(daemon.GitHubSyncRunnerConfig) daemon.GitHubSyncRunner {
			return runner
		}
	})
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	projectID := resolvePIDViaHTTP(t, env.URL, dir)
	return githubSyncCLIFixture{env: env, dir: dir, projectID: projectID, fetcher: fetcher, runner: runner, progress: progress}
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

func (f *fakeGitHubSyncCLIFetcher) ParentData(context.Context, githubsync.Binding, githubsync.ParentRequest) (githubsync.ParentData, error) {
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

func TestGitHubSyncSinceEnableAndValidation(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)
	runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo", "--since", "2026-01-01")
	binding, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	assert.Contains(t, string(binding.Config), `"since":"2026-01-01T00:00:00Z"`)
	_, _, err = runCLIWithErr(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo", "--since", "bad")
	_ = requireCLIError(t, err, ExitValidation)
	assert.Len(t, f.fetcher.calls, 1, "invalid cutoff must reject before repository lookup")
	runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo", "--interval", "10m")
	binding, err = f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	assert.Contains(t, string(binding.Config), `"since":"2026-01-01T00:00:00Z"`)
	assert.Equal(t, 600, binding.IntervalSeconds)

	runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo", "--since=")
	binding, err = f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	assert.NotContains(t, string(binding.Config), `"since"`)
}

func TestGitHubSyncProgressDetailedStatusAllModes(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)
	runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo", "--since", "2026-01-01")
	binding, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, claimed, err := f.env.DB.ClaimIssueSyncBinding(context.Background(), binding.ID, "github", at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	f.progress.Begin(binding.ID, at)
	f.progress.Update(binding.ID, at, "issues", 200, 0, at.Add(time.Second))
	human := runCLI(t, f.env, f.dir, "sync", "github", "status")
	for _, want := range []string{"GitHub sync running", "example-owner/example-repo", "issues", "200", "Since:", "Last attempt:", "Started:", "No successful run yet"} {
		assert.Contains(t, human, want)
	}
	agent := runCLI(t, f.env, f.dir, "--agent", "sync", "github", "status")
	for _, want := range []string{"state=running", "phase=issues", "completed=200", "total=0", "since=2026-01-01T00:00:00Z", "last_attempt_at=", "sync_started_at=", "interval_seconds=300"} {
		assert.Contains(t, agent, want)
	}
	raw := runCLI(t, f.env, f.dir, "--json", "sync", "github", "status")
	var body struct {
		Status struct {
			State    string
			Progress struct {
				Phase     string
				Completed int
			}
		}
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &body))
	assert.Equal(t, "running", body.Status.State)
	assert.Equal(t, "issues", body.Status.Progress.Phase)
	assert.Equal(t, 200, body.Status.Progress.Completed)
	f.progress.Finish(binding.ID, at)
	human = runCLI(t, f.env, f.dir, "sync", "github", "status")
	assert.Contains(t, human, "GitHub sync running")
	assert.NotContains(t, human, "Progress:")
}

func TestGitHubSyncDetailedStatusLabelsPreviousSuccessAfterFailure(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)
	runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo")
	binding, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, _, err = f.env.DB.ClaimIssueSyncBinding(context.Background(), binding.ID, "github", at, at.Add(-time.Hour))
	require.NoError(t, err)
	_, err = f.env.DB.RecordIssueSyncSuccess(context.Background(), db.IssueSyncSuccessParams{BindingID: binding.ID, StartedAt: at, At: at, CursorAt: at, LastCreated: 12, LastComments: 34})
	require.NoError(t, err)
	later := at.Add(time.Minute)
	_, _, err = f.env.DB.ClaimIssueSyncBinding(context.Background(), binding.ID, "github", later, at.Add(-time.Hour))
	require.NoError(t, err)
	_, err = f.env.DB.RecordIssueSyncError(context.Background(), db.IssueSyncErrorParams{BindingID: binding.ID, StartedAt: later, At: later, Error: "upstream failed"})
	require.NoError(t, err)
	human := runCLI(t, f.env, f.dir, "sync", "github", "status")
	assert.Contains(t, human, "Last successful run:")
	assert.Contains(t, human, "created=12")
	assert.Contains(t, human, "comments=34")
	assert.Contains(t, human, "upstream failed")
	agent := runCLI(t, f.env, f.dir, "--agent", "sync", "github", "status")
	assert.Contains(t, agent, "last_success_at=")
	assert.Contains(t, agent, "last_created=12")
	assert.Contains(t, agent, "last_comments=34")
}

func TestGitHubTitlePrefixReenablePresence(t *testing.T) {
	f := newGitHubSyncCLIFixture(t)
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, true}, {[]string{"--title-prefix=false"}, false}, {nil, false}, {[]string{"--title-prefix=true"}, true},
	} {
		runCLI(t, f.env, f.dir, append([]string{"sync", "github", "enable", "--repo", "example-owner/example-repo"}, tc.args...)...)
		b, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
		require.NoError(t, err)
		c, err := githubsync.DecodeConfig(b.Config)
		require.NoError(t, err)
		require.Equal(t, tc.want, c.UseTitlePrefix())
	}
}

func TestGitHubTitlePrefixStatus(t *testing.T) {
	for _, mode := range []string{"human", "agent"} {
		for _, choice := range []string{"legacy", "true", "false"} {
			t.Run(mode+"/"+choice, func(t *testing.T) {
				f := newGitHubSyncCLIFixture(t)
				runCLI(t, f.env, f.dir, "sync", "github", "enable", "--repo", "example-owner/example-repo", "--title-prefix="+fmt.Sprint(choice != "false"))
				if choice == "legacy" {
					binding, err := f.env.DB.IssueSyncBindingByProject(context.Background(), f.projectID)
					require.NoError(t, err)
					var fields map[string]any
					require.NoError(t, json.Unmarshal(binding.Config, &fields))
					delete(fields, "title_prefix")
					raw, err := json.Marshal(fields)
					require.NoError(t, err)
					_, err = f.env.DB.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: binding.ProjectID, Provider: binding.Provider, SourceKey: binding.SourceKey, RemoteID: binding.RemoteID, DisplayName: binding.DisplayName, Config: raw, IntervalSeconds: binding.IntervalSeconds})
					require.NoError(t, err)
				}
				args := []string{"sync", "github", "status"}
				label := "Title prefix: "
				if mode == "agent" {
					args = append([]string{"--agent"}, args...)
					label = "title_prefix="
				}
				out := runCLI(t, f.env, f.dir, args...)
				require.Contains(t, out, label+fmt.Sprint(choice != "false"))
			})
		}
	}
}
