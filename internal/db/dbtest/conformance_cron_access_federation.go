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

func cronAccessRestricted(t *testing.T, store db.Storage, project db.Project) db.ProjectCronAccessPolicy {
	t.Helper()
	policy, err := store.CronAccessPolicy(t.Context(), project.UID)
	require.NoError(t, err)
	policy.Define = db.CronGrant{Mode: "teams"}
	policy.Observe = db.CronGrant{Mode: "teams"}
	policy, _, err = store.SetCronAccessPolicy(t.Context(), policy, "owner")
	require.NoError(t, err)
	return policy
}

func cronAccessWorkflowEvent(t *testing.T, project db.Project, actor, origin string) db.RemoteEvent {
	t.Helper()
	workflowUID, err := uid.New()
	require.NoError(t, err)
	definition, err := json.Marshal(cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}})
	require.NoError(t, err)
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(db.CronDefinitionEvent{UID: workflowUID, ProjectUID: project.UID, Name: "Example workflow", Definition: definition, Author: actor, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	return newRemoteEvent(t, project, nil, "cron.workflow.created", actor, origin, 300, jsontext.Value(payload))
}

func cronAccessRunEvent(t *testing.T, project db.Project, workflow db.RemoteEvent, actor, origin string) db.RemoteEvent {
	t.Helper()
	var doc db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal(workflow.Payload, &doc))
	runUID, err := uid.New()
	require.NoError(t, err)
	now := time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC)
	payload, err := json.Marshal(db.CronRunObservation{ProjectUID: project.UID, UID: runUID, WorkflowUID: &doc.UID, WorkflowDefinitionEventUID: &workflow.EventUID, Actor: actor, Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	return newRemoteEvent(t, project, nil, "cron.run.observed", actor, origin, 301, jsontext.Value(payload))
}

func checkCronAccessLegacyIngest(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, p.ID, "owner")
	require.NoError(t, err)
	policy := cronAccessRestricted(t, store, p)
	origin, err := uid.New()
	require.NoError(t, err)
	workflow := cronAccessWorkflowEvent(t, p, "runtime", origin)
	ingest := func(event db.RemoteEvent) (db.FederationIngestResult, error) {
		return store.IngestFederationEvents(ctx, db.FederationIngestParams{EventFeatures: db.CronEventFeature, ProjectID: p.ID, SpokeInstanceUID: origin, BoundActor: "runtime", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	}
	_, err = ingest(workflow)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	policy.Define = db.CronGrant{Mode: "any"}
	policy, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
	require.NoError(t, err)
	accepted, err := ingest(workflow)
	require.NoError(t, err)
	require.Equal(t, 1, accepted.Accepted)
	run := cronAccessRunEvent(t, p, workflow, "runtime", origin)
	_, err = ingest(run)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	// A .snapshot name alone is not an adoption baseline exemption.
	var observation db.CronRunObservation
	require.NoError(t, json.Unmarshal(run.Payload, &observation))
	observation.ObservationEventUID = run.EventUID
	observation.ObservationHLC = &db.CronDefinitionHLC{Version: 1, PhysicalMS: run.HLCPhysicalMS, Counter: run.HLCCounter, OriginInstanceUID: origin}
	snapshot := newRemoteEvent(t, p, nil, "cron.run.snapshot", "runtime", origin, 302, mustNativeJSON(t, observation))
	_, err = ingest(snapshot)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	policy.Define = db.CronGrant{Mode: "teams"}
	_, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
	require.NoError(t, err)
	retained, err := ingest(workflow)
	require.NoError(t, err)
	require.Equal(t, 1, retained.Duplicates)
	return nil
}

func checkCronAccessRelayIngest(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "root-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleHub, HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "runtime", AdminActor: "owner", PlaintextToken: "cron-access-parent-test-token"})
	require.NoError(t, err)
	peer, err := uid.New()
	require.NoError(t, err)
	leaf, err := uid.New()
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: p.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: "cron-access-relay-test-token", ServeDownstream: true})
	require.NoError(t, err)
	policy := cronAccessRestricted(t, store, p)
	workflow := cronAccessWorkflowEvent(t, p, "source-runtime", leaf)
	seal := func(event db.RemoteEvent, sequence int64) db.RelayEnvelope {
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: p.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peer, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: sequence, Stream: db.RelayStreamEvent, Path: []string{leaf, peer}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		return envelope
	}
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "source-runtime")
	batch := db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{seal(workflow, 1)}}
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	team, _, err := store.CreateTeam(ctx, "definers", "owner")
	require.NoError(t, err)
	_, err = store.SetTeamMembership(ctx, team.UID, "source-runtime", true, "owner")
	require.NoError(t, err)
	policy.Define = db.CronGrant{Mode: "teams", TeamUIDs: []string{team.UID}}
	policy, _, err = store.SetCronAccessPolicy(ctx, policy, "owner")
	require.NoError(t, err)
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
	require.ErrorIs(t, err, db.ErrCronPermissionDenied, "source actor cannot grant hop authority")
	_, err = store.SetTeamMembership(ctx, team.UID, "runtime", true, "owner")
	require.NoError(t, err)
	accepted, err := store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Len(t, accepted.InsertedEvents, 1)
	run := cronAccessRunEvent(t, p, workflow, "source-runtime", leaf)
	_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, After: 1, Envelopes: []db.RelayEnvelope{seal(run, 2)}})
	require.ErrorIs(t, err, db.ErrCronPermissionDenied)
	_, err = store.SetTeamMembership(ctx, team.UID, "runtime", false, "owner")
	require.NoError(t, err)
	retained, err := store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, batch)
	require.NoError(t, err)
	require.Empty(t, retained.InsertedEvents)
	return nil
}
