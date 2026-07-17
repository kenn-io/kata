package kata

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
)

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
