package federation_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
)

// Contract: a direct spoke rebuilds from the retained run set even when the
// root stopped after prune commit and before its immediate baseline refresh.
func TestCronPruneDirectSpokeReset(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, push := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/push=%t", backend, push), func(t *testing.T) {
				ctx := t.Context()
				hub, spoke := nativeStore(t, backend), nativeStore(t, backend)
				httpHub := nativeHTTP(t, hub)
				project, err := hub.CreateProject(ctx, "hub-project")
				require.NoError(t, err)
				job, _ := nativeJob(t, hub, project)
				q := hub.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				})
				for i := range 3 {
					id, err := uid.New()
					require.NoError(t, err)

					require.NoError(t, err)
					at := time.Now().Add(-72*time.Hour + time.Duration(i)*time.Second).UTC().Format(db.CronRunCreatedAtFormat)
					_, err = q.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,definition_event_uid,actor,status,created_at,updated_at) VALUES($1,$2,$3,$4,'worker','succeeded',$5,$5)`, id, project.ID, job.UID, job.DefinitionEventUID, at)
					require.NoError(t, err)
				}
				rootBinding, err := hub.EnableProjectFederation(ctx, project.ID, "worker")
				require.NoError(t, err)
				capabilities := "pull"
				if push {
					capabilities = "pull,push"
				}
				grant, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &project.ID, SpokeInstanceUID: spoke.InstanceUID(), Capabilities: capabilities, Actor: "worker"})
				require.NoError(t, err)
				replica, err := spoke.CreateProjectWithUID(ctx, "spoke-project", project.UID)
				require.NoError(t, err)
				binding, err := spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: httpHub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: rootBinding.ReplayHorizonEventID, Enabled: true, PushEnabled: push, Actor: "worker"})
				require.NoError(t, err)
				credential := config.FederationCredential{HubURL: httpHub.URL, HubProjectID: project.ID, Token: grant.Token}
				require.NoError(t, federation.SyncFederationOnce(ctx, spoke, binding, credential))
				runs, err := spoke.ListCronRuns(ctx, db.CronRunList{ProjectID: replica.ID})
				require.NoError(t, err)
				require.Len(t, runs, 3)
				pruned, err := hub.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: project.ID, Actor: "worker", Before: time.Now().Add(-48 * time.Hour), Keep: 1})
				require.NoError(t, err)
				require.Equal(t, 2, pruned.Pruned)
				// Deliberately omit RefreshProjectFederationBaseline: metadata must recover.
				binding, err = spoke.FederationBindingByProject(ctx, replica.ID)
				require.NoError(t, err)
				require.NoError(t, federation.SyncFederationOnce(ctx, spoke, binding, credential))
				got, err := spoke.ListCronRuns(ctx, db.CronRunList{ProjectID: replica.ID})
				require.NoError(t, err)
				want, err := hub.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID})
				require.NoError(t, err)
				require.Len(t, got, 1)
				require.Equal(t, want[0].UID, got[0].UID)
			})
		}
	}
}
