package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func TestCronSpokeForwardRefusesProjectArchivedDuringPreflight(t *testing.T) {
	f := newCronForwardFixture(t, func(f *cronForwardFixture, cfg *daemon.ServerConfig) {
		cfg.Auth = config.AuthConfig{Token: "owner-token"}
		push := cfg.FederationPushPending
		cfg.FederationPushPending = func(ctx context.Context, b db.FederationBinding, c config.FederationCredential) error {
			if err := push(ctx, b, c); err != nil {
				return err
			}
			project, _, err := f.spoke.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.sp.ID, Actor: "tester", Force: true})
			require.NoError(t, err)
			require.NotNil(t, project.DeletedAt)
			return nil
		}
	})
	response, raw := envDoRaw(t, f.target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", f.sp.ID, cronHTTPUID(t)), map[string]any{"actor": "tester", "name": "Review", "definition": f.definition}, map[string]string{"Authorization": "Bearer owner-token"})
	jobs, err := f.hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: f.hp.ID})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode, "%s", raw)
	require.Contains(t, string(raw), `"code":"project_not_found"`)
	require.Zero(t, f.writes.Load(), "archived projects must refuse before sending a hub job mutation")
	require.Empty(t, jobs)
}

// A hub-side conversion of a replicated adapter job to native can leave the
// spoke projection stale until replication catches up. A lifecycle request
// carrying the hub's current revision must be forwarded so the hub's
// authoritative state decides, instead of conflicting locally against the
// stale adapter revision.
func TestCronSpokeForwardLifecycleAfterHubConversion(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	job, _, err := f.spoke.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: f.sp.ID, Name: "Inspection", Actor: "tester", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	// Flush the spoke's pending events to the hub without pulling, so the hub
	// holds the adapter job while the spoke keeps its projection.
	client, err := federation.NewClient(t.Context(), f.credential.HubURL, f.credential.Token, clientpkg.Opts{FederationSigning: f.credential.Signing})
	require.NoError(t, err)
	binding, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
	require.NoError(t, err)
	require.NoError(t, federation.PushDirectSpoke(t.Context(), f.spoke.DB, binding, f.hp.ID, client))
	pushed, err := f.hub.DB.CronJob(t.Context(), f.hp.ID, job.UID)
	require.NoError(t, err, "pushed adapter job must exist on the hub")
	hubParent, _, err := f.hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.hp.ID, Title: "Hub parent", Author: "tester"})
	require.NoError(t, err)
	native := f.definition
	native.Issue.ParentUID = hubParent.UID
	response, raw := envDoRaw(t, f.hub, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", f.hp.ID, job.UID), map[string]any{"actor": "tester", "name": "Review", "definition": native, "expected_event_uid": pushed.DefinitionEventUID}, nil)
	require.Equalf(t, http.StatusOK, response.StatusCode, "%s", raw)
	hubJob, err := f.hub.DB.CronJob(t.Context(), f.hp.ID, job.UID)
	require.NoError(t, err)
	require.NotEqual(t, hubJob.DefinitionEventUID, job.DefinitionEventUID, "hub conversion must advance the revision")

	response, raw = envDoRaw(t, f.target, http.MethodDelete, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s", f.sp.ID, job.UID), map[string]any{"actor": "tester", "expected_event_uid": hubJob.DefinitionEventUID}, nil)
	require.Positive(t, f.writes.Load(), "lifecycle must forward when the hub state is authoritative")
	require.Equalf(t, http.StatusOK, response.StatusCode, "%s", raw)
	jobs, err := f.hub.DB.ListCronJobs(t.Context(), db.CronList{ProjectID: f.hp.ID, IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.NotNil(t, jobs[0].DeletedAt)
}
