package dbtest

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunArchiveAndLeaveDoesNotCreateRelayDebt exercises the terminal archive
// audit path without creating a new relay delivery on every native backend.
func RunArchiveAndLeaveDoesNotCreateRelayDebt(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "example-actor",
		Enabled: true, PushEnabled: true,
	})
	require.NoError(t, err)
	publicKey, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	rootUID := "00000000000000000000000002"
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: rootUID,
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}))
	bindingUID := "00000000000000000000000005"
	configuration := db.RelayBindingConfig{
		ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
		UpstreamInstanceUID: rootUID, AuthorityUID: rootUID,
		HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "example-actor", ResetEpoch: 1,
	}
	_, err = store.SetRelayBindingConfig(ctx, project.ID, configuration)
	require.NoError(t, err)

	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "operator", Force: true, SkipFederationRelay: true,
	})
	require.NoError(t, err)
	leave, err := store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err, "archive-and-leave must finish after draining the negotiated relay")
	require.Equal(t, db.FederationRoleSpoke, leave.Role)
	_, err = store.FederationBindingByProject(ctx, project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
