package main

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The API and both CLI machine formats expose the recorded last editor on
// mutation, show and list responses, independently of the preserved creator.
func TestCronCLIDefinitionEditors(t *testing.T) {
	for _, mode := range []string{"--json", "--agent"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("KATA_AUTHOR", "creator")
			env := testenv.New(t)
			project, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			run := func(args ...string) map[string]any {
				t.Helper()
				resetFlags(t)
				command := newRootCmd()
				var out, diagnostic bytes.Buffer
				command.SetOut(&out)
				command.SetErr(&diagnostic)
				command.SetContext(contextWithBaseURL(t.Context(), env.URL))
				command.SetArgs(append([]string{"--project", "spoke-project", mode, "cron"}, args...))
				require.NoError(t, command.Execute(), diagnostic.String())
				var result map[string]any
				require.NoError(t, json.Unmarshal(out.Bytes(), &result))
				return result
			}
			check := func(value any, editor any) map[string]any {
				t.Helper()
				fields := value.(map[string]any)
				require.Contains(t, fields, "updated_by")
				require.Equal(t, editor, fields["updated_by"])
				require.Equal(t, "creator", fields["author"])
				return fields
			}
			for _, resource := range []string{"job", "workflow"} {
				id := cronCLIUID(t)
				var input any = cronCLIJob(t)
				if resource == "workflow" {
					input = api.PutCronWorkflowBody{Name: "Inspection", Definition: api.CronWorkflowDefinition{Version: 1, Steps: []api.CronWorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}}
				}
				file := cronCLIFile(t, input)
				fields := check(run(resource, "create", "--uid", id, "--file", file)[resource], "creator")
				for _, step := range []struct{ operation, actor string }{{"update", "editor"}, {"delete", "archiver"}, {"restore", "restorer"}} {
					t.Setenv("KATA_AUTHOR", step.actor)
					args := []string{resource, step.operation, id, "--expected-event-uid", fields["definition_event_uid"].(string)}
					if step.operation == "update" {
						args = append(args, "--file", file)
					}
					fields = check(run(args...)[resource], step.actor)
					check(run(resource, "show", id)[resource], step.actor)
					values := run(resource, "list", "--include-deleted")[resource+"s"].([]any)
					require.Len(t, values, 1)
					check(values[0], step.actor)
				}
				t.Setenv("KATA_AUTHOR", "creator")
			}
			// Adoption keeps definition UIDs but discards their original events.
			_, err = env.DB.AdoptProjectIntoFederation(t.Context(), db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: cronCLIUID(t), ReplayHorizonEventID: 1, Actor: "snapshot-actor"})
			require.NoError(t, err)
			for _, resource := range []string{"job", "workflow"} {
				values := run(resource, "list")[resource+"s"].([]any)
				require.Len(t, values, 1)
				fields := check(values[0], nil)
				check(run(resource, "show", fields["uid"].(string))[resource], nil)
			}
		})
	}
}
