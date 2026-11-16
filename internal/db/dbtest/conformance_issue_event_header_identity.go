package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func issueCreationPayload(issueUID, actor string) jsontext.Value {
	return jsontext.Value(`{"uid":"` + issueUID + `","short_id":"` + strings.ToLower(issueUID[len(issueUID)-4:]) + `","title":"Creation","body":"","author":"` + actor + `","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)
}

// headerlessIssueEvents returns issue events that name their issue only in
// the payload: an edit of knownIssueUID naming it as issue_uid, and a creation
// of a new issue naming it as uid. No Kata release writes this shape; every
// writer copies the issue UID into the event header.
func headerlessIssueEvents(t *testing.T, project db.Project, knownIssueUID, actor, origin string) []db.RemoteEvent {
	t.Helper()
	newIssueUID, err := uid.New()
	require.NoError(t, err)
	return []db.RemoteEvent{
		newRemoteEvent(t, project, nil, "issue.updated", actor, origin, 600,
			jsontext.Value(`{"issue_uid":"`+knownIssueUID+`","title":"Header-less edit","updated_at":"2026-05-23T12:01:00.000Z"}`)),
		newRemoteEvent(t, project, nil, "issue.created", actor, origin, 601, issueCreationPayload(newIssueUID, actor)),
	}
}

// checkIssueEventsRequireHeaderIdentity proves that pushed and pulled issue
// events must carry their issue UID in the event header, and that a rejected
// event is not stored.
func checkIssueEventsRequireHeaderIdentity(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	push := func(sourceEventID int64, event db.RemoteEvent) (db.FederationIngestResult, error) {
		return store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "worker",
			Events: []db.FederationIngestEvent{{SourceEventID: sourceEventID, Event: event}},
		})
	}

	created := newRemoteEvent(t, project, &issueUID, "issue.created", "worker", origin, 500, issueCreationPayload(issueUID, "worker"))
	result, err := push(1, created)
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted, "a creation with its issue UID in the header is accepted")

	for _, event := range headerlessIssueEvents(t, project, issueUID, "worker", origin) {
		_, err := push(2, event)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation, "push must reject a header-less %s", event.Type)
		_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{event})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation, "pull must reject a header-less %s", event.Type)
		_, err = store.EventsByUIDs(ctx, project.ID, []string{event.EventUID})
		require.ErrorIs(t, err, db.ErrNotFound, "a rejected %s must not be stored", event.Type)
	}
	return nil
}

// relayOffers enrolls a relay peer on a fresh root hub project. The returned
// function offers one event to the hub at the given relay sequence.
func relayOffers(t *testing.T, store db.Storage, name string) (db.Project, string, func(sequence int64, event db.RemoteEvent) error) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, name)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: name + "-parent-test-token", Actor: "relay-member", AdminActor: "admin"})
	require.NoError(t, err)
	peer, err := uid.New()
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: name + "-relay-test-token"})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "relay-member")
	offer := func(sequence int64, event db.RemoteEvent) error {
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peer, ReceiverInstanceUID: store.InstanceUID(), Epoch: grant.Enrollment.RelayResetEpoch, Sequence: sequence, Stream: db.RelayStreamEvent, Path: []string{peer}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, After: sequence - 1, Envelopes: []db.RelayEnvelope{envelope}})
		return err
	}
	return project, peer, offer
}

// RunRelayRejectsHeaderlessIssueEvents proves relay acceptance applies the
// same header identity rule as direct federation ingest.
func RunRelayRejectsHeaderlessIssueEvents(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, peer, offer := relayOffers(t, store, "relay-header-project")
	issueUID, err := uid.New()
	require.NoError(t, err)

	created := newRemoteEvent(t, project, &issueUID, "issue.created", "relay-member", peer, 500, issueCreationPayload(issueUID, "relay-member"))
	require.NoError(t, offer(1, created), "a creation with its issue UID in the header is accepted")

	for _, event := range headerlessIssueEvents(t, project, issueUID, "relay-member", peer) {
		require.ErrorIs(t, offer(2, event), db.ErrFederationIngestValidation, "relay must reject a header-less %s", event.Type)
		_, err = store.EventsByUIDs(ctx, project.ID, []string{event.EventUID})
		require.ErrorIs(t, err, db.ErrNotFound, "a rejected %s must not be stored", event.Type)
	}
}
