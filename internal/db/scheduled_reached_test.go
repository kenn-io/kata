package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanScheduledReachedResolvesDueDateOnce(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	metadata := json.RawMessage(`{"scheduled_on":"2026-09-16T09:00","timezone":"Europe/Berlin"}`)

	reached, err := PlanScheduledReached(metadata, now, time.Time{}, "UTC", nil)
	require.NoError(t, err)
	require.NotNil(t, reached)
	assert.Equal(t, IssuePlanningDate{
		Field: "scheduled_on", Value: "2026-09-16T09:00", Timezone: "Europe/Berlin",
		Instant: time.Date(2026, 9, 16, 7, 0, 0, 0, time.UTC),
	}, *reached)

	payload, err := json.Marshal(reached)
	require.NoError(t, err)
	history := []Event{{Type: "issue.scheduled_reached", Payload: string(payload)}}
	reached, err = PlanScheduledReached(metadata, now, time.Time{}, "UTC", history)
	require.NoError(t, err)
	assert.Nil(t, reached, "a value already reached is never reached again")
}

func TestPlanScheduledReachedSkipsFutureAbsentAndEarlierValues(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	earlier := []Event{{Type: "issue.scheduled_reached", Payload: `{"field":"scheduled_on","value":"2026-09-15","timezone":"UTC","instant":"2026-09-15T00:00:00Z"}`}}
	for _, test := range []struct {
		name, metadata string
		history        []Event
		want           string
	}{
		{name: "future", metadata: `{"scheduled_on":"2026-09-17"}`},
		{name: "absent", metadata: `{"deadline_on":"2026-09-15"}`},
		{name: "empty", metadata: ``},
		{name: "returned to a reached value", metadata: `{"scheduled_on":"2026-09-15"}`, history: earlier},
		{name: "moved to a new value", metadata: `{"scheduled_on":"2026-09-16"}`, history: earlier, want: "2026-09-16"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached, err := PlanScheduledReached(json.RawMessage(test.metadata), now, time.Time{}, "UTC", test.history)
			require.NoError(t, err)
			if test.want == "" {
				assert.Nil(t, reached)
				return
			}
			require.NotNil(t, reached)
			assert.Equal(t, test.want, reached.Value)
		})
	}
}

// Only schedules reached or set after the floor fire, so the first sweep after
// an upgrade does not replay every overdue schedule.
func TestPlanScheduledReachedSkipsSchedulesBeforeFloor(t *testing.T) {
	floor := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	before, after := floor.Add(-time.Hour), floor.Add(time.Hour)
	created := func(at time.Time, metadata string) Event {
		return Event{Type: "issue.created", Payload: `{"metadata":` + metadata + `}`, CreatedAt: at}
	}
	snapshot := func(at time.Time, metadata string) Event {
		return Event{Type: "issue.snapshot", Payload: `{"metadata":` + metadata + `}`, CreatedAt: at}
	}
	scheduled := func(at time.Time, from, to string) Event {
		return Event{Type: "issue.metadata_updated", Payload: `{"diff":{"scheduled_on":{"from":` + from + `,"to":` + to + `}}}`, CreatedAt: at}
	}
	for _, test := range []struct {
		name, metadata string
		history        []Event
		want           string
	}{
		{name: "reached before the floor", metadata: `{"scheduled_on":"2026-09-01"}`,
			history: []Event{created(before, `{"scheduled_on":"2026-09-01"}`)}},
		{name: "reached after the floor", metadata: `{"scheduled_on":"2026-09-12"}`,
			history: []Event{created(before, `{"scheduled_on":"2026-09-12"}`)}, want: "2026-09-12"},
		{name: "past date set after the floor", metadata: `{"scheduled_on":"2026-09-01"}`,
			history: []Event{created(before, `{}`), scheduled(after, `null`, `"2026-09-01"`)}, want: "2026-09-01"},
		{name: "past date created after the floor", metadata: `{"scheduled_on":"2026-09-01"}`,
			history: []Event{created(after, `{"scheduled_on":"2026-09-01"}`)}, want: "2026-09-01"},
		{name: "past date set before the floor", metadata: `{"scheduled_on":"2026-09-01"}`,
			history: []Event{created(before, `{}`), scheduled(before, `null`, `"2026-09-01"`)}},
		{name: "past date copied by a snapshot after the floor", metadata: `{"scheduled_on":"2026-09-01"}`,
			history: []Event{snapshot(after, `{"scheduled_on":"2026-09-01"}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached, err := PlanScheduledReached(json.RawMessage(test.metadata), now, floor, "UTC", test.history)
			require.NoError(t, err)
			if test.want == "" {
				assert.Nil(t, reached)
				return
			}
			require.NotNil(t, reached)
			assert.Equal(t, test.want, reached.Value)
		})
	}
}

func TestPlanScheduledReachedUsesOrderedScheduleProvenance(t *testing.T) {
	floor := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	now := floor.Add(time.Hour)
	event := func(uid, eventType string, physical int64, created time.Time, payload string) Event {
		return Event{
			UID: uid, OriginInstanceUID: "00000000000000000000000001",
			Type: eventType, HLCPhysicalMS: physical, CreatedAt: created, Payload: payload,
		}
	}

	t.Run("snapshot preserves a known assignment", func(t *testing.T) {
		history := []Event{
			event("created", "issue.created", 100, floor.Add(-time.Hour), `{"metadata":{}}`),
			event("edit", "issue.metadata_updated", 200, floor.Add(time.Minute), `{"diff":{"scheduled_on":{"from":null,"to":"2026-09-01"}}}`),
			event("snapshot", "issue.snapshot", 300, floor.Add(2*time.Minute), `{"metadata":{"scheduled_on":"2026-09-01"}}`),
		}

		reached, err := PlanScheduledReached(json.RawMessage(`{"scheduled_on":"2026-09-01"}`), now, floor, "UTC", history)
		require.NoError(t, err)
		require.NotNil(t, reached, "a snapshot that copies the current schedule must preserve its post-floor assignment time")
		assert.Equal(t, "2026-09-01", reached.Value)
	})

	t.Run("delayed older edit does not replace the HLC winner", func(t *testing.T) {
		for _, test := range []struct {
			name                                                     string
			newerPhysical, newerCounter, olderPhysical, olderCounter int64
			newerOrigin, newerUID, olderOrigin, olderUID             string
		}{
			{name: "physical", newerPhysical: 300, newerCounter: 1, olderPhysical: 200, olderCounter: 9,
				newerOrigin: "origin-a", newerUID: "newer", olderOrigin: "origin-z", olderUID: "older"},
			{name: "counter", newerPhysical: 300, newerCounter: 2, olderPhysical: 300, olderCounter: 1,
				newerOrigin: "origin-a", newerUID: "newer", olderOrigin: "origin-z", olderUID: "older"},
			{name: "origin", newerPhysical: 300, newerCounter: 1, olderPhysical: 300, olderCounter: 1,
				newerOrigin: "origin-z", newerUID: "newer", olderOrigin: "origin-a", olderUID: "older"},
			{name: "event uid", newerPhysical: 300, newerCounter: 1, olderPhysical: 300, olderCounter: 1,
				newerOrigin: "origin-a", newerUID: "z-newer", olderOrigin: "origin-a", olderUID: "a-older"},
		} {
			t.Run(test.name, func(t *testing.T) {
				newer := event(test.newerUID, "issue.metadata_updated", test.newerPhysical, floor.Add(time.Minute), `{"diff":{"scheduled_on":{"from":null,"to":"2026-09-01"}}}`)
				newer.HLCCounter, newer.OriginInstanceUID = test.newerCounter, test.newerOrigin
				older := event(test.olderUID, "issue.metadata_updated", test.olderPhysical, floor.Add(2*time.Minute), `{"diff":{"scheduled_on":{"from":null,"to":"2026-09-02"}}}`)
				older.HLCCounter, older.OriginInstanceUID = test.olderCounter, test.olderOrigin
				history := []Event{
					event("created", "issue.created", 100, floor.Add(-time.Hour), `{"metadata":{}}`),
					newer,
					older,
				}

				reached, err := PlanScheduledReached(json.RawMessage(`{"scheduled_on":"2026-09-01"}`), now, floor, "UTC", history)
				require.NoError(t, err)
				require.NotNil(t, reached, "the assignment selected by complete HLC ordering must be recognized as post-floor")
				assert.Equal(t, "2026-09-01", reached.Value)
			})
		}
	})

	t.Run("recurrence materialization assigns its occurrence date", func(t *testing.T) {
		history := []Event{
			event("created", "issue.created", 100, floor.Add(-time.Hour), `{"metadata":{}}`),
			event("materialized", "recurrence.materialized", 200, floor.Add(time.Minute), `{"occurrence_key":"2026-09-01"}`),
		}

		reached, err := PlanScheduledReached(json.RawMessage(`{"scheduled_on":"2026-09-01"}`), now, floor, "UTC", history)
		require.NoError(t, err)
		require.NotNil(t, reached, "a recurrence attached after the floor assigns its first occurrence after the floor")
		assert.Equal(t, "2026-09-01", reached.Value)
	})
}
