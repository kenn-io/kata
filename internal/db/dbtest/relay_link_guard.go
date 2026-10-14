package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayDeferredLinkGuardOutlivesBinding proves the cross-project guard for
// deferred relay links is a property of the event, not of the binding's
// delivery rows. Relay ingest accepts a link whose same-project peer has not
// arrived yet. If that peer UID later appears in a sibling project of the hub
// group, the group fold must not materialize the edge, before and after the
// grant that delivered the link is revoked. A peer that arrives in the same
// project still resolves.
func RunRelayDeferredLinkGuardOutlivesBinding(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "relay-link-guard")
	require.NoError(t, err)
	sibling, err := store.CreateProject(ctx, "relay-link-guard-sibling")
	require.NoError(t, err)
	for _, p := range []db.Project{project, sibling} {
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleHub, HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
		require.NoError(t, err)
	}
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "link-guard-parent-test-token", Actor: "relay-member", AdminActor: "admin"})
	require.NoError(t, err)
	peerInstanceUID, err := uid.New()
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peerInstanceUID, ProtocolVersion: db.RelayProtocolVersion, Token: "link-guard-relay-test-token"})
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Relayed source", Author: "relay-member"})
	require.NoError(t, err)
	foreignPeer, err := uid.New()
	require.NoError(t, err)
	localPeer, err := uid.New()
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "relay-member")
	var sequence int64
	for _, peer := range []string{foreignPeer, localPeer} {
		sequence++
		event := newRemoteEvent(t, project, &source.UID, "issue.linked", "relay-member", peerInstanceUID, 500+sequence, jsontext.Value(`{"issue_uid":"`+source.UID+`","from_uid":"`+source.UID+`","to_uid":"`+peer+`","type":"blocks"}`))
		event.RelatedIssueUID = &peer
		event.ContentHash = remoteEventHash(t, event)
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peerInstanceUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: grant.Enrollment.RelayResetEpoch, Sequence: sequence, Stream: db.RelayStreamEvent, Path: []string{peerInstanceUID}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
		require.NoError(t, err, "a link whose peer has not arrived stays deferred")
	}
	linkTargets := func() []string {
		t.Helper()
		links, err := store.LinksByIssue(ctx, source.ID)
		require.NoError(t, err)
		targets := []string{}
		for _, link := range links {
			targets = append(targets, link.ToIssueUID)
		}
		return targets
	}
	require.Empty(t, linkTargets())

	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sibling.ID, UID: foreignPeer, Title: "Sibling peer", Author: "sibling-member"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, UID: localPeer, Title: "Local peer", Author: "relay-member"})
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	require.Equal(t, []string{localPeer}, linkTargets(), "before revocation only the same-project peer resolves")

	// An ordinary peer's link-affecting event folds the group's link history
	// again through the incremental ingest path; that fold applies the same
	// boundary as a full materialization.
	legacyPeer, err := uid.New()
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Ordinary link source", Author: "relay-member"})
	require.NoError(t, err)
	legacyLink := newRemoteEvent(t, project, &other.UID, "issue.linked", "legacy-member", legacyPeer, 900, jsontext.Value(`{"issue_uid":"`+other.UID+`","from_uid":"`+other.UID+`","to_uid":"`+localPeer+`","type":"related"}`))
	legacyLink.RelatedIssueUID = &localPeer
	legacyLink.ContentHash = remoteEventHash(t, legacyLink)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: legacyPeer, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: legacyLink}}})
	require.NoError(t, err)
	require.Equal(t, []string{localPeer}, linkTargets(), "an incremental ingest does not resolve the sibling-project peer either")

	require.NoError(t, store.RevokeFederationEnrollment(ctx, grant.Enrollment.ID))
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	require.Equal(t, []string{localPeer}, linkTargets(), "after revocation the sibling-project peer still does not resolve")
}
