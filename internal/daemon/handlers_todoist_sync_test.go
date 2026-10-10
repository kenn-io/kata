package daemon_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/todoistsync"
)

type lifecycleTodoist struct{ calls atomic.Int32 }

func (f *lifecycleTodoist) Account(context.Context) (string, error) {
	f.calls.Add(1)
	return "1234567", nil
}
func (f *lifecycleTodoist) ForRun(context.Context, todoistsync.Config) (todoistsync.Session, error) {
	f.calls.Add(1)
	return f, nil
}
func (f *lifecycleTodoist) Project(_ context.Context, c todoistsync.Config) (todoistsync.Project, error) {
	return todoistsync.Project{ID: c.ProjectID, Name: "Example tasks"}, nil
}
func (f *lifecycleTodoist) Tasks(_ context.Context, c todoistsync.Config, _, _ time.Time) ([]todoistsync.Task, error) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return []todoistsync.Task{{ID: "task123", ProjectID: c.ProjectID, Content: "Example task", AddedAt: at, UpdatedAt: at, Priority: 1}}, nil
}
func (f *lifecycleTodoist) ReadStatus(context.Context, todoistsync.Config, todoistsync.StatusTarget) (issuesync.StatusObservation, error) {
	return issuesync.StatusObservation{Status: "open", RawStatus: new("open"), Version: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}, nil
}
func (f *lifecycleTodoist) WriteStatus(context.Context, todoistsync.Config, todoistsync.StatusTarget, string, func() error) (issuesync.StatusObservation, error) {
	return issuesync.StatusObservation{}, &issuesync.StatusError{Message: "fixture read-only", Blocked: true}
}
func TestTodoistSyncLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := &lifecycleTodoist{}
	var wakes atomic.Int32
	s := startTestServer(t, daemon.ServerConfig{DB: d.db, TodoistSyncFetcher: f, TodoistSyncWake: func() { wakes.Add(1) }})
	enable := func(body map[string]any, want int) issueSyncResponseBody {
		t.Helper()
		resp, raw := postJSON(t, s, issueSyncEndpoint(p.ID, "todoist", "enable"), body)
		require.Equal(t, want, resp.StatusCode, string(raw))
		var out issueSyncResponseBody
		if want == 200 {
			decodeJSON(t, raw, &out)
		}
		return out
	}
	out := enable(map[string]any{"config": map[string]any{"project_id": "project123", "history_since": "2026-09-01", "title_prefix": false}, "interval": "2m", "status_sync": "two-way"}, 200)
	require.Equal(t, "todoist:https://api.todoist.com/1234567/project123", out.Binding.SourceKey)
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	require.Equal(t, "two-way", out.Binding.Config["status_sync"])
	require.Equal(t, int32(1), wakes.Load())
	out = enable(map[string]any{}, 200)
	require.Equal(t, "2026-09-01T00:00:00Z", out.Binding.Config["history_since"])
	require.Equal(t, false, out.Binding.Config["title_prefix"])
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	for _, config := range []map[string]any{{"token": "secret"}, {"token_env": "OTHER"}, {"api_origin": "https://foreign.example"}, {"account_id": "different"}, {"project_id": "different"}, {"history_since": "2026-08-01"}, {"title_prefix": nil}} {
		before := f.calls.Load()
		enable(map[string]any{"config": config}, 400)
		require.Equal(t, before, f.calls.Load())
	}
	resp, raw := postJSON(t, s, issueSyncEndpoint(p.ID, "todoist", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	mapping, err := d.db.ImportMappingBySource(ctx, p.ID, out.Binding.SourceKey, "issue", "task:task123")
	require.NoError(t, err)
	issue, err := d.db.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "Example task", issue.Title)
	resp, raw = postJSON(t, s, issueSyncEndpoint(p.ID, "todoist", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"unchanged":1`)
	resp, raw = getStatusBody(t, s, issueSyncEndpoint(p.ID, "todoist", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "last_success_at")
	resp, raw = postJSON(t, s, issueSyncEndpoint(p.ID, "todoist", "disable"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	binding, err := d.db.IssueSyncBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	require.False(t, binding.Enabled)
	_, err = d.db.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
}
