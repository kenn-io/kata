package dbtest

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Plane state-group edits leave work-item timestamps unchanged. Derived status
// may refresh only while the source still owns the stored scalar version.
func checkImportDerivedStatus(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	p, err := store.CreateProject(ctx, "example-derived-status")
	require.NoError(t, err)
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	batch := db.ImportBatchParams{ProjectID: p.ID, Source: "plane:example", Actor: "plane-sync", Items: []db.ImportItem{{ExternalID: "work-item:example", Title: "Source task", Body: "Source body", Author: "plane-unknown", Owner: new("plane:example-person"), Priority: new(int64(2)), Status: "open", CreatedAt: at.Add(-time.Hour), UpdatedAt: at}}}
	_, _, err = store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(ctx, p.ID, batch.Source, "issue", batch.Items[0].ExternalID)
	require.NoError(t, err)
	initial, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	// Existing import callers cannot gain equal-version status authority.
	batch.Items[0].Status, batch.Items[0].ClosedReason, batch.Items[0].ClosedAt = "closed", new("done"), &at
	r, events, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, r.Unchanged)
	require.Empty(t, events)
	batch.ReconcileStatusForUnchanged = true
	// Unrelated same-version fields must never hitchhike on derived status.
	batch.Items[0].Body = "Untrusted same-version body"
	batch.Items[0].Owner = nil
	batch.Items[0].Priority = nil
	previous := initial
	for _, reason := range []string{"done", "wontfix", "", "done"} {
		item := &batch.Items[0]
		item.Status, item.ClosedReason, item.ClosedAt = "open", nil, nil
		if reason != "" {
			item.Status, item.ClosedReason, item.ClosedAt = "closed", new(reason), &at
		}
		r, events, err = store.ImportBatch(ctx, batch)
		require.NoError(t, err)
		require.Equal(t, 1, r.Updated)
		require.Len(t, events, 1)
		require.Equal(t, "issue.updated", events[0].Type)
		require.Equal(t, "plane-sync", events[0].Actor)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
		if item.Status != previous.Status {
			require.Equal(t, item.Status, payload["status"])
		} else {
			require.NotContains(t, payload, "status")
		}
		require.NotContains(t, payload, "body")
		require.NotContains(t, payload, "owner")
		require.NotContains(t, payload, "priority")
		if reason == "" {
			require.Nil(t, payload["closed_reason"])
			require.Nil(t, payload["closed_at"])
		} else {
			require.Equal(t, reason, payload["closed_reason"])
		}
		got, err := store.IssueByID(ctx, initial.ID)
		require.NoError(t, err)
		require.Equal(t, item.Status, got.Status)
		require.Equal(t, item.ClosedReason, got.ClosedReason)
		require.Equal(t, initial.Title, got.Title)
		require.Equal(t, initial.Body, got.Body)
		require.Equal(t, initial.Owner, got.Owner)
		require.Equal(t, initial.Priority, got.Priority)
		require.True(t, got.UpdatedAt.Equal(at))
		if got.Status != previous.Status {
			require.Greater(t, got.Revision, previous.Revision)
		}
		previous = got
		r, events, err = store.ImportBatch(ctx, batch)
		require.NoError(t, err)
		require.Equal(t, 1, r.Unchanged)
		require.Empty(t, events)
	}
	// An older source cannot reopen an issue or move the source observation back.
	batch.Items[0].Status, batch.Items[0].ClosedReason, batch.Items[0].ClosedAt = "open", nil, nil
	batch.Items[0].UpdatedAt = at.Add(-time.Minute)
	_, _, err = store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	got, err := store.IssueByID(ctx, initial.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", got.Status)
	batch.Items[0].UpdatedAt = at
	_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{IssueID: initial.ID, Body: new("Newer local work"), Actor: "editor"})
	require.NoError(t, err)
	local, err := store.IssueByID(ctx, initial.ID)
	require.NoError(t, err)
	_, events, err = store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Empty(t, events)
	got, err = store.IssueByID(ctx, initial.ID)
	require.NoError(t, err)
	require.Equal(t, local, got)
	return nil
}
