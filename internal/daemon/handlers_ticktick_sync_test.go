package daemon_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/tickticksync"
)

type lifecycleTickTick struct {
	calls int
	data  tickticksync.ProjectData
}

func (f *lifecycleTickTick) ForRun(context.Context, tickticksync.Config) (tickticksync.Session, error) {
	f.calls++
	return f, nil
}
func (f *lifecycleTickTick) Project(context.Context) (tickticksync.Project, error) {
	return f.data.Project, nil
}
func (f *lifecycleTickTick) Data(context.Context) (tickticksync.ProjectData, error) {
	return f.data, nil
}
func (f *lifecycleTickTick) Task(context.Context, string) (tickticksync.Task, error) {
	return f.data.Tasks[0], nil
}
func TestTickTickSyncOperatorLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := &lifecycleTickTick{data: tickticksync.ProjectData{Project: tickticksync.Project{ID: "project-1", Name: "Example tasks", Kind: "TASK"}, Tasks: []tickticksync.Task{{ID: "task-1", ProjectID: "project-1", Title: "Task", Status: new(0)}}}}
	wakes := 0
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, TickTickSyncFetcher: f, TickTickSyncWake: func() { wakes++ }})
	enable := func(body map[string]any, want int) issueSyncResponseBody {
		t.Helper()
		res, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "ticktick", "enable"), body)
		require.Equal(t, want, res.StatusCode, string(raw))
		var out issueSyncResponseBody
		if want == 200 {
			decodeJSON(t, raw, &out)
		}
		return out
	}
	out := enable(map[string]any{"config": map[string]any{"project_id": "project-1", "title_prefix": false}, "interval": "2m"}, 200)
	require.Equal(t, "ticktick:project-1", out.Binding.SourceKey)
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	require.Equal(t, 1, wakes)
	for _, bad := range []map[string]any{{"project_id": "other-project"}, {"project_id": "../project"}, {"token": "secret"}, {"api_origin": "https://foreign.example"}, {"_provider_checkpoint": map[string]any{}}, {"title_prefix": "false"}} {
		calls := f.calls
		enable(map[string]any{"config": bad}, 400)
		require.Equal(t, calls, f.calls)
	}
	res, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "ticktick", "once"), map[string]any{})
	require.Equal(t, 200, res.StatusCode, string(raw))
	require.NotContains(t, string(raw), "_provider_checkpoint")
	saved, err := d.db.IssueSyncBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	cp, err := tickticksync.DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.Len(t, cp.Versions, 1)
	res, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "ticktick", "disable"), map[string]any{})
	require.Equal(t, 200, res.StatusCode, string(raw))
	out = enable(map[string]any{}, 200)
	require.Equal(t, false, out.Binding.Config["title_prefix"])
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	saved, err = d.db.IssueSyncBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	again, err := tickticksync.DecodeCheckpoint(saved.Config)
	require.NoError(t, err)
	require.Equal(t, cp, again)
	res, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "ticktick", "once"), map[string]any{})
	require.Equal(t, 200, res.StatusCode, string(raw))
	require.Contains(t, string(raw), `"unchanged":1`)
	f.data.Project.Closed = true
	enable(map[string]any{}, 400)
}
