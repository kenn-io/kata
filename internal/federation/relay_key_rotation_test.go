package federation_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R3/A5: the ordinary enrolled connection forwards an unchanged signed key
// transition from root through a personal hub to a leaf. Old proof stays valid.
func TestRelaySignedKeyRotationPropagates(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			personal := newRelayMatrixNode(t, backend, "personal-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			original := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, root.store.PinRootAuthority(t.Context(), original))
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
			issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Keep the first key's creation proof"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, personal)
			syncRelayMatrixNode(t, leaf)
			oldProof, err := root.store.EntityAttribution(t.Context(), project.UID, "issue", issue.UID)
			require.NoError(t, err)
			nextPublic, nextPrivate, err := ed25519.GenerateKey(nil)
			require.NoError(t, err)
			next := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: original.AuthorityUID, KeyID: db.RootPublicKeyID(nextPublic), PublicKey: nextPublic}
			transition, err := db.SignRootKeyTransition(original, next, root.signer.PrivateKey)
			require.NoError(t, err)
			require.NoError(t, root.store.RotateRootAuthority(t.Context(), transition))
			root.signer.PrivateKey = nextPrivate
			comment, _, err := root.store.CreateComment(db.WithRootAttribution(t.Context(), db.RootAttributionSigner{AuthorityUID: original.AuthorityUID, PrivateKey: nextPrivate}, root.account), db.CreateCommentParams{IssueID: issue.ID, Author: "source-agent", Body: "Proof issued by the replacement key"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, personal)
			syncRelayMatrixNode(t, leaf)
			for _, node := range []*relayMatrixNode{root, personal, leaf} {
				pin, err := node.store.RootAuthority(t.Context(), project.UID)
				require.NoError(t, err)
				require.Equal(t, next, pin)
				history, err := node.store.RootKeyTransitions(t.Context(), project.UID)
				require.NoError(t, err)
				require.Equal(t, []db.RootKeyTransition{transition}, history)
				proof, err := node.store.EntityAttribution(t.Context(), project.UID, "issue", issue.UID)
				require.NoError(t, err)
				require.Equal(t, oldProof, proof)
				require.NoError(t, db.VerifyRootReceipt(original, proof))
				proof, err = node.store.EntityAttribution(t.Context(), project.UID, "comment", comment.UID)
				require.NoError(t, err)
				require.NoError(t, db.VerifyRootReceipt(next, proof))
			}
		})
	}
}
