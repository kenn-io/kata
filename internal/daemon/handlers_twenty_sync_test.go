package daemon_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/twentysync"
)

const twentyWorkspaceID = "11111111-1111-4111-8111-111111111111"
const twentyTaskID = "22222222-2222-4222-8222-222222222222"

type lifecycleTwentyFetcher struct {
	workspace       twentysync.Workspace
	schema          twentysync.Schema
	tasks           []twentysync.Task
	failure         error
	beforeWorkspace func()
	calls           atomic.Int32
}

func (f *lifecycleTwentyFetcher) ForRun(_ context.Context, _ twentysync.Config) (twentysync.Session, error) {
	f.calls.Add(1)
	return f, f.failure
}
func (f *lifecycleTwentyFetcher) Workspace(ctx context.Context, _ twentysync.Config) (twentysync.Workspace, error) {
	if f.beforeWorkspace != nil {
		f.beforeWorkspace()
	}
	return f.workspace, ctx.Err()
}
func (f *lifecycleTwentyFetcher) Schema(ctx context.Context, _ twentysync.Config) (twentysync.Schema, error) {
	return f.schema, ctx.Err()
}
func (f *lifecycleTwentyFetcher) Tasks(ctx context.Context, _ twentysync.Config) ([]twentysync.Task, error) {
	return f.tasks, ctx.Err()
}
func (f *lifecycleTwentyFetcher) ReadStatus(ctx context.Context, _ twentysync.Config, _ string) (issuesync.StatusObservation, error) {
	return issuesync.StatusObservation{RawStatus: new("TODO"), Status: "open", Version: f.tasks[0].UpdatedAt}, ctx.Err()
}
func (f *lifecycleTwentyFetcher) WriteStatus(_ context.Context, _ twentysync.Config, _ string, _ string, _ func() error) (issuesync.StatusObservation, error) {
	return issuesync.StatusObservation{}, errors.New("unexpected status write")
}
func newLifecycleTwentyFetcher() *lifecycleTwentyFetcher {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return &lifecycleTwentyFetcher{workspace: twentysync.Workspace{ID: twentyWorkspaceID, DisplayName: "Example workspace"}, schema: twentysync.Schema{StatusOptions: []string{"TODO", "IN_PROGRESS", "DONE"}}, tasks: []twentysync.Task{{ID: twentyTaskID, Title: "Example task", Markdown: "Details", Status: new("TODO"), CreatedAt: at, UpdatedAt: at}}}
}

func TestTwentySyncAPILifecycle(t *testing.T) {
	d := openTestDB(t)
	p, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	f := newLifecycleTwentyFetcher()
	var wakes, otherWakes atomic.Int32
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, TwentySyncFetcher: f, TwentySyncWake: func() { wakes.Add(1) }, PlaneSyncWake: func() { otherWakes.Add(1) }})
	enable := func(body map[string]any, want int) issueSyncResponseBody {
		t.Helper()
		resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), body)
		require.Equal(t, want, resp.StatusCode, string(raw))
		var out issueSyncResponseBody
		if want == 200 {
			decodeJSON(t, raw, &out)
		}
		return out
	}
	out := enable(map[string]any{}, 200)
	require.Equal(t, "twenty:https://api.twenty.com/"+twentyWorkspaceID, out.Binding.SourceKey)
	require.Equal(t, twentyWorkspaceID, out.Binding.RemoteID)
	require.Equal(t, 300, out.Binding.IntervalSeconds)
	require.Equal(t, "one-way", out.Binding.Config["status_sync"])
	require.Equal(t, int32(1), wakes.Load())
	require.Zero(t, otherWakes.Load())
	out = enable(map[string]any{"config": map[string]any{"since": "2026-09-01", "title_prefix": false}, "interval": "2m"}, 200)
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	require.Equal(t, "2026-09-01T00:00:00Z", out.Binding.Config["since"])
	out = enable(map[string]any{}, 200)
	require.Equal(t, false, out.Binding.Config["title_prefix"])
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	for _, body := range []map[string]any{
		{"config": map[string]any{"token": "secret"}}, {"config": map[string]any{"api_origin": "https://other.example"}}, {"config": map[string]any{"workspace_id": twentyTaskID}}, {"config": map[string]any{"open_statuses": []any{}}}, {"config": map[string]any{"closed_status": 42}}, {"config": map[string]any{"title_prefix": nil}}, {"config": map[string]any{"since": "bad"}}, {"interval_seconds": 0}, {"interval": "1ms"},
	} {
		before := f.calls.Load()
		enable(body, 400)
		require.Equal(t, before, f.calls.Load())
	}
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"created":1`)
	mapping, err := d.db.ImportMappingBySource(t.Context(), p.ID, out.Binding.SourceKey, "issue", "task:"+twentyTaskID)
	require.NoError(t, err)
	issue, err := d.db.IssueByID(t.Context(), *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "Example task", issue.Title)
	resp, raw = getStatusBody(t, server, issueSyncEndpoint(p.ID, "twenty", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "last_success_at")
	out = enable(map[string]any{"status_sync": "two-way"}, 200)
	require.Equal(t, "two-way", out.Binding.Config["status_sync"])
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "disable"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "once"), map[string]any{})
	require.Equal(t, 400, resp.StatusCode, string(raw))
	f.workspace.ID = twentyTaskID
	enable(map[string]any{}, 400)
}

func TestTwentyEnableSelfHostedOriginsAndErrors(t *testing.T) {
	for _, scenario := range []string{"self-hosted", "credentials", "timeout", "unknown-option"} {
		t.Run(scenario, func(t *testing.T) {
			d := openTestDB(t)
			p, err := d.db.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			f := newLifecycleTwentyFetcher()
			want := 200
			switch scenario {
			case "credentials":
				f.failure = errors.New("example-secret")
				want = 400
			case "timeout":
				f.failure = context.DeadlineExceeded
				want = 504
			case "unknown-option":
				f.schema.StatusOptions = append(f.schema.StatusOptions, "CUSTOM")
				want = 400
			}
			server := startTestServer(t, daemon.ServerConfig{DB: d.db, TwentySyncConfig: config.TwentySyncConfig{APIOrigin: "https://twenty.example", WebOrigin: "https://ui.example"}, TwentySyncFetcher: f})
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{})
			require.Equal(t, want, resp.StatusCode, string(raw))
			require.NotContains(t, string(raw), "example-secret")
			if want == 200 {
				require.Contains(t, string(raw), "twenty:https://twenty.example/")
				require.Contains(t, string(raw), "https://ui.example")
			} else {
				_, err := d.db.IssueSyncBindingByProject(t.Context(), p.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
			}
		})
	}
}

func TestTwentyEnableCustomStatusMappings(t *testing.T) {
	d := openTestDB(t)
	p, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	f := newLifecycleTwentyFetcher()
	f.schema.StatusOptions = []string{"READY", "ACTIVE", "COMPLETE"}
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, TwentySyncFetcher: f})
	body := map[string]any{"status_sync": "two-way", "config": map[string]any{"open_status": "READY", "closed_status": "COMPLETE", "open_statuses": []string{"READY", "ACTIVE"}}}
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), body)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"closed_status":"COMPLETE"`)
	f.schema.StatusOptions = []string{"ACTIVE", "COMPLETE"}
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{"status_sync": "one-way"})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{"status_sync": "two-way"})
	require.Equal(t, 400, resp.StatusCode, string(raw))
}

func TestTwentyEnableRejectsEmptyStatusMappings(t *testing.T) {
	for _, key := range []string{"closed_status", "open_status"} {
		t.Run(key, func(t *testing.T) {
			d := openTestDB(t)
			p, err := d.db.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			f := newLifecycleTwentyFetcher()
			f.schema.StatusOptions = []string{"TODO", "IN_PROGRESS", "DONE", "READY"}
			server := startTestServer(t, daemon.ServerConfig{DB: d.db, TwentySyncFetcher: f})
			body := map[string]any{"status_sync": "two-way", "config": map[string]any{"open_status": "READY", "open_statuses": []string{"TODO", "IN_PROGRESS", "READY"}}}
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), body)
			require.Equal(t, 200, resp.StatusCode, string(raw))

			before, err := d.db.IssueSyncBindingByProject(t.Context(), p.ID)
			require.NoError(t, err)
			fetchCalls := f.calls.Load()

			resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{"config": map[string]any{key: ""}})
			require.Equal(t, 400, resp.StatusCode, string(raw))
			require.Contains(t, string(raw), "nonempty")
			require.Equal(t, fetchCalls, f.calls.Load())

			after, err := d.db.IssueSyncBindingByProject(t.Context(), p.ID)
			require.NoError(t, err)
			require.Equal(t, before.Config, after.Config)
		})
	}
}

func TestTwentyEnablePreservesOnlyOutboundStatusCursor(t *testing.T) {
	d := openTestDB(t)
	p, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	f := newLifecycleTwentyFetcher()
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, TwentySyncFetcher: f})
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	before, err := d.db.IssueSyncBindingByProject(t.Context(), p.ID)
	require.NoError(t, err)
	require.NotNil(t, before.LastCursorAt)
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{"status_sync": "two-way", "config": map[string]any{"open_status": "IN_PROGRESS"}})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	after, err := d.db.IssueSyncBindingByProject(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, before.LastCursorAt, after.LastCursorAt)
	f.schema.StatusOptions = []string{"TODO", "IN_PROGRESS", "COMPLETE"}
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{"config": map[string]any{"closed_status": "COMPLETE"}})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	after, err = d.db.IssueSyncBindingByProject(t.Context(), p.ID)
	require.NoError(t, err)
	require.Nil(t, after.LastCursorAt)
}

func TestTwentyEnableConcurrentBindingChangeIsFenced(t *testing.T) {
	d := openTestDB(t)
	p, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	f := newLifecycleTwentyFetcher()
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, TwentySyncFetcher: f})
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var calls atomic.Int32
	f.beforeWorkspace = func() {
		if calls.Add(1) != 1 {
			return
		}
		b, err := d.db.IssueSyncBindingByProject(t.Context(), p.ID)
		require.NoError(t, err)
		_, err = d.db.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: b.Provider, SourceKey: b.SourceKey, RemoteID: b.RemoteID, DisplayName: b.DisplayName, Config: b.Config, IntervalSeconds: 17})
		require.NoError(t, err)
	}
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "twenty", "enable"), map[string]any{"interval": "2m"})
	require.Equal(t, 409, resp.StatusCode, string(raw))
	b, err := d.db.IssueSyncBindingByProject(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, 17, b.IntervalSeconds)
}
