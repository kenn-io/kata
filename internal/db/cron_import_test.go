package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestCronImportRejectsUnknownCronEventType(t *testing.T) {
	event := &db.EventExport{Type: "cron.example.unknown", UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", Payload: []byte(`{}`)}
	require.Error(t, db.ValidateImportRecords([]db.ImportRecord{event}))
}

func TestCronImportValidatesEventPayload(t *testing.T) {
	const projectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAD"
	const origin = "01ARZ3NDEKTSV4RRFFQ69G5FAE"
	job, definition := "01ARZ3NDEKTSV4RRFFQ69G5FAF", "01ARZ3NDEKTSV4RRFFQ69G5FAG"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	run := db.CronRun{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", JobUID: &job, DefinitionEventUID: &definition, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	payload, err := json.Marshal(db.NewCronRunObservation(run, projectUID))
	require.NoError(t, err)
	event := db.EventExport{ID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAH", ProjectID: 1, OriginInstanceUID: origin, Actor: "worker", Type: "cron.run.observed", Payload: payload, HLCPhysicalMS: at.UnixMilli(), CreatedAt: at.Format(db.EventTimestampFormat)}
	project := &db.ProjectExport{ID: 1, UID: projectUID, Name: "example-project"}
	require.NoError(t, db.ValidateImportRecords([]db.ImportRecord{project, &event}))
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(payload, &fields))
	fields["unknown_field"] = []byte(`1`)
	event.Payload, err = json.Marshal(fields)
	require.NoError(t, err)
	require.Error(t, db.ValidateImportRecords([]db.ImportRecord{project, &event}), "cron event payloads reject unknown fields")
	event.Payload = payload
	project.UID = "01ARZ3NDEKTSV4RRFFQ69G5FAJ"
	require.Error(t, db.ValidateImportRecords([]db.ImportRecord{project, &event}), "event project must match imported project")
}

func TestCronRunHistoryTimestampDomain(t *testing.T) {
	const project = "01ARZ3NDEKTSV4RRFFQ69G5FAD"
	job, definition := "01ARZ3NDEKTSV4RRFFQ69G5FAF", "01ARZ3NDEKTSV4RRFFQ69G5FAG"
	good := time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC)
	for _, at := range []time.Time{
		good,
		time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(0, 12, 31, 23, 59, 59, 0, time.UTC),
		time.Date(1, 1, 1, 0, 0, 1, 0, time.FixedZone("east", 3600)),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("west", -3600)),
	} {
		t.Run(at.Format(time.RFC3339), func(t *testing.T) {
			run := db.CronRun{ID: 1, ProjectID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", JobUID: &job, DefinitionEventUID: &definition, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: good}
			record := db.CronRunExport(run)
			importErr := db.ValidateCronRecord(&record)
			payload, err := json.Marshal(db.NewCronRunObservation(run, project))
			require.NoError(t, err)
			federationErr := db.ValidateCronFederationEvent(db.RemoteEvent{Type: "cron.run.observed", Actor: "worker", ProjectUID: project, EventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAH", OriginInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAE", HLCPhysicalMS: 1, Payload: payload})
			if at.Year() < 1 || at.UTC().Year() < 1 || at.UTC().Year() > 9999 {
				assert.Error(t, importErr)
				assert.Error(t, federationErr)
			} else {
				require.NoError(t, importErr)
				require.NoError(t, federationErr)
			}
		})
	}
}
