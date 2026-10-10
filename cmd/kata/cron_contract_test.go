package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// An incomplete namespace rename breaks real CLI requests or the HTTP routes
// used by clients, even when both components still compile.
func TestCronNamespaceExecutesAgainstDaemon(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, command := range [][]string{
		{"capabilities"},
		{"job", "list"},
		{"workflow", "list"},
		{"run", "list", "--limit", "100"},
	} {
		t.Run(command[0]+fmt.Sprint(command[1:]), func(t *testing.T) {
			resetFlags(t)
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetContext(contextWithBaseURL(context.Background(), env.URL))
			root.SetArgs(append([]string{"--project", "spoke-project", "--json", "cron"}, command...))
			require.NoError(t, root.Execute(), out.String())
			var response map[string]any
			require.NoError(t, json.Unmarshal(out.Bytes(), &response))
			if command[0] == "capabilities" {
				require.Contains(t, response["event_features"], "cron_v1")
			} else {
				require.Contains(t, response, command[0]+"s")
			}
		})
	}
	t.Run("HTTP capabilities", func(t *testing.T) {
		status, body := env.Get(t, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID))
		require.Equal(t, 200, status, string(body))
		var response map[string]any
		require.NoError(t, json.Unmarshal(body, &response))
		require.Contains(t, response["event_features"], "cron_v1")
	})
}
