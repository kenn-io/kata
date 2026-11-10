package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronBundleHistoricalReadBypassesCurrentSDKShape(t *testing.T) {
	project, workflow, event := cronCLIUID(t), cronCLIUID(t), cronCLIUID(t)
	definition := jsontext.Value(`{"version":99,"options":[1,9007199254740993],"steps":[]}`)
	_, err := cron.DecodeWorkflow(definition)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = fmt.Fprintf(w, `{"projects":[{"id":7,"name":"spoke-project","uid":%q}]}`, project)
		case "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"aliases":[]}`)
		case "/api/v1/projects/7/cron/workflows/" + workflow:
			require.NoError(t, json.MarshalWrite(w, map[string]any{"workflow": map[string]any{"uid": workflow, "project_id": 7, "name": "Historical", "definition_event_uid": event, "definition": definition}}))
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	out, _, err := executeCronCLI(t, server.URL, nil, "export", "--workflow", workflow)
	require.NoError(t, err)
	b, err := cron.ParseBundle([]byte(out))
	require.NoError(t, err)
	require.Contains(t, string(b.Workflows[0].Definition), `[1,9007199254740993]`)
}

func TestCronBundleHistoricalReadUsesAPIPresence(t *testing.T) {
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	id := cronCLIUID(t)
	_, _, err = executeCronCLI(t, env.URL, nil, "job", "create", "--uid", id, "--file", cronCLIFile(t, cronCLIJob(t)))
	require.NoError(t, err)
	// Test-local row fixture: older readable shape omits enabled and violates current action admission.
	historical := `{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute"},"issue":{"kind":"per-run","title":"Inspect"},"overlap":"forbid","catchup":"skip"}`
	_, err = env.DB.ExecContext(t.Context(), `UPDATE cron_jobs SET definition_json=$1 WHERE uid=$2`, historical, id)
	require.NoError(t, err)
	shown, _, err := executeCronCLI(t, env.URL, nil, "job", "show", id)
	require.NoError(t, err)
	var response struct {
		Job struct {
			Definition jsontext.Value `json:"definition"`
		} `json:"job"`
	}
	require.NoError(t, json.Unmarshal([]byte(shown), &response))
	require.Contains(t, string(response.Job.Definition), `"enabled":false`, "the existing API supplies current field presence")
	out, _, err := executeCronCLI(t, env.URL, nil, "export", "--job", id)
	require.NoError(t, err)
	b, err := cron.ParseBundle([]byte(out))
	require.NoError(t, err)
	require.JSONEq(t, string(response.Job.Definition), string(b.Jobs[0].Definition))
}
