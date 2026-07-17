package kata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	internalconfig "go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
)

func TestServiceGitHubSyncConfigDrivesHandlersAndRunner(t *testing.T) {
	t.Setenv("KATA_GITHUB_SYNC_ALLOWED_HOSTS", "github.example")
	fetcher := &serviceGitHubFetcher{
		repositoryCalls: make(chan githubsync.Binding, 2),
	}
	var resolved internalconfig.GitHubSyncConfig
	service, err := newService(context.Background(), Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
		GitHubSync: GitHubSyncConfig{ //nolint:gosec // TokenEnv is an environment variable name, not a credential.
			TokenEnv:  " EXAMPLE_GITHUB_TOKEN ",
			TokenHost: " GitHub.Example ",
			Apps: []GitHubAppConfig{{
				Host: " GitHub.Example ", Owner: " example-owner ",
				AppID: 123, InstallationID: 456, PrivateKeyPath: " /keys/example.pem ",
			}},
		},
	}, serviceDeps{
		gitHubSyncFetcherFactory: func(cfg internalconfig.GitHubSyncConfig) githubsync.Fetcher {
			resolved = cfg
			return fetcher
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	assert.Equal(t, internalconfig.GitHubSyncConfig{ //nolint:gosec // TokenEnv is an environment variable name, not a credential.
		TokenEnv: "EXAMPLE_GITHUB_TOKEN", TokenHost: "github.example",
		Apps: []internalconfig.GitHubAppConfig{{
			Host: "github.example", Owner: "example-owner", AppID: 123,
			InstallationID: 456, PrivateKeyPath: "/keys/example.pem",
		}},
	}, resolved)

	project, err := service.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	body, err := json.Marshal(map[string]any{
		"config": map[string]any{
			"host": "github.example", "owner": "example-owner", "repo": "example-repo",
		},
	})
	require.NoError(t, err)
	response, err := http.Post(
		server.URL+"/api/v1/projects/"+fmt.Sprint(project.ID)+"/issue-sync/github/enable",
		"application/json", bytes.NewReader(body),
	)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)

	select {
	case call := <-fetcher.repositoryCalls:
		assert.Equal(t, githubsync.Binding{
			Host: "github.example", Owner: "example-owner", Repo: "example-repo",
		}, call)
	case <-time.After(2 * time.Second):
		require.Fail(t, "configured GitHub fetcher was not used by the HTTP handler")
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()
	select {
	case call := <-fetcher.repositoryCalls:
		assert.Equal(t, githubsync.Binding{
			Host: "github.example", Owner: "example-owner", Repo: "example-repo",
		}, call)
	case <-time.After(2 * time.Second):
		require.Fail(t, "configured GitHub fetcher was not used by the background runner")
	}
	cancelRun()
	require.NoError(t, <-runDone)
}

func TestServiceRunProcessesEnabledGitHubSyncBinding(t *testing.T) {
	fetcher := &serviceGitHubFetcher{
		repositoryCalls: make(chan githubsync.Binding, 1),
	}
	service, err := newService(context.Background(), Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	}, serviceDeps{gitHubSyncFetcher: fetcher})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	project, err := service.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	titlePrefix := true
	bindingConfig, err := githubsync.EncodeConfig(githubsync.Config{
		Host: "github.com", Owner: "example-owner", Repo: "example-repo",
		RepoID: 12345, TitlePrefix: &titlePrefix,
	})
	require.NoError(t, err)
	_, err = service.store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{
		ProjectID: project.ID, Provider: "github", SourceKey: "github:R_exampleNode",
		RemoteID: "R_exampleNode", DisplayName: "example-owner/example-repo",
		Config: bindingConfig, IntervalSeconds: 300,
	})
	require.NoError(t, err)

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()

	select {
	case call := <-fetcher.repositoryCalls:
		assert.Equal(t, githubsync.Binding{
			Host: "github.com", Owner: "example-owner", Repo: "example-repo",
		}, call)
	case <-time.After(2 * time.Second):
		require.Fail(t, "GitHub sync worker did not process the enabled binding")
	}
	require.Eventually(t, func() bool {
		status, statusErr := service.store.IssueSyncStatusByProject(context.Background(), project.ID)
		return statusErr == nil && status.LastSuccessAt != nil
	}, 2*time.Second, 10*time.Millisecond)

	cancelRun()
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(2 * time.Second):
		require.Fail(t, "Run did not stop after cancellation")
	}
}

type serviceGitHubFetcher struct {
	repositoryCalls chan githubsync.Binding
}

func (f *serviceGitHubFetcher) Repository(_ context.Context, host, owner, repo string) (githubsync.Repository, error) {
	f.repositoryCalls <- githubsync.Binding{Host: host, Owner: owner, Repo: repo}
	return githubsync.Repository{NodeID: "R_exampleNode", ID: 12345, FullName: "example-owner/example-repo"}, nil
}

func (*serviceGitHubFetcher) Issues(context.Context, githubsync.Binding, *time.Time) ([]githubsync.Issue, error) {
	return nil, nil
}

func (*serviceGitHubFetcher) Comments(context.Context, githubsync.Binding, int) ([]githubsync.Comment, error) {
	return nil, nil
}

func (*serviceGitHubFetcher) ParentData(context.Context, githubsync.Binding) (githubsync.ParentData, error) {
	return githubsync.ParentData{Unsupported: true}, nil
}
