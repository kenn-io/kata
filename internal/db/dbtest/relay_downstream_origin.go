package dbtest

import (
	"crypto/ed25519"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayDownstreamOrigin exercises an intermediate relay hub receiving a push
// from a downstream relay enrollment. Root-origin and other upstream-origin
// deliveries reach the hub only over its upstream binding, so a downstream push
// naming the root or an upstream hub is rejected without changing state, while
// work from the downstream subtree is still accepted and forwarded upstream.
func RunRelayDownstreamOrigin(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", PushEnabled: true, Enabled: true})
	require.NoError(t, err)
	root := "00000000000000000000000002"
	upstreamHub := "00000000000000000000000003"
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	upstream := db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "00000000000000000000000005", UpstreamInstanceUID: upstreamHub, AuthorityUID: root, HubPath: []string{root, upstreamHub, store.InstanceUID()}, LocalActor: "personal-member", ServeDownstream: true, ResetEpoch: 1}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, upstream)
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "personal-member", AdminActor: "admin", PlaintextToken: "downstream-origin-parent-test-token"})
	require.NoError(t, err)
	downstream := "00000000000000000000000006"
	descendant := "00000000000000000000000007"
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	child, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: downstream, ProtocolVersion: db.RelayProtocolVersion, Token: "downstream-origin-relay-test-token", ServeDownstream: true})
	require.NoError(t, err)
	binding := child.Enrollment.RelayBindingUID
	seal := func(event db.RemoteEvent, sequence int64, path []string) db.RelayEnvelope {
		body, err := db.EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: binding, ProjectUID: project.UID, AuthorityUID: root, SenderInstanceUID: downstream, ReceiverInstanceUID: store.InstanceUID(), Epoch: 1, Sequence: sequence, Stream: db.RelayStreamEvent, Path: path, SourceUID: event.EventUID, SourceHash: event.ContentHash, Body: body})
		require.NoError(t, err)
		return envelope
	}
	created := func(origin string, physicalMS int64) (string, db.RemoteEvent) {
		issueUID, err := uid.New()
		require.NoError(t, err)
		payload := jsontext.Value(`{"uid":"` + issueUID + `","title":"Relayed task","author":"source-assistant","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)
		return issueUID, newRemoteEvent(t, project, &issueUID, "issue.created", "source-assistant", origin, physicalMS, payload)
	}
	forgedIssues := []string{}
	for _, forged := range []struct {
		name   string
		origin string
		path   []string
	}{
		{"root origin", root, []string{root, downstream}},
		{"upstream hub origin", upstreamHub, []string{upstreamHub, downstream}},
		{"root inside path", descendant, []string{descendant, root, downstream}},
		{"upstream hub inside path", descendant, []string{descendant, upstreamHub, downstream}},
	} {
		issueUID, event := created(forged.origin, 300)
		forgedIssues = append(forgedIssues, issueUID)
		_, err := store.AcceptRelayDeliveries(ctx, binding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{seal(event, 1, forged.path)}})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation, forged.name)
	}
	for _, issueUID := range forgedIssues {
		_, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound, "a rejected push leaves no projection")
	}
	queued, err := store.PendingRelayDeliveries(ctx, upstream.BindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Empty(t, queued, "a rejected push queues nothing upstream")

	ownUID, own := created(downstream, 301)
	descendantUID, descended := created(descendant, 302)
	accepted, err := store.AcceptRelayDeliveries(ctx, binding, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{
		seal(own, 1, []string{downstream}),
		seal(descended, 2, []string{descendant, downstream}),
	}})
	require.NoError(t, err, "rejected pushes did not advance the accepted prefix")
	require.Equal(t, int64(2), accepted.Through)
	for _, issueUID := range []string{ownUID, descendantUID} {
		_, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
		require.NoError(t, err)
	}
	forwarded, err := store.PendingRelayDeliveries(ctx, upstream.BindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Len(t, forwarded, 2)
	require.Equal(t, []string{downstream, store.InstanceUID()}, forwarded[0].Path)
	require.Equal(t, []string{descendant, downstream, store.InstanceUID()}, forwarded[1].Path)
}
