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

// RunRelayIngestValidatesCronReplay exercises cron history arriving over a
// relay push. Legacy ingest refuses a cron definition whose version names an
// unrelated event; a relay push of the same document must be refused with the
// same error class and leave no definition behind, while a well-formed
// definition from the same sender is still accepted.
func RunRelayIngestValidatesCronReplay(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", PushEnabled: true, Enabled: true})
	require.NoError(t, err)
	root := "00000000000000000000000002"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000005", UpstreamInstanceUID: root, AuthorityUID: root, HubPath: []string{root, store.InstanceUID()}, LocalActor: "personal-member", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "personal-member", AdminActor: "admin", PlaintextToken: "cron-replay-parent-test-token"})
	require.NoError(t, err)
	downstream := "00000000000000000000000006"
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	child, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: downstream, ProtocolVersion: db.RelayProtocolVersion, Token: "cron-replay-relay-test-token"})
	require.NoError(t, err)
	binding := child.Enrollment.RelayBindingUID
	seal := func(event db.RemoteEvent, sequence int64) db.RelayEnvelope {
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: binding, ProjectUID: project.UID, AuthorityUID: root, SenderInstanceUID: downstream, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: sequence, Stream: db.RelayStreamEvent, Path: []string{downstream}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		return envelope
	}
	definition, err := json.Marshal(cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review open work"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	cronEvent := func(target db.Project, kind, jobUID string, versionEventUID string) db.RemoteEvent {
		document := db.CronDefinitionEvent{UID: jobUID, ProjectUID: target.UID, Name: "Nightly review", Definition: definition, Author: "source-assistant", CreatedAt: now, UpdatedAt: now}
		if versionEventUID != "" {
			document.DefinitionEventUID = versionEventUID
			document.DefinitionHLC = &db.CronDefinitionHLC{Version: 1, PhysicalMS: 300, Counter: 0, OriginInstanceUID: downstream}
		}
		payload, err := json.Marshal(document)
		require.NoError(t, err)
		return newRemoteEvent(t, target, nil, kind, "source-assistant", downstream, 310, jsontext.Value(payload))
	}

	issueUID, err := uid.New()
	require.NoError(t, err)
	issue := newRemoteEvent(t, project, &issueUID, "issue.created", "source-assistant", downstream, 300, jsontext.Value(`{"uid":"`+issueUID+`","title":"Relayed task","author":"source-assistant","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	accepted, err := store.AcceptRelayDeliveries(ctx, binding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{seal(issue, 1)}})
	require.NoError(t, err)
	require.Equal(t, int64(1), accepted.Through)

	// Legacy ingest is the reference: a snapshot whose definition version names
	// an unrelated event in the project is refused as a validation failure.
	legacy, err := store.CreateProject(ctx, "legacy-project")
	require.NoError(t, err)
	_, legacyIssueEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: legacy.ID, Title: "Unrelated", Author: "personal-member"})
	require.NoError(t, err)
	forgedJobUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.InsertRemoteEvent(ctx, legacy.ID, cronEvent(legacy, "cron.job.snapshot", forgedJobUID, legacyIssueEvent.UID))
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)

	forged := cronEvent(project, "cron.job.snapshot", forgedJobUID, issue.EventUID)
	_, err = store.AcceptRelayDeliveries(ctx, binding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{seal(forged, 2)}})
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "relay accept applies the same cron replay validation as legacy ingest")
	_, err = store.CronJob(ctx, project.ID, forgedJobUID)
	require.ErrorIs(t, err, db.ErrNotFound, "a refused push materializes nothing")
	stored, err := store.EventsByUIDs(ctx, project.ID, []string{forged.EventUID})
	require.ErrorIs(t, err, db.ErrNotFound)
	require.Empty(t, stored)

	jobUID, err := uid.New()
	require.NoError(t, err)
	created := cronEvent(project, "cron.job.created", jobUID, "")
	accepted, err = store.AcceptRelayDeliveries(ctx, binding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{seal(created, 2)}})
	require.NoError(t, err, "a refused push does not advance the accepted prefix")
	require.Equal(t, int64(2), accepted.Through)
	job, err := store.CronJob(ctx, project.ID, jobUID)
	require.NoError(t, err)
	require.Equal(t, "Nightly review", job.Name)
	require.Equal(t, created.EventUID, job.DefinitionEventUID)
}
