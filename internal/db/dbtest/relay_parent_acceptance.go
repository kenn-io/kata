package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayRootAcceptanceParent exercises relay root acceptance parent on the supplied native store.
// R3/R5: root acceptance rechecks the bridge's parent even when a source event
// is already durable. Source labels and a previously admitted grant cannot
// create verified proof after parent revocation.
func RunRelayRootAcceptanceParent(t *testing.T, store db.Storage) {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleHub, HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
	require.NoError(t, err)
	pub, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: p.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(pub), PublicKey: pub}))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "member", AdminActor: "admin", PlaintextToken: "acceptance-parent-test-token"})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: p.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "acceptance-relay-test-token"})
	require.NoError(t, err)
	_, first, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Accepted before revoke", Author: "assistant"})
	require.NoError(t, err)
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}
	proof, err := store.RecordRootAttribution(ctx, grant.Enrollment.ID, db.RemoteEventFromStored(first), signer)
	require.NoError(t, err)
	require.Equal(t, "member", proof.AccountableActor)
	issue, pending, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Pending root acceptance", Author: "assistant"})
	require.NoError(t, err)
	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = store.RecordRootAttribution(ctx, grant.Enrollment.ID, db.RemoteEventFromStored(pending), signer)
	require.Error(t, err, "revoked parent must stop root proof issuance")
	_, err = store.EntityAttribution(ctx, p.UID, "issue", issue.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
