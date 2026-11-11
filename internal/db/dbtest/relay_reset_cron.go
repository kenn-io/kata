package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	localWinner, localEvent, err := downstream.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: replica.ID, Name: "Local winner", Actor: "worker", Definition: workflowDefinition})
	require.NoError(t, err)
	require.NoError(t, downstream.PinRootAuthority(ctx, pin))
	// The existing local workflow was already pushed before relay enrollment.
	_, err = downstream.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: project.ID, HubProjectUID: project.UID, Actor: "company-member", Enabled: true, PushEnabled: true, PushCursorEventID: localEvent.ID})
	require.NoError(t, err)
	_, err = downstream.SetRelayBindingConfig(ctx, replica.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: root.InstanceUID(), UpstreamInstanceUID: root.InstanceUID(), HubPath: []string{root.InstanceUID(), downstream.InstanceUID()}, LocalActor: "company-member", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
	for _, section := range []string{"source", "entity"} {
		for _, definition := range []string{`null`, `{"version":1,"steps":null}`} {
			bad := checkpointWithWorkflowDefinition(t, checkpoint, project, pin, private, section, definition)
			_, _, err := db.DecodeRootResetPayload(pin, bad.Snapshot)
			require.NoError(t, err, "owner backup integrity decoding preserves held content")
			err = installer.InstallRelayReset(ctx, bindingUID, bad.Manifest, bad.Snapshot, bad.Translation)
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures, "unsupported signed workflow must not be installed")
			assert.ErrorIs(t, err, db.ErrHeldWorkflowEvidence, "held signed workflow must retain its recovery classification")
			binding, err := downstream.FederationBindingByProject(ctx, replica.ID)
			require.NoError(t, err)
			require.Equal(t, int64(1), binding.RelayConfig.ResetEpoch)
			require.Equal(t, localEvent.ID, binding.PushCursorEventID)
			_, err = downstream.CronWorkflow(ctx, replica.ID, localWinner.UID)
			require.NoError(t, err)
		}
	}

	// A compatible checkpoint must not erase older held or future workflow evidence.
	q := downstream.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	for _, definition := range []string{`{"version":1,"steps":[{"kind":"argv","argv":["true"]}]}`} {
		event := argvCompatRemoteWorkflow(t, replica, downstream.InstanceUID(), definition)
		payload := make(map[string]jsontext.Value)
		require.NoError(t, json.Unmarshal(event.Payload, &payload))
		payload["uid"], err = json.Marshal(localWinner.UID)
		require.NoError(t, err)
		event.Payload, err = json.Marshal(payload)
		require.NoError(t, err)
		event = newRemoteEvent(t, replica, nil, "cron.workflow.deleted", "worker", downstream.InstanceUID(), event.HLCPhysicalMS, event.Payload)
		_, err = q.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, event.EventUID, downstream.InstanceUID(), replica.ID, replica.Name, "cron.workflow.deleted", "worker", string(event.Payload), event.HLCPhysicalMS, event.HLCCounter, event.ContentHash, event.CreatedAt.UTC().Format(db.EventTimestampFormat))
		require.NoError(t, err)
		err = installer.InstallRelayReset(ctx, bindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation)
		require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
		binding, err := downstream.FederationBindingByProject(ctx, replica.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), binding.RelayConfig.ResetEpoch)
		require.Equal(t, localEvent.ID, binding.PushCursorEventID)
		_, err = downstream.CronWorkflow(ctx, replica.ID, localWinner.UID)
		require.NoError(t, err)
		events, err := downstream.EventsByUIDs(ctx, replica.ID, []string{event.EventUID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		_, err = q.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, event.EventUID)
		require.NoError(t, err)
	}
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

	// An authentic retained checkpoint can come from an owner restore even when
	// this version cannot use its workflow evidence for a new downstream hop.
	childParent, _, err := downstream.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "cron-reset-forward-parent-test-token", Actor: "worker", AdminActor: "admin"}) // #nosec G101 -- synthetic fixture token.
	require.NoError(t, err)
	child, err := downstream.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: replica.ID, ParentTokenID: childParent.ID, SpokeInstanceUID: "00000000000000000000000008", ProtocolVersion: db.RelayProtocolVersion, Token: "cron-reset-forward-child-test-token"}) // #nosec G101 -- synthetic fixture token.
	require.NoError(t, err)
	drainRelayGrant(t, downstream, child.Enrollment.RelayBindingUID, child.Enrollment.RelayResetEpoch)
	installedKey := db.RelayResetMetadataPrefix + project.UID
	reader := downstream.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var installedRaw string
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, installedKey).Scan(&installedRaw))
	var installed db.RelayResetCheckpoint
	require.NoError(t, json.Unmarshal([]byte(installedRaw), &installed))
	bad := checkpointWithWorkflowDefinition(t, installed, project, pin, private, "source", `null`)
	raw, err := json.Marshal(bad)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE meta SET value=$1 WHERE key=$2`, string(raw), installedKey)
	require.NoError(t, err)
	_, _, err = db.DecodeRootResetPayload(pin, bad.Snapshot)
	require.NoError(t, err)
	_, err = installer.CreateRelayReset(ctx, child.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	assert.ErrorIs(t, err, db.ErrHeldWorkflowEvidence)
	var retained string
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, installedKey).Scan(&retained))
	require.Equal(t, string(raw), retained)
	var childCaches int
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key=$1`, installedKey+"."+child.Enrollment.RelayBindingUID).Scan(&childCaches))
	require.Zero(t, childCaches, "refusal must not prepare a downstream checkpoint")

	// Restore an authentic installed checkpoint through the ordinary owner path.
	bad = checkpointWithWorkflowDefinition(t, installed, project, pin, private, "source", `{"version":1,"steps":[{"key":"inspect","kind":"command","command":"git status"}]}`, true)
	require.NoError(t, db.VerifyRootResetManifest(pin, bad.Manifest, bad.Snapshot))
	require.NoError(t, db.ValidateRelayResetTranslation(bad.Translation.Authority, bad.Manifest, bad.Translation))
	_, _, err = db.DecodeRootResetPayload(pin, bad.Snapshot)
	require.NoError(t, err)
	raw, err = json.Marshal(bad)
	require.NoError(t, err)
	filter := db.ExportFilter{IncludeDeleted: true}
	records := replaceRelayOwnerMeta(t, relayOwnerRecords(t, downstream, filter), installedKey, string(raw))
	require.NoError(t, db.ValidateImportReplay(records, db.ImportOptions{}))
	require.NoError(t, downstream.ImportReplay(ctx, records, db.ImportOptions{}))
	live, err := downstream.CronWorkflow(ctx, replica.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, workflowDefinition, live.Definition)
	before := relayOwnerRecords(t, downstream, filter)
	maximum, err := downstream.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, installedKey).Scan(&retained))
	require.Equal(t, string(raw), retained)
	_, err = installer.CreateRelayReset(ctx, child.Enrollment.RelayBindingUID, db.RootAttributionSigner{})
	assert.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	assert.NotErrorIs(t, err, db.ErrHeldWorkflowEvidence)
	afterMaximum, err := downstream.MaxEventID(ctx)
	require.NoError(t, err)
	assert.Equal(t, maximum, afterMaximum)
	assert.Equal(t, before, relayOwnerRecords(t, downstream, filter), "forward refusal preserves the installed checkpoint and child state")
	require.NoError(t, reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key=$1`, installedKey+"."+child.Enrollment.RelayBindingUID).Scan(&childCaches))
	assert.Zero(t, childCaches, "refusal must not prepare a downstream checkpoint")
}

// checkpointWithWorkflowDefinition signs a historical record without changing
// the existing records or their creation receipts.
func checkpointWithWorkflowDefinition(t *testing.T, checkpoint db.RelayResetCheckpoint, project db.Project, pin db.RootKeyPin, private ed25519.PrivateKey, section, definition string, unknownMember ...bool) db.RelayResetCheckpoint {
	t.Helper()
	var bodies [][]byte
	selected := &checkpoint.Snapshot.Events
	if section == "entity" {
		selected = &checkpoint.Snapshot.Entities
	}
	require.NoError(t, json.Unmarshal(*selected, &bodies))
	var original db.RemoteEvent
	for _, body := range bodies {
		event, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		if strings.HasPrefix(event.Type, "cron.workflow.") {
			original = event
			break
		}
	}
	require.NotEmpty(t, original.Type)
	payload := make(map[string]jsontext.Value)
	require.NoError(t, json.Unmarshal(original.Payload, &payload))
	if definition == "" {
		delete(payload, "definition")
	} else {
		payload["definition"] = jsontext.Value(definition)
	}
	if len(unknownMember) > 0 && unknownMember[0] {
		payload["future_event_field"] = jsontext.Value(`true`)
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	event := newRemoteEvent(t, project, nil, original.Type, original.Actor, pin.AuthorityUID, original.HLCPhysicalMS, jsontext.Value(raw))
	body, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	bodies = append(bodies, body)
	*selected, err = json.Marshal(bodies)
	require.NoError(t, err)
	checkpoint.Manifest, err = db.SignRootResetManifest(checkpoint.Manifest, checkpoint.Snapshot, private)
	require.NoError(t, err)
	checkpoint.Translation.SnapshotDigest = checkpoint.Manifest.SnapshotDigest
	return checkpoint
}
