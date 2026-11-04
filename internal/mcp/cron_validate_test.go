package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCronValidateDiscovery(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
	cronOK(t, session, "load_cron", map[string]any{})
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, name := range []string{"kata.cron_job_validate", "kata.cron_workflow_validate"} {
		for _, tool := range tools.Tools {
			if tool.Name != name {
				continue
			}
			raw, err := json.Marshal(tool.OutputSchema)
			require.NoError(t, err)
			var schema struct{ Properties map[string]json.RawMessage }
			require.NoError(t, json.Unmarshal(raw, &schema))
			require.JSONEq(t, `true`, string(schema.Properties["definition"]), "the definition schema permits raw JSON")
		}
	}
}

func TestCronValidateOpaqueNumbers(t *testing.T) {
	f := newCronFixture(t)
	before, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)

	for _, resource := range []string{"job", "workflow"} {
		definition := cronDefinition()
		if resource == "workflow" {
			definition = cronWorkflowDefinition()
		}
		tool := "cron_" + resource + "_validate"
		result := cronOK(t, f.session, tool, map[string]any{"name": "  Review  ", "definition": definition})
		require.Equal(t, true, result["valid"])
		require.Equal(t, "Review", result["name"])

		result = cronOK(t, f.session, tool, map[string]any{"name": "", "definition": definition})
		require.Equal(t, "request", result["errors"].([]any)[0].(map[string]any)["stage"])
		result = cronOK(t, f.session, tool, map[string]any{"name": "Review"})
		require.Equal(t, "document", result["errors"].([]any)[0].(map[string]any)["stage"])
	}
	for _, resource := range []string{"job", "workflow"} {
		definition := cronDefinition()
		if resource == "workflow" {
			definition = cronWorkflowDefinition()
		}
		for _, raw := range []string{`null`, `[]`, `"text"`, `1`, `{"unknown":1}`, `{"version":1e1000}`} {
			result := cronOK(t, f.session, "cron_"+resource+"_validate", map[string]any{"name": "Review", "definition": json.RawMessage(raw)})
			require.Equal(t, false, result["valid"], raw)
			require.Equal(t, "document", result["errors"].([]any)[0].(map[string]any)["stage"])
		}
		opaque := json.RawMessage(`{"integer":12345678901234567890123,"decimal":1.0000000000000000001,"nested":[-0.00,1E+003]}`)
		definition["options"] = opaque
		if resource == "workflow" {
			definition["steps"].([]any)[0].(map[string]any)["options"] = opaque
		}
		result := cronCall(t, f.session, "cron_"+resource+"_validate", map[string]any{"name": "Review", "definition": definition})
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		raw := result.Content[0].(*sdkmcp.TextContent).Text
		for _, literal := range []string{"12345678901234567890123", "1.0000000000000000001", "-0.00", "1E+003"} {
			require.Contains(t, raw, literal)
		}
	}

	after, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "MCP validation adds no events")

}

func TestCronValidateParentScopeAndQuery(t *testing.T) {
	f := newCronFixture(t)
	parent, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Periodic review", Author: "operator"})
	require.NoError(t, err)
	definition := cronDefinition()
	definition["enabled"] = true
	definition["trigger"] = map[string]any{"kind": "interval", "interval_seconds": 60}
	definition["action"] = map[string]any{"kind": "notify", "recipient": "current-owner-or-author"}
	definition["overlap"] = "allow"
	definition["catchup"] = "latest"
	args := map[string]any{"name": "Review", "definition": definition, "parent_ref": parent.ShortID, "count": 2, "from": "2026-11-03T00:00:00Z"}
	// MCP startup retains its existing accountable actor; validation supplies
	// no per-call actor and performs no mutation attribution.
	session := connectTestServerWithOptions(t, Options{Client: f.client, ProjectID: f.project.ID, ProjectName: f.project.Name, Actor: "reader", Version: "test"})
	result := cronOK(t, session, "cron_job_validate", args)
	require.Equal(t, true, result["valid"])
	require.Equal(t, parent.UID, result["definition"].(map[string]any)["issue"].(map[string]any)["parent_uid"])
	schedule := result["schedule"].(map[string]any)
	require.Equal(t, "fresh_anchor", schedule["basis"])
	require.Equal(t, "2026-11-03T00:00:00Z", schedule["from"])
	require.Len(t, schedule["next"], 2)
	for _, parentUID := range []string{"", "01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		definition["issue"].(map[string]any)["parent_uid"] = parentUID
		failure := cronCall(t, session, "cron_job_validate", args)
		require.True(t, failure.IsError)
		require.Contains(t, failure.Content[0].(*sdkmcp.TextContent).Text, "parent_ref disagrees with definition.issue.parent_uid")
	}

}

func TestCronValidateRevisionConflictIsResult(t *testing.T) {
	f := newCronFixture(t)
	for _, resource := range []string{"job", "workflow"} {
		definition := cronDefinition()
		if resource == "workflow" {
			definition = cronWorkflowDefinition()
		}
		id := cronNewUID(t, f.session)
		args := map[string]any{"uid": id, "name": "Review", "definition": definition}
		stored := cronRecord(t, f.session, "cron_"+resource+"_create", args, resource)
		out := cronOK(t, f.session, "cron_"+resource+"_validate", args)
		require.Equal(t, false, out["valid"])
		args["expected_event_uid"] = "  " + strings.ToLower(stored["definition_event_uid"].(string)) + "  "
		require.Equal(t, true, cronOK(t, f.session, "cron_"+resource+"_validate", args)["valid"])
	}
}
