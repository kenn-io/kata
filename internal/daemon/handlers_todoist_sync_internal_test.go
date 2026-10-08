package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/todoistsync"
)

// Contract: a new binding's history floor defaults to thirty days before
// enable; saved bindings keep omitted options and reject identity changes.
func TestTodoistSyncConfigMerge(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 30, 15, 500, time.UTC)
	origin := "https://api.todoist.com"
	project := "project123"
	c, err := todoistSyncConfig(origin, todoistSyncRequest{projectID: &project}, nil, now)
	require.NoError(t, err)
	require.Equal(t, "2026-09-07T12:30:15Z", c.HistorySince)
	require.True(t, c.UseTitlePrefix())

	saved := todoistsync.Config{APIOrigin: origin, AccountID: "1234567", ProjectID: project, HistorySince: "2026-09-01T00:00:00Z", StatusSync: "two-way", TitlePrefix: new(false)}
	c, err = todoistSyncConfig(origin, todoistSyncRequest{}, &saved, now)
	require.NoError(t, err)
	require.Equal(t, saved, c)

	future := "2026-10-08"
	empty := ""
	other := "different"
	earlier := "2026-08-01"
	for name, tc := range map[string]struct {
		r     todoistSyncRequest
		saved *todoistsync.Config
	}{
		"future floor":    {todoistSyncRequest{projectID: &project, historySince: &future}, nil},
		"empty floor":     {todoistSyncRequest{projectID: &project, historySince: &empty}, nil},
		"missing project": {todoistSyncRequest{}, nil},
		"changed project": {todoistSyncRequest{projectID: &other}, &saved},
		"changed floor":   {todoistSyncRequest{historySince: &earlier}, &saved},
	} {
		_, err := todoistSyncConfig(origin, tc.r, tc.saved, now)
		require.Error(t, err, name)
	}
}
