package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cronbundle"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/projectscope"
	kataclient "go.kenn.io/kata/pkg/client"
)

// The shared orchestration must retain the configured client's credentials,
// project narrowing, redirect policy, response bound, and cancellation.
func TestCronBundleAdapterRetainsClientPolicy(t *testing.T) {
	const projectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	var mode atomic.Int32
	var reached atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		require.Contains(t, r.Header.Values(projectscope.Header), `["`+projectUID+`"]`)
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 1:
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		case 2:
			_, _ = w.Write([]byte(`{"padding":"response larger than bound"}`))
			return
		}
		if r.Method == http.MethodPut {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "fixture-actor", body["actor"])
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"Read-only fixture"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jobs":[],"workflows":[]}`))
	}))
	t.Cleanup(server.Close)
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client, err := kataclient.NewWithHTTPClient(server.URL, httpClient, kataclient.WithRequestEditor(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer fixture-token")
		return nil
	}))
	require.NoError(t, err)
	client, err = client.WithProjectScope([]string{projectUID})
	require.NoError(t, err)
	d := cronBundleDaemon{client}
	for _, kind := range []string{"job", "workflow"} {
		for _, id := range []string{"", projectUID} {
			status, _, err := d.Read(t.Context(), 7, kind, id)
			require.NoError(t, err)
			require.Equal(t, 200, status)
		}
		definition := cronDefinition()
		if kind == "workflow" {
			definition = cronWorkflowDefinition()
		}
		raw, err := json.Marshal(definition)
		require.NoError(t, err)
		status, body, err := d.Put(t.Context(), 7, kind, projectUID, cronbundle.PutBody{Actor: "fixture-actor", Name: "Fixture", Definition: raw})
		require.NoError(t, err)
		require.Equal(t, 403, status)
		require.Contains(t, string(body), "Read-only fixture")
	}
	mode.Store(1)
	before := reached.Load()
	status, _, err := d.Read(t.Context(), 7, "job", projectUID)
	require.NoError(t, err)
	require.Equal(t, 307, status)
	require.Equal(t, before+1, reached.Load(), "configured redirect rejection must survive adapter calls")
	mode.Store(2)
	_, _, err = (cronBundleDaemon{client.WithResponseLimit(8)}).Read(t.Context(), 7, "job", projectUID)
	require.ErrorIs(t, err, kataclient.ErrResponseTooLarge)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before = reached.Load()
	_, _, err = d.Read(ctx, 7, "job", projectUID)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, reached.Load())
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
