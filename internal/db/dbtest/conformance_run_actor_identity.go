package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A run's actor is the immutable identity of whoever first observed it. A
// spoke that enables push substitutes its bound actor as the attribution of
// later observations, but that must not turn an in-flight run into an
// identity conflict.
func checkRunActorSurvivesPushEnablement(t *testing.T, spoke db.Storage) error {
	ctx := t.Context()
	local, err := spoke.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitionsAs(t, spoke, local, "requester")
	runUID, err := uid.New()
	require.NoError(t, err)
	started, err := spoke.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: local.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "requester", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	require.Equal(t, "requester", started.Run.Actor)

	hubUID, err := uid.New()
	require.NoError(t, err)
	adopted, err := spoke.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: hubUID, Actor: "operator"})
	require.NoError(t, err)
	_, err = spoke.EnableFederationPush(ctx, local.ID, adopted.Binding.PushCursorEventID)
	require.NoError(t, err)

	finished, err := spoke.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: local.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "requester", Status: "succeeded", Summary: cron.Summary{Version: 1}, ExpectedRevision: started.Run.Revision})
	require.NoError(t, err, "the requesting actor still owns its in-flight run")
	require.Equal(t, "requester", finished.Run.Actor, "the run keeps the actor that started it")
	require.Len(t, finished.Events, 1)
	require.Equal(t, "operator", finished.Events[0].Actor, "the observation is attributed to the bound actor")

	_, err = spoke.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: local.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "someone-else", Status: "failed", Summary: cron.Summary{Version: 1}, ExpectedRevision: finished.Run.Revision})
	require.ErrorIs(t, err, db.ErrCronConflict, "an actor that neither started the run nor is substituted for its starter cannot update it")

	fresh, err := uid.New()
	require.NoError(t, err)
	created, err := spoke.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: local.ID, UID: fresh, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "requester", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	require.Equal(t, "operator", created.Run.Actor, "a run started under push belongs to the bound actor")
	return nil
}

// A hub accepts a pushed observation for a run it already holds under the same
// actor, even when that actor is not the pushing spoke's bound actor. It still
// refuses a run the spoke tries to start or claim under another actor.
func checkHubAcceptsKnownRunActor(t *testing.T, hub db.Storage) error {
	ctx := t.Context()
	project, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	job, _ := nativeFederationDefinitionsAs(t, hub, project, "requester")
	runUID, err := uid.New()
	require.NoError(t, err)
	started, err := hub.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "requester", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	spokeUID, err := uid.New()
	require.NoError(t, err)
	push := func(run db.CronRun, sourceEventID int64) error {
		payload, err := json.Marshal(db.NewCronRunObservation(run, project.UID))
		require.NoError(t, err)
		eventUID, err := uid.New()
		require.NoError(t, err)
		at := run.UpdatedAt.Add(time.Minute)
		event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: spokeUID, ProjectUID: project.UID, ProjectName: project.Name, Type: "cron.run.observed", Actor: "worker", CreatedAt: at, HLCPhysicalMS: at.UnixMilli(), Payload: jsontext.Value(payload)}
		resignNativeEvent(t, &event)
		_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: project.ID, SpokeInstanceUID: spokeUID, BoundActor: "worker", Events: []db.FederationIngestEvent{{SourceEventID: sourceEventID, Event: event}}})
		return err
	}

	update := started.Run
	update.Status = "succeeded"
	update.Revision = 2
	update.UpdatedAt = started.Run.UpdatedAt.Add(time.Minute)
	require.NoError(t, push(update, 1), "an update keeps the run's original actor")
	stored, err := hub.CronRun(ctx, project.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", stored.Status)
	require.Equal(t, "requester", stored.Actor)

	claimed := update
	claimed.Actor = "someone-else"
	claimed.Revision = 3
	require.ErrorIs(t, push(claimed, 2), db.ErrFederationIngestValidation, "a spoke cannot reattribute a run")

	foreign := started.Run
	foreign.UID, err = uid.New()
	require.NoError(t, err)
	require.ErrorIs(t, push(foreign, 3), db.ErrFederationIngestValidation, "a new run must carry the bound actor")
	return nil
}
