package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
)

func TestPlanDueNotificationRoutesAndOrdersExistingInboxValues(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		metadata  string
		owner     string
		author    string
		wantKey   string
		wantValue string
	}{
		{
			name: "owner receives scheduled date before deadline",
			metadata: `{"scheduled_on":"2026-09-16T09:00",` +
				`"deadline_on":"2026-09-15"}`,
			owner:     "worker-a",
			author:    "coordinator",
			wantKey:   notification.MetadataKey("worker-a"),
			wantValue: `{"from":"system","message":"Scheduled date reached: 2026-09-16T09:00"}`,
		},
		{
			name:      "author receives unowned deadline",
			metadata:  `{"deadline_on":"2026-09-15"}`,
			author:    "coordinator",
			wantKey:   notification.MetadataKey("coordinator"),
			wantValue: `{"from":"system","message":"Deadline reached: 2026-09-15"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			patch, err := PlanDueNotification(json.RawMessage(test.metadata), test.owner, test.author, now, "UTC", "UTC", nil)
			require.NoError(t, err)
			require.Len(t, patch, 1)
			assert.JSONEq(t, test.wantValue, string(patch[test.wantKey]))
		})
	}
}

func TestPlanDueNotificationPreservesManualSlotAndCleansOnlyStaleAutomaticValues(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	key := notification.MetadataKey("worker-a")
	manual := `{"from":"coordinator","message":"Please review"}`
	patch, err := PlanDueNotification(
		json.RawMessage(`{"scheduled_on":"2026-09-16","`+key+`":`+manual+`}`),
		"worker-a", "coordinator", now, "UTC", "UTC", nil,
	)
	require.NoError(t, err)
	assert.Empty(t, patch)

	oldKey := notification.MetadataKey("worker-old")
	patch, err = PlanDueNotification(
		json.RawMessage(`{"scheduled_on":"2026-09-16","`+oldKey+`":`+
			`{"from":"system","message":"Scheduled date reached: 2026-09-16"}}`),
		"worker-a", "coordinator", now, "UTC", "UTC", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage("null"), patch[oldKey])
	assert.JSONEq(t,
		`{"from":"system","message":"Scheduled date reached: 2026-09-16"}`,
		string(patch[key]),
	)
}

func TestPlanDueNotificationTreatsWrittenAndClearedValuesAsDelivered(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	key := notification.MetadataKey("worker-a")
	value := `{"from":"system","message":"Scheduled date reached: 2026-09-16"}`
	history := []Event{
		{Type: "issue.metadata_updated", Payload: `{"diff":{"` + key + `":{"from":` + value + `,"to":null}},"revision_new":3}`},
	}
	patch, err := PlanDueNotification(
		json.RawMessage(`{"scheduled_on":"2026-09-16"}`),
		"worker-a", "coordinator", now, "UTC", "UTC", history,
	)
	require.NoError(t, err)
	assert.Empty(t, patch)

	history = []Event{
		{Type: "issue.snapshot", Payload: `{"metadata":{"scheduled_on":"2026-09-16","` + key + `":` + value + `}}`},
	}
	patch, err = PlanDueNotification(
		json.RawMessage(`{"scheduled_on":"2026-09-16"}`),
		"worker-a", "coordinator", now, "UTC", "UTC", history,
	)
	require.NoError(t, err)
	assert.Empty(t, patch)
}

func TestPlanDueNotificationRearmsChangedValueAndRejectsInvalidRecipient(t *testing.T) {
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	key := notification.MetadataKey("worker-a")
	old := `{"from":"system","message":"Scheduled date reached: 2026-09-16"}`
	history := []Event{{Type: "issue.metadata_updated", Payload: `{"diff":{"` + key + `":{"from":null,"to":` + old + `}}}`}}

	patch, err := PlanDueNotification(
		json.RawMessage(`{"scheduled_on":"2026-09-17"}`),
		"worker-a", "coordinator", now, "UTC", "UTC", history,
	)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"from":"system","message":"Scheduled date reached: 2026-09-17"}`,
		string(patch[key]),
	)

	_, err = PlanDueNotification(json.RawMessage(`{"scheduled_on":"2026-09-17"}`), "bad\nowner", "coordinator", now, "UTC", "UTC", nil)
	require.Error(t, err)
}
