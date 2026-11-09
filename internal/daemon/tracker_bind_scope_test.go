package daemon

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"testing"
)

func TestTrackerBindScopesValidateBeforeCredentialLookup(t *testing.T) {
	const object = "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		provider, resource string
		input              map[string]any
	}{
		{"github", "github:repository:github.com/example-owner/example-repo", map[string]any{"owner": "example-owner", "repo": "example-repo"}},
		{"notion", "notion:" + object, map[string]any{"data_source_id": object}},
		{"linear", "linear:" + object + "/" + object, map[string]any{"workspace_id": object, "team_id": object}},
		{"plane", "plane:https://api.example/example-workspace/" + object, map[string]any{"workspace": "example-workspace", "project_id": object}},
		{"todoist", "todoist:https://api.todoist.com/project:project123", map[string]any{"project_id": "project123"}},
		{"ticktick", "ticktick:project123", map[string]any{"project_id": "project123"}},
		{"twenty", "twenty:https://api.example/" + object, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			connection := config.TrackerConnectionConfig{Provider: c.provider, Plane: &config.PlaneSyncConfig{APIOrigin: "https://api.example"}, Todoist: &config.TodoistSyncConfig{APIOrigin: "https://api.todoist.com"}, Twenty: &config.TwentySyncConfig{APIOrigin: "https://api.example"}}
			got, err := trackerBindResource(connection, c.resource, c.input)
			require.NoError(t, err)
			require.Equal(t, c.resource, got)
			_, err = trackerBindResource(connection, "other-resource", c.input)
			require.Error(t, err)
			c.input["owner repo"] = "unknown-field"
			_, err = trackerBindResource(connection, c.resource, c.input)
			require.Error(t, err)
			delete(c.input, "owner repo")
			c.input["token_env"] = "FORBIDDEN_SELECTOR"
			_, err = trackerBindResource(connection, c.resource, c.input)
			require.Error(t, err)
		})
	}
}
