package daemon_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/planesync"
)

const planeOpenStateID = "44444444-4444-4444-8444-444444444444"
const planeClosedStateID = "55555555-5555-4555-8555-555555555555"

type statusLifecyclePlaneFetcher struct{ *lifecyclePlaneFetcher }

func (f *statusLifecyclePlaneFetcher) ForRun(_ context.Context, _ planesync.Config) (planesync.Session, error) {
	f.calls.Add(1)
	return f, f.failure
}
func (*statusLifecyclePlaneFetcher) ReadStatus(context.Context, planesync.Config, string) (issuesync.StatusObservation, error) {
	panic("enable must not read individual statuses")
}
func (*statusLifecyclePlaneFetcher) WriteStatus(context.Context, planesync.Config, string, string, func() error) (issuesync.StatusObservation, error) {
	panic("enable must not mutate Plane")
}
func newStatusLifecyclePlaneFetcher() *statusLifecyclePlaneFetcher {
	f := newLifecyclePlaneFetcher()
	f.states = append(f.states, planesync.State{ID: planeOpenStateID, Group: "unstarted"}, planesync.State{ID: planeClosedStateID, Group: "completed"})
	return &statusLifecyclePlaneFetcher{f}
}

func TestPlaneEnableStatusModeAndOverridesPreserve(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.db.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	f := newStatusLifecyclePlaneFetcher()
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, PlaneSyncFetcher: f})
	body := planeEnableBody()
	body["status_sync"] = "two-way"
	body["config"].(map[string]any)["closed_state_id"] = planeClosedStateID
	body["config"].(map[string]any)["open_state_id"] = planeOpenStateID
	for _, request := range []map[string]any{body, {}, {"config": map[string]any{"title_prefix": false}}} {
		resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), request)
		require.Equal(t, 200, resp.StatusCode, string(raw))
		var out issueSyncResponseBody
		decodeJSON(t, raw, &out)
		require.Equal(t, "two-way", out.Binding.Config["status_sync"])
		require.Equal(t, planeClosedStateID, out.Binding.Config["closed_state_id"])
		require.Equal(t, planeOpenStateID, out.Binding.Config["open_state_id"])
		require.Equal(t, "plane:https://api.plane.so/example-workspace/"+planeProjectID, out.Binding.SourceKey)
	}
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), map[string]any{"status_sync": "one-way", "config": map[string]any{"closed_state_id": "", "open_state_id": ""}})
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var out issueSyncResponseBody
	decodeJSON(t, raw, &out)
	require.Equal(t, "one-way", out.Binding.Config["status_sync"])
	require.Empty(t, out.Binding.Config["closed_state_id"])
	require.Empty(t, out.Binding.Config["open_state_id"])
}

func TestPlaneEnableStatusValidationLeavesBindingAbsent(t *testing.T) {
	for _, kind := range []string{"invalid mode", "invalid UUID", "non-string override", "unknown override", "wrong closed group", "wrong open group", "missing completed", "missing unstarted", "read-only session"} {
		t.Run(kind, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()
			p, err := d.db.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			f := newStatusLifecyclePlaneFetcher()
			cfg := daemon.ServerConfig{DB: d.db, PlaneSyncFetcher: f}
			body := planeEnableBody()
			body["status_sync"] = "two-way"
			c := body["config"].(map[string]any)
			beforeRemote := false
			switch kind {
			case "invalid mode":
				body["status_sync"] = "invalid"
				beforeRemote = true
			case "invalid UUID":
				c["closed_state_id"] = "bad"
				beforeRemote = true
			case "non-string override":
				c["open_state_id"] = 42
				beforeRemote = true
			case "unknown override":
				c["closed_state_id"] = planeItemID
			case "wrong closed group":
				c["closed_state_id"] = planeStateID
			case "wrong open group":
				c["open_state_id"] = planeClosedStateID
			case "missing completed":
				f.states = f.states[:2]
			case "missing unstarted":
				f.states = []planesync.State{f.states[0], f.states[2]}
			case "read-only session":
				cfg.PlaneSyncFetcher = f.lifecyclePlaneFetcher
			}
			server := startTestServer(t, cfg)
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), body)
			require.Equal(t, 400, resp.StatusCode, string(raw))
			if beforeRemote {
				require.Zero(t, f.calls.Load())
			}
			_, err = d.db.IssueSyncBindingByProject(ctx, p.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

func TestPlaneEnableOneWayDoesNotRequireStatusTargets(t *testing.T) {
	d := openTestDB(t)
	p, err := d.db.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, PlaneSyncFetcher: newLifecyclePlaneFetcher()})
	resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), planeEnableBody())
	require.Equal(t, 200, resp.StatusCode, string(raw))
	var out issueSyncResponseBody
	decodeJSON(t, raw, &out)
	require.Equal(t, "one-way", out.Binding.Config["status_sync"])
}

func TestPlaneEnableOneWayCancelsPendingWithStaleSavedTargets(t *testing.T) {
	for _, change := range []string{"removed", "moved"} {
		t.Run(change, func(t *testing.T) {
			d := openTestDB(t)
			ctx := context.Background()
			p, err := d.db.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			f := newStatusLifecyclePlaneFetcher()
			server := startTestServer(t, daemon.ServerConfig{DB: d.db, PlaneSyncFetcher: f})
			body := planeEnableBody()
			body["status_sync"] = "two-way"
			body["config"].(map[string]any)["closed_state_id"] = planeClosedStateID
			resp, raw := postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), body)
			require.Equal(t, 200, resp.StatusCode, string(raw))
			binding, err := d.db.IssueSyncBindingByProject(ctx, p.ID)
			require.NoError(t, err)
			issue, _, err := d.db.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Example task", Author: "worker"})
			require.NoError(t, err)
			_, err = d.db.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: p.ID, Source: binding.SourceKey, ExternalID: "work-item:" + planeItemID, ObjectType: "issue", IssueID: &issue.ID})
			require.NoError(t, err)
			_, _, _, err = d.db.CloseIssue(ctx, issue.ID, "done", "worker", "Completed example task", nil)
			require.NoError(t, err)
			pending, err := d.db.CountPendingIssueStatuses(ctx, binding.ID)
			require.NoError(t, err)
			require.Equal(t, 1, pending)
			if change == "removed" {
				f.states = f.states[:2]
			} else {
				f.states[2].Group = "started"
			}
			resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), map[string]any{"status_sync": "two-way"})
			require.Equal(t, 400, resp.StatusCode, string(raw))
			pending, err = d.db.CountPendingIssueStatuses(ctx, binding.ID)
			require.NoError(t, err)
			require.Equal(t, 1, pending)
			resp, raw = postJSON(t, server, issueSyncEndpoint(p.ID, "plane", "enable"), map[string]any{"status_sync": "one-way"})
			require.Equal(t, 200, resp.StatusCode, string(raw))
			var out map[string]any
			decodeJSON(t, raw, &out)
			require.Equal(t, "one-way", out["status"].(map[string]any)["status_sync"])
			require.Equal(t, float64(0), out["status"].(map[string]any)["pending_count"])
			require.Equal(t, planeClosedStateID, out["binding"].(map[string]any)["config"].(map[string]any)["closed_state_id"])
			pending, err = d.db.CountPendingIssueStatuses(ctx, binding.ID)
			require.NoError(t, err)
			require.Zero(t, pending)
		})
	}
}
