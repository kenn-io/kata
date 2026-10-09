package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayAuthorityDepth exercises relay authority depth on the supplied native store.
// R1: eight hubs may serve a final leaf; a ninth hub is refused before an
// enrollment or secret is retained. Node roles come from negotiated serving
// state, never a source event's asserted visited path.
func RunRelayAuthorityDepth(t *testing.T, store db.Storage) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "depth-project")
	require.NoError(t, err)
	root := "00000000000000000000000002"
	upstream := "00000000000000000000000008"
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", PushEnabled: true, Enabled: true})
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{Actor: "local-member", AdminActor: "admin", PlaintextToken: "depth-parent-test-token"})
	require.NoError(t, err)
	configuration := db.RelayBindingConfig{ProtocolVersion: 1, BindingUID: "00000000000000000000000012", UpstreamInstanceUID: upstream, AuthorityUID: root, HubPath: []string{root, "00000000000000000000000003", "00000000000000000000000004", "00000000000000000000000005", "00000000000000000000000006", "00000000000000000000000007", upstream, store.InstanceUID()}, LocalActor: parent.Actor, ServeDownstream: true, ResetEpoch: 1}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, configuration)
	require.NoError(t, err)
	child := db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000009", ProtocolVersion: 1, Token: "depth-child-test-token", ServeDownstream: true}
	_, err = store.CreateRelayEnrollment(ctx, child)
	require.Error(t, err, "a ninth hub must not be enrolled")
	grants, err := store.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Empty(t, grants)
	child.ServeDownstream = false
	leaf, err := store.CreateRelayEnrollment(ctx, child)
	require.NoError(t, err)
	_, err = store.AuthorizeFederationToken(ctx, leaf.Token, project.ID, "pull")
	require.NoError(t, err)
}
