package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

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

	// Restore history from before these keys were reserved, then repair both
	// scopes through the guarded API. The immutable old event must survive reset.
	legacyPayload := jsontext.Value(`{"diff":{"cron.passover":{"from":null,"to":false},"cron.passover.` + job.UID + `":{"from":null,"to":false}}}`)
	legacy := newRemoteEvent(t, project, nil, "project.metadata_updated", "worker", root.InstanceUID(), time.Now().Add(-time.Second).UnixMilli(), legacyPayload)
	inserted, err := root.InsertRemoteEvent(ctx, project.ID, legacy)
	require.NoError(t, err)
	require.True(t, inserted)
	seedPassover(t, root, project.ID, map[string]jsontext.Value{"cron.passover": jsontext.Value(`false`), "cron.passover." + job.UID: jsontext.Value(`false`)})
	revision, err := root.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	_, err = root.SetCronProjectPause(db.WithRootAttribution(ctx, signer, "worker"), db.SetCronProjectPauseIn{ProjectID: project.ID, Actor: "worker", ExpectedRevision: &revision.Revision, DiscardMissed: true})
	require.NoError(t, err)
	resumed, err := root.ResumeCronJob(db.WithRootAttribution(ctx, signer, "worker"), db.ResumeCronJobIn{ProjectID: project.ID, UID: job.UID, Actor: "worker", DiscardMissed: true, ExpectedEventUID: job.DefinitionEventUID})
	require.NoError(t, err)
	job = resumed.Job
	jobDefinition = job.Definition
	policy, err := root.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	retainedPassover := db.CronPassoverFromMetadata(policy.Metadata, job.UID)
	require.NotNil(t, retainedPassover)
	workflowDefinition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}
	workflow, _, err := root.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Release checks", Definition: workflowDefinition, Actor: "worker"})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	occurrence := "manual:2026-10-06"
	observed, err := root.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	requestID, err := uid.New()
	require.NoError(t, err)
	request, err := root.RequestCron(ctx, db.RequestCronIn{ProjectID: project.ID, UID: requestID, Kind: "cancel", TargetUID: runUID, Actor: "worker", Now: time.Now().UTC()})
	require.NoError(t, err)
	require.NotNil(t, request.Event)
	rootProject, err := root.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	var rootMetadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(rootProject.Metadata), &rootMetadata))
	rootMetadata["cron.pause"] = jsontext.Value("null")
	seedPassover(t, root, project.ID, rootMetadata)
	rootProject, err = root.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	require.Nil(t, db.CronProjectPauseFromMetadata(rootProject.Metadata))
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

	foundLegacy := false
	for _, event := range decoded {
		if event.EventUID == legacy.EventUID {
			foundLegacy = true
			require.Equal(t, legacy.Payload, event.Payload)
			require.Equal(t, legacy.ContentHash, event.ContentHash)
		}
	}
	require.True(t, foundLegacy, "recovery retains immutable superseded legacy history")
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
	// A correctly signed checkpoint still has to satisfy the request-value
	// contract before any project state is replaced.
	beforeInvalid, err := downstream.ProjectByID(ctx, replica.ID)
	require.NoError(t, err)
	for _, key := range []string{"cron.pause", "cron.request." + requestID} {
		for _, entitySection := range []bool{false, true} {
			badSnapshot := checkpoint.Snapshot
			bodies := badSnapshot.Events
			if entitySection {
				bodies = badSnapshot.Entities
			}
			var sources [][]byte
			require.NoError(t, json.Unmarshal(bodies, &sources))
			raw, e := json.Marshal(map[string]any{"project_uid": project.UID, "diff": map[string]any{key: map[string]any{"from": nil, "to": false}}})
			require.NoError(t, e)
			event := newRemoteEvent(t, project, nil, "project.metadata_updated", "operator", root.InstanceUID(), time.Now().UnixMilli(), raw)
			body, e := db.EncodeRelaySourceEvent(event)
			require.NoError(t, e)
			bodies, e = json.Marshal(append(sources, body))
			require.NoError(t, e)
			if entitySection {
				badSnapshot.Entities = bodies
			} else {
				badSnapshot.Events = bodies
			}
			manifest, e := db.SignRootResetManifest(checkpoint.Manifest, badSnapshot, private)
			require.NoError(t, e)
			translation := checkpoint.Translation
			translation.SnapshotDigest = manifest.SnapshotDigest
			require.NoError(t, db.VerifyRootResetManifest(pin, manifest, badSnapshot))
			require.ErrorIs(t, installer.InstallRelayReset(ctx, bindingUID, manifest, badSnapshot, translation), db.ErrFederationIngestValidation, key)
			after, e := downstream.ProjectByID(ctx, replica.ID)
			require.NoError(t, e)
			require.Equal(t, beforeInvalid, after, "invalid controls cannot partially replace replica")
			_, e = downstream.CronJob(ctx, replica.ID, job.UID)
			require.ErrorIs(t, e, db.ErrNotFound)
		}
	}
	// A correctly signed malformed cutoff must fail atomically in either
	// retained source history or synthesized entity state.
	for _, entitySection := range []bool{false, true} {
		broken := checkpoint.Snapshot
		bodies := broken.Events
		if entitySection {
			bodies = broken.Entities
		}
		var sources [][]byte
		require.NoError(t, json.Unmarshal(bodies, &sources))
		changed := false
		for i, body := range sources {
			event, err := db.DecodeRelaySourceEvent(body)
			require.NoError(t, err)
			if event.Type != "project.metadata_updated" {
				continue
			}
			var payload map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			diff := make(map[string]map[string]jsontext.Value)
			require.NoError(t, json.Unmarshal(payload["diff"], &diff))
			key := "cron.passover." + job.UID
			if _, ok := diff[key]; !ok {
				continue
			}
			diff[key]["to"] = jsontext.Value(`false`)
			payload["diff"], err = json.Marshal(diff)
			require.NoError(t, err)
			event.Payload, err = json.Marshal(payload)
			require.NoError(t, err)
			resignNativeEvent(t, &event)
			sources[i], err = db.EncodeRelaySourceEvent(event)
			require.NoError(t, err)
			changed = true
		}
		require.True(t, changed, "checkpoint must contain passover in both sections")
		raw, err := json.Marshal(sources)
		require.NoError(t, err)
		if entitySection {
			broken.Entities = raw
		} else {
			broken.Events = raw
		}
		manifest, err := db.SignRootResetManifest(checkpoint.Manifest, broken, private)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootResetManifest(pin, manifest, broken))
		translation := checkpoint.Translation
		translation.SnapshotDigest = manifest.SnapshotDigest
		require.ErrorContains(t, installer.InstallRelayReset(ctx, bindingUID, manifest, broken, translation), "invalid cron passover")
		_, err = downstream.CronJob(ctx, replica.ID, job.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation))

	installedProject, err := downstream.ProjectByID(ctx, replica.ID)
	require.NoError(t, err)
	require.Nil(t, db.CronProjectPauseFromMetadata(installedProject.Metadata))
	require.Equal(t, retainedPassover, db.CronPassoverFromMetadata(installedProject.Metadata, job.UID))
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
	installedProject, err = downstream.ProjectByID(ctx, replica.ID)
	require.NoError(t, err)
	var installedMetadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(installedProject.Metadata), &installedMetadata))
	require.JSONEq(t, string(rootMetadata["cron.request."+requestID]), string(installedMetadata["cron.request."+requestID]), "signed reset preserves the exact retained request")
	require.NoError(t, installer.InstallRelayReset(ctx, bindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation), "lost commit response retries the exact retained checkpoint")
}
