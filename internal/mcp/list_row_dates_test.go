package mcpserver

import (
	"encoding/json"
	"encoding/json/jsontext"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPListRowsCarryPlanningDates(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	// 23:30Z on the 29th is already the 30th in the issue's zone.
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "dated", Author: "example-agent",
		Metadata: map[string]jsontext.Value{
			"scheduled_on": jsontext.Value(`"2026-10-29T23:30:00Z"`),
			"deadline_on":  jsontext.Value(`"2026-11-02"`),
			"timezone":     jsontext.Value(`"Asia/Tokyo"`),
		},
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "undated", Author: "example-agent"})
	require.NoError(t, err)

	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	bound, err := NewBoundScope(ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
	require.NoError(t, err)

	for name, scope := range map[string]*Scope{"bound project": bound, "every project": NewAllScope()} {
		t.Run(name, func(t *testing.T) {
			session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "example-agent", Version: "test"})
			result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{}})
			require.NoError(t, err)
			require.False(t, result.IsError, "%s", mustJSON(t, result))
			var page struct {
				Issues []map[string]any `json:"issues"`
			}
			require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &page))
			rows := map[any]map[string]any{}
			for _, row := range page.Issues {
				rows[row["title"]] = row
			}
			require.Len(t, rows, 2)
			assert.Equal(t, "2026-10-30", rows["dated"]["scheduled_on_date"])
			assert.Equal(t, "2026-11-02", rows["dated"]["deadline_on_date"])
			assert.NotContains(t, rows["undated"], "scheduled_on_date")
			assert.NotContains(t, rows["undated"], "deadline_on_date")
		})
	}
}
