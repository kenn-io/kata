package db_test

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestCronImportRunIdentity(t *testing.T) {
	const projectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAD"
	const origin = "01ARZ3NDEKTSV4RRFFQ69G5FAE"
	job, definition, occurrence := "01ARZ3NDEKTSV4RRFFQ69G5FAF", "01ARZ3NDEKTSV4RRFFQ69G5FAG", "same-occurrence"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	original := db.CronRun{ID: 1, ProjectID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", JobUID: &job, DefinitionEventUID: &definition, OccurrenceKey: &occurrence, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	eventFor := func(run db.CronRun, id int64, uid string) *db.EventExport {
		identity := projectUID
		if run.ProjectID == 2 {
			identity = "01ARZ3NDEKTSV4RRFFQ69G5FAM"
		}
		payload, err := json.Marshal(db.NewCronRunObservation(run, identity))
		require.NoError(t, err)
		return &db.EventExport{ID: id, UID: uid, ProjectID: run.ProjectID, OriginInstanceUID: origin, Actor: run.Actor, Type: "cron.run.observed", Payload: payload, HLCPhysicalMS: at.UnixMilli() + id, CreatedAt: at.Format(db.EventTimestampFormat)}
	}
	project := &db.ProjectExport{ID: 1, UID: projectUID, Name: "example-project"}
	otherProject := &db.ProjectExport{ID: 2, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAM", Name: "another-project"}
	row := db.CronRunExport(original)
	first := eventFor(original, 1, "01ARZ3NDEKTSV4RRFFQ69G5FAH")
	require.NoError(t, db.ValidateImportRecords([]db.ImportRecord{first, &row, project}), "out-of-order identical identity and compacted definition are valid")
	for _, tc := range []struct {
		name   string
		change func(*db.CronRun)
	}{
		{"actor", func(r *db.CronRun) { r.Actor = "another-worker" }},
		{"creation", func(r *db.CronRun) { r.CreatedAt = r.CreatedAt.Add(-time.Second) }},
		{"project", func(r *db.CronRun) { r.ProjectID = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := original
			tc.change(&changed)
			altered := db.CronRunExport(changed)
			require.Error(t, db.ValidateImportRecords([]db.ImportRecord{project, otherProject, &altered, first}), "row/event identity conflict")
			require.Error(t, db.ValidateImportRecords([]db.ImportRecord{project, otherProject, &row, &altered}), "row/row identity conflict")
			second := eventFor(changed, 2, "01ARZ3NDEKTSV4RRFFQ69G5FAJ")
			require.Error(t, db.ValidateImportRecords([]db.ImportRecord{project, otherProject, second, first}), "event/event identity conflict")
		})
	}
	independent := original
	independent.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAK"
	independent.Actor = "another-worker"
	require.NoError(t, db.ValidateImportRecords([]db.ImportRecord{project, &row, first, eventFor(independent, 2, "01ARZ3NDEKTSV4RRFFQ69G5FAJ")}), "same occurrence permits independent run UIDs")
	later := original
	later.Status = "succeeded"
	later.Revision = 2
	later.UpdatedAt = at.Add(time.Second)
	later.EndedAt = &later.UpdatedAt
	require.NoError(t, db.ValidateImportRecords([]db.ImportRecord{project, eventFor(later, 2, "01ARZ3NDEKTSV4RRFFQ69G5FAJ"), first, &row}), "mutable evidence may progress out of input order")
}
