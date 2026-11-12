package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Session-bearing adoption requires all features of the emitted baseline.
// Refusal preserves the project and its evidence, so upgrading permits retry.
func checkAdoptionSessionFeatures(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, sameUID := range []bool{false, true} {
		name := fmt.Sprintf("example-%t", sameUID)
		t.Run(name, func(t *testing.T) {
			project, err := store.CreateProject(ctx, name)
			require.NoError(t, err)
			// A moved issue leaves an issue.moved event in discarded history;
			// the adoption baseline emits an issue.snapshot instead.
			source, err := store.CreateProject(ctx, name+"-source")
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Example issue", Author: "worker"})
			require.NoError(t, err)
			_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: project.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			job, _ := nativeFederationDefinitions(t, store, project)
			runUID, err := uid.New()
			require.NoError(t, err)
			sessions := []cron.Session{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}}
			summary := cron.Summary{Version: 1, Sessions: &sessions}
			_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: summary})
			require.NoError(t, err)
			project, err = store.ProjectByID(ctx, project.ID)
			require.NoError(t, err)
			before, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
			require.NoError(t, err)
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			hubUID := project.UID
			if !sameUID {
				hubUID, err = uid.New()
				require.NoError(t, err)
			}
			tx, err := store.(interface {
				BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
			}).BeginTx(ctx, nil)
			require.NoError(t, err)
			baseline, err := db.PrepareCronAdoption(ctx, tx, project, hubUID, false)
			require.NoError(t, err)
			require.NoError(t, tx.Rollback())
			params := db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: hubUID, Actor: "worker"}
			params.HubEventFeatures = db.CronEventFeature
			_, err = store.AdoptProjectIntoFederation(ctx, params)
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			current, err := store.ProjectByID(ctx, project.ID)
			require.NoError(t, err)
			require.Equal(t, project, current)
			run, err := store.CronRun(ctx, project.ID, runUID)
			require.NoError(t, err)
			require.Equal(t, summary, run.Summary)
			_, err = store.FederationBindingByProject(ctx, project.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
			after, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterCursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, afterCursor)
			params.HubEventFeatures = db.MergeEventFeatures(db.CronEventFeature, db.CronRunSessionsEventFeature)
			adopted, err := store.AdoptProjectIntoFederation(ctx, params)
			require.NoError(t, err)
			require.Equal(t, hubUID, adopted.Project.UID)
			events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
			require.NoError(t, err)
			var emitted []db.CronEvent
			for _, event := range events {
				if strings.HasPrefix(event.Type, "cron.") {
					emitted = append(emitted, db.CronEvent{Type: event.Type, Payload: event.Payload})
				}
			}
			var expected []db.CronEvent
			for _, event := range baseline {
				expected = append(expected, db.CronEvent{Type: event.Type, Payload: event.Payload})
			}
			require.Len(t, emitted, len(expected))
			for i := range expected {
				require.Equal(t, expected[i].Type, emitted[i].Type)
				require.JSONEq(t, expected[i].Payload, emitted[i].Payload)
			}
			required := ""
			for _, event := range events {
				features, err := db.PayloadRequiredFeatures(event.Type, event.Payload)
				require.NoError(t, err)
				required = db.MergeEventFeatures(required, features)
			}
			require.NoError(t, db.RequireEventFeatures(params.HubEventFeatures, required))
			require.Error(t, db.RequireEventFeatures(db.CronRunSessionsEventFeature, required))
		})
	}
	t.Run("session-free-prepared-baseline", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "prepared-baseline-example")
		require.NoError(t, err)
		job, _ := nativeFederationDefinitions(t, store, project)
		runUID, err := uid.New()
		require.NoError(t, err)
		summary := cron.Summary{Version: 1, Message: "sessions are described here but none were recorded"}
		_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: summary})
		require.NoError(t, err)
		hubUID, err := uid.New()
		require.NoError(t, err)
		tx, err := store.(interface {
			BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
		}).BeginTx(ctx, nil)
		require.NoError(t, err)
		baseline, err := db.PrepareCronAdoption(ctx, tx, project, hubUID, false)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		var prepared string
		for _, event := range baseline {
			if event.Type == "cron.run.snapshot" {
				prepared = event.Payload
			}
		}
		require.NotEmpty(t, prepared)
		require.Contains(t, prepared, "sessions are described")
		features, err := db.PayloadRequiredFeatures("cron.run.snapshot", prepared)
		require.NoError(t, err)
		require.NotContains(t, features, db.CronRunSessionsEventFeature)
		require.NoError(t, db.RequireAdoptionEventFeatures("", baseline))
		_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: hubUID, Actor: "worker"})
		require.NoError(t, err)
		events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
		require.NoError(t, err)
		var emitted string
		for _, event := range events {
			if event.Type == "cron.run.snapshot" {
				emitted = event.Payload
			}
		}
		require.NotEmpty(t, emitted)
		require.JSONEq(t, prepared, emitted, "the feature check applies to the emitted prepared record")
		features, err = db.PayloadRequiredFeatures("cron.run.snapshot", emitted)
		require.NoError(t, err)
		require.NotContains(t, features, db.CronRunSessionsEventFeature)
	})
	return nil
}
