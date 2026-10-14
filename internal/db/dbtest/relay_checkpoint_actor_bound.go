package dbtest

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayCheckpointStatusBoundsEncodedActor proves that a status estimate
// bounds the reset of every live relay even when the account with the most
// characters is not the one with the largest encoding. A multibyte or
// JSON-escaped actor writes more bytes per record and receipt than a longer
// ASCII one.
func RunRelayCheckpointStatusBoundsEncodedActor(t *testing.T, store db.Storage) {
	ctx := t.Context()
	measurer, ok := store.(db.RelayCheckpointMeasurer)
	require.True(t, ok, "native checkpoint measurement is required")
	creator, ok := store.(db.RelayResetStore)
	require.True(t, ok, "native root checkpoint generation is required")
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
	project, err := store.CreateProject(ctx, "encoded-actor-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	for _, title := range []string{"First issue", "Second issue", "Third issue"} {
		_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "assistant"})
		require.NoError(t, err)
	}
	// 45 characters and 45 bytes, against 40 characters and 120 bytes.
	ascii := strings.Repeat("a", 45)
	wide := strings.Repeat("成", 40)
	grants := map[string]db.CreatedFederationEnrollment{}
	for i, actor := range []string{ascii, wide} {
		parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "encoded-actor-parent-test-token-" + string(rune('a'+i)), Actor: actor, AdminActor: "admin"})
		require.NoError(t, err)
		grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "0000000000000000000000000" + string(rune('6'+i)), ProtocolVersion: db.RelayProtocolVersion, Token: "encoded-actor-relay-test-token-" + string(rune('a'+i))})
		require.NoError(t, err)
		grants[actor] = grant
	}
	binding := grants[wide].Enrollment.RelayBindingUID
	drainRelayDeliveries(t, store, binding)
	estimate, err := measurer.RelayCheckpointSize(ctx, project.ID)
	require.NoError(t, err)
	checkpoint, err := creator.CreateRelayReset(db.WithRootAttribution(ctx, signer, wide), binding, signer)
	require.NoError(t, err)
	require.GreaterOrEqual(t, estimate.Bytes, db.RootResetSnapshotBytes(checkpoint.Snapshot), "status bounds the reset of the relay whose actor encodes largest")
}
