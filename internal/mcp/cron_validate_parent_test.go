package mcpserver

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCronValidateParentDocumentStage(t *testing.T) {
	f := newCronFixture(t)
	parent, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Periodic review", Author: "operator"})
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		issue any
		omit  bool
	}{
		{"missing", nil, true}, {"null", nil, false}, {"array", []any{}, false}, {"string", "invalid", false}, {"number", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := cronDefinition()
			delete(definition, "issue")
			if !tc.omit {
				definition["issue"] = tc.issue
			}
			result := cronOK(t, f.session, "cron_job_validate", map[string]any{"name": "Inspect", "definition": definition, "parent_ref": parent.ShortID})
			require.Equal(t, false, result["valid"])
			failures := result["errors"].([]any)
			require.Len(t, failures, 1)
			require.Equal(t, "document", failures[0].(map[string]any)["stage"])
		})
	}
	// Preserve the reviewer's exact empty-definition reproduction too.
	result := cronOK(t, f.session, "cron_job_validate", map[string]any{"name": "Inspect", "definition": map[string]any{}, "parent_ref": parent.ShortID})
	require.Equal(t, false, result["valid"])
	require.Equal(t, "document", result["errors"].([]any)[0].(map[string]any)["stage"])
}

func TestCronValidateNullParentUIDMatchesCreate(t *testing.T) {
	f := newCronFixture(t)
	parent, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Periodic review", Author: "operator"})
	require.NoError(t, err)
	definition := cronDefinition()
	definition["trigger"] = map[string]any{"kind": "interval", "interval_seconds": 60}
	definition["action"] = map[string]any{"kind": "notify", "recipient": "current-owner-or-author"}
	definition["overlap"] = "allow"
	definition["catchup"] = "latest"
	definition["issue"].(map[string]any)["parent_uid"] = nil
	args := map[string]any{"uid": cronNewUID(t, f.session), "name": "Inspect", "definition": definition, "parent_ref": parent.ShortID}
	saved := cronRecord(t, f.session, "cron_job_create", args, "job")
	args["expected_event_uid"] = saved["definition_event_uid"]
	before, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	response := cronCall(t, f.session, "cron_job_validate", args)
	require.False(t, response.IsError, "%s", mustJSON(t, response.Content))
	result := response.StructuredContent.(map[string]any)
	require.Equal(t, true, result["valid"])
	require.Equal(t, parent.UID, result["definition"].(map[string]any)["issue"].(map[string]any)["parent_uid"])
	after, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestCronValidateParentOptionParity(t *testing.T) {
	f := newCronFixture(t)
	parent, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Review target", Author: "operator"})
	require.NoError(t, err)
	for _, issue := range []string{"missing", "null"} {
		t.Run(issue, func(t *testing.T) {
			definition := cronDefinition()
			definition["enabled"] = true
			definition["trigger"] = map[string]any{"kind": "issue-scheduled", "issue_uid": parent.UID}
			definition["action"] = map[string]any{"kind": "notify", "recipient": "current-owner-or-author", "message": "Review"}
			delete(definition, "issue")
			if issue == "null" {
				definition["issue"] = nil
			}
			args := map[string]any{"uid": cronNewUID(t, f.session), "name": "Review", "definition": definition}
			control := cronOK(t, f.session, "cron_job_validate", args)
			require.Equal(t, true, control["valid"], "otherwise valid definition")
			args["parent_ref"] = parent.ShortID
			before, err := f.store.MaxEventID(t.Context())
			require.NoError(t, err)
			created := cronCall(t, f.session, "cron_job_create", args)
			require.True(t, created.IsError)
			require.Contains(t, created.Content[0].(*sdkmcp.TextContent).Text, "parent_ref requires definition.issue")
			validated := cronOK(t, f.session, "cron_job_validate", args)
			after, err := f.store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after)
			t.Logf("create rejects: %s; validation valid=%v errors=%v schedule=%v", created.Content[0].(*sdkmcp.TextContent).Text, validated["valid"], validated["errors"], validated["schedule"])
			require.Equal(t, false, validated["valid"], "validation must reject the parent option that create rejects")
			require.NotContains(t, validated, "schedule")
			failures := validated["errors"].([]any)
			require.Len(t, failures, 1)
			require.Equal(t, "request", failures[0].(map[string]any)["stage"])
			require.Equal(t, "validation", failures[0].(map[string]any)["code"])
			require.Equal(t, created.Content[0].(*sdkmcp.TextContent).Text, failures[0].(map[string]any)["message"])
		})
	}
}
