package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cronbundle"
	"go.kenn.io/kata/internal/daemon"
	kataclient "go.kenn.io/kata/pkg/client"
)

// Adapter cases assert the requests it owns and retain raw responses even when
// a historical definition cannot decode into the generated current-write shape.
func TestCronBundleAdapterRoutesAndRawResponses(t *testing.T) {
	const definitionUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	type response struct {
		status int
		body   string
	}
	type request struct {
		method, path string
		body         []byte
		err          error
	}
	responses := make(chan response, 1)
	requests := make(chan request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		requests <- request{r.Method, r.URL.Path, body, err}
		next := <-responses
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(next.status)
		_, _ = io.WriteString(w, next.body)
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	d := cronBundleDaemon{client}
	for _, tc := range []struct{ kind, id, path, body string }{
		{"job", "", "/api/v1/projects/7/cron/jobs", `{"jobs":[]}`},
		{"job", definitionUID, "/api/v1/projects/7/cron/jobs/" + definitionUID, `{"job":{"name":"Job fixture"}}`},
		{"workflow", "", "/api/v1/projects/7/cron/workflows", `{"workflows":[]}`},
		{"workflow", definitionUID, "/api/v1/projects/7/cron/workflows/" + definitionUID, `{"workflow":{"uid":"` + definitionUID + `","project_id":7,"name":"Historical","definition_event_uid":"` + definitionUID + `","definition":{"version":99,"options":[1,9007199254740993],"steps":[]}}}`},
	} {
		responses <- response{http.StatusOK, tc.body}
		status, body, err := d.Read(t.Context(), 7, tc.kind, tc.id)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, []byte(tc.body), body)
		sent := <-requests
		require.NoError(t, sent.err)
		require.Equal(t, http.MethodGet, sent.method)
		require.Equal(t, tc.path, sent.path)
		require.Empty(t, sent.body)
	}
	const denied = `{"error":{"code":"forbidden","message":"Read-only fixture"}}`
	for _, kind := range []string{"job", "workflow"} {
		definition := cronDefinition()
		if kind == "workflow" {
			definition = cronWorkflowDefinition()
		}
		raw, err := json.Marshal(definition)
		require.NoError(t, err)
		responses <- response{http.StatusForbidden, denied}
		status, body, err := d.Put(t.Context(), 7, kind, definitionUID, cronbundle.PutBody{Actor: "fixture-actor", Name: "Fixture", Definition: raw, Expected: definitionUID})
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, status)
		require.Equal(t, []byte(denied), body)
		sent := <-requests
		require.NoError(t, sent.err)
		require.Equal(t, http.MethodPut, sent.method)
		require.Equal(t, "/api/v1/projects/7/cron/"+kind+"s/"+definitionUID, sent.path)
		expected, err := json.Marshal(map[string]any{"actor": "fixture-actor", "name": "Fixture", "definition": definition, "expected_event_uid": definitionUID})
		require.NoError(t, err)
		require.JSONEq(t, string(expected), string(sent.body))
	}
	responses <- response{http.StatusOK, `{"padding":"response larger than bound"}`}
	_, _, err = (cronBundleDaemon{client.WithResponseLimit(8)}).Read(t.Context(), 7, "job", definitionUID)
	require.ErrorIs(t, err, kataclient.ErrResponseTooLarge)
	sent := <-requests
	require.NoError(t, sent.err)
	require.Equal(t, http.MethodGet, sent.method)
	require.Equal(t, "/api/v1/projects/7/cron/jobs/"+definitionUID, sent.path)
}

func TestCronBundleReadonlyImportStopsBeforeJobs(t *testing.T) {
	f := newCronFixture(t)
	workflow := cronRecord(t, f.session, "cron_workflow_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Fixture", "definition": cronWorkflowDefinition()}, "workflow")
	definition := cronDefinition()
	definition["action"] = map[string]any{"kind": "execute", "workflow_uid": workflow["uid"]}
	job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Fixture job", "definition": definition}, "job")
	bundle := cronOK(t, f.session, "cron_export", map[string]any{"job_uids": []any{job["uid"]}})
	readonly := daemon.NewServer(daemon.ServerConfig{DB: f.store, InsecureReadonly: true})
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	server := httptest.NewServer(readonly.Handler())
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: f.project.ID, ProjectName: f.project.Name, Actor: "example-agent", Version: "test"})
	before, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	result := cronCall(t, session, "cron_import", map[string]any{"bundle": bundle})
	require.True(t, result.IsError)
	report := result.StructuredContent.(map[string]any)
	items := report["items"].([]any)
	require.Len(t, items, 1)
	require.Equal(t, "workflow", items[0].(map[string]any)["kind"])
	require.Equal(t, "failed", items[0].(map[string]any)["outcome"])
	after, err := f.store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}
