package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkIssueStatusFederationIntent(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	fixture, err := createIssueFixture(ctx, store, "example-hub", "Mapped task", "worker", nil)
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, fixture.Project.ID, "worker")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "page-1", ObjectType: "issue", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status=$1, observed_status_at=$2, remote_locator=$3 WHERE id=$4`, "status-option", "2026-09-29T12:00:00Z", "remote-17", mapping.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	_, ok, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", now, now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: now}
	reader := store.(db.IssueStatusReader)
	read := func() db.IssueStatusMapping {
		m, err := reader.IssueStatusMappingByID(ctx, guard, mapping.ID)
		require.NoError(t, err)
		var raw, observedAt, locator *string
		require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT observed_status, CAST(observed_status_at AS TEXT), remote_locator FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&raw, &observedAt, &locator))
		require.NotNil(t, raw)
		require.Equal(t, "status-option", *raw)
		require.NotNil(t, observedAt)
		require.Equal(t, "2026-09-29T12:00:00Z", *observedAt)
		require.NotNil(t, locator)
		require.Equal(t, "remote-17", *locator, "federation only updates the pending event")
		return m
	}
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: fixture.Project.ID, Limit: 100})
	require.NoError(t, err)
	var base int64
	for _, event := range events {
		if event.HLCPhysicalMS > base {
			base = event.HLCPhysicalMS
		}
	}
	spoke, err := uid.New()
	require.NoError(t, err)
	event := func(kind, status string, offset int64) db.RemoteEvent {
		body := jsontext.Value(`{"status":"closed","closed_reason":"done","closed_at":"2026-09-29T12:00:00Z"}`)
		if status == "open" {
			body = jsontext.Value(`{"status":"open","closed_reason":null,"closed_at":null}`)
		}
		if kind == "issue.closed" {
			body = jsontext.Value(`{"reason":"done","closed_at":"2026-09-29T12:00:00Z"}`)
		}
		if kind == "issue.reopened" {
			body = jsontext.Value(`{"reopened_at":"2026-09-29T12:00:00Z"}`)
		}
		return newRemoteEvent(t, fixture.Project, &fixture.Issue.UID, kind, "worker", spoke, base+offset, body)
	}
	var sourceID int64
	ingest := func(batch ...db.RemoteEvent) db.FederationIngestResult {
		p := db.FederationIngestParams{ProjectID: fixture.Project.ID, SpokeInstanceUID: spoke, BoundActor: "worker"}
		for _, e := range batch {
			sourceID++
			p.Events = append(p.Events, db.FederationIngestEvent{SourceEventID: sourceID, Event: e})
		}
		result, err := store.IngestFederationEvents(ctx, p)
		require.NoError(t, err)
		return result
	}
	closed := event("issue.closed", "closed", 10)
	ingest(closed)
	require.Equal(t, closed.EventUID, read().State.PendingEventUID)
	require.Zero(t, ingest(closed).Accepted)
	require.Equal(t, closed.EventUID, read().State.PendingEventUID)
	ingest(event("issue.updated", "closed", 20))
	require.Equal(t, closed.EventUID, read().State.PendingEventUID)
	// Historical opposite intent loses to the previously effective restatement.
	ingest(event("issue.reopened", "open", 15))
	require.Equal(t, closed.EventUID, read().State.PendingEventUID)
	reopened := event("issue.reopened", "open", 30)
	ingest(reopened)
	require.Equal(t, reopened.EventUID, read().State.PendingEventUID)
	ingest(event("issue.updated", "open", 40))
	require.Equal(t, reopened.EventUID, read().State.PendingEventUID)
	ingest(event("issue.updated", "open", 60), event("issue.updated", "closed", 50))
	require.Empty(t, read().State.PendingEventUID)
	ingest(event("issue.closed", "closed", 45))
	require.Empty(t, read().State.PendingEventUID, "late historical close must never manufacture a new delivery")
	latest := event("issue.closed", "closed", 80)
	ingest(latest, event("issue.reopened", "open", 70))
	require.Equal(t, latest.EventUID, read().State.PendingEventUID)
	if err := store.MaterializeFederatedProject(ctx, fixture.Project.ID); err != nil {
		require.NoError(t, err)
	}
	require.Equal(t, latest.EventUID, read().State.PendingEventUID, "manual rebuild cannot manufacture or replace intent")

	issueUID := fixture.Issue.UID
	reopenedWithoutEnvelope := newRemoteEvent(t, fixture.Project, nil, "issue.reopened", "worker", spoke, base+90,
		jsontext.Value(`{"issue_uid":"`+issueUID+`","reopened_at":"2026-09-29T12:00:00Z"}`))
	ingest(reopenedWithoutEnvelope)
	updatedIssue, err := store.IssueByID(ctx, fixture.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, "open", updatedIssue.Status, "payload issue UID must identify the materialized issue")
	require.Equal(t, reopenedWithoutEnvelope.EventUID, read().State.PendingEventUID,
		"an effective status event with only a payload issue UID must enqueue and load intent")
	closedWithoutEnvelope := newRemoteEvent(t, fixture.Project, nil, "issue.closed", "worker", spoke, base+100,
		jsontext.Value(`{"uid":"`+issueUID+`","reason":"done","closed_at":"2026-09-29T12:00:00Z"}`))
	ingest(closedWithoutEnvelope)
	updatedIssue, err = store.IssueByID(ctx, fixture.Issue.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", updatedIssue.Status, "payload uid must identify the materialized issue")
	require.Equal(t, closedWithoutEnvelope.EventUID, read().State.PendingEventUID,
		"an effective status event with only a payload uid must enqueue and load intent")
	return nil
}
