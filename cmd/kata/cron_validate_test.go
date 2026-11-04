package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Preflight prints its result even when invalid/quiet, exits 3, and never
// allocates a definition UID or requires mutation attribution.
func TestCronCLIValidate(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Periodic review", Author: "operator"})
	require.NoError(t, err)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	for _, resource := range []string{"job", "workflow"} {
		var body any = cronCLIJob(t)
		if resource == "workflow" {
			body = api.PutCronWorkflowBody{Name: "Review", Definition: api.CronWorkflowDefinition{Version: 1, Steps: []api.CronWorkflowStep{{Key: "inspect", Kind: "prompt", Prompt: "Inspect"}}}}
		}
		file := cronCLIFile(t, body)
		out, diagnostic, err := executeCronCLI(t, env.URL, nil, resource, "validate", "--file", file)
		require.NoError(t, err)
		require.NotContains(t, diagnostic, "Definition UID")
		var result db.CronValidation
		require.NoError(t, json.Unmarshal([]byte(out), &result))
		require.True(t, result.Valid)
		for _, withParent := range []bool{false, true} {
			if withParent && resource != "job" {
				continue
			}
			bodies := []string{`{"name":"Inspect","definition":{"version":1e1000}}`, `{"name":"Inspect"}`}
			if withParent {
				bodies = append(bodies, `{"name":"Inspect","definition":null}`, `{"name":"Inspect","definition":[]}`, `{"name":"Inspect","definition":{"issue":{"kind":"existing"}}}`)
			}
			for _, raw := range bodies {
				args := []string{resource, "validate", "--file", "-", "--quiet"}
				if withParent {
					args = append(args, "--parent", parent.ShortID)
				}
				out, _, err = executeCronCLIWithoutJSON(t, env.URL, strings.NewReader(raw), args...)
				var cliErr *cliError
				require.ErrorAs(t, err, &cliErr)
				require.Equal(t, ExitValidation, cliErr.ExitCode)
				require.NoError(t, json.Unmarshal([]byte(out), &result), "document failures print their staged result")
				require.False(t, result.Valid)
				require.Equal(t, "document", result.Errors[0].Stage)
			}
		}
		out, _, err = executeCronCLI(t, env.URL, nil, resource, "validate", "--file", cronCLIFile(t, map[string]any{"name": "Review", "definition": body, "uid": nil}))
		require.ErrorContains(t, err, "uid must be a string")
		require.Empty(t, out)

		_, _, err = executeCronCLI(t, env.URL, nil, resource, "validate")
		require.ErrorContains(t, err, "--file is required")
	}
	// Rendered recurring example is valid once its parent is resolved.
	examples := cronHelpRequests(t, string(executeRoot(t, newRootCmd(), "cron", "job", "create", "--help")))
	out, _, err := executeCronCLI(t, env.URL, bytes.NewReader(examples[0].body), "job", "validate", "--file", "-", "--parent", parent.ShortID, "--count", "2", "--from", "2026-11-03T00:00:00Z")
	require.NoError(t, err)
	var result db.CronValidation
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.True(t, result.Valid)
	require.Len(t, result.Schedule.Next, 2)
	require.Contains(t, string(result.Definition), parent.UID)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "CLI preflight adds no events")
}

func TestCronCLIValidateOldDaemonAndMalformedResponse(t *testing.T) {
	for _, tc := range []struct{ name, version, response, want string }{
		{"old", "0.41.0", `{}`, "upgrade the daemon"},
		{"missing-valid", "0.42.0", `{"name":"Inspect","definition":{},"errors":[],"warnings":[]}`, "validation response"},
		{"wrong-valid", "0.42.0", `{"valid":"true"}`, "CronValidationResult.valid"},
		{"invalid-json", "0.42.0", `{`, "unexpected end of JSON input"},
		{"null-definition", "0.42.0", `{"valid":true,"name":"Inspect","definition":null,"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`, "validation response"},
		{"scalar-definition", "0.42.0", `{"valid":true,"name":"Inspect","definition":42,"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`, "validation response"},
		{"missing-name", "0.42.0", `{"valid":true,"definition":{},"errors":[],"warnings":[],"schedule":{"delivery":"adapter","basis":"none","zone":"UTC","from":"2026-11-03T00:00:00Z","next":[]}}`, "validation response"},
		{"missing-schedule", "0.42.0", `{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[]}`, "validation response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v1/health":
					_, _ = fmt.Fprintf(w, `{"api_schema_version":%q}`, tc.version)
				case strings.HasSuffix(r.URL.Path, "/validate"):
					calls++
					_, _ = fmt.Fprint(w, tc.response)
				default:
					_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"},"projects":[{"id":1,"name":"spoke-project"}]}`)
				}
			}))
			defer server.Close()
			out, _, err := executeCronCLI(t, server.URL, nil, "job", "validate", "--file", cronCLIFile(t, cronCLIJob(t)))
			require.Error(t, err)
			require.Empty(t, out, "malformed daemon success must not be printed")
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			}
			if tc.name == "old" {
				require.Zero(t, calls)
			}
		})
	}
}

func TestCronCLIValidateConflictAndIdentity(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	input := cronCLIJob(t)
	stored, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: input.Name, Definition: input.Definition.Native(), Actor: "operator"})
	require.NoError(t, err)
	file := cronCLIFile(t, input)
	out, _, err := executeCronCLIWithoutJSON(t, env.URL, nil, "job", "validate", "--file", file, "--uid", stored.UID, "--quiet")
	var cliErr *cliError
	require.ErrorAs(t, err, &cliErr)
	require.Equal(t, ExitValidation, cliErr.ExitCode)
	var result db.CronValidation
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.False(t, result.Valid)
	require.Equal(t, stored.DefinitionEventUID, result.Errors[0].DefinitionEventUID)
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "validate", "--file", file, "--uid", stored.UID, "--expected-event-uid", stored.DefinitionEventUID)
	require.NoError(t, err)
	withUID := map[string]any{"uid": stored.UID, "name": input.Name, "definition": input.Definition}
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "validate", "--file", cronCLIFile(t, withUID), "--uid", cronCLIUID(t))
	require.ErrorContains(t, err, "uid in body and flag must match")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/validate"):
			calls++
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			require.NoError(t, conn.Close())
		case r.URL.Path == "/api/v1/health":
			_, _ = fmt.Fprint(w, `{"api_schema_version":"0.42.0"}`)
		default:
			_, _ = fmt.Fprint(w, `{"project":{"id":1,"name":"spoke-project"},"projects":[{"id":1,"name":"spoke-project"}]}`)
		}
	}))
	defer server.Close()
	_, _, err = executeCronCLI(t, server.URL, nil, "job", "validate", "--file", file)
	require.ErrorContains(t, err, "selected daemon connection closed before response headers arrived")
	require.Equal(t, 1, calls, "transport failure must occur on validation, after project resolution")
}

func TestCronCLIValidateOutputFailure(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetOut(extraFailWriter{})
	err := emitCronValidation(cmd, []byte(`{"valid":true,"name":"Inspect","definition":{},"errors":[],"warnings":[]}`), false, false)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}
