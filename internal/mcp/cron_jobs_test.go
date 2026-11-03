package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronFixture struct {
	session *sdkmcp.ClientSession
	store   *sqlitestore.Store
	project db.Project
	client  *kataclient.Client
	handler http.Handler
}

func newCronFixture(t *testing.T) cronFixture {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: project.ID, ProjectName: project.Name, Actor: "example-agent", Version: "test", Teammate: "default-worker"})
	return cronFixture{session, store, project, client, server.Handler()}
}

func cronCall(t *testing.T, s *sdkmcp.ClientSession, name string, args any) *sdkmcp.CallToolResult {
	t.Helper()
	result, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata." + name, Arguments: args})
	require.NoError(t, err)
	return result
}
func cronOK(t *testing.T, s *sdkmcp.ClientSession, name string, args any) map[string]any {
	t.Helper()
	result := cronCall(t, s, name, args)
	require.False(t, result.IsError, "%s: %s", name, mustJSON(t, result))
	return result.StructuredContent.(map[string]any)
}
func cronRecord(t *testing.T, s *sdkmcp.ClientSession, name string, args any, key string) map[string]any {
	t.Helper()
	return cronOK(t, s, name, args)[key].(map[string]any)
}
func cronNewUID(t *testing.T, s *sdkmcp.ClientSession) string {
	t.Helper()
	return cronOK(t, s, "cron_new_uid", map[string]any{})["uid"].(string)
}
func cronDefinition() map[string]any {
	return map[string]any{"version": 1, "kind": "job", "enabled": false, "trigger": map[string]any{"kind": "manual"}, "action": map[string]any{"kind": "execute", "prompt": "Inspect fixture"}, "issue": map[string]any{"kind": "per-run", "title": "Inspect fixture"}, "overlap": "forbid", "catchup": "skip"}
}

func TestCronProgressiveDiscovery(t *testing.T) {
	s := connectRawTestServerWithOptions(t, Options{Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
	loaded := cronOK(t, s, "load_cron", map[string]any{})
	require.Equal(t, true, loaded["loaded"])
	after, err := s.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Contains(t, toolNames(after.Tools), "kata.cron_job_create")
	require.NotContains(t, toolNames(after.Tools), "kata.create")
	require.Equal(t, loaded, cronOK(t, s, "load_cron", map[string]any{}))
}
func TestCronUIDHelperHasNoDaemonMutation(t *testing.T) {
	s := connectRawTestServerWithOptions(t, Options{Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
	cronOK(t, s, "load_cron", map[string]any{})
	first, second := cronNewUID(t, s), cronOK(t, s, "cron_new_uid", map[string]any{"project": "unbound-project"})["uid"].(string)
	require.True(t, uid.Valid(first))
	require.True(t, uid.Valid(second))
	require.NotEqual(t, first, second)
}
func TestCronJobLifecycle(t *testing.T) {
	f := newCronFixture(t)
	id := cronNewUID(t, f.session)
	args := map[string]any{"uid": strings.ToLower(id), "name": "Fixture job", "definition": cronDefinition()}
	job := cronRecord(t, f.session, "cron_job_create", args, "job")
	require.Equal(t, id, job["uid"])
	require.Equal(t, "example-agent", job["author"])
	shown := cronRecord(t, f.session, "cron_job", map[string]any{"uid": id}, "job")
	require.Equal(t, job, shown)
	require.True(t, cronCall(t, f.session, "cron_job_create", args).IsError, "create retry must retain daemon conflict")
	require.Len(t, cronOK(t, f.session, "cron_jobs", map[string]any{})["jobs"], 1)
	args["name"] = "Replacement"
	args["expected_event_uid"] = job["definition_event_uid"]
	updated := cronRecord(t, f.session, "cron_job_update", args, "job")
	require.Equal(t, "Replacement", updated["name"])
	deleted := cronRecord(t, f.session, "cron_job_delete", map[string]any{"uid": id, "expected_event_uid": updated["definition_event_uid"]}, "job")
	require.NotNil(t, deleted["deleted_at"])
	require.Empty(t, cronOK(t, f.session, "cron_jobs", map[string]any{})["jobs"])
	require.Len(t, cronOK(t, f.session, "cron_jobs", map[string]any{"include_deleted": true})["jobs"], 1)
	restored := cronRecord(t, f.session, "cron_job_restore", map[string]any{"uid": id, "expected_event_uid": deleted["definition_event_uid"]}, "job")
	require.Nil(t, restored["deleted_at"])
}
func TestCronCreateLostResponseReadback(t *testing.T) {
	f := newCronFixture(t)
	id := cronNewUID(t, f.session)
	// Commit the PUT, then close the connection without returning any response bytes.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			captured := httptest.NewRecorder()
			f.handler.ServeHTTP(captured, r)
			require.Equal(t, http.StatusOK, captured.Code)
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	s := connectTestServerWithOptions(t, Options{Client: client, ProjectID: f.project.ID, ProjectName: f.project.Name, Actor: "example-agent", Version: "test"})
	require.True(t, cronCall(t, s, "cron_job_create", map[string]any{"uid": id, "name": "Recoverable", "definition": cronDefinition()}).IsError)

	recovered := cronRecord(t, f.session, "cron_job", map[string]any{"uid": id}, "job")
	require.Equal(t, id, recovered["uid"])
	require.Equal(t, "Recoverable", recovered["name"])
}
func TestCronJobStaleExpectedEvent(t *testing.T) {
	f := newCronFixture(t)
	id := cronNewUID(t, f.session)
	args := map[string]any{"uid": id, "name": "Fixture", "definition": cronDefinition()}
	first := cronRecord(t, f.session, "cron_job_create", args, "job")
	require.True(t, cronCall(t, f.session, "cron_job_update", args).IsError)
	args["expected_event_uid"] = first["definition_event_uid"]
	args["name"] = "Updated"
	latest := cronRecord(t, f.session, "cron_job_update", args, "job")
	args["name"] = "Stale"
	require.True(t, cronCall(t, f.session, "cron_job_update", args).IsError)
	require.True(t, cronCall(t, f.session, "cron_job_delete", map[string]any{"uid": id, "expected_event_uid": first["definition_event_uid"]}).IsError)
	require.Equal(t, latest, cronRecord(t, f.session, "cron_job", map[string]any{"uid": id}, "job"))
}
func TestCronParentScopeAndMismatch(t *testing.T) {
	f := newCronFixture(t)
	parent, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Parent", Author: "example-agent"})
	require.NoError(t, err)
	other, err := f.store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	foreign, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "Foreign", Author: "example-agent"})
	require.NoError(t, err)
	definition := cronDefinition()
	definition["overlap"] = "allow"
	definition["catchup"] = "latest"
	definition["action"] = map[string]any{"kind": "notify", "recipient": "example-agent/worker"}
	definition["trigger"] = map[string]any{"kind": "interval", "interval_seconds": 60}
	args := map[string]any{"uid": cronNewUID(t, f.session), "name": "Native notify", "parent_ref": parent.UID, "definition": definition}
	created := cronRecord(t, f.session, "cron_job_create", args, "job")
	require.Equal(t, parent.UID, created["definition"].(map[string]any)["issue"].(map[string]any)["parent_uid"])
	args["uid"] = cronNewUID(t, f.session)
	args["parent_ref"] = "other-project#" + foreign.ShortID
	require.True(t, cronCall(t, f.session, "cron_job_create", args).IsError)
	args["parent_ref"] = parent.UID
	definition["issue"].(map[string]any)["parent_uid"] = foreign.UID
	require.True(t, cronCall(t, f.session, "cron_job_create", args).IsError)
	args["parent_ref"] = ""
	require.True(t, cronCall(t, f.session, "cron_job_create", args).IsError, "daemon rejects a cross-project parent UID")
}
func TestCronDefinitionOpaqueNumbers(t *testing.T) {
	f := newCronFixture(t)
	id := cronNewUID(t, f.session)
	definition := cronDefinition()
	definition["options"] = json.RawMessage(`{"integer":9007199254740993,"decimal":1.0000000000000000001}`)
	result := cronCall(t, f.session, "cron_job_create", map[string]any{"uid": id, "name": "Exact options", "definition": definition})
	require.False(t, result.IsError, "%s", mustJSON(t, result))

	shown, err := f.client.ShowCronJob(t.Context(), &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: f.project.ID, CronUID: id}})
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740993"), shown.Job.Definition.Options["integer"])
	require.Equal(t, json.Number("1.0000000000000000001"), shown.Job.Definition.Options["decimal"])
	require.NotEmpty(t, result.Content)
	require.Contains(t, result.Content[0].(*sdkmcp.TextContent).Text, "9007199254740993")
	require.Contains(t, result.Content[0].(*sdkmcp.TextContent).Text, "1.0000000000000000001")
}
func TestCronAuthorityAndCapabilities(t *testing.T) {
	f := newCronFixture(t)
	caps := cronOK(t, f.session, "cron_capabilities", map[string]any{})
	require.Equal(t, f.project.UID, caps["project_uid"])
	require.Contains(t, caps["event_features"], "cron_v1")
	require.True(t, cronCall(t, f.session, "cron_capabilities", map[string]any{"project": "outside-project"}).IsError)
	// Preserve the daemon's authorization and capability failure, without upgrading it.
	for _, tc := range []struct {
		status        int
		code, message string
	}{
		{http.StatusForbidden, "forbidden", "cron access denied"},
		{http.StatusNotImplemented, "unsupported", "cron feature disabled"},
		{http.StatusServiceUnavailable, "unavailable", "cron backend offline"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				writeJSON(w, map[string]any{"status": tc.status, "error": map[string]any{"code": tc.code, "message": tc.message}})
			}))
			t.Cleanup(server.Close)
			client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			s := connectTestServerWithClient(t, client)
			for _, call := range []struct {
				name string
				args map[string]any
			}{
				{"cron_capabilities", map[string]any{}},
				{"cron_job_create", map[string]any{"uid": cronNewUID(t, s), "name": "Denied", "definition": cronDefinition()}},
			} {
				result := cronCall(t, s, call.name, call.args)
				require.True(t, result.IsError)
				require.Len(t, result.Content, 1)
				text, ok := result.Content[0].(*sdkmcp.TextContent)
				require.True(t, ok)
				require.Contains(t, text.Text, tc.code, call.name)
				require.Contains(t, text.Text, tc.message, call.name)
			}
		})
	}
}
func TestCronInvalidDefinitionsAndUIDs(t *testing.T) {
	f := newCronFixture(t)
	for _, args := range []map[string]any{
		{"uid": "invalid", "name": "Invalid", "definition": cronDefinition()},
		{"uid": cronNewUID(t, f.session), "name": "Invalid", "definition": map[string]any{"version": 1}},
	} {
		require.True(t, cronCall(t, f.session, "cron_job_create", args).IsError)
	}
	definition := cronDefinition()
	definition["secret_refs"] = map[string]any{"TOKEN": "env:TOKEN"}
	require.True(t, cronCall(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Invalid secret", "definition": definition}).IsError)
	require.Empty(t, cronOK(t, f.session, "cron_jobs", map[string]any{})["jobs"])
}

func TestCronReadonlyAndIssueScopedCredentials(t *testing.T) {
	f := newCronFixture(t)
	readonly := daemon.NewServer(daemon.ServerConfig{DB: f.store, InsecureReadonly: true})
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	httpServer := httptest.NewServer(readonly.Handler())
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	s := connectTestServerWithOptions(t, Options{Client: client, ProjectID: f.project.ID, ProjectName: f.project.Name, Actor: "example-agent", Version: "test"})
	cronOK(t, s, "cron_capabilities", map[string]any{})
	require.True(t, cronCall(t, s, "cron_job_create", map[string]any{"uid": cronNewUID(t, s), "name": "Denied", "definition": cronDefinition()}).IsError)
	parent, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Scoped root", Author: "example-agent"})
	require.NoError(t, err)
	expires := time.Now().UTC().Add(time.Hour)
	scopedToken := strings.Repeat("t", 32)
	_, _, err = f.store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: scopedToken, Actor: "example-agent", AdminActor: db.BootstrapActor, ExpiresAt: &expires, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.project.UID, RootIssueUID: parent.UID}})
	require.NoError(t, err)
	authenticated := daemon.NewServer(daemon.ServerConfig{DB: f.store, Auth: config.AuthConfig{Token: "fixture-bootstrap-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, authenticated.Close()) })
	tokenServer := httptest.NewServer(authenticated.Handler())
	t.Cleanup(tokenServer.Close)
	scopedClient, err := kataclient.NewWithHTTPClient(tokenServer.URL, tokenServer.Client(), kataclient.WithRequestEditor(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer "+scopedToken)
		return nil
	}))
	require.NoError(t, err)
	scoped := connectTestServerWithOptions(t, Options{Client: scopedClient, ProjectID: f.project.ID, ProjectName: f.project.Name, Actor: "example-agent", Version: "test"})
	for _, tc := range []struct {
		name      string
		arguments map[string]any
	}{
		{"cron_capabilities", map[string]any{}},
		{"cron_job_create", map[string]any{"uid": cronNewUID(t, scoped), "name": "Denied", "definition": cronDefinition()}},
	} {
		denied := cronCall(t, scoped, tc.name, tc.arguments)
		require.True(t, denied.IsError)
		require.Contains(t, string(mustJSON(t, denied)), "scoped_operation_forbidden")
	}
	require.Empty(t, cronOK(t, f.session, "cron_jobs", map[string]any{})["jobs"])
}
