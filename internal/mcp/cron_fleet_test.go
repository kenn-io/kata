package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"hegel.dev/go/hegel"
)

func TestCronFleetMCPScopesAndFilters(t *testing.T) {
	f := newCronFixture(t)
	other, err := f.store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	defRaw, err := json.Marshal(cronDefinition())
	require.NoError(t, err)
	def, err := cron.ParseJob(defRaw)
	require.NoError(t, err)
	for _, p := range []db.Project{f.project, other} {
		job, _, err := f.store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: def})
		require.NoError(t, err)
		for _, status := range []string{"failed", "succeeded"} {
			id, err := uid.New()
			require.NoError(t, err)
			label, handle := "executor-a", "runner"
			_, err = f.store.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: status, ExecutorLabel: &label, Teammate: &handle, Summary: cron.Summary{Version: 1}})
			require.NoError(t, err)
		}
	}
	allow, err := NewAllowlistScope([]ProjectIdentity{{ID: f.project.ID, UID: f.project.UID, Name: f.project.Name}})
	require.NoError(t, err)
	bound, err := NewBoundScope(ProjectIdentity{ID: f.project.ID, UID: f.project.UID, Name: f.project.Name})
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		scope *Scope
		want  int
	}{{"bound", bound, 1}, {"allowlist", allow, 1}, {"all", NewAllScope(), 2}} {
		t.Run(tc.name, func(t *testing.T) {
			session := connectTestServerWithOptions(t, Options{Client: f.client, Scope: tc.scope, Actor: "operator", Teammate: "inherited-runner", Version: "test"})
			jobs := cronOK(t, session, "cron_jobs", map[string]any{})["jobs"].([]any)
			require.Len(t, jobs, tc.want)
			runs := cronOK(t, session, "cron_runs", map[string]any{})["runs"].([]any)
			require.Len(t, runs, 2*tc.want)
			filtered := cronOK(t, session, "cron_runs", map[string]any{"status": []string{"failed"}, "executor_label": "executor-a", "teammate": "runner", "since": "2000-01-01T00:00:00Z"})
			require.Len(t, filtered["runs"].([]any), tc.want)
			require.Equal(t, []any{"failed"}, filtered["filters"].(map[string]any)["status"])
		})
	}
}

func TestCronFleetMCPUnavailableAllowlistNeverBroadens(t *testing.T) {
	f := newCronFixture(t)
	allow, err := NewAllowlistScope([]ProjectIdentity{{ID: f.project.ID, UID: f.project.UID, Name: f.project.Name}})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: f.client, Scope: allow, Actor: "operator", Version: "test"})
	_, _, err = f.store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: f.project.ID, Actor: "operator", Force: true})
	require.NoError(t, err)
	for _, name := range []string{"cron_jobs", "cron_runs"} {
		result := cronCall(t, session, name, map[string]any{})
		require.True(t, result.IsError, "an unavailable fixed scope must fail closed")
	}
}

func TestCronFleetMCPLegacyUIDWhitespace(t *testing.T) {
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	query, err := cronRunQuery(CronRunsInput{JobUID: "  01arz3ndektsv4rrffq69g5fav  ", BeforeUID: " " + id + " "})
	require.NoError(t, err)
	require.Equal(t, id, query.JobUID)
	require.Equal(t, id, query.BeforeUID)
}

func TestCronFleetMCPFilterEchoSkew(t *testing.T) {
	f := newCronFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cron/runs") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"runs":[]}`))
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: f.project.ID, ProjectName: f.project.Name, Actor: "operator", Version: "test"})
	cronOK(t, session, "cron_runs", map[string]any{})
	result := cronCall(t, session, "cron_runs", map[string]any{"status": []string{"failed"}})
	require.True(t, result.IsError)
	require.Contains(t, result.Content[0].(*sdkmcp.TextContent).Text, "daemon does not support cron run filters")
}

func TestCronFleetMCPStatusProperty(t *testing.T) {
	f := newCronFixture(t)
	job := cronTestJob(t, f)
	jobUID, eventUID := job["uid"].(string), job["definition_event_uid"].(string)
	var records []db.CronRun
	for _, status := range []string{"failed", "succeeded"} {
		id, err := uid.New()
		require.NoError(t, err)
		result, observeErr := f.store.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: f.project.ID, UID: id, JobUID: &jobUID, DefinitionEventUID: &eventUID, Actor: "operator", Status: status, Summary: cron.Summary{Version: 1}})
		require.NoError(t, observeErr)
		records = append(records, result.Run)
	}
	slices.SortFunc(records, func(a, b db.CronRun) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.UID, a.UID)
	})
	session := connectTestServerWithOptions(t, Options{Client: f.client, Scope: NewAllScope(), Actor: "operator", Version: "test"})
	hegel.Test(t, func(ht *hegel.T) {
		statuses := []string{hegel.Draw(ht, hegel.SampledFrom([]string{"running", "succeeded", "failed", "cancelled", "unknown"})), hegel.Draw(ht, hegel.SampledFrom([]string{"running", "succeeded", "failed", "cancelled", "unknown"}))}
		limit := hegel.Draw(ht, hegel.Integers(1, 100))
		want := []string{}
		for _, record := range records {
			if slices.Contains(statuses, record.Status) {
				want = append(want, record.UID)
			}
		}
		if len(want) > limit {
			want = want[:limit]
		}

		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.cron_runs", Arguments: map[string]any{"status": statuses, "limit": limit}})
		require.NoError(ht, err)
		require.False(ht, result.IsError)
		got := []string{}
		for _, raw := range result.StructuredContent.(map[string]any)["runs"].([]any) {
			got = append(got, raw.(map[string]any)["uid"].(string))
		}
		require.Equal(ht, want, got)
	}, hegel.WithTestCases(100), hegel.WithDatabase(t.TempDir()))
}

func TestCronFleetRejectsExplicitBlankMCPProject(t *testing.T) {
	f := newCronFixture(t)
	session := connectTestServerWithOptions(t, Options{Client: f.client, Scope: NewAllScope(), Actor: "operator", Version: "test"})
	for _, name := range []string{"cron_jobs", "cron_runs"} {
		for _, value := range []any{"", " ", nil} {
			result := cronCall(t, session, name, map[string]any{"project": value})
			require.True(t, result.IsError, "explicit blank project must not select the fleet")
		}
	}
}

func TestCronFleetRejectsExplicitEmptyMCPFilters(t *testing.T) {
	f := newCronFixture(t)
	for _, field := range []string{"issue_uid", "executor_label", "teammate", "since", "status"} {
		values := []any{"", nil}
		if field == "status" {
			values = []any{[]string{}, nil}
		}
		for _, value := range values {
			result := cronCall(t, f.session, "cron_runs", map[string]any{field: value})
			require.True(t, result.IsError, "explicit empty %s must not remove the predicate", field)
		}
	}
}
