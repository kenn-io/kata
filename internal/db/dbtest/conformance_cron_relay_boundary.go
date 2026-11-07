package dbtest

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Adoption invalidates local readers without discarding upstream history.
// A relay's new downstream can still drain that history without a checkpoint.
func checkRelayAdoptionResetBoundary(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "adopted-project")
	require.NoError(t, err)
	hubProject, err := uid.New()
	require.NoError(t, err)
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: hubProject, Actor: "relay-member", RelayProtocolVersion: db.RelayProtocolVersion})
	require.NoError(t, err)
	require.Positive(t, adopted.ResetAfterEventID)
	rootUID, err := uid.New()
	require.NoError(t, err)
	public, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: hubProject, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	bindingUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.SetRelayBindingConfig(ctx, project.ID, db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID, AuthorityUID: rootUID, UpstreamInstanceUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "relay-member", ServeDownstream: true, ResetEpoch: 1})
	require.NoError(t, err)
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "adoption-relay-parent", Actor: "relay-member", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	peer, err := uid.New()
	require.NoError(t, err)
	child, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion})
	require.NoError(t, err)
	for range 2 {
		required, err := store.(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, child.Enrollment.RelayBindingUID)
		require.NoError(t, err)
		require.False(t, required, "a local adoption reset is not root history loss")
	}
	drainRelayGrant(t, store, child.Enrollment.RelayBindingUID, child.Enrollment.RelayResetEpoch)
	return nil
}
