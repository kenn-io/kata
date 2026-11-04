package db

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

func versionTestEvent(t require.TestingT, index int, kind string) FoldEvent {
	raw, err := json.Marshal(CronDefinitionEvent{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Name: fmt.Sprintf("Version %d", index), Author: "operator", CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC(), Definition: []byte(`{"version":1,"steps":[{"key":"inspect","kind":"command","command":"true"}]}`)})
	require.NoError(t, err)
	if kind == "job" {
		var body CronDefinitionEvent
		require.NoError(t, json.Unmarshal(raw, &body))
		body.Definition = []byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`)
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	return FoldEvent{UID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", index), OriginInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAC", Actor: "operator", Type: "cron." + kind + ".updated", HLCPhysicalMS: 1, Payload: raw}
}

func TestCronDefinitionVersionsFoldOrderProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{"job", "workflow"}))
		order := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, math.MaxInt)))
		if len(order) > 12 {
			order = order[:12]
		}
		events := []FoldEvent{}
		for i := 0; i < len(order)+1; i++ {
			event := versionTestEvent(ht, i, kind)
			event.HLCPhysicalMS = hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
			event.HLCCounter = hegel.Draw(ht, hegel.Integers(int64(0), int64(math.MaxInt64)))
			event.OriginInstanceUID = hegel.Draw(ht, hegel.SampledFrom([]string{"01ARZ3NDEKTSV4RRFFQ69G5FAC", "01ARZ3NDEKTSV4RRFFQ69G5FAD"}))
			events = append(events, event)
		}
		projection := FoldEvents(events)
		winner := projection.CronWorkflows["01ARZ3NDEKTSV4RRFFQ69G5FAA"].DefinitionEventUID
		if kind == "job" {
			winner = projection.CronJobs["01ARZ3NDEKTSV4RRFFQ69G5FAA"].DefinitionEventUID
		}
		versions, skipped := cronVersionCandidates(events, winner)
		require.Empty(ht, skipped)
		require.Len(ht, versions, len(events))
		require.Equal(ht, winner, versions[0].EventUID)
		require.True(ht, versions[0].Current)
		slices.Reverse(events)
		reversed, _ := cronVersionCandidates(events, winner)
		require.Equal(ht, versions, reversed)
	}, hegel.WithTestCases(1000))
}

func TestCronDefinitionVersionsSnapshotPrecedence(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		original := versionTestEvent(t, 1, kind)
		var body CronDefinitionEvent
		require.NoError(t, json.Unmarshal(original.Payload, &body))
		body.DefinitionEventUID = original.UID
		body.DefinitionHLC = &cron.HLC{Version: 1, PhysicalMS: 1, OriginInstanceUID: original.OriginInstanceUID}
		early := original
		early.Type = "cron." + kind + ".snapshot"
		early.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
		early.HLCPhysicalMS = 2
		early.Payload, _ = json.Marshal(body)
		late := early
		late.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
		late.HLCPhysicalMS = 3
		body.Name = "Later snapshot"
		late.Payload, _ = json.Marshal(body)
		for _, events := range [][]FoldEvent{{late, early}, {early, late}, {late, original, early}} {
			versions, skipped := cronVersionCandidates(events, original.UID)
			require.Empty(t, skipped)
			require.Len(t, versions, 1)
			require.Equal(t, "Version 1", versions[0].Name)
			require.True(t, versions[0].Current)
			if len(events) == 3 {
				require.Equal(t, "updated", versions[0].Operation)
				require.Equal(t, "operator", *versions[0].Actor)
			} else {
				require.Equal(t, "baseline", versions[0].Operation)
				require.Nil(t, versions[0].Actor)
			}
		}
	}
}

func TestCronDefinitionVersionsClockTieBreakers(t *testing.T) {
	for field := range 4 {
		old, next := versionTestEvent(t, 1, "job"), versionTestEvent(t, 2, "job")
		switch field {
		case 0:
			next.HLCPhysicalMS++
		case 1:
			next.HLCCounter++
		case 2:
			next.OriginInstanceUID = "01ARZ3NDEKTSV4RRFFQ69G5FAD"
		}
		versions, _ := cronVersionCandidates([]FoldEvent{next, old}, next.UID)
		require.Equal(t, next.UID, versions[0].EventUID)
	}
}

func TestCronDefinitionVersionsSkippedDiagnostics(t *testing.T) {
	events := []FoldEvent{}
	for i := range 25 {
		event := versionTestEvent(t, i, "job")
		var body CronDefinitionEvent
		require.NoError(t, json.Unmarshal(event.Payload, &body))
		body.Definition = []byte(`{"version":"bad"}`)
		event.Payload, _ = json.Marshal(body)
		events = append(events, event)
	}
	winner := events[0].UID
	versions, skipped := cronVersionCandidates(events, winner)
	require.NotNil(t, versions)
	require.Empty(t, versions)
	require.Len(t, skipped, 20)
	require.Equal(t, events[0].UID, skipped[0])
	require.Equal(t, events[19].UID, skipped[19])
	slices.Reverse(events)
	_, reversed := cronVersionCandidates(events, winner)
	require.Equal(t, skipped, reversed)
}

// Accepted snapshots can carry different valid clocks for the same named
// version. Use the fold's highest definition clock, then earliest envelope.
func TestCronDefinitionVersionsSnapshotClockAgreement(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		original := versionTestEvent(t, 1, kind)
		var body CronDefinitionEvent
		require.NoError(t, json.Unmarshal(original.Payload, &body))
		body.DefinitionEventUID = original.UID
		body.DefinitionHLC = &cron.HLC{Version: 1, PhysicalMS: 1, OriginInstanceUID: original.OriginInstanceUID}
		early := original
		early.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
		early.Type = "cron." + kind + ".snapshot"
		early.HLCPhysicalMS = 2
		early.Payload, _ = json.Marshal(body)
		late := early
		late.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
		late.HLCPhysicalMS = 4
		body.DefinitionHLC.PhysicalMS = 3
		body.Name = "Higher clock"
		late.Payload, _ = json.Marshal(body)
		for _, events := range [][]FoldEvent{{late, early}, {early, late}} {
			for _, event := range events {
				require.NoError(t, ValidateAcceptedCronEvent(RemoteEvent{EventUID: event.UID, ProjectUID: body.ProjectUID, OriginInstanceUID: event.OriginInstanceUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: time.Unix(1, 0).UTC(), Payload: event.Payload}))
			}
			projection := FoldEvents(events)
			var folded CronDefinition
			if kind == "job" {
				folded = projection.CronJobs[body.UID].CronDefinition
			} else {
				folded = projection.CronWorkflows[body.UID].CronDefinition
			}
			versions, _ := cronVersionCandidates(events, folded.DefinitionEventUID)
			require.Equal(t, folded.Name, versions[0].Name)
			require.Equal(t, folded.DefinitionHLC, versions[0].DefinitionHLC)
		}
	}
}

func TestCronDefinitionVersionsSnapshotPermutationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{"job", "workflow"}))
		original := versionTestEvent(ht, 1, kind)
		var body CronDefinitionEvent
		require.NoError(ht, json.Unmarshal(original.Payload, &body))
		body.DefinitionEventUID = original.UID
		original.HLCPhysicalMS = hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		other := versionTestEvent(ht, 2, kind)
		other.HLCPhysicalMS = hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		events := []FoldEvent{original, other}
		for i := range 3 {
			event := original
			event.Type = "cron." + kind + ".snapshot"
			event.UID = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", i+10)
			event.HLCPhysicalMS = hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
			body.Name = fmt.Sprintf("Snapshot %d", i)
			body.DefinitionHLC = &cron.HLC{Version: 1, PhysicalMS: hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64))), Counter: hegel.Draw(ht, hegel.Integers(int64(0), int64(math.MaxInt64))), OriginInstanceUID: original.OriginInstanceUID}
			if hegel.Draw(ht, hegel.Booleans()) {
				body.DefinitionHLC = &cron.HLC{Version: 1, PhysicalMS: original.HLCPhysicalMS, OriginInstanceUID: original.OriginInstanceUID}
			}
			event.Payload, _ = json.Marshal(body)
			events = append(events, event)
		}
		projection := FoldEvents(events)
		var expected CronDefinition
		if kind == "job" {
			expected = projection.CronJobs[body.UID].CronDefinition
		} else {
			expected = projection.CronWorkflows[body.UID].CronDefinition
		}
		versions, skipped := cronVersionCandidates(events, expected.DefinitionEventUID)
		require.Empty(ht, skipped)
		require.Len(ht, versions, 2)
		require.Equal(ht, expected.Name, versions[0].Name)
		require.Equal(ht, expected.DefinitionHLC, versions[0].DefinitionHLC)
		slices.Reverse(events)
		reversed, _ := cronVersionCandidates(events, expected.DefinitionEventUID)
		require.Equal(ht, versions, reversed)
	}, hegel.WithTestCases(1000))
}

func TestCronDefinitionVersionsMixedSnapshotCurrent(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		original, next := versionTestEvent(t, 1, kind), versionTestEvent(t, 2, kind)
		next.HLCPhysicalMS = 2
		var body CronDefinitionEvent
		require.NoError(t, json.Unmarshal(original.Payload, &body))
		body.DefinitionEventUID = original.UID
		body.Name = "Snapshot winner"
		body.DefinitionHLC = &cron.HLC{Version: 1, PhysicalMS: 3, OriginInstanceUID: original.OriginInstanceUID}
		snapshot := original
		snapshot.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
		snapshot.Type = "cron." + kind + ".snapshot"
		snapshot.HLCPhysicalMS = 4
		snapshot.Payload, _ = json.Marshal(body)
		require.NoError(t, ValidateAcceptedCronEvent(RemoteEvent{EventUID: snapshot.UID, ProjectUID: body.ProjectUID, OriginInstanceUID: snapshot.OriginInstanceUID, Type: snapshot.Type, Actor: snapshot.Actor, HLCPhysicalMS: snapshot.HLCPhysicalMS, CreatedAt: time.Unix(1, 0).UTC(), Payload: snapshot.Payload}))
		events := []FoldEvent{original, next, snapshot}
		projection := FoldEvents(events)
		var expected CronDefinition
		if kind == "job" {
			expected = projection.CronJobs[body.UID].CronDefinition
		} else {
			expected = projection.CronWorkflows[body.UID].CronDefinition
		}
		versions, _ := cronVersionCandidates(events, expected.DefinitionEventUID)
		require.Equal(t, expected.DefinitionEventUID, versions[0].EventUID)
		require.Equal(t, expected.Name, versions[0].Name)
		require.Equal(t, expected.DefinitionHLC, versions[0].DefinitionHLC)
		require.True(t, versions[0].Current)
		require.Equal(t, "baseline", versions[0].Operation)
		require.Nil(t, versions[0].Actor)
	}
}
