package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusContentIsolation(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	fixture, err := createIssueFixture(ctx, store, "example-project", "Mapped task", "worker", nil)
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "example-page", ObjectType: "issue", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	base := fixture.Issue.CreatedAt.Add(time.Minute).UTC()
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", base, base.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: base}
	statusIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.Project.ID, Title: "Status clock task", Author: "worker"})
	require.NoError(t, err)
	statusMapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "example-status-page", ObjectType: "issue", IssueID: &statusIssue.ID})
	require.NoError(t, err)
	contentAt := statusIssue.CreatedAt.Add(2 * time.Minute).UTC()
	statusAt := contentAt.Add(time.Minute)
	complete := "complete-a"
	changed, _, err := store.(db.IssueStatusWriter).ObserveIssueStatus(ctx, db.IssueStatusObservationParams{
		Guard: guard, MappingID: statusMapping.ID, ExternalID: statusMapping.ExternalID, IssueUID: statusIssue.UID,
		Observation: db.IssueStatusObservation{Raw: &complete, Version: statusAt}, Status: "closed",
	})
	require.NoError(t, err)
	require.True(t, changed)
	clockItem := db.ImportItem{ExternalID: statusMapping.ExternalID, Title: "Fresh upstream title", Body: "Fresh upstream content", Author: "notion-sync", CreatedAt: statusIssue.CreatedAt, UpdatedAt: contentAt, Status: "open"}
	clockResult, _, err := store.ImportBatch(ctx, db.ImportBatchParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, Actor: "notion-sync", IssueSyncGuard: &guard, ManageStatusSeparately: true, Items: []db.ImportItem{clockItem}})
	require.NoError(t, err)
	require.Equal(t, 1, clockResult.Updated, "a newer content timestamp must remain importable even when a later status observation was recorded")
	clockIssue, err := store.IssueByID(ctx, statusIssue.ID)
	require.NoError(t, err)
	require.Equal(t, clockItem.Title, clockIssue.Title)
	require.Equal(t, clockItem.Body, clockIssue.Body)
	require.Equal(t, "closed", clockIssue.Status)
	closed, closeEvent, _, err := store.CloseIssue(ctx, fixture.Issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	item := db.ImportItem{ExternalID: mapping.ExternalID, Title: "Upstream title", Body: "Upstream content", Author: "notion-sync", CreatedAt: fixture.Issue.CreatedAt, UpdatedAt: base.Add(time.Minute), Status: "open"}
	batch := db.ImportBatchParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, Actor: "notion-sync", IssueSyncGuard: &guard, ManageStatusSeparately: true, Items: []db.ImportItem{item}}
	result, events, err := store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.Len(t, events, 1)
	assertContentOnly := func(events []db.Event) {
		for _, event := range events {
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
			require.NotContains(t, payload, "status")
			require.NotContains(t, payload, "closed_at")
			require.NotContains(t, payload, "closed_reason")
		}
	}
	assertContentOnly(events)
	updated, err := store.IssueByID(ctx, fixture.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, item.Title, updated.Title)
	require.Equal(t, item.Body, updated.Body)
	require.Equal(t, closed.Status, updated.Status)
	require.Equal(t, closed.ClosedReason, updated.ClosedReason)
	require.Equal(t, closed.ClosedAt, updated.ClosedAt)
	reader := store.(db.IssueStatusReader)
	current, err := reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, closeEvent.UID, current.State.PendingEventUID)
	_, reopenEvent, _, err := store.ReopenIssue(ctx, fixture.Issue.ID, "worker")
	require.NoError(t, err)
	item.Title = "Another upstream title"
	item.Status = "closed"
	reason := "done"
	item.ClosedReason = &reason
	closedAt := base.Add(2 * time.Minute)
	item.ClosedAt = &closedAt
	item.UpdatedAt = closedAt
	batch.Items = []db.ImportItem{item}
	_, events, err = store.ImportBatch(ctx, batch)
	require.NoError(t, err)
	assertContentOnly(events)
	updated, err = store.IssueByID(ctx, fixture.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, item.Title, updated.Title)
	require.Equal(t, "open", updated.Status)
	require.Nil(t, updated.ClosedReason)
	require.Nil(t, updated.ClosedAt)
	current, err = reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	require.Equal(t, reopenEvent.UID, current.State.PendingEventUID)
	batch.IssueSyncGuard = nil
	_, _, err = store.ImportBatch(ctx, batch)
	require.ErrorIs(t, err, db.ErrImportValidation, "only claimed internal provider imports can opt into status isolation")
	return nil
}
