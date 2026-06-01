package sqlitestore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	katauid "go.kenn.io/kata/internal/uid"
)

func TestFederationSchemaVersionAndTable(t *testing.T) {
	d := openTestDB(t)

	assert.Equal(t, 13, db.CurrentSchemaVersion())
	assertSchemaVersion(t, d, db.CurrentSchemaVersion())
	assertSchemaObject(t, d, "federation_bindings")
	assertSchemaObject(t, d, "idx_federation_bindings_role_enabled")
	assertSchemaObject(t, d, "federation_enrollments")
	assertSchemaObject(t, d, "federation_sync_status")
	assertSchemaObject(t, d, "federation_quarantine")
	assertSchemaObject(t, d, "uniq_federation_quarantine_active")
	assertSchemaObject(t, d, "idx_events_origin_project_id")
	assertFederationBindingColumn(t, d, "push_enabled")
	assertFederationBindingColumn(t, d, "push_cursor_event_id")
	for _, column := range []string{
		"last_pull_started_at", "last_pull_success_at",
		"last_push_started_at", "last_push_success_at",
		"last_error_at", "last_error", "last_reset_at",
	} {
		assertFederationSyncStatusColumn(t, d, column)
	}
	for _, column := range []string{
		"direction", "first_event_id", "last_event_id", "event_uids",
		"error", "skipped_at", "skipped_by", "skip_reason",
	} {
		assertFederationQuarantineColumn(t, d, column)
	}
}

func TestFederationSchemaIssueClaims(t *testing.T) {
	d := openTestDB(t)

	assertSchemaObject(t, d, "issue_claims")
	assertSchemaObject(t, d, "uniq_issue_claims_live_issue")
	assertSchemaObject(t, d, "idx_issue_claims_project_issue")
	assertSchemaObject(t, d, "idx_issue_claims_timed_expiry")
	assertSchemaObject(t, d, "pending_claim_requests")
	assertSchemaObject(t, d, "uniq_pending_claim_active")
	assertIndexColumns(t, d, "uniq_pending_claim_active",
		"issue_uid", "holder_instance_uid", "holder", "client_kind")
}

func TestFederationBindingsFreshDBEmpty(t *testing.T) {
	d := openTestDB(t)

	got, err := d.ListFederationBindings(context.Background())

	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func assertIndexColumns(t *testing.T, d *sqlitestore.Store, indexName string, want ...string) {
	t.Helper()
	rows, err := d.QueryContext(context.Background(), `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, indexName)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got = append(got, name)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, want, got)
}

func TestFederationBindingUpsertRoundTrip(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	binding := db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 7,
		PullCursorEventID:    6,
		PushEnabled:          true,
		PushCursorEventID:    5,
		Enabled:              true,
	}
	created, err := d.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)

	got, err := d.FederationBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ProjectID, got.ProjectID)
	assert.Equal(t, db.FederationRoleSpoke, got.Role)
	assert.Equal(t, "http://127.0.0.1:7373", got.HubURL)
	assert.Equal(t, int64(42), got.HubProjectID)
	assert.Equal(t, p.UID, got.HubProjectUID)
	assert.Equal(t, int64(7), got.ReplayHorizonEventID)
	assert.Equal(t, int64(6), got.PullCursorEventID)
	assert.True(t, got.PushEnabled)
	assert.Equal(t, int64(5), got.PushCursorEventID)
	assert.True(t, got.Enabled)

	require.NoError(t, d.AdvanceFederationPullCursor(ctx, p.ID, 11))
	got, err = d.FederationBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(11), got.PullCursorEventID)

	all, err := d.ListFederationBindings(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, p.ID, all[0].ProjectID)
}

func TestFederationSyncStatusRecordsOperationalState(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	pullStarted := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	pullSuccess := pullStarted.Add(2 * time.Second)
	pushStarted := pullSuccess.Add(3 * time.Second)
	pushSuccess := pushStarted.Add(4 * time.Second)
	resetAt := pushSuccess.Add(time.Second)
	errorAt := resetAt.Add(time.Second)

	require.NoError(t, d.RecordFederationSyncPullStarted(ctx, p.ID, pullStarted))
	require.NoError(t, d.RecordFederationSyncPullSuccess(ctx, p.ID, pullSuccess))
	require.NoError(t, d.RecordFederationSyncPushStarted(ctx, p.ID, pushStarted))
	require.NoError(t, d.RecordFederationSyncPushSuccess(ctx, p.ID, pushSuccess))
	require.NoError(t, d.RecordFederationSyncReset(ctx, p.ID, resetAt))
	require.NoError(t, d.RecordFederationSyncError(ctx, p.ID, errors.New("hub offline"), errorAt))

	got, err := d.FederationSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ProjectID)
	assertTimePtrEqual(t, pullStarted, got.LastPullStartedAt)
	assertTimePtrEqual(t, pullSuccess, got.LastPullSuccessAt)
	assertTimePtrEqual(t, pushStarted, got.LastPushStartedAt)
	assertTimePtrEqual(t, pushSuccess, got.LastPushSuccessAt)
	assertTimePtrEqual(t, resetAt, got.LastResetAt)
	assertTimePtrEqual(t, errorAt, got.LastErrorAt)
	require.NotNil(t, got.LastError)
	assert.Equal(t, "hub offline", *got.LastError)
}

func TestFederationSyncStatusSuccessDoesNotClearPriorError(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	errorAt := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	successAt := errorAt.Add(time.Minute)

	require.NoError(t, d.RecordFederationSyncError(ctx, p.ID, errors.New("hub offline"), errorAt))
	require.NoError(t, d.RecordFederationSyncPullSuccess(ctx, p.ID, successAt))

	got, err := d.FederationSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assertTimePtrEqual(t, successAt, got.LastPullSuccessAt)
	assertTimePtrEqual(t, errorAt, got.LastErrorAt)
	require.NotNil(t, got.LastError)
	assert.Equal(t, "hub offline", *got.LastError)
}

func TestFederationSyncStatusClearErrorExplicitly(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	errorAt := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	require.NoError(t, d.RecordFederationSyncError(ctx, p.ID, errors.New("hub offline"), errorAt))
	require.NoError(t, d.ClearFederationSyncError(ctx, p.ID))

	got, err := d.FederationSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastErrorAt)
	assert.Nil(t, got.LastError)
}

func TestFederationQuarantineRecordAndActiveRoundTrip(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	createdAt := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	params := db.RecordFederationQuarantineParams{
		ProjectID:    p.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7", "evt-8", "evt-9"},
		Error:        "hub rejected batch",
		CreatedAt:    createdAt,
	}

	recorded, err := d.RecordFederationQuarantine(ctx, params)
	require.NoError(t, err)
	active, err := d.ActiveFederationQuarantine(ctx, p.ID, db.FederationQuarantineDirectionPush)

	require.NoError(t, err)
	assert.Equal(t, recorded.ID, active.ID)
	assert.Equal(t, p.ID, active.ProjectID)
	assert.Equal(t, db.FederationQuarantineDirectionPush, active.Direction)
	assert.Equal(t, int64(7), active.FirstEventID)
	assert.Equal(t, int64(9), active.LastEventID)
	assert.Equal(t, []string{"evt-7", "evt-8", "evt-9"}, active.EventUIDs)
	assert.Equal(t, "hub rejected batch", active.Error)
	assertTimeEqual(t, createdAt, active.CreatedAt)
	assert.Nil(t, active.SkippedAt)
	assert.Nil(t, active.SkippedBy)
	assert.Nil(t, active.SkipReason)
}

func TestFederationQuarantineRecordIsIdempotentPerProjectDirection(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	first, err := d.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    p.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7"},
		Error:        "first error",
		CreatedAt:    time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	second, err := d.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    p.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 10,
		LastEventID:  12,
		EventUIDs:    []string{"evt-10"},
		Error:        "second error",
		CreatedAt:    time.Date(2026, 5, 24, 12, 1, 0, 0, time.UTC),
	})

	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, int64(7), second.FirstEventID)
	assert.Equal(t, int64(9), second.LastEventID)
	assert.Equal(t, "first error", second.Error)
}

func TestSkipFederationQuarantineAdvancesPushCursor(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		PushCursorEventID:    3,
		Enabled:              true,
	})
	require.NoError(t, err)
	recorded, err := d.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    p.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7", "evt-8", "evt-9"},
		Error:        "hub rejected batch",
		CreatedAt:    time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	skippedAt := time.Date(2026, 5, 24, 12, 2, 0, 0, time.UTC)

	skipped, err := d.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{
		ID:        recorded.ID,
		ProjectID: p.ID,
		Actor:     "operator",
		Reason:    "accepted data loss",
		Now:       skippedAt,
	})

	require.NoError(t, err)
	require.NotNil(t, skipped.SkippedAt)
	assertTimeEqual(t, skippedAt, *skipped.SkippedAt)
	require.NotNil(t, skipped.SkippedBy)
	assert.Equal(t, "operator", *skipped.SkippedBy)
	require.NotNil(t, skipped.SkipReason)
	assert.Equal(t, "accepted data loss", *skipped.SkipReason)
	_, err = d.ActiveFederationQuarantine(ctx, p.ID, db.FederationQuarantineDirectionPush)
	assert.ErrorIs(t, err, db.ErrNotFound)
	binding, err := d.FederationBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(9), binding.PushCursorEventID)
}

func TestSkipFederationQuarantineRejectsWrongProject(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	other, err := d.CreateProject(ctx, "other")
	require.NoError(t, err)
	_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		PushCursorEventID:    3,
		Enabled:              true,
	})
	require.NoError(t, err)
	recorded, err := d.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    p.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7"},
		Error:        "hub rejected batch",
		CreatedAt:    time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	_, err = d.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{
		ID:        recorded.ID,
		ProjectID: other.ID,
		Actor:     "operator",
		Reason:    "wrong project",
		Now:       time.Date(2026, 5, 24, 12, 2, 0, 0, time.UTC),
	})

	require.ErrorIs(t, err, db.ErrNotFound)
	active, err := d.ActiveFederationQuarantine(ctx, p.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	assert.Nil(t, active.SkippedAt)
	binding, err := d.FederationBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), binding.PushCursorEventID)
}

func TestPendingFederationPushEvents(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	binding, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		PushEnabled:          true,
		Enabled:              true,
	})
	require.NoError(t, err)
	localIssue, localEvent, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "local",
		Author:    "tester",
	})
	require.NoError(t, err)
	foreign := remoteEvent(t, p.UID, p.Name, &localIssue.UID, nil, "issue.updated", "hub", 100,
		`{"title":"foreign","updated_at":"2026-05-23T12:00:00.000Z"}`)
	foreign.OriginInstanceUID = "01HZNQ7VFPK1XGD8R5MABCD4EF"
	foreign.ContentHash = remoteEventHash(t, foreign)
	inserted, err := d.InsertRemoteEvent(ctx, p.ID, foreign)
	require.NoError(t, err)
	require.True(t, inserted)
	_, localComment, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: localIssue.ID,
		Author:  "tester",
		Body:    "local comment",
	})
	require.NoError(t, err)
	_, ownerEvent, changed, err := d.UpdateOwner(ctx, localIssue.ID, strPtr("alice"), "tester")
	require.NoError(t, err)
	require.True(t, changed)
	priority := int64(2)
	_, priorityEvent, changed, err := d.UpdatePriority(ctx, localIssue.ID, &priority, "tester")
	require.NoError(t, err)
	require.True(t, changed)
	_, err = d.ExecContext(ctx, `
		INSERT INTO events(
			uid, origin_instance_uid, project_id, project_name,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash
		)
		VALUES(
			'01HZNQ7VFPK1XGD8R5MABCD4PX', ?, ?, ?,
			'project.removed', 'tester', '{}', 1, 0,
			'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'
		)`,
		d.InstanceUID(), p.ID, p.Name)
	require.NoError(t, err)

	got, err := d.PendingFederationPushEvents(ctx, p.ID, d.InstanceUID(), binding.PushCursorEventID, 10)
	require.NoError(t, err)

	require.Len(t, got, 4)
	assert.Equal(t,
		[]int64{localEvent.ID, localComment.ID, ownerEvent.ID, priorityEvent.ID},
		[]int64{got[0].ID, got[1].ID, got[2].ID, got[3].ID})
	for _, ev := range got {
		assert.Equal(t, d.InstanceUID(), ev.OriginInstanceUID)
	}

	got, err = d.PendingFederationPushEvents(ctx, p.ID, d.InstanceUID(), localEvent.ID, 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, localComment.ID, got[0].ID)

	got, err = d.PendingFederationPushEvents(ctx, p.ID, d.InstanceUID(), 0, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, localEvent.ID, got[0].ID)

	assertSchemaObject(t, d, "idx_events_origin_project_id")
}

func TestAdvanceFederationPushCursor(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)

	require.NoError(t, d.AdvanceFederationPushCursor(ctx, p.ID, 12))
	got, err := d.FederationBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(12), got.PushCursorEventID)

	assert.ErrorIs(t, d.AdvanceFederationPushCursor(ctx, 999, 1), db.ErrNotFound)
}

func TestEnableFederationPush(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushCursorEventID:    4,
		Enabled:              true,
	})
	require.NoError(t, err)

	got, err := d.EnableFederationPush(ctx, p.ID, 3)
	require.NoError(t, err)
	assert.True(t, got.PushEnabled)
	assert.Equal(t, int64(4), got.PushCursorEventID)

	got, err = d.EnableFederationPush(ctx, p.ID, 7)
	require.NoError(t, err)
	assert.True(t, got.PushEnabled)
	assert.Equal(t, int64(7), got.PushCursorEventID)
}

func TestResetFederatedProjectIfNoPendingPushRejectsPendingLocalEvents(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "pending",
		Author:    "tester",
	})
	require.NoError(t, err)

	err = d.ResetFederatedProjectIfNoPendingPush(ctx, p.ID, 20, 19, d.InstanceUID(), 0)

	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	events, listErr := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 10})
	require.NoError(t, listErr)
	require.Len(t, events, 1)
	assert.Equal(t, evt.ID, events[0].ID)
}

func TestResetFederatedProjectIfNoPendingPushIgnoresUnsupportedLocalEvents(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		INSERT INTO events(
			uid, origin_instance_uid, project_id, project_name,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash
		)
		VALUES(
			'01HZNQ7VFPK1XGD8R5MABCD4PZ', ?, ?, ?,
			'project.restored', 'tester', '{}', 1, 0,
			'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee'
		)`,
		d.InstanceUID(), p.ID, p.Name)
	require.NoError(t, err)

	err = d.ResetFederatedProjectIfNoPendingPush(ctx, p.ID, 20, 19, d.InstanceUID(), 0)

	require.NoError(t, err)
	events, listErr := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 10})
	require.NoError(t, listErr)
	assert.Empty(t, events)
}

func TestResetFederatedProjectIfNoPendingPushRejectsActiveQuarantine(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	binding, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushEnabled:          true,
		PushCursorEventID:    99,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, err = d.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    p.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7"},
		Error:        "poison",
		CreatedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)

	err = d.ResetFederatedProjectIfNoPendingPush(ctx, p.ID, 20, 19, d.InstanceUID(), binding.PushCursorEventID)

	require.ErrorIs(t, err, db.ErrFederationResetBlockedByQuarantine)
}

func TestResetFederatedProjectIfNoPendingPushAllowsAckedLocalEvents(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "already pushed",
		Author:    "tester",
	})
	require.NoError(t, err)

	err = d.ResetFederatedProjectIfNoPendingPush(ctx, p.ID, 20, 19, d.InstanceUID(), evt.ID)

	require.NoError(t, err)
	events, listErr := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 10})
	require.NoError(t, listErr)
	assert.Empty(t, events)
	_, err = d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestResetFederatedProjectClearsClaimProjectionState(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	other, err := d.CreateProject(ctx, "other")
	require.NoError(t, err)
	targetIssue := seedFederatedResetClaimState(ctx, t, d, p.ID, "target")
	otherIssue := seedFederatedResetClaimState(ctx, t, d, other.ID, "other")
	_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		Enabled:              true,
	})
	require.NoError(t, err)

	err = d.ResetFederatedProject(ctx, p.ID, 20, 19)

	require.NoError(t, err)
	assertClaimProjectionCounts(ctx, t, d, p.ID, 0, 0)
	assertClaimProjectionCounts(ctx, t, d, other.ID, 1, 1)
	_, err = d.IssueByUID(ctx, targetIssue.UID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = d.IssueByUID(ctx, otherIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
}

func TestResetFederatedProjectIfNoPendingPushClearsClaimProjectionState(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	other, err := d.CreateProject(ctx, "other")
	require.NoError(t, err)
	targetIssue := seedFederatedResetClaimState(ctx, t, d, p.ID, "target")
	otherIssue := seedFederatedResetClaimState(ctx, t, d, other.ID, "other")
	_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	_, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "acked local event",
		Author:    "tester",
	})
	require.NoError(t, err)

	err = d.ResetFederatedProjectIfNoPendingPush(ctx, p.ID, 20, 19, d.InstanceUID(), evt.ID)

	require.NoError(t, err)
	assertClaimProjectionCounts(ctx, t, d, p.ID, 0, 0)
	assertClaimProjectionCounts(ctx, t, d, other.ID, 1, 1)
	_, err = d.IssueByUID(ctx, targetIssue.UID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = d.IssueByUID(ctx, otherIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
}

func TestFederationBindingPhase1StyleDefaultsPushState(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	_, err := d.ExecContext(ctx, `
		INSERT INTO federation_bindings(
			project_id, role, hub_url, hub_project_id, hub_project_uid,
			replay_horizon_event_id, pull_cursor_event_id, enabled
		)
		VALUES(?, 'spoke', 'http://127.0.0.1:7373', 42, ?, 7, 6, 1)`,
		p.ID, p.UID)
	require.NoError(t, err)

	got, err := d.FederationBindingByProject(ctx, p.ID)
	require.NoError(t, err)
	assert.False(t, got.PushEnabled)
	assert.Equal(t, int64(0), got.PushCursorEventID)
}

func seedFederatedResetClaimState(
	ctx context.Context,
	t *testing.T,
	d *sqlitestore.Store,
	projectID int64,
	title string,
) db.Issue {
	t.Helper()
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	issue := makeIssue(t, ctx, d, projectID, title, "tester")
	require.NoError(t, d.UpsertClaimCache(ctx, cachedClaim(t, issue, claimPrincipal(t, title+"-holder"), "hard", now, nil)))
	_, err := d.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: projectID,
		IssueRef:  issue.ShortID,
		Principal: claimPrincipal(t, title+"-pending"),
		ClaimKind: "hard",
		Now:       now,
	})
	require.NoError(t, err)
	return issue
}

func assertClaimProjectionCounts(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, wantClaims, wantPending int) {
	t.Helper()
	var claims, pending int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_claims WHERE project_id = ?`, projectID).Scan(&claims))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_claim_requests WHERE project_id = ?`, projectID).Scan(&pending))
	assert.Equal(t, wantClaims, claims)
	assert.Equal(t, wantPending, pending)
}

func TestFederationTokenHash(t *testing.T) {
	got := db.FederationTokenHash("kata-federation-token")

	assert.Equal(t, "4260028b25f27cdfc5555050688678320f251c8f9a02ca473d4309ffe3e53947", got)
	assert.Len(t, got, 64)
}

func TestCanonicalFederationCapabilities(t *testing.T) {
	cases := map[string]string{
		"push,pull,push": "pull,push",
		" pull , push ":  "pull,push",
		"pull":           "pull",
		"push":           "push",
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			got, err := db.CanonicalFederationCapabilities(raw)

			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}

	for _, raw := range []string{"", "   ", "pull,", "pull,,push", "pull,admin"} {
		t.Run("reject "+raw, func(t *testing.T) {
			_, err := db.CanonicalFederationCapabilities(raw)

			require.Error(t, err)
		})
	}
}

func TestFederationEnrollmentCreateStoresOnlyTokenHash(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	token := "plaintext-enrollment-token"
	spokeUID := newTestUID(t)

	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: spokeUID,
		ProjectID:        &p.ID,
		Capabilities:     "push",
	})

	require.NoError(t, err)
	assert.Equal(t, spokeUID, created.Enrollment.SpokeInstanceUID)
	require.NotNil(t, created.Enrollment.ProjectID)
	assert.Equal(t, p.ID, *created.Enrollment.ProjectID)
	assert.Equal(t, "push", created.Enrollment.Capabilities)
	assert.Equal(t, db.FederationTokenHash(token), created.Enrollment.TokenHash)
	assert.NotEqual(t, token, created.Enrollment.TokenHash)

	var storedHash string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT token_hash FROM federation_enrollments WHERE id = ?`,
		created.Enrollment.ID,
	).Scan(&storedHash))
	assert.Equal(t, db.FederationTokenHash(token), storedHash)
	assert.Regexp(t, `^[0-9a-f]{64}$`, storedHash)

	var plaintextRows int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM federation_enrollments WHERE token_hash = ?`,
		token,
	).Scan(&plaintextRows))
	assert.Equal(t, 0, plaintextRows)
}

func TestFederationEnrollmentCreateGeneratesTokenOnceAndStoresHash(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	spokeUID := newTestUID(t)

	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: spokeUID,
		Capabilities:     "pull",
	})

	require.NoError(t, err)
	require.NotEmpty(t, created.Token)
	assert.Equal(t, db.FederationTokenHash(created.Token), created.Enrollment.TokenHash)
	assert.NotEqual(t, created.Token, created.Enrollment.TokenHash)

	var storedHash string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT token_hash FROM federation_enrollments WHERE id = ?`,
		created.Enrollment.ID,
	).Scan(&storedHash))
	assert.Equal(t, db.FederationTokenHash(created.Token), storedHash)
	assert.NotEqual(t, created.Token, storedHash)
}

func TestFederationEnrollmentCreateCanonicalizesCapabilities(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "canonical-token",
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &p.ID,
		Capabilities:     "push,pull,push",
	})

	require.NoError(t, err)
	assert.Equal(t, "pull,push", created.Enrollment.Capabilities)

	var stored string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT capabilities FROM federation_enrollments WHERE id = ?`,
		created.Enrollment.ID,
	).Scan(&stored))
	assert.Equal(t, "pull,push", stored)
}

func TestFederationEnrollmentAuthorizeReturnsSpokeInstanceUID(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	token := "spoke-bound-token"
	spokeUID := newTestUID(t)
	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: spokeUID,
		ProjectID:        &p.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)

	got, err := d.AuthorizeFederationToken(ctx, token, p.ID, "push")

	require.NoError(t, err)
	assert.Equal(t, created.Enrollment.ID, got.ID)
	assert.Equal(t, spokeUID, got.SpokeInstanceUID)
	assert.Equal(t, "push", got.Capabilities)
}

func TestFederationEnrollmentWildcardAuthorizesAnyEnabledHubProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	first := createProject(ctx, t, d, "first")
	second := createProject(ctx, t, d, "second")
	upsertTestHubFederationBinding(ctx, t, d, first, true)
	upsertTestHubFederationBinding(ctx, t, d, second, true)
	token := "wildcard-token"
	spokeUID := newTestUID(t)
	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: spokeUID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)
	require.Nil(t, created.Enrollment.ProjectID)

	for _, p := range []db.Project{first, second} {
		t.Run(p.Name, func(t *testing.T) {
			got, err := d.AuthorizeFederationToken(ctx, token, p.ID, "pull")

			require.NoError(t, err)
			assert.Equal(t, created.Enrollment.ID, got.ID)
			assert.Equal(t, spokeUID, got.SpokeInstanceUID)
			assert.Nil(t, got.ProjectID)
		})
	}
}

func TestFederationEnrollmentWildcardRejectsNonFederatedDisabledAndNonHubProjects(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	plain := createProject(ctx, t, d, "plain")
	disabledHub := createProject(ctx, t, d, "disabled-hub")
	spoke := createProject(ctx, t, d, "spoke")
	upsertTestHubFederationBinding(ctx, t, d, disabledHub, false)
	upsertTestSpokeFederationBinding(ctx, t, d, spoke, true)
	token := "wildcard-reject-token"
	_, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: newTestUID(t),
		Capabilities:     "pull",
	})
	require.NoError(t, err)

	for _, p := range []db.Project{plain, disabledHub, spoke} {
		t.Run(p.Name, func(t *testing.T) {
			_, err := d.AuthorizeFederationToken(ctx, token, p.ID, "pull")

			require.Error(t, err)
			assert.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

func TestFederationEnrollmentRejectsArchivedHubProject(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	token := "archived-hub-token" //nolint:gosec // test-only bearer token
	_, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &p.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)
	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: p.ID, Actor: "tester"})
	require.NoError(t, err)

	_, err = d.AuthorizeFederationToken(ctx, token, p.ID, "pull")

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestFederationEnrollmentProjectSpecificAuthorizesOnlyThatProject(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	allowed := createProject(ctx, t, d, "allowed")
	other := createProject(ctx, t, d, "other")
	upsertTestHubFederationBinding(ctx, t, d, allowed, true)
	upsertTestHubFederationBinding(ctx, t, d, other, true)
	token := "specific-project-token"
	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &allowed.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)

	got, err := d.AuthorizeFederationToken(ctx, token, allowed.ID, "push")
	require.NoError(t, err)
	assert.Equal(t, created.Enrollment.ID, got.ID)

	_, err = d.AuthorizeFederationToken(ctx, token, other.ID, "push")
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestFederationEnrollmentProjectSpecificRejectsNonFederatedDisabledAndNonHubProjects(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	plain := createProject(ctx, t, d, "plain")
	disabledHub := createProject(ctx, t, d, "disabled-hub")
	spoke := createProject(ctx, t, d, "spoke")
	upsertTestHubFederationBinding(ctx, t, d, disabledHub, false)
	upsertTestSpokeFederationBinding(ctx, t, d, spoke, true)

	for _, p := range []db.Project{plain, disabledHub, spoke} {
		t.Run(p.Name, func(t *testing.T) {
			token := "specific-token-" + p.Name
			_, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
				Token:            token,
				SpokeInstanceUID: newTestUID(t),
				ProjectID:        &p.ID,
				Capabilities:     "pull",
			})
			require.NoError(t, err)

			_, err = d.AuthorizeFederationToken(ctx, token, p.ID, "pull")

			require.Error(t, err)
			assert.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

func TestFederationEnrollmentRevokedRowsAreRejected(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	token := "revoked-token"
	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &p.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)
	require.NoError(t, d.RevokeFederationEnrollment(ctx, created.Enrollment.ID))

	_, err = d.AuthorizeFederationToken(ctx, token, p.ID, "pull")

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrNotFound)
	var revoked int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT revoked_at IS NOT NULL FROM federation_enrollments WHERE id = ?`,
		created.Enrollment.ID,
	).Scan(&revoked))
	assert.Equal(t, 1, revoked)
}

func TestFederationEnrollmentMissingCapabilityIsRejected(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	token := "missing-capability-token"
	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            token,
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &p.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)

	got, err := d.AuthorizeFederationToken(ctx, token, p.ID, "pull")
	require.NoError(t, err)
	assert.Equal(t, created.Enrollment.ID, got.ID)

	_, err = d.AuthorizeFederationToken(ctx, token, p.ID, "push")
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestFederationEnrollmentUnknownTokenIsRejected(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestHubFederationBinding(ctx, t, d, p, true)
	_, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "known-token",
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &p.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)

	_, err = d.AuthorizeFederationToken(ctx, "unknown-token", p.ID, "pull")

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestEnableProjectFederationEmitsBaselineSnapshotsAtHorizon(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	owner := "alice"
	priority := int64(1)
	active, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "active issue",
		Body:      "body text",
		Author:    "alice",
		Owner:     &owner,
		Priority:  &priority,
		Labels:    []string{"area:db"},
	})
	require.NoError(t, err)
	deleted, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "deleted issue",
		Author:    "bob",
	})
	require.NoError(t, err)
	_, err = d.CreateLink(ctx, db.CreateLinkParams{
		ProjectID:   p.ID,
		FromIssueID: active.ID,
		ToIssueID:   deleted.ID,
		Type:        "related",
		Author:      "alice",
	})
	require.NoError(t, err)
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: active.ID,
		Author:  "carol",
		Body:    "baseline comment",
	})
	require.NoError(t, err)
	metaOut, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: active.ID, IfMatchRev: active.Revision, Actor: "alice",
		Patch: map[string]json.RawMessage{
			"definitely_not_a_key": json.RawMessage(`"yellow"`),
		},
	})
	require.NoError(t, err)
	active = metaOut.Issue
	projectMetaOut, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID:  p.ID,
		IfMatchRev: p.Revision,
		Actor:      "alice",
		Patch: map[string]json.RawMessage{
			"area": json.RawMessage(`"federation"`),
		},
	})
	require.NoError(t, err)
	p = projectMetaOut.Project
	deleted, _, _, err = d.SoftDeleteIssue(ctx, deleted.ID, "bob")
	require.NoError(t, err)

	binding, err := d.EnableProjectFederation(ctx, p.ID, "federation")
	require.NoError(t, err)

	assert.Equal(t, db.FederationRoleHub, binding.Role)
	assert.Equal(t, p.ID, binding.ProjectID)
	assert.Equal(t, p.UID, binding.HubProjectUID)
	assert.Greater(t, binding.ReplayHorizonEventID, int64(0))
	assert.Equal(t, binding.ReplayHorizonEventID-1, binding.PullCursorEventID)

	events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 100})
	require.NoError(t, err)
	var enableEvent *db.Event
	snapshots := map[string]db.Event{}
	for i := range events {
		ev := events[i]
		switch ev.Type {
		case "project.federation_enabled":
			enableEvent = &ev
		case "issue.snapshot":
			require.NotNil(t, ev.IssueUID)
			snapshots[*ev.IssueUID] = ev
		}
	}
	require.NotNil(t, enableEvent)
	enablePayload := unmarshalPayload[struct {
		Metadata json.RawMessage `json:"metadata"`
	}](t, enableEvent.Payload)
	assert.JSONEq(t, `{"area":"federation"}`, string(enablePayload.Metadata))
	assert.Equal(t, binding.ReplayHorizonEventID, enableEvent.ID)
	require.Len(t, snapshots, 2)

	for _, snapshot := range snapshots {
		assert.Equal(t, enableEvent.HLCPhysicalMS, snapshot.HLCPhysicalMS)
		assert.Equal(t, enableEvent.HLCCounter, snapshot.HLCCounter)
	}

	activePayload := unmarshalPayload[federationSnapshotPayload](t, snapshots[active.UID].Payload)
	assert.Equal(t, active.UID, activePayload.UID)
	assert.Equal(t, active.ShortID, activePayload.ShortID)
	assert.Equal(t, active.Title, activePayload.Title)
	assert.Equal(t, active.Body, activePayload.Body)
	assert.Equal(t, active.Author, activePayload.Author)
	require.NotNil(t, activePayload.Owner)
	assert.Equal(t, owner, *activePayload.Owner)
	require.NotNil(t, activePayload.Priority)
	assert.Equal(t, priority, *activePayload.Priority)
	assert.Equal(t, "open", activePayload.Status)
	assert.JSONEq(t, `{"definitely_not_a_key":"yellow"}`, string(activePayload.Metadata))
	assert.Equal(t, []string{"area:db"}, activePayload.Labels)
	require.Len(t, activePayload.Comments, 1)
	assert.Equal(t, comment.UID, activePayload.Comments[0].CommentUID)
	assert.Equal(t, "baseline comment", activePayload.Comments[0].Body)
	require.Len(t, activePayload.Links, 1)
	assert.Equal(t, "related", activePayload.Links[0].Type)
	assert.Equal(t, deleted.UID, activePayload.Links[0].ToIssueUID)

	deletedPayload := unmarshalPayload[federationSnapshotPayload](t, snapshots[deleted.UID].Payload)
	assert.Equal(t, deleted.UID, deletedPayload.UID)
	require.NotNil(t, deletedPayload.DeletedAt)

	_, postEnableEvent, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "post baseline",
		Author:    "dave",
	})
	require.NoError(t, err)
	assert.True(t,
		postEnableEvent.HLCPhysicalMS > enableEvent.HLCPhysicalMS ||
			(postEnableEvent.HLCPhysicalMS == enableEvent.HLCPhysicalMS &&
				postEnableEvent.HLCCounter > enableEvent.HLCCounter),
		"post-baseline event must sort after baseline boundary",
	)
}

func assertFederationBindingColumn(t *testing.T, d *sqlitestore.Store, column string) {
	t.Helper()
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('federation_bindings') WHERE name = ?`,
		column,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "missing federation_bindings.%s", column)
}

func assertFederationSyncStatusColumn(t *testing.T, d *sqlitestore.Store, column string) {
	t.Helper()
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('federation_sync_status') WHERE name = ?`,
		column,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "missing federation_sync_status.%s", column)
}

func assertFederationQuarantineColumn(t *testing.T, d *sqlitestore.Store, column string) {
	t.Helper()
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('federation_quarantine') WHERE name = ?`,
		column,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "missing federation_quarantine.%s", column)
}

func assertTimeEqual(t *testing.T, want, got time.Time) {
	t.Helper()
	assert.True(t, want.Equal(got), "want %s, got %s", want, got)
}

func assertTimePtrEqual(t *testing.T, want time.Time, got *time.Time) {
	t.Helper()
	require.NotNil(t, got)
	assert.True(t, want.Equal(*got), "want %s, got %s", want, *got)
}

func TestInsertRemoteEventPreservesPortableFieldsAndDedupe(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issueUID := newTestUID(t)
	remoteProjectUID := p.UID
	ev := remoteEvent(t, remoteProjectUID, "hub", &issueUID, nil,
		"issue.snapshot", "remote-agent", 100, `{"uid":"`+issueUID+`","short_id":"`+shortID(issueUID)+`","title":"remote","body":"","author":"remote-agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)

	inserted, err := d.InsertRemoteEvent(ctx, p.ID, ev)
	require.NoError(t, err)
	assert.True(t, inserted)

	events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	got := events[0]
	assert.NotZero(t, got.ID)
	assert.Equal(t, ev.EventUID, got.UID)
	assert.Equal(t, ev.OriginInstanceUID, got.OriginInstanceUID)
	assert.Equal(t, ev.ProjectUID, got.ProjectUID)
	assert.Equal(t, ev.ProjectName, got.ProjectName)
	require.NotNil(t, got.IssueUID)
	assert.Equal(t, issueUID, *got.IssueUID)
	assert.Nil(t, got.IssueID)
	assert.Equal(t, ev.Type, got.Type)
	assert.Equal(t, ev.Actor, got.Actor)
	assert.JSONEq(t, string(ev.Payload), got.Payload)
	assert.Equal(t, ev.HLCPhysicalMS, got.HLCPhysicalMS)
	assert.Equal(t, ev.HLCCounter, got.HLCCounter)
	assert.Equal(t, ev.ContentHash, got.ContentHash)
	assert.Equal(t, ev.CreatedAt.UTC(), got.CreatedAt.UTC())

	inserted, err = d.InsertRemoteEvent(ctx, p.ID, ev)
	require.NoError(t, err)
	assert.False(t, inserted)
}

func TestInsertRemoteEventRejectsDuplicateUIDWithDifferentHash(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issueUID := newTestUID(t)
	remoteProjectUID := p.UID
	ev := remoteEvent(t, remoteProjectUID, "hub", &issueUID, nil,
		"issue.snapshot", "remote-agent", 100, `{"uid":"`+issueUID+`","short_id":"`+shortID(issueUID)+`","title":"remote","body":"","author":"remote-agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)
	inserted, err := d.InsertRemoteEvent(ctx, p.ID, ev)
	require.NoError(t, err)
	require.True(t, inserted)

	conflict := ev
	conflict.Actor = "other-agent"
	conflict.ContentHash = remoteEventHash(t, conflict)
	inserted, err = d.InsertRemoteEvent(ctx, p.ID, conflict)

	assert.False(t, inserted)
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrRemoteEventConflict)
}

func TestInsertRemoteEventRejectsContentHashMismatch(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issueUID := newTestUID(t)
	remoteProjectUID := p.UID
	ev := remoteEvent(t, remoteProjectUID, "hub", &issueUID, nil,
		"issue.snapshot", "remote-agent", 100, `{"uid":"`+issueUID+`","short_id":"`+shortID(issueUID)+`","title":"remote","body":"","author":"remote-agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)
	ev.ContentHash = strings.Repeat("0", 64)

	inserted, err := d.InsertRemoteEvent(ctx, p.ID, ev)

	assert.False(t, inserted)
	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrRemoteEventHashMismatch)
	events, listErr := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 10})
	require.NoError(t, listErr)
	assert.Empty(t, events)
}

func TestIngestFederationEvents(t *testing.T) {
	t.Run("empty batch succeeds with zero cursor", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)

		got, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
		})

		require.NoError(t, err)
		assert.Equal(t, db.FederationIngestResult{}, got)
	})

	t.Run("inserts valid foreign event and materializes", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		issueUID := newTestUID(t)
		ev := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, issueUID, 100)

		got, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events: []db.FederationIngestEvent{{
				SourceEventID: 12,
				Event:         ev,
			}},
		})

		require.NoError(t, err)
		assert.Equal(t, 1, got.Accepted)
		assert.Equal(t, 0, got.Duplicates)
		assert.Equal(t, int64(12), got.PushCursorEventID)
		assert.Equal(t, []string{ev.EventUID}, got.InsertedEventUIDs)

		events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, events, 2, "project.federation_enabled + ingested event")
		inserted := events[1]
		assert.NotEqual(t, int64(12), inserted.ID, "hub must assign its own local row id")
		assert.Equal(t, ev.EventUID, inserted.UID)
		assert.Equal(t, spokeUID, inserted.OriginInstanceUID)
		assert.Equal(t, ev.ContentHash, inserted.ContentHash)

		issue, err := d.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		assert.Equal(t, "spoke work", issue.Title)
	})

	t.Run("accepts stale project name after hub rename", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		_, err := d.RenameProject(ctx, p.ID, "renamed")
		require.NoError(t, err)
		issueUID := newTestUID(t)
		ev := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, issueUID, 100)

		got, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events:           []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
		})

		require.NoError(t, err)
		assert.Equal(t, 1, got.Accepted)
		issue, err := d.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		assert.Equal(t, "spoke work", issue.Title)
	})

	t.Run("rejects hash mismatch before insert", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		ev := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 100)
		ev.ContentHash = strings.Repeat("0", 64)

		_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events:           []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, db.ErrRemoteEventHashMismatch)
		assertIngestedEventCount(ctx, t, d, p.ID, 0)
	})

	t.Run("rejects invalid source event cursor before insert", func(t *testing.T) {
		for _, sourceEventID := range []int64{0, -1} {
			t.Run(fmt.Sprintf("source_%d", sourceEventID), func(t *testing.T) {
				d, ctx, p, spokeUID := setupFederationIngestHub(t)
				ev := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 100)

				_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
					ProjectID:        p.ID,
					SpokeInstanceUID: spokeUID,
					Events:           []db.FederationIngestEvent{{SourceEventID: sourceEventID, Event: ev}},
				})

				require.Error(t, err)
				assert.ErrorIs(t, err, db.ErrFederationIngestValidation)
				assertIngestedEventCount(ctx, t, d, p.ID, 0)
			})
		}
	})

	t.Run("rejects duplicate uid with different hash all or nothing", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		issueUID := newTestUID(t)
		first := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, issueUID, 100)
		_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events:           []db.FederationIngestEvent{{SourceEventID: 1, Event: first}},
		})
		require.NoError(t, err)

		conflict := first
		conflict.Actor = "other"
		conflict.ContentHash = remoteEventHash(t, conflict)
		newIssue := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 101)

		_, err = d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events: []db.FederationIngestEvent{
				{SourceEventID: 2, Event: conflict},
				{SourceEventID: 3, Event: newIssue},
			},
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, db.ErrRemoteEventConflict)
		assertIngestedEventCount(ctx, t, d, p.ID, 1)
	})

	t.Run("duplicate same hash advances cursor without conflict", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		first := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 100)
		_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events:           []db.FederationIngestEvent{{SourceEventID: 4, Event: first}},
		})
		require.NoError(t, err)

		retry, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events:           []db.FederationIngestEvent{{SourceEventID: 9, Event: first}},
		})

		require.NoError(t, err)
		assert.Equal(t, 0, retry.Accepted)
		assert.Equal(t, 1, retry.Duplicates)
		assert.Equal(t, int64(9), retry.PushCursorEventID)
		assert.Empty(t, retry.InsertedEventUIDs)
		assertIngestedEventCount(ctx, t, d, p.ID, 1)
	})

	t.Run("mixed duplicate same hash and new event succeeds", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		first := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 100)
		_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events:           []db.FederationIngestEvent{{SourceEventID: 4, Event: first}},
		})
		require.NoError(t, err)
		second := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 101)

		got, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events: []db.FederationIngestEvent{
				{SourceEventID: 5, Event: first},
				{SourceEventID: 6, Event: second},
			},
		})

		require.NoError(t, err)
		assert.Equal(t, 1, got.Accepted)
		assert.Equal(t, 1, got.Duplicates)
		assert.Equal(t, int64(6), got.PushCursorEventID)
		assert.Equal(t, []string{second.EventUID}, got.InsertedEventUIDs)
		assertIngestedEventCount(ctx, t, d, p.ID, 2)
	})

	t.Run("batch is all or nothing", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		valid := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 100)
		invalid := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 101)
		invalid.ProjectUID = newTestUID(t)
		invalid.ContentHash = remoteEventHash(t, invalid)

		_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events: []db.FederationIngestEvent{
				{SourceEventID: 1, Event: valid},
				{SourceEventID: 2, Event: invalid},
			},
		})

		require.Error(t, err)
		assertIngestedEventCount(ctx, t, d, p.ID, 0)
	})

	t.Run("target project must be enabled hub", func(t *testing.T) {
		d, ctx, p := setupTestProject(t)
		spokeUID := newTestUID(t)
		ev := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 100)

		_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

		require.Error(t, err)
		assert.ErrorIs(t, err, db.ErrNotFound)
		assertIngestedEventCount(ctx, t, d, p.ID, 0)
	})

	t.Run("accepted foreign HLC advances later hub local event", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		issueUID := newTestUID(t)
		highHLC := time.Now().Add(10 * time.Second).UnixMilli()
		ev := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, issueUID, highHLC)
		_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events:           []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
		})
		require.NoError(t, err)

		local, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID,
			Title:     "hub local",
			Author:    "hub",
		})
		require.NoError(t, err)

		events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 20})
		require.NoError(t, err)
		var localEvent db.Event
		for _, got := range events {
			if got.IssueUID != nil && *got.IssueUID == local.UID {
				localEvent = got
				break
			}
		}
		require.NotZero(t, localEvent.ID)
		assert.True(t,
			localEvent.HLCPhysicalMS > ev.HLCPhysicalMS ||
				(localEvent.HLCPhysicalMS == ev.HLCPhysicalMS && localEvent.HLCCounter > ev.HLCCounter),
			"local event HLC %#v must sort after accepted foreign HLC %#v", localEvent, ev)
	})
}

func TestIngestFederationEvents_Validation(t *testing.T) {
	t.Run("rejects project uid mismatch", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		ev := ingestIssueCreatedEvent(t, newTestUID(t), p.Name, spokeUID, newTestUID(t), 100)

		_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

		require.Error(t, err)
		assertIngestedEventCount(ctx, t, d, p.ID, 0)
	})

	t.Run("rejects origin mismatch", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		ev := ingestIssueCreatedEvent(t, p.UID, p.Name, newTestUID(t), newTestUID(t), 100)

		_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

		require.Error(t, err)
		assertIngestedEventCount(ctx, t, d, p.ID, 0)
	})

	t.Run("rejects project metadata payload for different project uid", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		ev := ingestEventWithPayload(t, p.UID, p.Name, spokeUID, nil, nil,
			"project.metadata_updated", 100,
			`{"project_uid":"`+newTestUID(t)+`","diff":{"area":{"from":null,"to":"db"}}}`)

		_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

		require.Error(t, err)
	})

	t.Run("rejects envelope and payload issue uid disagreement", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		envelopeUID := newTestUID(t)
		payloadUID := newTestUID(t)
		ev := ingestEventWithPayload(t, p.UID, p.Name, spokeUID, &envelopeUID, nil,
			"issue.created", 100,
			`{"uid":"`+payloadUID+`","short_id":"`+shortID(payloadUID)+`","title":"bad","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)

		_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

		require.Error(t, err)
	})

	t.Run("rejects non-create reference to unknown issue", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		unknown := newTestUID(t)
		ev := ingestEventWithPayload(t, p.UID, p.Name, spokeUID, &unknown, nil,
			"issue.updated", 100,
			`{"issue_uid":"`+unknown+`","title":"bad","updated_at":"2026-05-23T12:00:00.000Z"}`)

		_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

		require.Error(t, err)
	})

	for _, eventType := range []string{"issue.created", "issue.snapshot"} {
		t.Run("rejects fresh "+eventType+" for known issue uid", func(t *testing.T) {
			d, ctx, p, spokeUID := setupFederationIngestHub(t)
			issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: p.ID,
				Title:     "hub-owned title",
				Author:    "hub-agent",
			})
			require.NoError(t, err)
			_, err = d.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: p.ID,
				IssueRef:  issue.ShortID,
				Principal: db.ClaimPrincipal{HolderInstanceUID: d.InstanceUID(), Holder: "hub-agent"},
				ClaimKind: "hard",
				Now:       time.Now().UTC(),
			})
			require.NoError(t, err)
			ev := remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, eventType, "remote-agent")

			_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

			require.Error(t, err)
			assert.ErrorIs(t, err, db.ErrFederationIngestValidation)
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			assert.Equal(t, "hub-owned title", got.Title)
			assertEventCount(t, d, "claim.violated", 0)
		})
	}

	t.Run("allows non-create reference created earlier in batch", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		issueUID := newTestUID(t)
		create := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, issueUID, 100)
		update := ingestEventWithPayload(t, p.UID, p.Name, spokeUID, &issueUID, nil,
			"issue.updated", 101,
			`{"issue_uid":"`+issueUID+`","title":"after create","updated_at":"2026-05-23T12:01:00.000Z"}`)

		_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events: []db.FederationIngestEvent{
				{SourceEventID: 1, Event: create},
				{SourceEventID: 2, Event: update},
			},
		})

		require.NoError(t, err)
		issue, err := d.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		assert.Equal(t, "after create", issue.Title)
	})

	t.Run("rejects related issue outside project", func(t *testing.T) {
		d, ctx, p, spokeUID := setupFederationIngestHub(t)
		foreignProject, err := d.CreateProject(ctx, "foreign")
		require.NoError(t, err)
		foreign, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: foreignProject.ID,
			Title:     "foreign",
			Author:    "tester",
		})
		require.NoError(t, err)
		base := ingestIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t), 100)
		link := ingestEventWithPayload(t, p.UID, p.Name, spokeUID, base.IssueUID, &foreign.UID,
			"issue.linked", 101,
			`{"type":"related","from_uid":"`+*base.IssueUID+`","to_uid":"`+foreign.UID+`"}`)

		_, err = d.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID:        p.ID,
			SpokeInstanceUID: spokeUID,
			Events: []db.FederationIngestEvent{
				{SourceEventID: 1, Event: base},
				{SourceEventID: 2, Event: link},
			},
		})

		require.Error(t, err)
	})

	for _, eventType := range []string{"issue.moved", "recurrence.created", "recurrence.updated", "recurrence.deleted"} {
		t.Run("rejects "+eventType, func(t *testing.T) {
			d, ctx, p, spokeUID := setupFederationIngestHub(t)
			issueUID := newTestUID(t)
			ev := ingestEventWithPayload(t, p.UID, p.Name, spokeUID, &issueUID, nil,
				eventType, 100, `{"issue_uid":"`+issueUID+`"}`)

			_, err := d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

			require.Error(t, err)
		})
	}
}

func TestIngestClaimCloseReleasesLiveClaim(t *testing.T) {
	t.Run("holder close releases and emits release", func(t *testing.T) {
		d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
		_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "remote-agent"},
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)

		_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID,
			remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.closed", "remote-agent")))

		require.NoError(t, err)
		assertLiveClaimCount(t, d, issue.UID, 0)
		assertEventCount(t, d, "claim.released", 1)
		assertEventCount(t, d, "claim.violated", 0)
	})

	t.Run("non-holder close violates and still releases", func(t *testing.T) {
		d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
		_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "holder"},
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)

		_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID,
			remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.closed", "remote-agent")))

		require.NoError(t, err)
		assertLiveClaimCount(t, d, issue.UID, 0)
		assertEventCount(t, d, "claim.violated", 1)
		assertEventCount(t, d, "claim.released", 1)
	})

	t.Run("idempotent retry does not duplicate release", func(t *testing.T) {
		d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
		_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: p.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "remote-agent"},
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)
		ev := remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.closed", "remote-agent")
		_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))
		require.NoError(t, err)

		_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))

		require.NoError(t, err)
		assertEventCount(t, d, "claim.released", 1)
	})
}

func TestIngestClaimViolationWorkMutationCoverage(t *testing.T) {
	for _, eventType := range []string{
		"issue.updated", "issue.assigned", "issue.unassigned",
		"issue.priority_set", "issue.priority_cleared",
		"issue.closed", "issue.reopened", "issue.soft_deleted", "issue.restored",
		"issue.labeled", "issue.unlabeled", "issue.linked", "issue.unlinked",
		"issue.links_changed", "issue.metadata_updated",
	} {
		t.Run(eventType, func(t *testing.T) {
			d, ctx, p, spokeUID, issue, peer := setupIngestClaimIssue(t)
			_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: p.ID,
				IssueRef:  issue.ShortID,
				Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "holder"},
				ClaimKind: "hard",
				Now:       time.Now().UTC(),
			})
			require.NoError(t, err)

			_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID,
				remoteClaimWorkEvent(t, p, spokeUID, issue.UID, &peer.UID, eventType, "remote-agent")))

			require.NoError(t, err)
			assertEventCount(t, d, "claim.violated", 1)
		})
	}

	for _, eventType := range []string{"project.metadata_updated"} {
		t.Run(eventType, func(t *testing.T) {
			d, ctx, p, spokeUID, issue, peer := setupIngestClaimIssue(t)
			_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: p.ID,
				IssueRef:  issue.ShortID,
				Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "holder"},
				ClaimKind: "hard",
				Now:       time.Now().UTC(),
			})
			require.NoError(t, err)

			_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID,
				remoteClaimWorkEvent(t, p, spokeUID, issue.UID, &peer.UID, eventType, "remote-agent")))

			require.NoError(t, err)
			assertEventCount(t, d, "claim.violated", 0)
		})
	}
}

func TestIngestClaimViolationExpiresTimedClaimBeforeAudit(t *testing.T) {
	d, ctx, p, spokeUID, issue, _ := setupIngestClaimIssue(t)
	_, err := d.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: p.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{HolderInstanceUID: spokeUID, Holder: "remote-agent"},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       time.Now().Add(-2 * time.Minute).UTC(),
	})
	require.NoError(t, err)

	_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID,
		remoteClaimWorkEvent(t, p, spokeUID, issue.UID, nil, "issue.updated", "remote-agent")))

	require.NoError(t, err)
	assertLiveClaimCount(t, d, issue.UID, 0)
	assertEventCount(t, d, "claim.expired", 1)
	assertEventCount(t, d, "claim.violated", 0)
}

func TestMaterializeFederatedProject(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	remoteProjectUID := p.UID
	issueUID := newTestUID(t)
	relatedUID := newTestUID(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        remoteProjectUID,
		ReplayHorizonEventID: 1,
		PullCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)

	issueSnapshot := `{
		"uid":"` + issueUID + `",
		"short_id":"` + shortID(issueUID) + `",
		"title":"materialized",
		"body":"body",
		"author":"alice",
		"owner":"alice",
		"priority":1,
		"status":"open",
		"metadata":{"area":"db"},
		"labels":["area:db"],
		"links":[{"type":"related","to_issue_uid":"` + relatedUID + `"}],
		"comments":[{"comment_uid":"` + newTestUID(t) + `","author":"bob","body":"note","created_at":"2026-05-23T12:00:02.000Z"}],
		"created_at":"2026-05-23T12:00:00.000Z",
		"updated_at":"2026-05-23T12:00:09.000Z"
	}`
	relatedSnapshot := `{
		"uid":"` + relatedUID + `",
		"short_id":"` + shortID(relatedUID) + `",
		"title":"related",
		"body":"",
		"author":"alice",
		"status":"open",
		"metadata":{},
		"created_at":"2026-05-23T12:00:00.000Z"
	}`
	for _, ev := range []db.RemoteEvent{
		remoteEvent(t, remoteProjectUID, "hub", nil, nil, "project.federation_enabled", "remote-agent", 99,
			`{"project_uid":"`+remoteProjectUID+`","project_name":"hub","metadata":{"area":"federation"}}`),
		remoteEvent(t, remoteProjectUID, "hub", &issueUID, nil, "issue.snapshot", "remote-agent", 100, issueSnapshot),
		remoteEvent(t, remoteProjectUID, "hub", &relatedUID, nil, "issue.snapshot", "remote-agent", 101, relatedSnapshot),
	} {
		inserted, err := d.InsertRemoteEvent(ctx, p.ID, ev)
		require.NoError(t, err)
		require.True(t, inserted)
	}

	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))

	issue, err := d.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "materialized", issue.Title)
	assert.Equal(t, "body", issue.Body)
	require.NotNil(t, issue.Owner)
	assert.Equal(t, "alice", *issue.Owner)
	require.NotNil(t, issue.Priority)
	assert.Equal(t, int64(1), *issue.Priority)
	assert.Equal(t, "2026-05-23T12:00:09.000Z", issue.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"))
	assert.JSONEq(t, `{"area":"db"}`, string(issue.Metadata))
	project, err := d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	assert.JSONEq(t, `{"area":"federation"}`, string(project.Metadata))
	projectRevision := project.Revision
	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	unchangedProject, err := d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, projectRevision, unchangedProject.Revision, "duplicate materialization must not bump project revision")

	events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: p.ID, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	var resolvedIssueEvent bool
	for _, event := range events {
		if event.Type == "issue.snapshot" && event.IssueUID != nil && *event.IssueUID == issueUID {
			resolvedIssueEvent = event.IssueShortID != nil
		}
	}
	assert.True(t, resolvedIssueEvent, "stored remote issue events should resolve to materialized issue display ids")

	var commentBody string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT body FROM comments WHERE issue_id = ?`, issue.ID).Scan(&commentBody))
	assert.Equal(t, "note", commentBody)
	assertRowCount(ctx, t, d, 1, "label materialized",
		`SELECT count(*) FROM issue_labels WHERE issue_id = ? AND label = 'area:db'`, issue.ID)
	assertRowCount(ctx, t, d, 1, "link materialized",
		`SELECT count(*) FROM links WHERE project_id = ? AND type = 'related'`, p.ID)
}

func TestMaterializeFederatedProject_ReconcilesExistingRowsAndEdges(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issue, createdEvent, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "local title",
		Body:      "local body",
		Author:    "alice",
	})
	require.NoError(t, err)
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "peer",
		Author:    "alice",
	})
	require.NoError(t, err)
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  "alice",
		Body:    "local comment",
	})
	require.NoError(t, err)
	_, _, err = d.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
		EventType: "issue.labeled",
		Label:     "area:db",
		Actor:     "alice",
	})
	require.NoError(t, err)
	_, _, err = d.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		ProjectID:   p.ID,
		FromIssueID: issue.ID,
		ToIssueID:   peer.ID,
		Type:        "related",
		Author:      "alice",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: issue.ID,
		FromShortID:  issue.ShortID,
		FromUID:      issue.UID,
		ToShortID:    peer.ShortID,
		ToUID:        peer.UID,
		Actor:        "alice",
	})
	require.NoError(t, err)
	upsertTestSpokeFederationBindingWithPush(ctx, t, d, p, true, true)

	updatePayload := `{"issue_uid":"` + issue.UID + `","title":"remote title","body":"remote body","updated_at":"2026-05-23T12:10:00.000Z"}`
	remoteUpdate := remoteEvent(t, p.UID, "hub", &issue.UID, nil,
		"issue.updated", "remote-agent", createdEvent.HLCPhysicalMS+100, updatePayload)
	inserted, err := d.InsertRemoteEvent(ctx, p.ID, remoteUpdate)
	require.NoError(t, err)
	require.True(t, inserted)

	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	reconciled, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, issue.ID, reconciled.ID)
	assert.Equal(t, issue.ShortID, reconciled.ShortID)
	assert.Equal(t, "remote title", reconciled.Title)
	assert.Equal(t, "remote body", reconciled.Body)
	assert.Greater(t, reconciled.Revision, issue.Revision)
	reconciledRevision := reconciled.Revision

	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	unchanged, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, reconciledRevision, unchanged.Revision, "duplicate materialization must not bump issue revision")

	var eventIssueID int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT issue_id FROM events WHERE id = ?`, createdEvent.ID).Scan(&eventIssueID))
	assert.Equal(t, issue.ID, eventIssueID)

	assertRowCount(ctx, t, d, 1, "comment reconciled by uid",
		`SELECT count(*) FROM comments WHERE uid = ? AND issue_id = ?`, comment.UID, issue.ID)
	assertRowCount(ctx, t, d, 1, "label reconciled by key",
		`SELECT count(*) FROM issue_labels WHERE issue_id = ? AND label = 'area:db'`, issue.ID)
	assertRowCount(ctx, t, d, 1, "link reconciled by uid key",
		`SELECT count(*) FROM links WHERE project_id = ? AND type = 'related'`, p.ID)
}

func TestMaterializeFederatedProjectExtendsCollidingIncomingShortIDs(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	upsertTestSpokeFederationBinding(ctx, t, d, p, true)
	uidA := "01HZNQ7VFPK1XGD8R5MABCD4EC"
	uidB := "01HZNQ7VFPK1XGD8R5MABXD4EC"
	for i, issueUID := range []string{uidA, uidB} {
		payload := `{
			"uid":"` + issueUID + `",
			"short_id":"d4ec",
			"title":"remote ` + strconv.Itoa(i) + `",
			"body":"",
			"author":"remote-agent",
			"status":"open",
			"metadata":{},
			"created_at":"2026-05-23T12:00:00.000Z"
		}`
		ev := remoteEvent(t, p.UID, "hub", &issueUID, nil,
			"issue.snapshot", "remote-agent", int64(100+i), payload)
		inserted, err := d.InsertRemoteEvent(ctx, p.ID, ev)
		require.NoError(t, err)
		require.True(t, inserted)
	}

	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))

	a, err := d.IssueByUID(ctx, uidA, db.IncludeDeletedYes)
	require.NoError(t, err)
	b, err := d.IssueByUID(ctx, uidB, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, "d4ec", a.ShortID)
	assert.NotEqual(t, a.ShortID, b.ShortID)
	assert.Greater(t, len(b.ShortID), len("d4ec"))
	assert.True(t, strings.HasSuffix(uidB, strings.ToUpper(b.ShortID)))
}

func TestFederatedSpokeWriteGatePushDisabledRejectsAndPushEnabledPermits(t *testing.T) {
	cases := map[string]func(context.Context, *sqlitestore.Store, db.Project, db.Issue, db.Issue) error{
		"create issue": func(ctx context.Context, d *sqlitestore.Store, p db.Project, _, _ db.Issue) error {
			_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: p.ID,
				Title:     "new local work",
				Author:    "tester",
			})
			return err
		},
		"edit issue": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, issue, _ db.Issue) error {
			title := "edited"
			_, _, _, err := d.EditIssue(ctx, db.EditIssueParams{
				IssueID: issue.ID,
				Title:   &title,
				Actor:   "tester",
			})
			return err
		},
		"comment": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, issue, _ db.Issue) error {
			_, _, err := d.CreateComment(ctx, db.CreateCommentParams{
				IssueID: issue.ID,
				Author:  "tester",
				Body:    "local comment",
			})
			return err
		},
		"label": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, issue, _ db.Issue) error {
			_, _, err := d.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{
				EventType: "issue.labeled",
				Label:     "area:db",
				Actor:     "tester",
			})
			return err
		},
		"link": func(ctx context.Context, d *sqlitestore.Store, p db.Project, issue, peer db.Issue) error {
			_, _, err := d.CreateLinkAndEvent(ctx, db.CreateLinkParams{
				ProjectID:   p.ID,
				FromIssueID: issue.ID,
				ToIssueID:   peer.ID,
				Type:        "related",
				Author:      "tester",
			}, db.LinkEventParams{
				EventType:    "issue.linked",
				EventIssueID: issue.ID,
				FromShortID:  issue.ShortID,
				FromUID:      issue.UID,
				ToShortID:    peer.ShortID,
				ToUID:        peer.UID,
				Actor:        "tester",
			})
			return err
		},
		"close": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, issue, _ db.Issue) error {
			_, _, _, err := d.CloseIssue(ctx, issue.ID, "done", "tester", "", nil)
			return err
		},
		"soft delete": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, issue, _ db.Issue) error {
			_, _, _, err := d.SoftDeleteIssue(ctx, issue.ID, "tester")
			return err
		},
		"issue metadata": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, issue, _ db.Issue) error {
			_, err := d.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
				IssueID:    issue.ID,
				IfMatchRev: issue.Revision,
				Actor:      "tester",
				Patch: map[string]json.RawMessage{
					"definitely_not_a_key": json.RawMessage(`"value"`),
				},
			})
			return err
		},
		"import batch": func(ctx context.Context, d *sqlitestore.Store, p db.Project, _, _ db.Issue) error {
			ts := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
			_, _, err := d.ImportBatch(ctx, db.ImportBatchParams{
				ProjectID: p.ID,
				Source:    "beads",
				Actor:     "tester",
				Items: []db.ImportItem{{
					ExternalID: "a",
					Title:      "imported",
					Author:     "tester",
					Status:     "open",
					CreatedAt:  ts,
					UpdatedAt:  ts,
				}},
			})
			return err
		},
		"reopen": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, issue, _ db.Issue) error {
			_, _, _, err := d.ReopenIssue(ctx, issue.ID, "tester")
			return err
		},
		"project metadata": func(ctx context.Context, d *sqlitestore.Store, p db.Project, _ db.Issue, _ db.Issue) error {
			_, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
				ProjectID:  p.ID,
				IfMatchRev: p.Revision,
				Actor:      "tester",
				Patch: map[string]json.RawMessage{
					"definitely_not_a_key": json.RawMessage(`"value"`),
				},
			})
			return err
		},
	}

	for name, run := range cases {
		t.Run(name+"/push disabled rejects", func(t *testing.T) {
			d, ctx, p, issue, peer := setupFederatedWriteGateProject(t, name)
			upsertTestSpokeFederationBindingWithPush(ctx, t, d, p, true, false)

			err := run(ctx, d, p, issue, peer)

			require.Error(t, err)
			assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
		})
		t.Run(name+"/push enabled permits", func(t *testing.T) {
			d, ctx, p, issue, peer := setupFederatedWriteGateProject(t, name)
			upsertTestSpokeFederationBindingWithPush(ctx, t, d, p, true, true)

			err := run(ctx, d, p, issue, peer)

			require.NoError(t, err)
		})
	}

	t.Run("disabled binding does not block writes", func(t *testing.T) {
		d, ctx, p, _, _ := setupFederatedWriteGateProject(t, "create issue")
		upsertTestSpokeFederationBindingWithPush(ctx, t, d, p, false, false)

		_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID,
			Title:     "local work",
			Author:    "tester",
		})

		require.NoError(t, err)
	})

	t.Run("hub binding permits writes", func(t *testing.T) {
		d, ctx, p, _, _ := setupFederatedWriteGateProject(t, "create issue")
		upsertTestHubFederationBinding(ctx, t, d, p, true)

		_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID,
			Title:     "hub work",
			Author:    "tester",
		})

		require.NoError(t, err)
	})
}

func TestFederatedSpokeRejectsRecurrence(t *testing.T) {
	cases := map[string]func(context.Context, *sqlitestore.Store, db.Project, db.Recurrence) error{
		"create recurrence": func(ctx context.Context, d *sqlitestore.Store, p db.Project, _ db.Recurrence) error {
			_, err := d.CreateRecurrence(ctx, db.CreateRecurrenceIn{
				ProjectID: p.ID,
				Actor:     "tester",
				Rule:      "FREQ=DAILY;COUNT=1",
				DTStart:   "2026-05-23",
				Timezone:  "UTC",
				Template: db.RecurrenceTemplate{
					Title: "next",
				},
			})
			return err
		},
		"patch recurrence": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, rec db.Recurrence) error {
			title := "patched"
			_, err := d.PatchRecurrence(ctx, db.PatchRecurrenceIn{
				RecurrenceID: rec.ID,
				IfMatchRev:   rec.Revision,
				Actor:        "tester",
				Update:       db.RecurrenceUpdate{TemplateTitle: &title},
			})
			return err
		},
		"delete recurrence": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, rec db.Recurrence) error {
			return d.SoftDeleteRecurrence(ctx, rec.ID, "tester")
		},
		"materialize recurrence": func(ctx context.Context, d *sqlitestore.Store, _ db.Project, rec db.Recurrence) error {
			_, err := d.MaterializeNext(ctx, rec.ID, "", "tester")
			return err
		},
	}

	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			d, ctx, p := setupTestProject(t)
			var rec db.Recurrence
			var err error
			if name != "create recurrence" {
				rec, err = d.CreateRecurrence(ctx, db.CreateRecurrenceIn{
					ProjectID: p.ID,
					Actor:     "tester",
					Rule:      "FREQ=DAILY;COUNT=1",
					DTStart:   "2026-05-23",
					Timezone:  "UTC",
					Template: db.RecurrenceTemplate{
						Title: "template",
					},
				})
				require.NoError(t, err)
			}
			upsertTestSpokeFederationBindingWithPush(ctx, t, d, p, true, true)

			err = run(ctx, d, p, rec)

			require.Error(t, err)
			assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
			assert.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported)
		})
	}
}

func TestFederatedMoveUnsupported(t *testing.T) {
	t.Run("source federated", func(t *testing.T) {
		d, ctx, source := setupTestProject(t)
		target, err := d.CreateProject(ctx, "target")
		require.NoError(t, err)
		issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: source.ID,
			Title:     "move me",
			Author:    "tester",
		})
		require.NoError(t, err)
		upsertTestHubFederationBinding(ctx, t, d, source, true)

		_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID:       issue.ID,
			FromProjectID: source.ID,
			ToProjectID:   target.ID,
			IfMatchRev:    issue.Revision,
			Actor:         "tester",
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
		assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
	})

	t.Run("target federated", func(t *testing.T) {
		d, ctx, source := setupTestProject(t)
		target, err := d.CreateProject(ctx, "target")
		require.NoError(t, err)
		issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: source.ID,
			Title:     "move me",
			Author:    "tester",
		})
		require.NoError(t, err)
		upsertTestSpokeFederationBindingWithPush(ctx, t, d, target, true, true)

		_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID:       issue.ID,
			FromProjectID: source.ID,
			ToProjectID:   target.ID,
			IfMatchRev:    issue.Revision,
			Actor:         "tester",
		})

		require.Error(t, err)
		assert.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
		assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
	})
}

func TestFederatedSpokeRejectsHardPurge(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "purge me",
		Author:    "tester",
	})
	require.NoError(t, err)
	upsertTestSpokeFederationBindingWithPush(ctx, t, d, p, true, true)

	_, err = d.PurgeIssue(ctx, issue.ID, "tester", nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
	assert.ErrorIs(t, err, db.ErrFederatedSpokeUnsupported)
}

type federationSnapshotPayload struct {
	UID          string          `json:"uid"`
	ShortID      string          `json:"short_id"`
	Title        string          `json:"title"`
	Body         string          `json:"body"`
	Author       string          `json:"author"`
	Owner        *string         `json:"owner"`
	Priority     *int64          `json:"priority"`
	Status       string          `json:"status"`
	ClosedReason *string         `json:"closed_reason"`
	ClosedAt     *string         `json:"closed_at"`
	DeletedAt    *string         `json:"deleted_at"`
	Metadata     json.RawMessage `json:"metadata"`
	Labels       []string        `json:"labels"`
	Links        []struct {
		Type       string `json:"type"`
		ToIssueUID string `json:"to_issue_uid"`
	} `json:"links"`
	Comments []struct {
		CommentUID string `json:"comment_uid"`
		Author     string `json:"author"`
		Body       string `json:"body"`
		CreatedAt  string `json:"created_at"`
	} `json:"comments"`
}

func newTestUID(t *testing.T) string {
	t.Helper()
	v, err := katauid.New()
	require.NoError(t, err)
	return v
}

func shortID(uid string) string {
	return strings.ToLower(uid[len(uid)-4:])
}

func upsertTestHubFederationBinding(ctx context.Context, t *testing.T, d *sqlitestore.Store, p db.Project, enabled bool) {
	t.Helper()
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:     p.ID,
		Role:          db.FederationRoleHub,
		HubProjectUID: p.UID,
		Enabled:       enabled,
	})
	require.NoError(t, err)
}

func upsertTestSpokeFederationBinding(ctx context.Context, t *testing.T, d *sqlitestore.Store, p db.Project, enabled bool) {
	t.Helper()
	upsertTestSpokeFederationBindingWithPush(ctx, t, d, p, enabled, false)
}

func upsertTestSpokeFederationBindingWithPush(
	ctx context.Context, t *testing.T, d *sqlitestore.Store, p db.Project, enabled, pushEnabled bool,
) {
	t.Helper()
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        newTestUID(t),
		ReplayHorizonEventID: 1,
		PushEnabled:          pushEnabled,
		Enabled:              enabled,
	})
	require.NoError(t, err)
}

func setupFederatedWriteGateProject(
	t *testing.T, opName string,
) (*sqlitestore.Store, context.Context, db.Project, db.Issue, db.Issue) {
	t.Helper()
	d, ctx, p := setupTestProject(t)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "replicated",
		Author:    "tester",
	})
	require.NoError(t, err)
	if opName == "reopen" {
		issue, _, _, err = d.CloseIssue(ctx, issue.ID, "done", "tester", "", nil)
		require.NoError(t, err)
	}
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "peer",
		Author:    "tester",
	})
	require.NoError(t, err)
	return d, ctx, p, issue, peer
}

func setupFederationIngestHub(t *testing.T) (*sqlitestore.Store, context.Context, db.Project, string) {
	t.Helper()
	d, ctx, p := setupTestProject(t)
	_, err := d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)
	return d, ctx, p, newTestUID(t)
}

func setupIngestClaimIssue(t *testing.T) (*sqlitestore.Store, context.Context, db.Project, string, db.Issue, db.Issue) {
	t.Helper()
	d, ctx, p, spokeUID := setupFederationIngestHub(t)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "claim ingest target",
		Author:    "tester",
	})
	require.NoError(t, err)
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "claim ingest peer",
		Author:    "tester",
	})
	require.NoError(t, err)
	return d, ctx, p, spokeUID, issue, peer
}

func ingestParams(projectID int64, spokeUID string, ev db.RemoteEvent) db.FederationIngestParams {
	return db.FederationIngestParams{
		ProjectID:        projectID,
		SpokeInstanceUID: spokeUID,
		Events: []db.FederationIngestEvent{{
			SourceEventID: 1,
			Event:         ev,
		}},
	}
}

func remoteClaimWorkEvent(
	t *testing.T,
	project db.Project,
	spokeUID string,
	issueUID string,
	relatedIssueUID *string,
	eventType string,
	actor string,
) db.RemoteEvent {
	t.Helper()
	payload := remoteClaimWorkPayload(issueUID, relatedIssueUID, eventType)
	var envelopeIssueUID *string
	if eventType != "project.metadata_updated" {
		envelopeIssueUID = &issueUID
	}
	ev := remoteEvent(t, project.UID, project.Name, envelopeIssueUID, relatedIssueUID,
		eventType, actor, time.Now().UnixMilli(), payload)
	ev.OriginInstanceUID = spokeUID
	ev.ContentHash = remoteEventHash(t, ev)
	return ev
}

func remoteClaimWorkPayload(issueUID string, relatedIssueUID *string, eventType string) string {
	switch eventType {
	case "issue.created", "issue.snapshot":
		return `{"uid":"` + issueUID + `","short_id":"` + shortID(issueUID) + `","title":"remote","body":"","author":"remote-agent","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`
	case "issue.updated":
		return `{"issue_uid":"` + issueUID + `","title":"remote"}`
	case "issue.assigned":
		return `{"issue_uid":"` + issueUID + `","owner":"remote-agent"}`
	case "issue.unassigned":
		return `{"issue_uid":"` + issueUID + `","owner":null}`
	case "issue.priority_set":
		return `{"issue_uid":"` + issueUID + `","priority":1}`
	case "issue.priority_cleared":
		return `{"issue_uid":"` + issueUID + `","old_priority":1}`
	case "issue.closed":
		return `{"issue_uid":"` + issueUID + `","reason":"done","closed_at":"2026-05-23T12:00:00.000Z"}`
	case "issue.reopened":
		return `{"issue_uid":"` + issueUID + `","reopened_at":"2026-05-23T12:00:00.000Z"}`
	case "issue.soft_deleted":
		return `{"issue_uid":"` + issueUID + `","deleted_at":"2026-05-23T12:00:00.000Z"}`
	case "issue.restored":
		return `{"issue_uid":"` + issueUID + `","restored_at":"2026-05-23T12:00:00.000Z"}`
	case "issue.commented":
		return `{"issue_uid":"` + issueUID + `","comment_uid":"01HZNQ7VFPK1XGD8R5MABCD4EZ","author":"remote-agent","body":"comment","created_at":"2026-05-23T12:00:00.000Z"}`
	case "issue.labeled", "issue.unlabeled":
		return `{"issue_uid":"` + issueUID + `","label":"area:db"}`
	case "issue.linked", "issue.unlinked":
		related := ""
		if relatedIssueUID != nil {
			related = *relatedIssueUID
		}
		return `{"issue_uid":"` + issueUID + `","from_uid":"` + issueUID + `","to_uid":"` + related + `","type":"related"}`
	case "issue.links_changed":
		related := ""
		if relatedIssueUID != nil {
			related = *relatedIssueUID
		}
		return `{"issue_uid":"` + issueUID + `","related_added_uids":["` + related + `"]}`
	case "issue.metadata_updated":
		return `{"issue_uid":"` + issueUID + `","diff":{"area":{"from":null,"to":"db"}}}`
	case "project.metadata_updated":
		return `{"diff":{"area":{"from":null,"to":"db"}}}`
	default:
		return `{"issue_uid":"` + issueUID + `"}`
	}
}

func ingestIssueCreatedEvent(
	t *testing.T,
	projectUID, projectName, spokeUID, issueUID string,
	hlcPhysicalMS int64,
) db.RemoteEvent {
	t.Helper()
	payload := `{"uid":"` + issueUID + `","short_id":"` + shortID(issueUID) + `","title":"spoke work","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`
	return ingestEventWithPayload(t, projectUID, projectName, spokeUID, &issueUID, nil,
		"issue.created", hlcPhysicalMS, payload)
}

func ingestEventWithPayload(
	t *testing.T,
	projectUID, projectName, spokeUID string,
	issueUID, relatedIssueUID *string,
	eventType string,
	hlcPhysicalMS int64,
	payload string,
) db.RemoteEvent {
	t.Helper()
	ev := remoteEvent(t, projectUID, projectName, issueUID, relatedIssueUID,
		eventType, "spoke", hlcPhysicalMS, payload)
	ev.OriginInstanceUID = spokeUID
	ev.ContentHash = remoteEventHash(t, ev)
	return ev
}

func assertIngestedEventCount(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, want int) {
	t.Helper()
	var got int
	require.NoError(t, d.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM events
		 WHERE project_id = ?
		   AND type != 'project.federation_enabled'`, projectID).Scan(&got))
	assert.Equal(t, want, got)
}

func remoteEvent(
	t *testing.T,
	projectUID, projectName string,
	issueUID, relatedIssueUID *string,
	eventType, actor string,
	hlcPhysicalMS int64,
	payload string,
) db.RemoteEvent {
	t.Helper()
	eventUID := newTestUID(t)
	originUID := newTestUID(t)
	ev := db.RemoteEvent{
		EventUID:          eventUID,
		OriginInstanceUID: originUID,
		ProjectUID:        projectUID,
		ProjectName:       projectName,
		IssueUID:          issueUID,
		RelatedIssueUID:   relatedIssueUID,
		Type:              eventType,
		Actor:             actor,
		Payload:           json.RawMessage(payload),
		HLCPhysicalMS:     hlcPhysicalMS,
		HLCCounter:        0,
		CreatedAt:         time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}
	ev.ContentHash = remoteEventHash(t, ev)
	return ev
}

func remoteEventHash(t *testing.T, ev db.RemoteEvent) string {
	t.Helper()
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Payload:           ev.Payload,
	})
	require.NoError(t, err)
	return hash
}

func TestCountActiveFederationEnrollments(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	got, err := d.CountActiveFederationEnrollments(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), got, "no enrollments yet")

	_, err = d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "project-scoped-token",
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &p.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)
	_, err = d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "global-token",
		SpokeInstanceUID: newTestUID(t),
		Capabilities:     "pull",
	})
	require.NoError(t, err)
	revoked, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "revoked-token",
		SpokeInstanceUID: newTestUID(t),
		ProjectID:        &p.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)
	require.NoError(t, d.RevokeFederationEnrollment(ctx, revoked.Enrollment.ID))

	got, err = d.CountActiveFederationEnrollments(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got, "active project + active global counted; revoked excluded")
}
