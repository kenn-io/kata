package federation_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// A root purge travels root -> relay -> relay -> device as a forward of a
// forward. A relay whose own downstream still owes work the root has not
// accepted refuses its installation with the exact error that names that
// downstream, and succeeds once the root receipt has reached it.
func TestRelayRootResetForwardsThroughTwoRelays(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "root-member")
			upper := newRelayMatrixNode(t, backend, "upper-relay-member")
			lower := newRelayMatrixNode(t, backend, "lower-relay-member")
			device := newRelayMatrixNode(t, backend, "device-member")
			sibling := newRelayMatrixNode(t, backend, "sibling-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))
			enrollRelayMatrixReplica(t, root, upper, "upper-alias", true)
			syncRelayMatrixNode(t, upper)
			enrollRelayMatrixReplica(t, upper, lower, "lower-alias", true)
			syncRelayMatrixNode(t, lower)
			enrollRelayMatrixReplica(t, lower, device, "device-alias", false)
			enrollRelayMatrixReplica(t, lower, sibling, "sibling-alias", false)
			kept := createRootIssue(t, root, "Kept issue")
			purged := createRootIssue(t, root, "Purged issue")
			for _, node := range []*relayMatrixNode{upper, lower, device, sibling} {
				syncRelayMatrixNode(t, node)
			}
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept, purged}, nil)

			_, err = root.store.PurgeIssue(t.Context(), purged.ID, "admin", nil)
			require.NoError(t, err)
			for _, node := range []*relayMatrixNode{upper, lower, device, sibling} {
				syncRelayMatrixNode(t, node)
				requireRootIssueSet(t, node, root.project.UID, pin, []db.Issue{kept}, []db.Issue{purged})
			}

			// The root purges again and the upper relay has already forwarded that
			// reset when the sibling pushes its own issue to the lower relay, which
			// queues it for the offline device. The lower relay now owes the device
			// work the root has not accepted yet, so its installation must refuse
			// until the root receipt lands.
			second := createRootIssue(t, root, "Second purge")
			syncRelayMatrixNode(t, upper)
			syncRelayMatrixNode(t, lower)
			_, err = root.store.PurgeIssue(t.Context(), second.ID, "admin", nil)
			require.NoError(t, err)
			syncRelayMatrixNode(t, upper)
			local, _, err := sibling.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sibling.project.ID, Title: "Sibling issue", Author: "sibling-agent"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, sibling)
			lowerBinding, err := lower.store.FederationBindingByProject(t.Context(), lower.project.ID)
			require.NoError(t, err)
			deviceBinding, err := device.store.FederationBindingByProject(t.Context(), device.project.ID)
			require.NoError(t, err)
			err = federation.SyncFederationOnce(t.Context(), lower.store, lowerBinding, lower.credential)
			require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
			require.ErrorContains(t, err, "downstream enrollment "+deviceBinding.RelayConfig.BindingUID+" still holds events deliveries the root has not accepted; let it synchronize or revoke it, then retry")
			retained, err := lower.store.IssueByUID(t.Context(), local.UID, db.IncludeDeletedYes)
			require.NoError(t, err, "a refused installation keeps the relay's projection")
			require.Equal(t, "Sibling issue", retained.Title)

			// The upper relay carries the sibling's issue to the root and brings
			// the receipt back; the lower relay then installs and forwards.
			syncRelayMatrixNode(t, upper)
			syncRelayMatrixNode(t, lower)
			requireRootIssueSet(t, lower, root.project.UID, pin, []db.Issue{kept, retained}, []db.Issue{purged, second})
			syncRelayMatrixNode(t, device)
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept, retained}, []db.Issue{purged, second})
			syncRelayMatrixNode(t, sibling)
			requireRootIssueSet(t, sibling, root.project.UID, pin, []db.Issue{kept, retained}, []db.Issue{purged, second})
			for _, node := range []*relayMatrixNode{device, sibling} {
				quarantines, err := node.store.ActiveFederationQuarantinesByProject(t.Context(), node.project.ID)
				require.NoError(t, err)
				require.Empty(t, quarantines)
			}
		})
	}
}
