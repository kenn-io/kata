package planesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestPlaneRunnerStatusSurvivesContentFailureAndAcknowledgesExactEvent(t *testing.T) {
	ctx := context.Background()
	store := adapterStore(t)
	state := testStateID
	updated := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	badContent := false
	patches := 0
	var issue db.Issue
	rapidReopen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/api/v1/workspaces/example-workspace/projects/" + testProjectID + "/"
		switch r.URL.Path {
		case prefix:
			sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
		case prefix + "states/":
			sendPlaneJSON(t, w, wirePage(workflowRows(), false, ""))
		case prefix + "work-items/":
			if badContent {
				w.WriteHeader(403)
				return
			}
			row := wireItem()
			row["state"] = state
			row["updated_at"] = updated.Format(time.RFC3339)
			sendPlaneJSON(t, w, wirePage([]map[string]any{row}, false, ""))
		case prefix + "work-items/" + testItemID + "/":
			if r.Method == http.MethodPatch {
				patches++
				if state == testStateID {
					state = completedStateID
				} else {
					state = unstartedStateID
				}
				updated = updated.Add(time.Minute)
				if rapidReopen {
					_, _, _, err := store.ReopenIssue(ctx, issue.ID, "worker")
					require.NoError(t, err)
					rapidReopen = false
				}
			}
			sendPlaneJSON(t, w, map[string]any{"id": testItemID, "project": testProjectID, "state": state, "updated_at": updated.Format(time.RFC3339), "completed_at": nil})
		default:
			t.Errorf("unexpected Plane path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := statusConfig(t, server.URL)
	binding := adapterBinding(t, store, c)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: planeTestClient(server.URL)})
	result, err := runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 1, result.Import.Created)
	issue = importedIssue(t, store, binding)
	_, _, _, err = store.CloseIssueWithEvents(ctx, issue.ID, "done", "worker", "Completed mapped task", nil)
	require.NoError(t, err)
	rapidReopen = true
	result, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 1, patches, "a mapped native close must dispatch a Plane state-only write")
	got, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, "open", got.Status, "old acknowledgement cannot override a newer local reopen")
	var pending *string
	require.NoError(t, store.QueryRowContext(ctx, "SELECT pending_event_uid FROM import_mappings WHERE source=$1", binding.SourceKey).Scan(&pending))
	require.NotNil(t, pending, "newer explicit event survives old acknowledgement")
	result, err = runner.RunOnce(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, 2, patches)
	require.Equal(t, unstartedStateID, state)
	require.NoError(t, store.QueryRowContext(ctx, "SELECT pending_event_uid FROM import_mappings WHERE source=$1", binding.SourceKey).Scan(&pending))
	require.Nil(t, pending)
	state = cancelledStateID
	updated = updated.Add(time.Minute)
	badContent = true
	result, err = runner.RunOnce(ctx, binding.ID)
	require.Error(t, err, "content permissions cannot suppress independent status delivery")
	require.Equal(t, 1, result.StatusUpdated)
	got, err = store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", got.Status)
	require.Equal(t, "wontfix", *got.ClosedReason)
	var locator *string
	require.NoError(t, store.QueryRowContext(ctx, "SELECT remote_locator FROM import_mappings WHERE source=$1", binding.SourceKey).Scan(&locator))
	require.Nil(t, locator, "Plane already keeps its API UUID in external_id")
}
