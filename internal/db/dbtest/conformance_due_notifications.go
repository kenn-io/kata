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

func checkScheduledReached(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "scheduled-reached-project")
	require.NoError(t, err)
	owner := "worker-a"
	manual := `{"from":"coordinator","message":"Please review"}`
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Start scheduled work", Author: "coordinator", Owner: &owner,
		Metadata: map[string]jsontext.Value{
			"scheduled_on":                  jsontext.Value(`"2026-09-16T09:00"`),
			notification.MetadataKey(owner): jsontext.Value(manual),
		},
	})
	require.NoError(t, err)
	var floor time.Time
	reconcile := func(issueID int64, now time.Time) db.ReconcileDueNotificationOut {
		t.Helper()
		out, err := store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
			IssueID: issueID, Now: now, DefaultTimezone: "UTC", ScheduledReachedFloor: &floor,
		})
		require.NoError(t, err)
		return out
	}

	out := reconcile(issue.ID, time.Date(2026, 9, 16, 8, 59, 0, 0, time.UTC))
	assert.False(t, out.Changed)
	assert.Nil(t, out.Reached, "a future date is not reached")

	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	out = reconcile(issue.ID, now)
	require.True(t, out.Changed)
	assert.Nil(t, out.Event, "an occupied inbox slot suppresses only the notification")
	require.NotNil(t, out.Reached)
	assert.Equal(t, "issue.scheduled_reached", out.Reached.Type)
	assert.Equal(t, "system", out.Reached.Actor)
	require.NotNil(t, out.Reached.IssueUID)
	assert.Equal(t, issue.UID, *out.Reached.IssueUID)
	assert.JSONEq(t,
		`{"field":"scheduled_on","value":"2026-09-16T09:00","timezone":"UTC","instant":"2026-09-16T09:00:00Z"}`,
		out.Reached.Payload)
	stored, err := store.EventsByUIDs(ctx, project.ID, []string{out.Reached.UID})
	require.NoError(t, err)
	require.Len(t, stored, 1)
	got, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, issue.Revision, got.Revision, "reaching a date does not edit the issue")

	out = reconcile(issue.ID, now.Add(time.Hour))
	assert.False(t, out.Changed)
	assert.Nil(t, out.Reached, "a reached value is recorded once")

	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "coordinator",
		Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-09-16"`)},
	})
	require.NoError(t, err)
	out = reconcile(issue.ID, now)
	require.NotNil(t, out.Reached, "a rescheduled value is reached again")
	assert.Contains(t, out.Reached.Payload, `"value":"2026-09-16"`)

	invalid, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Unaddressable scheduled work", Author: "bad\nactor",
		Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-09-16"`)},
	})
	require.NoError(t, err)
	out, err = store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
		IssueID: invalid.ID, Now: now, DefaultTimezone: "UTC", ScheduledReachedFloor: &floor,
	})
	require.NoError(t, err)
	require.Error(t, out.NotificationErr)
	require.NotNil(t, out.Reached, "an invalid inbox recipient does not block the reached event")
	assert.Nil(t, out.Event)
	out = reconcile(invalid.ID, now)
	assert.Nil(t, out.Reached, "the reached event is still recorded once")

	closed, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Closed scheduled work", Author: "coordinator",
		Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-09-16"`)},
	})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, closed.ID, "done", "coordinator", "", nil)
	require.NoError(t, err)
	out = reconcile(closed.ID, now)
	assert.False(t, out.Changed)
	assert.Nil(t, out.Reached, "a closed issue is not reached")
	return nil
}

// checkScheduledReachedFloor pins that the first sweep's floor persists and
// that only schedules reached or set after it record issue.scheduled_reached.
// Floors are taken from stored event times, so no wall-clock ordering is assumed.
func checkScheduledReachedFloor(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "scheduled-floor-project")
	require.NoError(t, err)
	reconcile := func(issueID int64, now time.Time, floor *time.Time) db.ReconcileDueNotificationOut {
		t.Helper()
		out, err := store.ReconcileDueNotification(ctx, db.ReconcileDueNotificationIn{
			IssueID: issueID, Now: now, DefaultTimezone: "UTC", ScheduledReachedFloor: floor,
		})
		require.NoError(t, err)
		return out
	}

	historic, created, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Overdue before upgrade", Author: "coordinator",
		Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2020-01-01"`)},
	})
	require.NoError(t, err)
	first := created.CreatedAt.Add(time.Millisecond)
	floor, err := store.EnsureScheduledReachedFloor(ctx, first)
	require.NoError(t, err)
	assert.True(t, first.Equal(floor), "the first sweep sets the floor")
	again, err := store.EnsureScheduledReachedFloor(ctx, first.Add(time.Hour))
	require.NoError(t, err)
	assert.True(t, first.Equal(again), "a later sweep keeps the stored floor")

	now := floor.Add(time.Minute)
	out := reconcile(historic.ID, now, &floor)
	assert.Nil(t, out.Reached, "a schedule overdue before the floor stays silent")
	assert.True(t, out.Changed, "the inbox request is still delivered")
	require.NotNil(t, out.Event)

	patched, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: historic.ID, Actor: "coordinator",
		Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2020-01-02"`)},
	})
	require.NoError(t, err)
	editFloor := patched.Event.CreatedAt
	out = reconcile(historic.ID, now, nil)
	assert.Nil(t, out.Reached, "a sweep without a floor records no reached schedule")
	out = reconcile(historic.ID, now, &editFloor)
	require.NotNil(t, out.Reached, "a past date set at or after the floor is reached")
	assert.Contains(t, out.Reached.Payload, `"value":"2020-01-02"`)

	upcoming, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Scheduled after upgrade", Author: "coordinator",
		Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2099-01-01"`)},
	})
	require.NoError(t, err)
	out = reconcile(upcoming.ID, time.Date(2099, 1, 1, 0, 1, 0, 0, time.UTC), &floor)
	require.NotNil(t, out.Reached, "a date reached after the floor is recorded")

	behaviorNow := time.Now().UTC().Add(24 * time.Hour)
	snapshotProject, err := store.CreateProject(ctx, "scheduled-snapshot-project")
	require.NoError(t, err)
	snapshotIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: snapshotProject.ID, Title: "Schedule copied by federation", Author: "coordinator",
	})
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: snapshotIssue.ID, Actor: "coordinator",
		Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2020-02-01"`)},
	})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, snapshotProject.ID, "coordinator")
	require.NoError(t, err)
	out = reconcile(snapshotIssue.ID, behaviorNow, &floor)
	require.NotNil(t, out.Reached, "a federation snapshot that copies the current schedule preserves its post-floor assignment")
	assert.Contains(t, out.Reached.Payload, `"value":"2020-02-01"`)

	delayedProject, err := store.CreateProject(ctx, "scheduled-delayed-edit-project")
	require.NoError(t, err)
	delayedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: delayedProject.ID, Title: "Schedule with delayed history", Author: "coordinator",
	})
	require.NoError(t, err)
	olderEdit, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: delayedIssue.ID, Actor: "coordinator",
		Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2020-02-02"`)},
	})
	require.NoError(t, err)
	newerEdit, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: delayedIssue.ID, Actor: "coordinator",
		Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2020-02-03"`)},
	})
	require.NoError(t, err)
	delayed := newRemoteEvent(t, delayedProject, &delayedIssue.UID, "issue.metadata_updated", "peer-agent",
		"00000000000000000000000077", olderEdit.Event.HLCPhysicalMS, jsontext.Value(olderEdit.Event.Payload))
	delayed.HLCCounter = olderEdit.Event.HLCCounter
	delayed.CreatedAt = olderEdit.Event.CreatedAt
	delayed.ContentHash = remoteEventHash(t, delayed)
	require.True(t,
		olderEdit.Event.HLCPhysicalMS < newerEdit.Event.HLCPhysicalMS ||
			(olderEdit.Event.HLCPhysicalMS == newerEdit.Event.HLCPhysicalMS && olderEdit.Event.HLCCounter < newerEdit.Event.HLCCounter),
		"local metadata edits must have increasing HLC values",
	)
	inserted, err := store.InsertRemoteEvent(ctx, delayedProject.ID, delayed)
	require.NoError(t, err)
	require.True(t, inserted)
	out = reconcile(delayedIssue.ID, behaviorNow, &floor)
	require.NotNil(t, out.Reached, "a delayed older edit must not replace the newer HLC-winning assignment")
	assert.Contains(t, out.Reached.Payload, `"value":"2020-02-03"`)

	recurrenceProject, err := store.CreateProject(ctx, "scheduled-recurrence-project")
	require.NoError(t, err)
	recurrenceIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: recurrenceProject.ID, Title: "Schedule assigned by recurrence", Author: "coordinator",
	})
	require.NoError(t, err)
	_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{
		IssueID: recurrenceIssue.ID,
		Recurrence: db.CreateRecurrenceIn{
			ProjectID: recurrenceProject.ID, Actor: "coordinator", Rule: "FREQ=DAILY",
			DTStart: "2020-02-04", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "Scheduled follow-up"},
		},
	})
	require.NoError(t, err)
	out = reconcile(recurrenceIssue.ID, behaviorNow, &floor)
	require.NotNil(t, out.Reached, "attaching a recurrence records the reached event for its already-past first occurrence")
	assert.Contains(t, out.Reached.Payload, `"value":"2020-02-04"`)
	return nil
}
