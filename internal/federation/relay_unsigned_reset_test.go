package federation_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R4/A5: negotiated replicas require a verified root checkpoint and an
// authenticated hop translation. Legacy cursor numbers cannot authorize a reset.
func TestRelayRejectUnsignedReset(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, guarded := range []bool{false, true} {
			name := backend + "/unconditional"
			if guarded {
				name = backend + "/legacy_cursor_guard"
			}
			t.Run(name, func(t *testing.T) {
				root := newRelayMatrixNode(t, backend, "company-member")
				personal := newRelayMatrixNode(t, backend, "personal-member")
				project, err := root.store.CreateProject(t.Context(), "shared-project")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
				issue, source, err := personal.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: personal.project.ID, Author: "source-agent", Title: "Keep pending work"})
				require.NoError(t, err)
				before, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				offered, err := personal.store.PendingRelayDeliveries(t.Context(), before.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Len(t, offered, 1)
				if guarded {
					err = personal.store.ResetFederatedProjectIfNoPendingPush(t.Context(), personal.project.ID, 100, 100, personal.store.InstanceUID(), source.ID)
				} else {
					err = personal.store.ResetFederatedProject(t.Context(), personal.project.ID, 100, 100)
				}
				require.ErrorIs(t, err, db.ErrRelayResetRequiresRootProof, "unsigned legacy reset must not clear a negotiated replica")
				retained, err := personal.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, issue, retained)
				after, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
				require.NoError(t, err)
				require.Equal(t, before, after)
				retry, err := personal.store.PendingRelayDeliveries(t.Context(), before.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
				require.NoError(t, err)
				require.Equal(t, offered, retry, "refused reset preserves epoch, prefix and exact retry bytes")
			})
		}
	}
}
