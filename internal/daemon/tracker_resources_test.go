package daemon

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/todoistsync"
	"testing"
)

// Repository node IDs and Todoist account IDs are learned from the selected
// account. Preflight grant locators let operators scope use before those reads.
func TestTrackerBindingResourceAliasesRemainAccountAndOriginScoped(t *testing.T) {
	raw, err := githubsync.EncodeConfig(githubsync.Config{Host: "github.com", Owner: "example-owner", Repo: "example-repo", RepoID: 1})
	require.NoError(t, err)
	require.Equal(t, "github:repository:github.com/example-owner/example-repo", trackerBindingResourceAlias(db.IssueSyncBinding{Provider: "github", Config: raw}))
	raw, err = todoistsync.EncodeConfig(todoistsync.Config{APIOrigin: "https://api.todoist.com", AccountID: "1234567", ProjectID: "project123", HistorySince: "2026-09-01T00:00:00Z"})
	require.NoError(t, err)
	require.Equal(t, "todoist:https://api.todoist.com/project:project123", trackerBindingResourceAlias(db.IssueSyncBinding{Provider: "todoist", Config: raw}))
}
