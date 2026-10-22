package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The product contract is copyable request bodies in create AND update help.
// Extract the rendered bodies rather than duplicating fixtures: a missing
// issue policy, invalid field, or broken workflow dependency must fail here.
func TestCronHelpRequestExamples(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		for _, action := range []string{"create", "update"} {
			t.Run(resource+"/"+action, func(t *testing.T) {
				t.Setenv("KATA_AUTHOR", "worker")
				env := testenv.New(t)
				project, err := env.DB.CreateProject(t.Context(), "spoke-project")
				require.NoError(t, err)
				parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
					ProjectID: project.ID, Title: "Ongoing review", Author: "worker",
				})
				require.NoError(t, err)
				help := string(executeRoot(t, newRootCmd(), "cron", resource, action, "--help"))
				examples := cronHelpRequests(t, help)
				wantResources := []string{"workflow"}
				if resource == "job" {
					wantResources = []string{"job", "job", "workflow", "job"}
				}
				require.Len(t, examples, len(wantResources), "help must show every common request kind")
				for i, example := range examples {
					args := example.args
					require.Equal(t, []string{"kata", "cron", wantResources[i]}, args[:3])
					args = args[2:]
					identity := cronCLIUID(t)
					file := filepath.Join(t.TempDir(), "request.json")
					require.NoError(t, os.WriteFile(file, example.body, 0o600))
					for j, arg := range args {
						switch arg {
						case "<job-uid>", "<workflow-uid>":
							args[j] = identity
						case "<parent-ref>":
							args[j] = parent.ShortID
						}
						if j > 0 && args[j-1] == "--file" {
							args[j] = file
						}
					}
					if args[1] == "update" {
						createArgs := []string{args[0], "create", "--uid", identity, "--file", file}
						for j, arg := range args {
							if arg == "--parent" {
								createArgs = append(createArgs, arg, args[j+1])
							}
						}
						out, _, err := executeCronCLI(t, env.URL, nil, createArgs...)
						require.NoError(t, err)
						type definitionRevision struct {
							DefinitionEventUID string `json:"definition_event_uid"`
						}
						var result struct {
							Job      definitionRevision `json:"job"`
							Workflow definitionRevision `json:"workflow"`
						}
						require.NoError(t, json.Unmarshal([]byte(out), &result))
						eventUID := result.Job.DefinitionEventUID
						if args[0] == "workflow" {
							eventUID = result.Workflow.DefinitionEventUID
						}
						for j, arg := range args {
							if arg == "<event-uid>" {
								args[j] = eventUID
							}
						}
					}
					out, _, err := executeCronCLI(t, env.URL, nil, args...)
					require.NoError(t, err, "example %d: %s", i, example.body)
					if args[0] == "workflow" {
						var result struct {
							Workflow api.CronWorkflow `json:"workflow"`
						}
						require.NoError(t, json.Unmarshal([]byte(out), &result))
						steps := result.Workflow.Definition.Steps
						require.Len(t, steps, 2)
						require.Equal(t, []string{steps[0].Key}, steps[1].After)
						require.Equal(t, "command", steps[0].Kind)
						require.Equal(t, "command", steps[1].Kind)
					} else {
						var result struct {
							Job api.CronJob `json:"job"`
						}
						require.NoError(t, json.Unmarshal([]byte(out), &result))
						definition := result.Job.Definition
						require.True(t, definition.Enabled)
						require.Equal(t, "cron", definition.Trigger.Kind)
						switch i {
						case 0:
							require.Equal(t, "notify", definition.Action.Kind)
							require.Equal(t, parent.UID, definition.Issue.ParentUID)
						case 1:
							require.Equal(t, "execute", definition.Action.Kind)
							require.NotEmpty(t, definition.Action.Prompt)
							var options map[string]string
							require.NoError(t, json.Unmarshal(definition.Options, &options))
							require.Equal(t, "codex", options["agent"])
						case 3:
							require.Equal(t, "execute", definition.Action.Kind)
							require.NotEmpty(t, definition.Action.WorkflowUID)
						}
					}
					if args[1] == "update" {
						_, _, err := executeCronCLI(t, env.URL, nil, args...)
						require.Error(t, err, "the example's old event UID must no longer authorize an edit")
					}
				}
			})
		}
	}
}

type cronHelpRequest struct {
	body []byte
	args []string
}

func TestCronReferenceRequestExamples(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Ongoing review", Author: "worker",
	})
	require.NoError(t, err)
	doc, err := os.ReadFile("../../docs/reference/cron.md")
	require.NoError(t, err)
	count := 0
	for _, match := range regexp.MustCompile("(?s)```json\n(.*?)\n```").FindAllSubmatch(doc, -1) {
		var request struct {
			Name       string `json:"name"`
			Definition struct {
				Kind   string `json:"kind"`
				Action struct {
					Kind string `json:"kind"`
				} `json:"action"`
			} `json:"definition"`
		}
		require.NoError(t, json.Unmarshal(match[1], &request))
		if request.Name == "" {
			continue // Run observations have a different request contract.
		}
		resource, identity := "job", cronCLIUID(t)
		if request.Definition.Kind != "job" {
			resource, identity = "workflow", "01J00000000000000000000010"
		}
		args := []string{resource, "create", "--uid", identity, "--file", "-"}
		if request.Definition.Action.Kind == "notify" {
			args = append(args, "--parent", parent.ShortID)
		}
		_, _, err := executeCronCLI(t, env.URL, strings.NewReader(string(match[1])), args...)
		require.NoError(t, err, "reference request %q", request.Name)
		count++
	}
	require.Equal(t, 4, count, "exercise all job and workflow request examples")
}

func cronHelpRequests(t *testing.T, help string) []cronHelpRequest {
	t.Helper()
	var requests []cronHelpRequest
	// Examples indent each complete JSON body by two spaces, like commands.
	for _, start := range regexp.MustCompile(`(?m)^  \{\n`).FindAllStringIndex(help, -1) {
		decoder := jsontext.NewDecoder(strings.NewReader(help[start[0]:]))
		body, err := decoder.ReadValue()
		require.NoError(t, err)
		commands, err := helpCommandLines(help[start[0]+int(decoder.InputOffset()):], false)
		require.NoError(t, err)
		require.NotEmpty(t, commands, "each body needs its command")
		args, err := splitHelpWords(commands[0])
		require.NoError(t, err)
		requests = append(requests, cronHelpRequest{body: body, args: args})
	}
	return requests
}
