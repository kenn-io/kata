package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayResetCarriesCronDefinitions exercises a root checkpoint for a project
// that owns cron definitions and run history. The root must be able to sign the
// checkpoint, and a fresh downstream that installs it must end up with the same
// job, workflow and run that the root holds.
func RunRelayResetCarriesCronDefinitions(t *testing.T, root db.Storage, openDownstream func(*testing.T) db.Storage) {
	resetter, ok := root.(db.RelayResetStore)
	require.True(t, ok, "native root checkpoint generation is required")
	ctx := t.Context()
	project, err := root.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = root.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, root.PinRootAuthority(ctx, pin))
	signer := db.RootAttributionSigner{AuthorityUID: root.InstanceUID(), PrivateKey: private}
	jobDefinition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review open work"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := root.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Nightly review", Definition: jobDefinition, Actor: "worker"})
	require.NoError(t, err)
	workflowDefinition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}
	workflow, _, err := root.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Release checks", Definition: workflowDefinition, Actor: "worker"})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	occurrence := "manual:2026-10-06"
	observed, err := root.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := root.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "cron-reset-parent-test-token", Actor: "company-member", AdminActor: "admin"})
	require.NoError(t, err)

	downstream := openDownstream(t)
	grant, err := root.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: downstream.InstanceUID(), ProtocolVersion: db.RelayProtocolVersion, Token: "cron-reset-child-test-token"})
	require.NoError(t, err)
	bindingUID := grant.Enrollment.RelayBindingUID
	checkpoint, err := resetter.CreateRelayReset(ctx, bindingUID, signer)
	require.NoError(t, err, "a project with cron definitions must still produce a signed checkpoint")
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	decoded, _, err := db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
	require.NoError(t, err)
	kinds := map[string]int{}
	for _, event := range decoded {
		kinds[event.Type]++
	}
	require.Equal(t, 1, kinds["cron.job.snapshot"])
	require.Equal(t, 1, kinds["cron.workflow.snapshot"])
	require.Equal(t, 1, kinds["cron.run.snapshot"])

	installer, ok := downstream.(db.RelayResetStore)
	require.True(t, ok, "native atomic signed reset installation is required")
	replica, err := downstream.CreateProjectWithUID(ctx, "spoke-project", project.UID)
	require.NoError(t, err)
	require.NoError(t, downstream.PinRootAuthority(ctx, pin))
	_, err = downstream.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "company-member", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	_, err = downstream.SetRelayBindingConfig(ctx, replica.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: root.InstanceUID(), UpstreamInstanceUID: root.InstanceUID(), HubPath: []string{root.InstanceUID(), downstream.InstanceUID()}, LocalActor: "company-member", ResetEpoch: 1})
	require.NoError(t, err)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation))

	installedJob, err := downstream.CronJob(ctx, replica.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, "Nightly review", installedJob.Name)
	require.Equal(t, job.DefinitionEventUID, installedJob.DefinitionEventUID)
	require.Equal(t, jobDefinition, installedJob.Definition)
	installedWorkflow, err := downstream.CronWorkflow(ctx, replica.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, "Release checks", installedWorkflow.Name)
	require.Equal(t, workflow.DefinitionEventUID, installedWorkflow.DefinitionEventUID)
	installedRun, err := downstream.CronRun(ctx, replica.ID, runUID)
	require.NoError(t, err)
	require.Equal(t, observed.Run.Status, installedRun.Status)
	require.Equal(t, &job.UID, installedRun.JobUID)
	require.Equal(t, &occurrence, installedRun.OccurrenceKey)
	runs, err := downstream.ListCronRuns(ctx, db.CronRunList{ProjectID: replica.ID})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation), "lost commit response retries the exact retained checkpoint")
}
