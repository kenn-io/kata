package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A pulled page is inserted in one transaction: each event reports whether it
// was new, a repeated page is all duplicates, and a page that contradicts a
// run's identity is refused as a whole.
func checkRemoteEventBatchInsert(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, source, project)
	occurrence := "daily:2026-10-06"
	for range 3 {
		runUID, err := uid.New()
		require.NoError(t, err)
		_, err = source.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
		require.NoError(t, err)
	}
	events, err := source.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	page := []db.RemoteEvent{}
	var lastRun db.Event
	for _, event := range events {
		if db.EventRequiredFeatures(event.Type) == "" {
			continue
		}
		page = append(page, portableNativeEvent(event))
		if event.Type == "cron.run.observed" {
			lastRun = event
		}
	}
	require.Len(t, page, 5, "two definitions and three runs")

	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	created, err := target.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	adopted, err := target.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: created.ID, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "worker", EmptyOnly: true})
	require.NoError(t, err)
	local := adopted.Project

	// A run observation that changes the run's occurrence contradicts its identity.
	var contradicted db.CronRunObservation
	require.NoError(t, json.Unmarshal([]byte(lastRun.Payload), &contradicted))
	other := "daily:2026-10-07"
	contradicted.OccurrenceKey = &other
	contradicted.Revision = 2
	raw, err := json.Marshal(contradicted)
	require.NoError(t, err)
	conflict := portableNativeEvent(lastRun)
	conflict.EventUID, err = uid.New()
	require.NoError(t, err)
	conflict.HLCPhysicalMS++
	conflict.Payload = jsontext.Value(raw)
	resignNativeEvent(t, &conflict)
	_, err = target.InsertRemoteEvents(ctx, local.ID, append(append([]db.RemoteEvent{}, page...), conflict))
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	stored, err := target.EventsAfter(ctx, db.EventsAfterParams{ProjectID: local.ID, Limit: 100})
	require.NoError(t, err)
	for _, event := range stored {
		require.Empty(t, db.EventRequiredFeatures(event.Type), "a refused page inserts nothing")
	}

	inserted, err := target.InsertRemoteEvents(ctx, local.ID, page)
	require.NoError(t, err)
	require.Equal(t, []bool{true, true, true, true, true}, inserted)
	again, err := target.InsertRemoteEvents(ctx, local.ID, page)
	require.NoError(t, err)
	require.Equal(t, []bool{false, false, false, false, false}, again)
	require.NoError(t, target.MaterializeFederatedProject(ctx, local.ID))
	runs, err := target.ListCronRuns(ctx, db.CronRunList{ProjectID: local.ID})
	require.NoError(t, err)
	require.Len(t, runs, 3)
	return nil
}
