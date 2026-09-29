package db

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestFoldStatusIntentSurvivesOnlyEffectiveExplicitTransitions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		events     []FoldEvent
		state, uid string
	}{
		{"close survives closed snapshot", []FoldEvent{testEvent("issue.closed", 2, `{"reason":"done"}`), testEvent("issue.snapshot", 3, `{"uid":"issue-1","status":"closed","title":"Refreshed title"}`)}, "closed", "event-002"},
		{"reopen survives open update", []FoldEvent{testEvent("issue.reopened", 2, `{}`), testEvent("issue.updated", 3, `{"status":"open","body":"Updated body"}`)}, "open", "event-002"},
		{"opposite imported transition supersedes close", []FoldEvent{testEvent("issue.closed", 2, `{"reason":"done"}`), testEvent("issue.updated", 3, `{"status":"open"}`)}, "open", ""},
		{"later snapshot cannot resurrect superseded close", []FoldEvent{testEvent("issue.closed", 2, `{"reason":"done"}`), testEvent("issue.updated", 3, `{"status":"open"}`), testEvent("issue.snapshot", 4, `{"uid":"issue-1","status":"closed"}`)}, "closed", ""},
		{"delayed older close loses to reopen", []FoldEvent{testEvent("issue.reopened", 3, `{}`), testEvent("issue.closed", 2, `{"reason":"done"}`)}, "open", "event-003"},
		{"unrelated edit retains close", []FoldEvent{testEvent("issue.closed", 2, `{"reason":"done"}`), testEvent("issue.updated", 3, `{"title":"Edited title"}`)}, "closed", "event-002"},
		{"snapshot alone has no explicit authority", []FoldEvent{testEvent("issue.snapshot", 2, `{"uid":"issue-1","status":"closed"}`)}, "closed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := testEvent("issue.created", 1, `{"uid":"issue-1","status":"open","title":"Initial title"}`)
			got := FoldEvents(append([]FoldEvent{initial}, tc.events...)).Issues["issue-1"]
			require.Equal(t, tc.state, got.Status)
			require.Equal(t, tc.uid, got.StatusIntentUID)
		})
	}
}

func TestFoldStatusIntentMatchesTraceForGeneratedPermutations(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		requested := rapid.IntRange(1, int(^uint(0)>>1)).Draw(t, "event count")
		count := min(requested, 40)
		sharedTick := rapid.Int64Range(0, int64(^uint64(0)>>1)).Draw(t, "shared physical clock")
		tiedPhysical := rapid.Bool().Draw(t, "physical clocks tied")
		events := []FoldEvent{testEvent("issue.created", 0, `{"uid":"issue-1","status":"open"}`)}
		expectedState, expectedIntent := "open", ""
		for i := 1; i <= count; i++ {
			explicit := rapid.Bool().Draw(t, "explicit transition")
			closed := rapid.Bool().Draw(t, "closed")
			state := "open"
			if closed {
				state = "closed"
			}
			var event FoldEvent
			if explicit {
				kind := "issue.reopened"
				if closed {
					kind = "issue.closed"
				}
				event = testEvent(kind, int64(i), `{}`)
			} else {
				kind := "issue.updated"
				if rapid.Bool().Draw(t, "snapshot") {
					kind = "issue.snapshot"
				}
				event = testEvent(kind, int64(i), fmt.Sprintf(`{"uid":"issue-1","status":%q}`, state))
				if state != expectedState {
					expectedIntent = ""
				}
			}
			event.UID = fmt.Sprintf("intent-%d-%s", i, rapid.String().Draw(t, "event UID suffix"))
			event.OriginInstanceUID = "example-origin-" + rapid.String().Draw(t, "origin suffix")
			if tiedPhysical {
				event.HLCPhysicalMS = sharedTick
				event.HLCCounter = int64(i)
			} else {
				event.HLCCounter = rapid.Int64Range(0, int64(^uint64(0)>>1)).Draw(t, "counter")
			}
			if explicit {
				expectedIntent = event.UID
			}
			expectedState = state
			events = append(events, event)
		}
		// Any arrival permutation must produce the chronological model's state and
		// intent. All permutations are reachable, not only a reversal of the fixture.
		for i := len(events) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, "arrival swap")
			events[i], events[j] = events[j], events[i]
		}
		got := FoldEvents(events).Issues["issue-1"]
		require.Equal(t, expectedState, got.Status)
		require.Equal(t, expectedIntent, got.StatusIntentUID)
	})
}

func TestFoldStatusMetadataStaysOutOfSerialization(t *testing.T) {
	issue := FoldEvents([]FoldEvent{testEvent("issue.closed", 1, `{"reason":"done"}`)}).Issues["issue-1"]
	encoded, err := json.Marshal(issue)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(encoded, &fields))
	require.Equal(t, "closed", fields["Status"])
	require.NotContains(t, fields, "StatusIntentUID")
	require.NotContains(t, fields, "StatusClock")
}

func TestFoldStatusClockIncludesSameStateRestatements(t *testing.T) {
	events := []FoldEvent{
		testEvent("issue.created", 1, `{"uid":"issue-1","status":"open"}`),
		testEvent("issue.closed", 2, `{"reason":"done"}`),
		testEvent("issue.snapshot", 3, `{"uid":"issue-1","status":"closed"}`),
		testEvent("issue.updated", 4, `{"title":"Unrelated edit"}`),
	}
	got := FoldEvents(events).Issues["issue-1"]
	require.Equal(t, "event-003", got.StatusClock.EventUID)
	require.Equal(t, "event-002", got.StatusIntentUID)
}
