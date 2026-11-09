package daemon

import (
	"go.kenn.io/kata/internal/config"
	"testing"
)

// Construction must not require an environment token or inspect an account.
// The returned typed clients resolve only the selected owner's selector at use.
func TestTrackerClientsCoverSevenConfiguredProviders(t *testing.T) {
	cases := []config.TrackerConnectionConfig{
		{Provider: "github", GitHub: &config.GitHubSyncConfig{TokenEnv: "EXAMPLE_GITHUB_TOKEN"}},
		{Provider: "notion", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_NOTION_TOKEN"}},
		{Provider: "linear", Linear: &config.LinearSyncConfig{TokenEnv: "EXAMPLE_LINEAR_TOKEN"}},
		{Provider: "plane", Plane: &config.PlaneSyncConfig{TokenEnv: "EXAMPLE_PLANE_TOKEN"}},
		{Provider: "ticktick", TickTick: &config.TickTickSyncConfig{TokenEnv: "EXAMPLE_TICKTICK_TOKEN"}},
		{Provider: "todoist", Todoist: &config.TodoistSyncConfig{TokenEnv: "EXAMPLE_TODOIST_TOKEN"}},
		{Provider: "twenty", Twenty: &config.TwentySyncConfig{TokenEnv: "EXAMPLE_TWENTY_TOKEN"}},
	}
	for _, c := range cases {
		t.Run(c.Provider, func(t *testing.T) {
			clients := newTrackerClients(c)
			count := 0
			for _, set := range []bool{clients.github != nil, clients.notion != nil, clients.linear != nil, clients.plane != nil, clients.ticktick != nil, clients.todoist != nil, clients.twenty != nil} {
				if set {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("configured %s constructed %d clients", c.Provider, count)
			}
		})
	}
}
