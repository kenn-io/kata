package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

func checkDueNotifications(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "due-notification-project")
	if err != nil {
		return fmt.Errorf("create due notification project: %w", err)
	}
	owner := "worker-a"
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "Start scheduled work",
		Author:    "coordinator",
		Owner:     &owner,
		Metadata: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-09-16T09:00"`),
		},
	})
	if err != nil {
		return fmt.Errorf("create due notification issue: %w", err)
	}

	ids, err := store.ListDueNotificationIssueIDs(ctx)
	if err != nil {
		return fmt.Errorf("list due notification candidates: %w", err)
	}
	assert.Contains(t, ids, issue.ID)

	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	out, err := store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
		IssueID: issue.ID, Now: now, DefaultTimezone: "UTC",
	})
	if err != nil {
		return fmt.Errorf("reconcile due notification: %w", err)
	}
	require.True(t, out.Changed)
	require.NotNil(t, out.Event)
	assert.Equal(t, "issue.metadata_updated", out.Event.Type)
	assert.Equal(t, "system", out.Event.Actor)

	got, err := store.IssueByID(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("read reconciled issue: %w", err)
	}
	var values map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(got.Metadata), &values))
	assert.JSONEq(t,
		`{"from":"system","message":"Scheduled date reached: 2026-09-16T09:00"}`,
		string(values[notification.MetadataKey(owner)]),
	)

	out, err = store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
		IssueID: issue.ID, Now: now, DefaultTimezone: "UTC",
	})
	if err != nil {
		return fmt.Errorf("repeat due notification reconciliation: %w", err)
	}
	assert.False(t, out.Changed)
	assert.Nil(t, out.Event)

	key := notification.MetadataKey("coordinator")
	stale, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Rescheduled work", Author: "coordinator",
		Metadata: map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-09-18"`),
			key: json.RawMessage(
				`{"from":"system","message":"Scheduled date reached: 2026-09-16"}`,
			),
		},
	})
	if err != nil {
		return fmt.Errorf("create stale due notification issue: %w", err)
	}
	out, err = store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
		IssueID:         stale.ID,
		Now:             time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC),
		DefaultTimezone: "UTC",
	})
	if err != nil {
		return fmt.Errorf("clear stale due notification: %w", err)
	}
	require.True(t, out.Changed)

	got, err = store.IssueByID(ctx, stale.ID)
	if err != nil {
		return fmt.Errorf("read stale due notification issue: %w", err)
	}
	values = nil
	require.NoError(t, json.Unmarshal([]byte(got.Metadata), &values))
	_, exists := values[key]
	assert.False(t, exists)
	return nil
}
