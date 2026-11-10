package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCronBundleRoundTrip(t *testing.T) {
	f := newCronFixture(t)
	workflow := cronRecord(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Fixture workflow", "definition": cronWorkflowDefinition()}, "workflow")
	definition := cronDefinition()
	definition["enabled"] = true
	definition["action"] = map[string]any{"kind": "execute", "workflow_uid": workflow["uid"]}
	job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Fixture job", "definition": definition}, "job")
	bundle := cronOK(t, f.session, "cron_export", map[string]any{"job_uids": []any{job["uid"]}})
	require.Len(t, bundle["workflows"], 1)
	require.Len(t, bundle["jobs"], 1)
	before, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	args := map[string]any{"bundle": bundle, "dry_run": true}
	dry := cronOK(t, f.session, "cron_import", args)
	require.Len(t, dry["items"], 2)
	after, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	delete(args, "dry_run")
	report := cronOK(t, f.session, "cron_import", args)
	items := report["items"].([]any)
	copied := items[1].(map[string]any)
	shown := cronRecord(t, f.session, "cron_job", map[string]any{"uid": copied["uid"]}, "job")
	copiedDef := shown["definition"].(map[string]any)
	require.Equal(t, false, copiedDef["enabled"])
	cronOK(t, f.session, "cron_job_update", map[string]any{"uid": copied["uid"], "name": "Changed copy", "definition": copiedDef, "expected_event_uid": shown["definition_event_uid"]})
	conflict := cronCall(t, f.session, "cron_import", args)
	require.True(t, conflict.IsError)
	args["replace"] = true
	replaced := cronOK(t, f.session, "cron_import", args)
	require.Equal(t, "replaced", replaced["items"].([]any)[1].(map[string]any)["outcome"])
}

func TestCronBundleCrossProjectMappings(t *testing.T) {
	f := newCronFixture(t)
	source, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Source fixture", Author: "example-agent"})
	require.NoError(t, err)
	definition := cronDefinition()
	definition["action"] = map[string]any{"kind": "notify", "recipient": "example-agent/worker"}
	definition["trigger"] = map[string]any{"kind": "interval", "interval_seconds": 60}
	definition["overlap"] = "allow"
	definition["catchup"] = "latest"
	definition["issue"] = map[string]any{"kind": "per-run", "title": "Fixture", "parent_uid": source.UID}
	job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Mapped job", "definition": definition}, "job")
	bundle := cronOK(t, f.session, "cron_export", map[string]any{"job_uids": []any{job["uid"]}})
	target, err := f.store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "Target fixture", Author: "example-agent"})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: f.client, Scope: NewAllScope(), Actor: "example-agent", Version: "test"})
	args := map[string]any{"project": target.Name, "bundle": bundle}
	second, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "Second target fixture", Author: "example-agent"})
	require.NoError(t, err)
	args["issue_mappings"] = map[string]string{source.UID: issue.UID, strings.ToLower(source.UID): second.UID}
	conflict := cronCall(t, session, "cron_import", args)
	require.True(t, conflict.IsError)
	require.Equal(t, "conflicting issue mappings for "+source.UID, conflict.Content[0].(*sdkmcp.TextContent).Text)
	args["issue_mappings"] = map[string]string{source.UID: issue.UID}
	report := cronOK(t, session, "cron_import", args)
	copied := report["items"].([]any)[0].(map[string]any)
	shown := cronRecord(t, session, "cron_job", map[string]any{"project": target.Name, "uid": copied["uid"]}, "job")
	require.Equal(t, issue.UID, shown["definition"].(map[string]any)["issue"].(map[string]any)["parent_uid"])
}

func TestCronBundleExactNumbers(t *testing.T) {
	f := newCronFixture(t)
	workflow := cronRecord(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Exact workflow", "definition": cronWorkflowDefinition()}, "workflow")
	bundle := cronOK(t, f.session, "cron_export", map[string]any{"workflow_uids": []any{workflow["uid"]}})
	for _, token := range []string{"9007199254740993", "-9007199254740993", "1.0000000000000000001"} {
		entry := bundle["workflows"].([]any)[0].(map[string]any)
		entry["definition"].(map[string]any)["options"] = json.RawMessage(`{"number":` + token + `}`)
		result := cronCall(t, f.session, "cron_import", map[string]any{"bundle": bundle, "replace": true})
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		copied := result.StructuredContent.(map[string]any)["items"].([]any)[0].(map[string]any)
		exported := cronCall(t, f.session, "cron_export", map[string]any{"workflow_uids": []any{copied["uid"]}})
		require.False(t, exported.IsError)
		require.Contains(t, exported.Content[0].(*sdkmcp.TextContent).Text, token)
	}
}

func TestCronBundleSelectionAndStrictInput(t *testing.T) {
	f := newCronFixture(t)
	selection := cronCall(t, f.session, "cron_export", map[string]any{})
	require.True(t, selection.IsError)
	require.Equal(t, "select jobs and/or workflows (CLI: --job/--workflow; MCP: job_uids/workflow_uids), or all alone (CLI: --all; MCP: all)", selection.Content[0].(*sdkmcp.TextContent).Text)
	workflow := cronRecord(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Strict fixture", "definition": cronWorkflowDefinition()}, "workflow")
	bundle := cronOK(t, f.session, "cron_export", map[string]any{"workflow_uids": []any{workflow["uid"]}})
	before, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	bundle["unexpected"] = true
	require.True(t, cronCall(t, f.session, "cron_import", map[string]any{"bundle": bundle}).IsError)
	after, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}
