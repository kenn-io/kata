package dbtest

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Malformed run evidence is refused as invalid input before anything is
// stored, so a later read of the same run UID finds nothing.
func checkRunObservationValidation(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitions(t, store, project)
	started := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	ended := started.Add(-time.Second)
	longKey := strings.Repeat("k", 1025)
	longLabel := strings.Repeat("l", 257)
	emptySessions := []cron.Session{}
	for _, tc := range []struct {
		name   string
		mutate func(*db.ObserveCronRun)
	}{
		{name: "end before start", mutate: func(in *db.ObserveCronRun) { in.StartedAt, in.EndedAt = &started, &ended }},
		{name: "occurrence key over 1024 characters", mutate: func(in *db.ObserveCronRun) { in.OccurrenceKey = &longKey }},
		{name: "executor label over 256 bytes", mutate: func(in *db.ObserveCronRun) { in.ExecutorLabel = &longLabel }},
		{name: "teammate over 256 bytes", mutate: func(in *db.ObserveCronRun) { in.Teammate = &longLabel }},
		{name: "unknown status", mutate: func(in *db.ObserveCronRun) { in.Status = "paused" }},
		{name: "job without definition event", mutate: func(in *db.ObserveCronRun) { in.DefinitionEventUID = nil }},
		{name: "empty session slice", mutate: func(in *db.ObserveCronRun) { in.Summary.Sessions = &emptySessions }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runUID, err := uid.New()
			require.NoError(t, err)
			in := db.ObserveCronRun{
				ProjectID: project.ID, UID: runUID, JobUID: &job.UID,
				DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker",
				Status: "succeeded", Summary: cron.Summary{Version: 1},
			}
			_, err = store.ObserveCronRun(ctx, in)
			require.NoError(t, err, "the unmodified observation is valid")

			rejectedUID, err := uid.New()
			require.NoError(t, err)
			in.UID = rejectedUID
			tc.mutate(&in)
			_, err = store.ObserveCronRun(ctx, in)
			require.ErrorIs(t, err, cron.ErrInvalid)
			_, err = store.CronRun(ctx, project.ID, rejectedUID)
			require.ErrorIs(t, err, db.ErrNotFound, "a rejected observation stores nothing")
		})
	}
	session := []cron.Session{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}}
	incomplete := true
	runUID, err := uid.New()
	require.NoError(t, err)
	result, err := store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1, Sessions: &session, SessionsIncomplete: &incomplete}})
	require.NoError(t, err)
	require.Equal(t, &session, result.Run.Summary.Sessions)
	restored, err := store.CronRun(ctx, project.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, result.Run.Summary, restored.Summary)
	found := false
	for record, err := range store.ExportCronRuns(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		if record.UID == runUID {
			found = true
			require.Equal(t, result.Run.Summary, record.Summary)
		}
	}
	require.True(t, found)
	return nil
}
