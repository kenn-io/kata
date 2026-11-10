package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/githubsync"
)

// Contract: ordinary enable cannot turn a named re-enable into legacy-account
// preflight, or silently ignore raw selection fields in a provider config map.
func TestLegacyEnableRefusesNamedSelectionBeforeProviderPreflight(t *testing.T) {
	ctx := t.Context()
	for _, key := range []string{"_tracker_connection", "connection_uid", "connection_owner_instance_uid", "connection_generation"} {
		in := &api.EnableIssueSyncRequest{}
		in.Body.Config = map[string]any{key: "fixture-selection"}
		_, err := issueSyncEnableParams(ctx, ServerConfig{}, "github", in)
		var apiErr *api.APIError
		require.True(t, errors.As(err, &apiErr))
		require.Equal(t, "tracker_selection_required", apiErr.Code)
	}
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	selection := db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "github", SourceKey: "github:example-repository", RemoteID: "example-repository", DisplayName: "Example repository", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	in := &api.EnableIssueSyncRequest{ProjectID: project.ID}
	in.Body.Config = map[string]any{"owner": "example-owner", "repo": "example-repo"}
	_, err = issueSyncEnableParams(ctx, ServerConfig{DB: store, GitHubSyncFetcher: forbiddenNamedPreflightFetcher{t: t}}, "github", in)
	var apiErr *api.APIError
	require.True(t, errors.As(err, &apiErr))
	require.Equal(t, "tracker_selection_required", apiErr.Code)
}

// A broken refusal must fail this fixture before any real credential resolver.
type forbiddenNamedPreflightFetcher struct {
	githubsync.Fetcher
	t *testing.T
}

func (f forbiddenNamedPreflightFetcher) Repository(context.Context, string, string, string) (githubsync.Repository, error) {
	f.t.Fatal("named binding reached legacy provider preflight")
	return githubsync.Repository{}, errors.New("forbidden fixture preflight")
}
