package dbtest

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// checkImportStatusCloseClearsAssignmentExpiry models the status-only update
// produced when TickTick changes from two-way to one-way status sync. Closing
// keeps the local owner permanently and removes the deadline from both the row
// and the replayable issue.updated event.
func checkImportStatusCloseClearsAssignmentExpiry(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "import-status-expiry")
	require.NoError(t, err)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	externalID := "task:example"
	batch := db.ImportBatchParams{
		ProjectID: project.ID, Source: "ticktick:example", Actor: "ticktick-sync",
		Items: []db.ImportItem{{
			ExternalID: externalID, Title: "Source task", Body: "Source body", Author: "ticktick-unknown",
			Status: "open", CreatedAt: base.Add(-time.Hour), UpdatedAt: base,
		}},
	}
	created, _, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, created.Created)
	mapping, err := store.ImportMappingBySource(ctx, project.ID, batch.Source, "issue", externalID)
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)
	claim, err := store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: *mapping.IssueID, Actor: "worker", TTL: time.Minute, Now: base})
	require.NoError(t, err)
	require.NotNil(t, claim.Issue.AssignmentExpiresOn)
	deadline := *claim.Issue.AssignmentExpiresOn

	// The source reports completion without a new content version.
	rawStatus := "2"
	batch.ImportStatusObservations = map[string]db.IssueStatusObservation{externalID: {Raw: &rawStatus, Version: base.Add(time.Minute)}}
	batch.Items[0].Status, batch.Items[0].ClosedReason = "closed", new("done")
	batch.Items[0].ClosedAt = new(base)
	result, events, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.Len(t, events, 1)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	require.Equal(t, "closed", payload["status"])
	value, present := payload["assignment_expires_on"]
	require.True(t, present, "the status event must carry the assignment deadline clear")
	require.Nil(t, value)

	closed, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	require.Equal(t, new("worker"), closed.Owner)
	require.Nil(t, closed.AssignmentExpiresOn)
	folded, err := foldProjectIssues(ctx, store, project.ID)
	require.NoError(t, err)
	replayed := folded.Issues[closed.UID]
	require.Equal(t, closed.Status, replayed.Status)
	require.Equal(t, closed.Owner, replayed.Owner)
	require.Nil(t, replayed.AssignmentExpiresOn, "the event log must replay the cleared deadline")

	sweep, err := store.ExpireAssignments(ctx, db.ExpireAssignmentsParams{
		ProjectID: project.ID, Now: deadline.Add(time.Minute),
	})
	require.NoError(t, err)
	require.Empty(t, sweep, "a later sweep must not expire the retained owner")
	afterSweep, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, new("worker"), afterSweep.Owner)
	require.Nil(t, afterSweep.AssignmentExpiresOn)
	return nil
}

// checkImportStatusObservationAcknowledgement verifies a committed workflow
// observation cannot be replayed over a later local reopen.
func checkImportStatusObservationAcknowledgement(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "import-status-ack")
	require.NoError(t, err)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	externalID := "task:example"
	batch := db.ImportBatchParams{
		ProjectID: project.ID, Source: "ticktick:example", Actor: "ticktick-sync",
		Items: []db.ImportItem{{
			ExternalID: externalID, Title: "Source task", Body: "Source body", Author: "ticktick-unknown",
			Status: "open", CreatedAt: base.Add(-time.Hour), UpdatedAt: base,
		}},
	}
	created, _, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, created.Created)
	mapping, err := store.ImportMappingBySource(ctx, project.ID, batch.Source, "issue", externalID)
	require.NoError(t, err)
	require.NotNil(t, mapping.IssueID)

	observedAt := base.Add(10 * time.Minute)
	rawStatus := "2"
	batch.ImportStatusObservations = map[string]db.IssueStatusObservation{
		externalID: {Raw: &rawStatus, Version: observedAt},
	}
	batch.Items[0].Status = "closed"
	batch.Items[0].ClosedReason = new("done")
	batch.Items[0].ClosedAt = &observedAt
	closed, events, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, closed.Updated)
	require.Len(t, events, 1)

	_, _, changed, err := store.ReopenIssue(ctx, *mapping.IssueID, "worker")
	require.NoError(t, err)
	require.True(t, changed)
	opened, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "open", opened.Status)

	// Retrying the same checkpoint after a failed binding finalizer must not
	// re-close the issue after the owner has reopened it locally.
	batch.ImportStatusObservations[externalID] = db.IssueStatusObservation{Raw: &rawStatus, Version: observedAt.Add(time.Hour)}
	retried, events, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, retried.Unchanged)
	require.Empty(t, events)
	stillOpen, err := store.IssueByID(ctx, *mapping.IssueID)
	require.NoError(t, err)
	require.Equal(t, "open", stillOpen.Status)
	return nil
}

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
	rawStatus := "0"
	batch.ImportStatusObservations = map[string]db.IssueStatusObservation{batch.Items[0].ExternalID: {Raw: &rawStatus, Version: at.Add(time.Hour)}}
	batch.Items[0].Status, batch.Items[0].ClosedReason, batch.Items[0].ClosedAt = "open", nil, nil
	r, events, err = store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, r.Updated)
	require.Len(t, events, 1)
	got, err = store.IssueByID(ctx, initial.ID)
	require.NoError(t, err)
	require.Equal(t, "open", got.Status)
	require.Equal(t, local.Title, got.Title)
	require.Equal(t, local.Body, got.Body)
	require.Equal(t, local.UpdatedAt, got.UpdatedAt)
	return nil
}
