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
	"go.kenn.io/kata/internal/linearsync"
)

const linearWorkspaceID = "44444444-4444-4444-8444-444444444444"
const linearProjectID = "11111111-1111-4111-8111-111111111111"
const linearStateID = "22222222-2222-4222-8222-222222222222"
const linearItemID = "33333333-3333-4333-8333-333333333333"

type lifecycleLinearFetcher struct {
	scope       linearsync.Scope
	states      []linearsync.State
	items       []linearsync.Issue
	failure     error
	beforeScope func()
	calls       atomic.Int32
}

func (f *lifecycleLinearFetcher) ForRun(_ context.Context, _ linearsync.Config) (linearsync.Session, error) {
	f.calls.Add(1)
	return f, f.failure
}
func (f *lifecycleLinearFetcher) Scope(ctx context.Context, _ linearsync.Config) (linearsync.Scope, error) {
	if f.beforeScope != nil {
		f.beforeScope()
	}
	return f.scope, ctx.Err()
}
func (f *lifecycleLinearFetcher) States(ctx context.Context, _ linearsync.Config) ([]linearsync.State, error) {
	return f.states, ctx.Err()
}
func (f *lifecycleLinearFetcher) Issues(ctx context.Context, _ linearsync.Config) ([]linearsync.Issue, error) {
	return f.items, ctx.Err()
}
func newLifecycleLinearFetcher() *lifecycleLinearFetcher {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return &lifecycleLinearFetcher{scope: linearsync.Scope{WorkspaceID: linearWorkspaceID, TeamID: linearProjectID, Name: "Example tasks"}, states: []linearsync.State{{ID: linearStateID, Type: "started"}}, items: []linearsync.Issue{{ID: linearItemID, TeamID: linearProjectID, StateID: linearStateID, Identifier: "EX-1", Title: "Example task", URL: "https://linear.app/example-workspace/issue/EX-1/example-task", CreatedAt: at, UpdatedAt: at}}}
}
func linearEnableBody() map[string]any {
	return map[string]any{"config": map[string]any{"workspace_id": linearWorkspaceID, "team_id": linearProjectID}}
}
func TestLinearSyncLifecycle(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newLifecycleLinearFetcher()
	var wakes, otherWakes atomic.Int32
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, LinearSyncFetcher: f, LinearSyncWake: func() { wakes.Add(1) }, NotionSyncWake: func() { otherWakes.Add(1) }})
	enable := func(body map[string]any, want int) issueSyncResponseBody {
		t.Helper()
		resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), body)
		require.Equal(t, want, resp.StatusCode, string(raw))
		var out issueSyncResponseBody
		if want == 200 {
			decodeJSON(t, raw, &out)
		}
		return out
	}
	out := enable(linearEnableBody(), 200)
	require.Equal(t, "linear:"+linearWorkspaceID+"/"+linearProjectID, out.Binding.SourceKey)
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
	for _, c := range []map[string]any{{"token": "secret"}, {"token_env": "OTHER"}, {"api_origin": "https://other.example"}, {"web_origin": "https://other.example"}, {"workspace_id": linearItemID}, {"team_id": linearItemID}, {"project_id": linearItemID}, {"since": 3}, {"since": "0000-01-01"}, {"title_prefix": nil}, {"title_prefix": "false"}} {
		before := f.calls.Load()
		enable(map[string]any{"config": c}, 400)
		require.Equal(t, before, f.calls.Load())
	}
	for _, b := range []map[string]any{{"interval_seconds": 0}, {"interval_seconds": -1}, {"interval": "2m", "interval_seconds": 120}, {"interval": "1ms"}} {
		before := f.calls.Load()
		enable(b, 400)
		require.Equal(t, before, f.calls.Load())
	}
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "\"created\":1")
	mapping, err := d.db.ImportMappingBySource(ctx, p.ID, out.Binding.SourceKey, "issue", "issue:"+linearItemID)
	require.NoError(t, err)
	issue, err := d.db.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "Example task", issue.Title)
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "once"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "\"unchanged\":1")
	resp, raw = getStatusBody(t, server, issueSyncEndpoint(p.ID, "linear", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "last_success_at")
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "disable"), map[string]any{})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "once"), map[string]any{})
	require.Equal(t, 400, resp.StatusCode, string(raw))
}
func TestLinearEnableRejectsBeforeCredentials(t *testing.T) {
	for _, kind := range []string{"federation", "foreign provider", "bad origin", "unknown config"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()
			p, err := d.db.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			f := newLifecycleLinearFetcher()
			f.failure = errors.New("example-secret")
			cfg := daemon.ServerConfig{DB: d.db, LinearSyncFetcher: f}
			body := linearEnableBody()
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
				cfg.LinearSyncConfig = config.LinearSyncConfig{AuthType: "unsupported"}
			case "unknown config":
				body["config"].(map[string]any)["api_origin"] = "https://other.example"
			}
			server := startTestServer(t, cfg)
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), body)
			require.Equal(t, want, resp.StatusCode, string(raw))
			require.Zero(t, f.calls.Load())
			require.NotContains(t, string(raw), "example-secret")
			if kind == "federation" {
				require.Contains(t, string(raw), "enable Linear sync on the hub project")
			}
		})
	}
}
func TestLinearEnableRemoteFailureAndIdentity(t *testing.T) {
	for _, kind := range []string{"credentials", "timeout", "project", "states"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			p, err := d.db.CreateProject(context.Background(), "example-project")
			require.NoError(t, err)
			f := newLifecycleLinearFetcher()
			want := 400
			switch kind {
			case "credentials":
				f.failure = errors.New("example-secret")
			case "timeout":
				f.failure = context.DeadlineExceeded
				want = 504
			case "project":
				f.scope.TeamID = linearItemID
			case "states":
				f.states[0].Type = "unknown"
			}
			server := startTestServer(t, daemon.ServerConfig{DB: d.db, LinearSyncFetcher: f})
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), linearEnableBody())
			require.Equal(t, want, resp.StatusCode, string(raw))
			require.NotContains(t, string(raw), "example-secret")
			_, err = d.db.IssueSyncBindingByProject(context.Background(), p.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}
func TestLinearEnableConcurrentChangeAndOnceProgress(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newLifecycleLinearFetcher()
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, LinearSyncFetcher: f})
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), linearEnableBody())
	require.Equal(t, 200, resp.StatusCode, string(raw))
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.beforeScope = func() {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	finished := make(chan int, 1)
	go func() {
		resp, _ := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), map[string]any{"config": map[string]any{"since": "2026-01-01"}})
		finished <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("validation not reached")
	}
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), map[string]any{"interval": "2m"})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	close(release)
	require.Equal(t, 409, <-finished)
	entered, release = make(chan struct{}), make(chan struct{})
	f.beforeScope = func() { close(entered); <-release }
	go func() {
		resp, _ := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "once"), map[string]any{})
		finished <- resp.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("run not reached")
	}
	resp, raw = getStatusBody(t, server, issueSyncEndpoint(p.ID, "linear", "status"))
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var out issueSyncResponseBody
	decodeJSON(t, raw, &out)
	require.Equal(t, "running", out.Status.State)
	require.NotNil(t, out.Status.Progress)
	require.Equal(t, "scope", out.Status.Progress.Phase)
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "once"), map[string]any{})
	require.Equal(t, 409, resp.StatusCode, string(raw))
	close(release)
	require.Equal(t, 200, <-finished)
}

// Re-enable must retain the provider-neutral private cursor while public APIs hide it.
func TestLinearReenablePreservesPrivateStatusCheckpoint(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newLifecycleLinearFetcher()
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, LinearSyncFetcher: f})
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), linearEnableBody())
	require.Equal(t, 200, resp.StatusCode, string(raw))
	b, err := d.db.IssueSyncBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, ok, err := d.db.ClaimIssueSyncBinding(ctx, b.ID, "linear", at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = d.db.UpdateIssueStatusScan(ctx, db.IssueSyncImportGuard{BindingID: b.ID, Provider: "linear", StartedAt: at}, db.IssueStatusScanState{Pending: db.IssueStatusScanCursor{Through: 7}})
	require.NoError(t, err)
	_, err = d.db.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{BindingID: b.ID, StartedAt: at, At: at.Add(time.Second), CursorAt: at})
	require.NoError(t, err)
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), map[string]any{"interval": "2m"})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.NotContains(t, string(raw), "_status_sync")
	b, err = d.db.IssueSyncBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	scan, err := db.DecodeIssueStatusScan(b.Config)
	require.NoError(t, err)
	require.EqualValues(t, 7, scan.Pending.Through)
}

func (f *lifecycleLinearFetcher) ReadStatus(context.Context, linearsync.Config, string) (issuesync.StatusObservation, error) {
	return issuesync.StatusObservation{}, errors.New("unexpected status read")
}
func (f *lifecycleLinearFetcher) WriteStatus(context.Context, linearsync.Config, string, string, func() error) (issuesync.StatusObservation, error) {
	return issuesync.StatusObservation{}, errors.New("unexpected status write")
}

func TestLinearEnableValidatesTargetsAndCanReturnToOneWay(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newLifecycleLinearFetcher()
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, LinearSyncFetcher: f})
	body := linearEnableBody()
	body["status_sync"] = "two-way"
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), body)
	require.Equal(t, 400, resp.StatusCode, string(raw))
	f.states = []linearsync.State{{ID: linearStateID, Type: "unstarted"}, {ID: linearItemID, Type: "completed"}}
	body["config"].(map[string]any)["closed_state_id"] = linearStateID
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), body)
	require.Equal(t, 400, resp.StatusCode, string(raw))
	body["config"].(map[string]any)["closed_state_id"] = linearItemID
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), body)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var out issueSyncResponseBody
	decodeJSON(t, raw, &out)
	require.Equal(t, "two-way", out.Binding.Config["status_sync"])
	f.states = f.states[:1]
	resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "linear", "enable"), map[string]any{"status_sync": "one-way"})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	decodeJSON(t, raw, &out)
	require.Equal(t, "one-way", out.Binding.Config["status_sync"])
}
