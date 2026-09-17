package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/uid"
)

func checkDueNotificationTimezones(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "planning-timezone-project")
	require.NoError(t, err)
	for _, test := range []struct {
		name, field, value, recurrenceTimezone, issueTimezone, now string
		wantDue                                                    bool
	}{
		{"scheduled recurrence fallback", "scheduled_on", "2026-09-16T09:00", "America/Los_Angeles", "", "2026-09-16T10:00:00Z", false},
		{"deadline daemon fallback", "deadline_on", "2026-09-16T09:00", "America/Los_Angeles", "", "2026-09-16T10:00:00Z", true},
		{"deadline not early", "deadline_on", "2026-09-16T09:00", "Asia/Tokyo", "", "2026-09-16T08:00:00Z", false},
		{"deadline date daemon fallback", "deadline_on", "2026-09-16", "America/Los_Angeles", "", "2026-09-16T01:00:00Z", true},
		{"deadline date not early", "deadline_on", "2026-09-16", "Asia/Tokyo", "", "2026-09-15T23:00:00Z", false},
		{"deadline issue override", "deadline_on", "2026-09-16T09:00", "Asia/Tokyo", "America/Los_Angeles", "2026-09-16T10:00:00Z", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: test.name, Author: "worker-a",
			})
			require.NoError(t, err)
			_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{
				IssueID: issue.ID,
				Recurrence: db.CreateRecurrenceIn{
					ProjectID: project.ID, Actor: "worker-a", Rule: "FREQ=DAILY",
					DTStart: "2026-09-16", Timezone: test.recurrenceTimezone,
					Template: db.RecurrenceTemplate{Title: test.name},
				},
			})
			require.NoError(t, err)
			patch := map[string]jsontext.Value{
				"scheduled_on": jsontext.Value(`null`),
				"timezone":     jsontext.Value(`null`),
			}
			patch[test.field], err = json.Marshal(test.value)
			require.NoError(t, err)
			if test.issueTimezone != "" {
				patch["timezone"], err = json.Marshal(test.issueTimezone)
				require.NoError(t, err)
			}
			_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
				IssueID: issue.ID, Actor: "worker-a", Patch: patch,
			})
			require.NoError(t, err)
			now, err := time.Parse(time.RFC3339, test.now)
			require.NoError(t, err)
			_, err = store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
				IssueID: issue.ID, Now: now, DefaultTimezone: "UTC",
			})
			require.NoError(t, err)
			got, err := store.IssueByID(ctx, issue.ID)
			require.NoError(t, err)
			var values map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(got.Metadata), &values))
			_, notified := values[notification.MetadataKey("worker-a")]
			assert.Equal(t, test.wantDue, notified)
		})
	}
	return nil
}

func checkDueNotificationFederatedClear(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	hub, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, hub.ID, "operator")
	require.NoError(t, err)
	spokeUID, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	key := notification.MetadataKey("worker-a")
	value := `{"from":"system","message":"Scheduled date reached: 2026-09-16"}`
	created := newRemoteEvent(t, hub, &issueUID, "issue.created", "worker-a", spokeUID, 300,
		jsontext.Value(`{"uid":"`+issueUID+`","title":"Scheduled work","author":"worker-a","status":"open",`+
			`"metadata":{"scheduled_on":"2026-09-16","`+key+`":`+value+`},"created_at":"2026-09-16T00:00:00Z"}`))
	cleared := newRemoteEvent(t, hub, &issueUID, "issue.metadata_updated", "worker-a", spokeUID, 301,
		jsontext.Value(`{"diff":{"`+key+`":{"from":`+value+`,"to":null}},"revision_new":2,"updated_at":"2026-09-16T01:00:00Z"}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: hub.ID, SpokeInstanceUID: spokeUID, BoundActor: "worker-a",
		Events: []db.FederationIngestEvent{
			{SourceEventID: 1, Event: created},
			{SourceEventID: 2, Event: cleared},
		},
	})
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, hub.ID, []string{cleared.EventUID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Nil(t, events[0].IssueID)
	require.Equal(t, &issueUID, events[0].IssueUID)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &values))
	require.NotContains(t, values, key)
	_, err = store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
		IssueID: issue.ID, Now: time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC), DefaultTimezone: "UTC",
	})
	require.NoError(t, err)
	issue, err = store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &values))
	assert.NotContains(t, values, key, "a federated acknowledgment must keep the inbox clear")
	return nil
}

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
		Metadata: map[string]jsontext.Value{
			"scheduled_on": jsontext.Value(`"2026-09-16T09:00"`),
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
	var values map[string]jsontext.Value
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
		Metadata: map[string]jsontext.Value{
			"scheduled_on": jsontext.Value(`"2026-09-18"`),
			key: jsontext.Value(
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
