package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

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

func TestCronImportRejectsUnknownReservedWorkflowDocument(t *testing.T) {
	const projectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAD"
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(db.CronDefinitionEvent{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAF", ProjectUID: projectUID, Name: "Example", Author: "worker", CreatedAt: at, UpdatedAt: at, Definition: jsontext.Value(`{"version":1,"steps":[{"kind":"argv","argv":["true"]}]}`)})
	require.NoError(t, err)
	event := &db.EventExport{ID: 1, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAH", ProjectID: 1, OriginInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAE", Actor: "worker", Type: "cron.workflow.updated", Payload: payload, HLCPhysicalMS: at.UnixMilli(), CreatedAt: at.Format(db.EventTimestampFormat)}
	project := &db.ProjectExport{ID: 1, UID: projectUID, Name: "example-project"}
	require.Error(t, db.ValidateImportRecords([]db.ImportRecord{project, event}), "restore must not preserve an undecodable later event beside a runnable older projection")
}
