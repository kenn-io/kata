package federation_test

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
)

// Contract: a pulled observation retains its payload through the daemon event
// publisher on the pull-only path used by FederationPullOnce. This supplies
// publisher.Events explicitly; it does not test daemon callback installation,
// background Runner wiring, or fork admission.
func TestCronPulledObservationPayloadPublishing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hub := nativeStore(t, "sqlite")
	spoke := nativeStore(t, "sqlite")
	server := nativeHTTP(t, hub)
	project, err := hub.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	job, _ := nativeJob(t, hub, project)
	root, err := hub.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	grant, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &project.ID, Capabilities: "pull", Actor: "operator"})
	require.NoError(t, err)
	local, err := spoke.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	adopted, err := spoke.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: server.URL, HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: root.ReplayHorizonEventID, Actor: "operator", EmptyOnly: true})
	require.NoError(t, err)
	credential := config.FederationCredential{HubURL: server.URL, HubProjectID: project.ID, Token: grant.Token}
	require.NoError(t, federation.SyncFederationOnce(ctx, spoke, adopted.Binding, credential))
	runUID, err := uid.New()
	require.NoError(t, err)
	ended := time.Now().UTC()
	observed, err := hub.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "operator", Status: "failed", EndedAt: &ended, Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	broadcaster := daemon.NewEventBroadcaster()
	publisher := daemon.NewEventPublisher(broadcaster, nil)
	stream := broadcaster.Subscribe(daemon.SubFilter{ProjectID: local.ID})
	defer stream.Unsub()
	pull := func() {
		binding, err := spoke.FederationBindingByProject(ctx, local.ID)
		require.NoError(t, err)
		require.NoError(t, federation.SyncFederationPullOnlyOnceWithPulledEvents(ctx, spoke, binding, credential, clientpkg.Opts{}, publisher.Events))
	}
	pull()
	select {
	case msg := <-stream.Ch:
		require.NotNil(t, msg.Event)
		require.Equal(t, "cron.run.observed", msg.Event.Type)
		var payload struct {
			UID        string    `json:"uid"`
			JobUID     string    `json:"job_uid"`
			UpdatedAt  time.Time `json:"updated_at"`
			Status     string    `json:"status"`
			ProjectUID string    `json:"project_uid"`
		}
		require.NoError(t, json.Unmarshal([]byte(msg.Event.Payload), &payload))
		require.Equal(t, runUID, payload.UID)
		require.Equal(t, "failed", payload.Status)
		require.Equal(t, project.UID, payload.ProjectUID)
		require.Equal(t, job.UID, payload.JobUID)
		require.Equal(t, observed.Run.UpdatedAt, payload.UpdatedAt)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
