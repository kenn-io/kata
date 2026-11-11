package mcpserver

import (
	"encoding/json"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func cronWorkflowDefinition() map[string]any {
	return map[string]any{"version": 1, "steps": []any{map[string]any{"key": "inspect", "kind": "prompt", "prompt": "Inspect synthetic fixture"}}}
}
func TestCronWorkflowLifecycleAndInUseConflict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		definition map[string]any
	}{
		{"prompt", cronWorkflowDefinition()},
		{"argv", map[string]any{"version": 1, "steps": []any{map[string]any{"key": "run", "kind": "argv", "argv": []any{"printf", "", "a b"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCronFixture(t)
			id := cronNewUID(t, f.session)
			args := map[string]any{"uid": id, "name": "Fixture workflow", "definition": tc.definition}
			var expectedDefinition map[string]any
			require.NoError(t, json.Unmarshal(mustJSON(t, tc.definition), &expectedDefinition))
			check := func(workflow map[string]any) {
				t.Helper()
				require.Equal(t, expectedDefinition, workflow["definition"])
			}
			workflow := cronRecord(t, f.session, "cron_workflow_create", args, "workflow")
			require.Equal(t, id, workflow["uid"])
			require.Equal(t, "example-agent", workflow["author"])
			check(workflow)
			require.Equal(t, workflow, cronRecord(t, f.session, "cron_workflow", map[string]any{"uid": id}, "workflow"))
			listed := cronOK(t, f.session, "cron_workflows", map[string]any{})["workflows"].([]any)
			require.Len(t, listed, 1)
			check(listed[0].(map[string]any))
			require.True(t, cronCall(t, f.session, "cron_workflow_create", args).IsError)
			require.True(t, cronCall(t, f.session, "cron_workflow_update", args).IsError, "CAS must be caller supplied")
			args["expected_event_uid"] = workflow["definition_event_uid"]
			args["name"] = "Replacement"
			workflow = cronRecord(t, f.session, "cron_workflow_update", args, "workflow")
			check(workflow)
			t.Run("stale replacement", func(t *testing.T) {
				conflict := cronCall(t, f.session, "cron_workflow_update", args)
				require.True(t, conflict.IsError, "stale CAS conflicts")
				require.NotEmpty(t, conflict.Content)
				var envelope struct {
					Status int `json:"status"`
					Error  struct {
						Code string `json:"code"`
						Data struct {
							Current map[string]any `json:"current"`
						} `json:"data"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(conflict.Content[0].(*sdkmcp.TextContent).Text), &envelope))
				require.Equal(t, 409, envelope.Status)
				require.Equal(t, "cron_conflict", envelope.Error.Code)
				require.Equal(t, workflow["definition_event_uid"], envelope.Error.Data.Current["definition_event_uid"])
				check(envelope.Error.Data.Current)
			})
			definition := cronDefinition()
			definition["action"] = map[string]any{"kind": "execute", "workflow_uid": id}
			job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Workflow job", "definition": definition}, "job")
			action := map[string]any{"uid": id, "expected_event_uid": workflow["definition_event_uid"]}
			require.True(t, cronCall(t, f.session, "cron_workflow_delete", action).IsError, "in-use workflow cannot be archived")
			cronOK(t, f.session, "cron_job_delete", map[string]any{"uid": job["uid"], "expected_event_uid": job["definition_event_uid"]})
			deleted := cronRecord(t, f.session, "cron_workflow_delete", action, "workflow")
			check(deleted)
			require.NotNil(t, deleted["deleted_at"])
			require.Empty(t, cronOK(t, f.session, "cron_workflows", map[string]any{})["workflows"])
			listed = cronOK(t, f.session, "cron_workflows", map[string]any{"include_deleted": true})["workflows"].([]any)
			require.Len(t, listed, 1)
			check(listed[0].(map[string]any))
			require.True(t, cronCall(t, f.session, "cron_workflow_restore", action).IsError, "restore also requires current tombstone event")
			restored := cronRecord(t, f.session, "cron_workflow_restore", map[string]any{"uid": id, "expected_event_uid": deleted["definition_event_uid"]}, "workflow")
			check(restored)
			require.Nil(t, restored["deleted_at"])
		})
	}
}

func TestCronWorkflowStrictDefinitions(t *testing.T) {
	f := newCronFixture(t)
	for _, definition := range []map[string]any{
		{"version": 2, "steps": []any{}},
		{"version": 1, "steps": []any{map[string]any{"key": "inspect", "kind": "prompt", "prompt": "Inspect", "argv": nil}}},
		{"version": 1, "steps": []any{map[string]any{"key": "inspect", "kind": "command", "command": "true", "after": []string{"missing"}}}},
		{"version": 1, "steps": []any{map[string]any{"key": "inspect", "kind": "prompt", "prompt": "Inspect"}}, "options": map[string]any{"api_token": "secret-fixture"}},
		{"version": 1, "steps": []any{map[string]any{"key": "inspect", "kind": "prompt", "prompt": "Inspect", "unexpected": true}}},
	} {
		require.True(t, cronCall(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Invalid", "definition": definition}).IsError)
	}
	require.Empty(t, cronOK(t, f.session, "cron_workflows", map[string]any{})["workflows"])
}
func TestCronWorkflowOpaqueNumbers(t *testing.T) {
	f := newCronFixture(t)
	definition := cronWorkflowDefinition()
	opaque := json.RawMessage(`{"integer":9007199254740993,"nested":[-9007199254740993,1.0000000000000000001]}`)
	definition["options"] = opaque
	definition["steps"].([]any)[0].(map[string]any)["options"] = opaque
	result := cronCall(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Exact workflow", "definition": definition})
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	raw := result.Content[0].(*sdkmcp.TextContent).Text
	require.Contains(t, raw, "9007199254740993")
	require.Contains(t, raw, "1.0000000000000000001")
	var decoded struct {
		Workflow struct {
			Definition struct {
				Options map[string]json.RawMessage
				Steps   []struct{ Options map[string]json.RawMessage }
			}
		}
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
	require.Equal(t, "9007199254740993", string(decoded.Workflow.Definition.Options["integer"]))
	require.Equal(t, "9007199254740993", string(decoded.Workflow.Definition.Steps[0].Options["integer"]))
}

func TestCronWorkflowArgvInputPreservesValidation(t *testing.T) {
	f := newCronFixture(t)
	for _, step := range []map[string]any{
		{"key": "run", "kind": "argv", "argv": []any{"echo", nil}},
	} {
		definition := map[string]any{"version": 1, "steps": []any{step}}
		require.True(t, cronCall(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Invalid", "definition": definition}).IsError)
	}
	definition := map[string]any{"version": 1, "steps": []any{map[string]any{"key": "run", "kind": "argv", "argv": []string{"printf", "", "a b"}}}}
	workflow := cronRecord(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Literal", "definition": definition}, "workflow")
	stored := workflow["definition"].(map[string]any)
	step := stored["steps"].([]any)[0].(map[string]any)
	require.Equal(t, []any{"printf", "", "a b"}, step["argv"])
}
