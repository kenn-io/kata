package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// relayLinkBoundaryFixture is a root hub project with a sibling hub project in
// the same group, a pinned root key and a parent credential for relay grants.
type relayLinkBoundaryFixture struct {
	project, sibling db.Project
	parentTokenID    int64
	signer           db.RootAttributionSigner
}

func newRelayLinkBoundaryFixture(t *testing.T, store db.Storage, name string) relayLinkBoundaryFixture {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, name)
	require.NoError(t, err)
	sibling, err := store.CreateProject(ctx, name+"-sibling")
	require.NoError(t, err)
	for _, p := range []db.Project{project, sibling} {
		_, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleHub, HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
		require.NoError(t, err)
	}
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: name + "-parent-test-token", Actor: "relay-member", AdminActor: "admin"})
	require.NoError(t, err)
	return relayLinkBoundaryFixture{project: project, sibling: sibling, parentTokenID: parent.ID, signer: db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}}
}

func linkTargetUIDs(t *testing.T, store db.Storage, issueID int64) []string {
	t.Helper()
	links, err := store.LinksByIssue(t.Context(), issueID)
	require.NoError(t, err)
	targets := []string{}
	for _, link := range links {
		targets = append(targets, link.ToIssueUID)
	}
	sort.Strings(targets)
	return targets
}

// RunRelayLinkBoundaryKeepsSameProjectEffects proves the relay link boundary
// removes individual cross-project link effects from a composite event and
// keeps the event's same-project effects. A relay-delivered creation whose
// deferred peers later appear in this project and in a sibling project keeps
// the same-project link only. A legacy creation that carried a cross-project
// link keeps its same-project link after an operator removes the crossing link
// and enrolls a relay peer.
func RunRelayLinkBoundaryKeepsSameProjectEffects(t *testing.T, store db.Storage) {
	t.Run("relay composite creation", func(t *testing.T) {
		ctx := t.Context()
		fixture := newRelayLinkBoundaryFixture(t, store, "relay-composite")
		peerInstanceUID, err := uid.New()
		require.NoError(t, err)
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peerInstanceUID, ProtocolVersion: db.RelayProtocolVersion, Token: "relay-composite-relay-test-token"})
		require.NoError(t, err)
		createdUID, err := uid.New()
		require.NoError(t, err)
		localPeer, err := uid.New()
		require.NoError(t, err)
		foreignPeer, err := uid.New()
		require.NoError(t, err)
		event := newRemoteEvent(t, fixture.project, &createdUID, "issue.created", "relay-member", peerInstanceUID, 500, jsontext.Value(`{"uid":"`+createdUID+`","title":"Relayed composite","body":"","author":"relay-member","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"`+localPeer+`","author":"relay-member"},{"type":"blocks","to_issue_uid":"`+foreignPeer+`","author":"relay-member"}]}`))
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: grant.Enrollment.RelayBindingUID, ProjectUID: fixture.project.UID, AuthorityUID: store.InstanceUID(), SenderInstanceUID: peerInstanceUID, ReceiverInstanceUID: store.InstanceUID(), Epoch: grant.Enrollment.RelayResetEpoch, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{peerInstanceUID}, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		writeCtx := db.WithRootAttribution(ctx, fixture.signer, "relay-member")
		_, err = store.AcceptRelayDeliveries(writeCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
		require.NoError(t, err, "a creation whose peers have not arrived stays deferred")
		created, err := store.IssueByUID(ctx, createdUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Empty(t, linkTargetUIDs(t, store, created.ID))

		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, UID: foreignPeer, Title: "Sibling peer", Author: "sibling-member"})
		require.NoError(t, err)
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, UID: localPeer, Title: "Local peer", Author: "relay-member"})
		require.NoError(t, err)
		require.NoError(t, store.MaterializeFederatedProject(ctx, fixture.project.ID))
		require.Equal(t, []string{localPeer}, linkTargetUIDs(t, store, created.ID), "the same-project link survives while the cross-project link stays absent")
		require.NoError(t, store.RevokeFederationEnrollment(ctx, grant.Enrollment.ID))
		require.NoError(t, store.MaterializeFederatedProject(ctx, fixture.project.ID))
		require.Equal(t, []string{localPeer}, linkTargetUIDs(t, store, created.ID), "revoking the grant changes neither effect")
	})
	t.Run("legacy composite survives relay enrollment", func(t *testing.T) {
		ctx := t.Context()
		fixture := newRelayLinkBoundaryFixture(t, store, "legacy-composite")
		localPeer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Local peer", Author: "member"})
		require.NoError(t, err)
		siblingPeer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, Title: "Sibling peer", Author: "member"})
		require.NoError(t, err)
		spokeInstanceUID, err := uid.New()
		require.NoError(t, err)
		createdUID, err := uid.New()
		require.NoError(t, err)
		event := newRemoteEvent(t, fixture.project, &createdUID, "issue.created", "spoke", spokeInstanceUID, 600, jsontext.Value(`{"uid":"`+createdUID+`","title":"Legacy composite","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"`+localPeer.UID+`","author":"spoke"},{"type":"blocks","to_issue_uid":"`+siblingPeer.UID+`","author":"spoke"}]}`))
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: fixture.project.ID, SpokeInstanceUID: spokeInstanceUID, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
		require.NoError(t, err)
		created, err := store.IssueByUID(ctx, createdUID, db.IncludeDeletedYes)
		require.NoError(t, err)
		expected := []string{localPeer.UID, siblingPeer.UID}
		sort.Strings(expected)
		require.Equal(t, expected, linkTargetUIDs(t, store, created.ID), "legacy federation converges the cross-project link")

		links, err := store.LinksByIssue(ctx, created.ID)
		require.NoError(t, err)
		for _, link := range links {
			if link.ToIssueUID != siblingPeer.UID {
				continue
			}
			_, err = store.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: created.ID, FromShortID: created.ShortID, FromUID: created.UID, ToShortID: siblingPeer.ShortID, ToUID: siblingPeer.UID, Actor: "member"})
			require.NoError(t, err)
		}
		require.Equal(t, []string{localPeer.UID}, linkTargetUIDs(t, store, created.ID))
		peerInstanceUID, err := uid.New()
		require.NoError(t, err)
		//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
		_, err = store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peerInstanceUID, ProtocolVersion: db.RelayProtocolVersion, Token: "legacy-composite-relay-test-token"})
		require.NoError(t, err, "relay admission is allowed once no link crosses the project boundary")
		require.NoError(t, store.MaterializeFederatedProject(ctx, fixture.project.ID))
		require.Equal(t, []string{localPeer.UID}, linkTargetUIDs(t, store, created.ID), "the historical creation keeps its same-project link")
	})
}

// RunRelayLinkBoundaryFollowsActiveGrants proves the fold and the local write
// rule share one boundary. While a relay grant is active a local cross-project
// link is refused. After the last grant is revoked the link is allowed, and a
// legacy peer's unlink of it is applied, so the hub does not keep an edge its
// replicas removed.
func RunRelayLinkBoundaryFollowsActiveGrants(t *testing.T, store db.Storage) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "boundary-grants")
	peerInstanceUID, err := uid.New()
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID, SpokeInstanceUID: peerInstanceUID, ProtocolVersion: db.RelayProtocolVersion, Token: "boundary-grants-relay-test-token"})
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.project.ID, Title: "Hub issue", Author: "member"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, Title: "Sibling issue", Author: "member"})
	require.NoError(t, err)
	linkEvent := db.LinkEventParams{EventType: "issue.linked", EventIssueID: source.ID, FromShortID: source.ShortID, FromUID: source.UID, ToShortID: target.ShortID, ToUID: target.UID, Actor: "member"}
	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: source.ID, ToIssueID: target.ID, Type: "blocks", Author: "member"}, linkEvent)
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary, "an active relay grant refuses a local cross-project link")

	require.NoError(t, store.RevokeFederationEnrollment(ctx, grant.Enrollment.ID))
	_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: source.ID, ToIssueID: target.ID, Type: "blocks", Author: "member"}, linkEvent)
	require.NoError(t, err, "revoking the last relay grant allows local cross-project links again")
	require.Equal(t, []string{target.UID}, linkTargetUIDs(t, store, source.ID))

	spokeInstanceUID, err := uid.New()
	require.NoError(t, err)
	// The hub's own link event carries a current clock; the legacy unlink must
	// be the later writer for the fold to apply it.
	unlink := newRemoteEvent(t, fixture.project, &source.UID, "issue.unlinked", "spoke", spokeInstanceUID, time.Now().Add(time.Second).UnixMilli(), jsontext.Value(`{"issue_uid":"`+source.UID+`","from_uid":"`+source.UID+`","to_uid":"`+target.UID+`","link_from_uid":"`+source.UID+`","link_to_uid":"`+target.UID+`","type":"blocks"}`))
	unlink.RelatedIssueUID = &target.UID
	unlink.ContentHash = remoteEventHash(t, unlink)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: fixture.project.ID, SpokeInstanceUID: spokeInstanceUID, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: unlink}}})
	require.NoError(t, err)
	require.Empty(t, linkTargetUIDs(t, store, source.ID), "a legacy peer's unlink of the allowed cross-project link is applied")
	require.NoError(t, store.MaterializeFederatedProject(ctx, fixture.project.ID))
	require.Empty(t, linkTargetUIDs(t, store, source.ID), "a later group materialization does not bring the removed link back")
}
