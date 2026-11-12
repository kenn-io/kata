package dbtest

import (
	"fmt"
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
		for _, incompleteOnly := range []bool{false, true} {
			name := fmt.Sprintf("example-%t-%t", sameUID, incompleteOnly)
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
				marker := true
				summary := cron.Summary{Version: 1}
				if incompleteOnly {
					summary.SessionsIncomplete = &marker
				} else {
					sessions := []cron.Session{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}}
					summary.Sessions = &sessions
				}
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
				params := db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: hubUID, Actor: "worker"}
				for _, features := range []string{"", db.CronEventFeature, db.CronRunSessionsEventFeature} {
					params.HubEventFeatures = features
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
				}
				params.HubEventFeatures = db.MergeEventFeatures(db.CronEventFeature, db.CronRunSessionsEventFeature)
				adopted, err := store.AdoptProjectIntoFederation(ctx, params)
				require.NoError(t, err)
				require.Equal(t, hubUID, adopted.Project.UID)
				events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
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
	}
	return nil
}
