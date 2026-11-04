package federation_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/uid"
)

// Both HTTP transports must quarantine a denied definition/observation and
// deliver the same retained source after the owner grants its hop actor access.
func TestCronAccessQuarantineRecovery(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, transport := range []string{"legacy", "relay"} {
			t.Run(backend+"/"+transport, func(t *testing.T) {
				ctx := t.Context()
				root := newRecoveryRoot(t, backend)
				personal := newRelayMatrixNode(t, backend, "runtime")
				if transport == "relay" {
					enrollRelayMatrixReplica(t, root, personal, "spoke-project", true)
				} else {
					project, err := personal.store.CreateProjectWithUID(ctx, "spoke-project", root.project.UID)
					require.NoError(t, err)
					personal.project = project
					_, err = personal.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: root.http.URL, HubProjectID: root.project.ID, HubProjectUID: root.project.UID, ReplayHorizonEventID: 1, Enabled: true, PushEnabled: true, Actor: personal.account})
					require.NoError(t, err)
					grant, err := root.store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ProjectID: &root.project.ID, SpokeInstanceUID: personal.store.InstanceUID(), Actor: personal.account, Capabilities: "pull,push"})
					require.NoError(t, err)
					personal.credential = config.FederationCredential{HubURL: root.http.URL, HubProjectID: root.project.ID, Token: grant.Token, Actor: grant.Enrollment.Actor, Capabilities: "pull,push"}
				}
				// Complete initial synchronization before creating the denied data.
				syncRelayMatrixNode(t, personal)
				policy, err := root.store.CronAccessPolicy(ctx, root.project.UID)
				require.NoError(t, err)
				policy.Define = db.CronGrant{Mode: "teams"}
				policy.Observe = db.CronGrant{Mode: "teams"}
				policy, _, err = root.store.SetCronAccessPolicy(ctx, policy, "owner")
				require.NoError(t, err)
				workflow, _, err := personal.store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: personal.project.ID, Name: "Example workflow", Actor: personal.account, Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}})
				require.NoError(t, err)
				team, _, err := root.store.CreateTeam(ctx, "runtimes", "owner")
				require.NoError(t, err)
				_, err = root.store.SetTeamMembership(ctx, team.UID, personal.credential.Actor, true, "owner")
				require.NoError(t, err)
				retry := func(grant string) {
					t.Helper()
					binding, err := personal.store.FederationBindingByProject(ctx, personal.project.ID)
					require.NoError(t, err)
					require.ErrorContains(t, federation.SyncFederationOnce(ctx, personal.store, binding, personal.credential), "cron_permission_denied")
					quarantines, err := personal.store.ActiveFederationQuarantinesByProject(ctx, personal.project.ID)
					require.NoError(t, err)
					require.Len(t, quarantines, 1)
					q := quarantines[0]
					require.Equal(t, db.FederationQuarantineDirectionPush, q.Direction)
					require.Contains(t, q.Error, "cron_permission_denied")
					if grant == "define" {
						policy.Define = db.CronGrant{Mode: "teams", TeamUIDs: []string{team.UID}}
					} else {
						policy.Observe = db.CronGrant{Mode: "teams", TeamUIDs: []string{team.UID}}
					}
					policy, _, err = root.store.SetCronAccessPolicy(ctx, policy, "owner")
					require.NoError(t, err)
					_, err = personal.store.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{ID: q.ID, ProjectID: personal.project.ID, Actor: "owner", Reason: "hop actor now has the required cron grant"})
					require.NoError(t, err)
					syncRelayMatrixNode(t, personal)
					remaining, err := personal.store.ActiveFederationQuarantinesByProject(ctx, personal.project.ID)
					require.NoError(t, err)
					require.Empty(t, remaining)
				}
				retry("define")
				mirrored, err := root.store.CronWorkflow(ctx, root.project.ID, workflow.UID)
				require.NoError(t, err)
				require.Equal(t, workflow.DefinitionEventUID, mirrored.DefinitionEventUID)
				runUID, err := uid.New()
				require.NoError(t, err)
				observed, err := personal.store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: personal.project.ID, UID: runUID, WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: personal.account, Status: "running", Summary: cron.Summary{Version: 1}})
				require.NoError(t, err)
				retry("observe")
				run, err := root.store.CronRun(ctx, root.project.ID, runUID)
				require.NoError(t, err)
				require.Equal(t, observed.Run.Actor, run.Actor)
				require.Equal(t, observed.Run.UID, run.UID)
				require.Equal(t, observed.Run.Revision, run.Revision)
				require.Equal(t, observed.Run.WorkflowDefinitionEventUID, run.WorkflowDefinitionEventUID)
			})
		}
	}
}
