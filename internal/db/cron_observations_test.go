package db_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestRunObservationFoldOrderingAndBaseline(t *testing.T) {
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAD"
	const origin = "01ARZ3NDEKTSV4RRFFQ69G5FAE"
	job, event, occurrence := "01ARZ3NDEKTSV4RRFFQ69G5FAF", "01ARZ3NDEKTSV4RRFFQ69G5FAG", "daily:2026-10-06"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	first := db.CronRun{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", JobUID: &job, DefinitionEventUID: &event, OccurrenceKey: &occurrence, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	makeEvent := func(run db.CronRun, eventUID string, physical int64) db.FoldEvent {
		raw, err := json.Marshal(db.NewCronRunObservation(run, project))
		require.NoError(t, err)
		return db.FoldEvent{UID: eventUID, ProjectUID: project, OriginInstanceUID: origin, Actor: "worker", Type: "cron.run.observed", HLCPhysicalMS: physical, Payload: raw}
	}
	old := makeEvent(first, "01ARZ3NDEKTSV4RRFFQ69G5FAH", 100)
	second := first
	second.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAB"
	independent := makeEvent(second, "01ARZ3NDEKTSV4RRFFQ69G5FAJ", 101)
	first.Status = "succeeded"
	first.Revision = 2
	first.UpdatedAt = at.Add(time.Minute)
	newest := makeEvent(first, "01ARZ3NDEKTSV4RRFFQ69G5FAK", 102)
	expected := db.FoldEvents([]db.FoldEvent{old, independent, newest}).CronRuns
	require.Len(t, expected, 2)
	require.Equal(t, "succeeded", expected[first.UID].Status)
	for _, order := range [][]db.FoldEvent{{old, newest, independent}, {newest, old, independent}, {newest, independent, old}, {independent, old, newest}, {independent, newest, old}, {newest, old, independent, newest}} {
		require.Equal(t, expected, db.FoldEvents(order).CronRuns)
	}
	payload := db.NewCronRunObservation(first, project)
	payload.ObservationEventUID = newest.UID
	clock := db.CronDefinitionHLC{Version: 1, PhysicalMS: 102, OriginInstanceUID: origin}
	payload.ObservationHLC = &clock
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	baseline := db.FoldEvent{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAM", ProjectUID: project, OriginInstanceUID: origin, Actor: "worker", Type: "cron.run.snapshot", HLCPhysicalMS: 10000, Payload: raw}
	require.Equal(t, expected, db.FoldEvents([]db.FoldEvent{baseline, independent, old, newest}).CronRuns, "baseline cannot invent newer evidence provenance")
}
