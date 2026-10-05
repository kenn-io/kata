package dbtest

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type rootRotationStorage interface {
	RotateRootAuthority(context.Context, db.RootKeyTransition) error
	RootKeyTransitions(context.Context, string) ([]db.RootKeyTransition, error)
}

// RunRootKeyRotation exercises root key rotation on the supplied native store.
// R3/A5: a previously pinned key authorizes the exact next key. Rotation is
// atomic and retryable, and old creation receipts remain verifiable.
func RunRootKeyRotation(t *testing.T, store db.Storage) {
	t.Helper()
	rotation, ok := store.(rootRotationStorage)
	require.True(t, ok, "native storage must persist signed root-key transitions")
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "rotation-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	issue, _, err := store.CreateIssue(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: private}, "root-member"), db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Keep historical proof"})
	require.NoError(t, err)
	original, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	nextPublic, nextPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	next := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic}
	transition, err := db.SignRootKeyTransition(pin, next, private)
	require.NoError(t, err)
	bad := transition
	bad.Signature = append([]byte(nil), transition.Signature...)
	bad.Signature[0] ^= 1
	require.Error(t, rotation.RotateRootAuthority(ctx, bad))
	retired := transition
	retired.Next.Retired = true
	require.Error(t, rotation.RotateRootAuthority(ctx, retired))
	current, err := store.RootAuthority(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, pin, current, "rejected transitions leave the active pin unchanged")
	require.NoError(t, rotation.RotateRootAuthority(ctx, transition))
	require.NoError(t, rotation.RotateRootAuthority(ctx, transition), "exact retry is idempotent")
	current, err = store.RootAuthority(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, next, current)
	history, err := rotation.RootKeyTransitions(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, []db.RootKeyTransition{transition}, history)
	retained, err := store.EntityAttribution(ctx, project.UID, "issue", issue.UID)
	require.NoError(t, err)
	require.Equal(t, original, retained)
	require.NoError(t, db.VerifyRootReceipt(pin, retained))
	comment, _, err := store.CreateComment(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: pin.AuthorityUID, PrivateKey: nextPrivate}, "root-member"), db.CreateCommentParams{IssueID: issue.ID, Author: "source-agent", Body: "Use the replacement key"})
	require.NoError(t, err)
	proof, err := store.EntityAttribution(ctx, project.UID, "comment", comment.UID)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootReceipt(next, proof))
	require.Error(t, db.VerifyRootReceipt(pin, proof))
	// A second branch from the retired key is not a new authorization.
	otherPublic, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	other := next
	other.KeyID, other.PublicKey = db.RootPublicKeyID(otherPublic), otherPublic
	stale, err := db.SignRootKeyTransition(pin, other, private)
	require.NoError(t, err)
	require.Error(t, rotation.RotateRootAuthority(ctx, stale))
}

// RunRootKeyRotationReplicaAuthority exercises root key rotation replica authority on the supplied native store.
// R3/R6: a signed transition does not preserve a replica's revoked authority.
func RunRootKeyRotationReplicaAuthority(t *testing.T, store db.Storage) {
	for _, denial := range []string{"revoked", "paused", "membership"} {
		t.Run(denial, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "rotation-"+denial)
			require.NoError(t, err)
			binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID, Actor: "company-member", Enabled: true, PushEnabled: true}
			_, err = store.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			public, private, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			rootUID := "00000000000000000000000002"
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, store.PinRootAuthority(ctx, pin))
			config := db.RelayBindingConfig{ProtocolVersion: 1, BindingUID: project.UID, UpstreamInstanceUID: rootUID, AuthorityUID: rootUID, HubPath: []string{rootUID, store.InstanceUID()}, LocalActor: "personal-member", ResetEpoch: 1}
			_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
			require.NoError(t, err)
			nextPublic, _, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			next := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: rootUID, KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic}
			transition, err := db.SignRootKeyTransition(pin, next, private)
			require.NoError(t, err)
			switch denial {
			case "revoked":
				config.UpstreamRevoked = true
				_, err = store.SetRelayBindingConfig(ctx, project.ID, config)
			case "paused":
				binding.Enabled = false
				_, err = store.UpsertFederationBinding(ctx, binding)
			case "membership":
				_, _, err = store.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams"}, "admin")
			}
			require.NoError(t, err)
			require.Error(t, store.RotateRootAuthority(ctx, transition), "denied upstream authority must stop pin mutation")
			retained, err := store.RootAuthority(ctx, project.UID)
			require.NoError(t, err)
			require.Equal(t, pin, retained)
		})
	}
}
