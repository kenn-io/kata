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
	"go.kenn.io/kata/internal/planesync"
)

const planeProjectID = "11111111-1111-4111-8111-111111111111"
const planeStateID = "22222222-2222-4222-8222-222222222222"
const planeItemID = "33333333-3333-4333-8333-333333333333"

type lifecyclePlaneFetcher struct {
	project       planesync.Project
	states        []planesync.State
	items         []planesync.WorkItem
	failure       error
	beforeProject func()
	calls         atomic.Int32
}

func (f *lifecyclePlaneFetcher) ForRun(_ context.Context, _ planesync.Config) (planesync.Session, error) {
	f.calls.Add(1)
	return f, f.failure
}
func (f *lifecyclePlaneFetcher) Project(ctx context.Context, _ planesync.Config) (planesync.Project, error) {
	if f.beforeProject != nil {
		f.beforeProject()
	}
	return f.project, ctx.Err()
}
func (f *lifecyclePlaneFetcher) States(ctx context.Context, _ planesync.Config) ([]planesync.State, error) {
	return f.states, ctx.Err()
}
func (f *lifecyclePlaneFetcher) WorkItems(ctx context.Context, _ planesync.Config) ([]planesync.WorkItem, error) {
	return f.items, ctx.Err()
}
func newLifecyclePlaneFetcher() *lifecyclePlaneFetcher {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &lifecyclePlaneFetcher{project: planesync.Project{ID: planeProjectID, Name: "Example tasks", Identifier: "EX"}, states: []planesync.State{{ID: planeStateID, Group: "started"}}, items: []planesync.WorkItem{{ID: planeItemID, ProjectID: planeProjectID, StateID: planeStateID, SequenceID: 1, Name: "Example task", CreatedAt: at, UpdatedAt: at}}}
}
func planeEnableBody() map[string]any {
	return map[string]any{"config": map[string]any{"workspace": "example-workspace", "project_id": planeProjectID}}
}
func TestPlaneSyncLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newLifecyclePlaneFetcher()
	var wakes, otherWakes atomic.Int32
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, PlaneSyncFetcher: f, PlaneSyncWake: func() { wakes.Add(1) }, NotionSyncWake: func() { otherWakes.Add(1) }})
	enable := func(body map[string]any, want int) issueSyncResponseBody {
		t.Helper()
		resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), body)
		require.Equal(t, want, resp.StatusCode, string(raw))
		var out issueSyncResponseBody
		if want == 200 {
			decodeJSON(t, raw, &out)
		}
		return out
	}
	out := enable(planeEnableBody(), 200)
	require.Equal(t, "plane:https://api.plane.so/example-workspace/"+planeProjectID, out.Binding.SourceKey)
	require.Equal(t, 300, out.Binding.IntervalSeconds)
	require.Equal(t, true, out.Binding.Config["title_prefix"])
	require.Equal(t, int32(1), wakes.Load())
	require.Zero(t, otherWakes.Load())
	out = enable(map[string]any{"config": map[string]any{"since": "2026-08-01", "title_prefix": false}, "interval": "2m"}, 200)
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	require.Equal(t, "2026-08-01T00:00:00Z", out.Binding.Config["since"])
	out = enable(map[string]any{}, 200)
	require.Equal(t, false, out.Binding.Config["title_prefix"])
	require.Equal(t, 120, out.Binding.IntervalSeconds)
	for _, c := range []map[string]any{{"token": "secret"}, {"token_env": "OTHER"}, {"api_origin": "https://other.example"}, {"web_origin": "https://other.example"}, {"workspace": "different-workspace"}, {"project_id": planeItemID}, {"since": 3}, {"since": "0000-01-01"}, {"title_prefix": nil}, {"title_prefix": "false"}} {
		before := f.calls.Load()
		enable(map[string]any{"config": c}, 400)
		require.Equal(t, before, f.calls.Load())
	}
	for _, b := range []map[string]any{{"interval_seconds": 0}, {"interval_seconds": -1}, {"interval": "2m", "interval_seconds": 120}, {"interval": "1ms"}} {
		before := f.calls.Load()
		enable(b, 400)
		require.Equal(t, before, f.calls.Load())
	}
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "\"created\":1")
	mapping, err := d.db.ImportMappingBySource(ctx, p.ID, out.Binding.SourceKey, "issue", "work-item:"+planeItemID)
	require.NoError(t, err)
	issue, err := d.db.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "Example task", issue.Title)
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "\"unchanged\":1")
	resp, raw = getStatusBody(t, server, issueSyncEndpoint(p.ID, "plane", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "last_success_at")
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "disable"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "once"), map[string]any{})
	require.Equal(t, 400, resp.StatusCode, string(raw))
}
func TestPlaneEnableRejectsBeforeCredentials(t *testing.T) {
	for _, kind := range []string{"federation", "foreign provider", "bad origin", "unknown config"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()
			p, err := d.db.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			f := newLifecyclePlaneFetcher()
			f.failure = errors.New("example-secret")
			cfg := daemon.ServerConfig{DB: d.db, PlaneSyncFetcher: f}
			body := planeEnableBody()
			want := 400
			switch kind {
			case "federation":
				_, err = d.db.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373", HubProjectID: 42, HubProjectUID: p.UID, ReplayHorizonEventID: 1, Enabled: true})
				require.NoError(t, err)
				want = 409
			case "foreign provider":
				_, err = d.db.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: p.ID, Provider: "notion", SourceKey: "notion:" + notionSourceID, RemoteID: notionSourceID, DisplayName: "Example tasks", Config: []byte("{}"), IntervalSeconds: 300})
				require.NoError(t, err)
			case "bad origin":
				cfg.PlaneSyncConfig = config.PlaneSyncConfig{APIOrigin: "http://daemon.example"}
			case "unknown config":
				body["config"].(map[string]any)["api_origin"] = "https://other.example"
			}
			server := startTestServer(t, cfg)
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), body)
			require.Equal(t, want, resp.StatusCode, string(raw))
			require.Zero(t, f.calls.Load())
			require.NotContains(t, string(raw), "example-secret")
			if kind == "federation" {
				require.Contains(t, string(raw), "enable Plane sync on the hub project")
			}
		})
	}
}
func TestPlaneEnableRemoteFailureAndIdentity(t *testing.T) {
	for _, kind := range []string{"credentials", "timeout", "project", "states"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			p, err := d.db.CreateProject(context.Background(), "example-project")
			require.NoError(t, err)
			f := newLifecyclePlaneFetcher()
			want := 400
			switch kind {
			case "credentials":
				f.failure = errors.New("example-secret")
			case "timeout":
				f.failure = context.DeadlineExceeded
				want = 504
			case "project":
				f.project.ID = planeItemID
			case "states":
				f.states[0].Group = "unknown"
			}
			server := startTestServer(t, daemon.ServerConfig{DB: d.db, PlaneSyncFetcher: f})
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), planeEnableBody())
			require.Equal(t, want, resp.StatusCode, string(raw))
			require.NotContains(t, string(raw), "example-secret")
			_, err = d.db.IssueSyncBindingByProject(context.Background(), p.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}
func TestPlaneEnableConcurrentChangeAndOnceProgress(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newLifecyclePlaneFetcher()
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, PlaneSyncFetcher: f})
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), planeEnableBody())
	require.Equal(t, 200, resp.StatusCode, string(raw))
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.beforeProject = func() {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	finished := make(chan int, 1)
	go func() {
		resp, _ := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), map[string]any{"config": map[string]any{"since": "2026-01-01"}})
		finished <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("validation not reached")
	}
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), map[string]any{"interval": "2m"})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	close(release)
	require.Equal(t, 409, <-finished)
	entered, release = make(chan struct{}), make(chan struct{})
	f.beforeProject = func() { close(entered); <-release }
	go func() {
		resp, _ := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "once"), map[string]any{})
		finished <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run not reached")
	}
	resp, raw = getStatusBody(t, server, issueSyncEndpoint(p.ID, "plane", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var out issueSyncResponseBody
	decodeJSON(t, raw, &out)
	require.Equal(t, "running", out.Status.State)
	require.NotNil(t, out.Status.Progress)
	require.Equal(t, "project", out.Status.Progress.Phase)
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "once"), map[string]any{})
	require.Equal(t, 409, resp.StatusCode, string(raw))
	close(release)
	require.Equal(t, 200, <-finished)
}
