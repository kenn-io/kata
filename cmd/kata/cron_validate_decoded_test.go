package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLIValidateDecodedInputParity(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "operator")
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, name := range []string{"missing-enabled", "null-enabled", "null-after", "null-options", "null-workflow-options", "null-step-options"} {
		t.Run(name, func(t *testing.T) {
			resource := "job"
			raw, err := json.Marshal(cronCLIJob(t).Definition)
			require.NoError(t, err)
			var definition map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(raw, &definition))
			switch name {
			case "missing-enabled":
				delete(definition, "enabled")
			case "null-enabled":
				definition["enabled"] = jsontext.Value(`null`)
			case "null-options":
				definition["options"] = jsontext.Value(`null`)
			case "null-workflow-options":
				resource = "workflow"
				definition = map[string]jsontext.Value{"version": jsontext.Value(`1`), "options": jsontext.Value(`null`), "steps": jsontext.Value(`[{"key":"inspect","kind":"prompt","prompt":"Inspect"}]`)}
			case "null-step-options":
				resource = "workflow"
				definition = map[string]jsontext.Value{"version": jsontext.Value(`1`), "steps": jsontext.Value(`[{"key":"inspect","kind":"prompt","prompt":"Inspect","options":null}]`)}
			case "null-after":
				resource = "workflow"
				definition = map[string]jsontext.Value{"version": jsontext.Value(`1`), "steps": jsontext.Value(`[{"key":"inspect","kind":"prompt","prompt":"Inspect","after":null}]`)}
			}
			file := cronCLIFile(t, map[string]any{"name": name, "definition": definition})
			created, _, err := executeCronCLI(t, env.URL, nil, resource, "create", "--uid", cronCLIUID(t), "--file", file)
			require.NoError(t, err, "typed create accepts this decoded definition")
			out, _, err := executeCronCLI(t, env.URL, nil, resource, "validate", "--file", file)
			require.NoError(t, err, out)
			var result db.CronValidation
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			require.True(t, result.Valid)
			var envelope map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(created), &envelope))
			var saved struct {
				Definition jsontext.Value `json:"definition"`
			}
			require.NoError(t, json.Unmarshal(envelope[resource], &saved))
			require.JSONEq(t, string(saved.Definition), string(result.Definition))
			if name == "missing-enabled" || name == "null-enabled" {
				require.Equal(t, "paused", result.Warnings[0].Code)
				require.Contains(t, string(result.Definition), `"enabled":false`)
			}
		})
	}
}
