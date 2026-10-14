package federation_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// relayResetHierarchy builds root -> relay (serving downstream) -> device and
// synchronizes every hop so each node holds the same verified issues.
func relayResetHierarchy(t *testing.T, backend string) (root, relay, device *relayMatrixNode, pin db.RootKeyPin) {
	t.Helper()
	root = newRelayMatrixNode(t, backend, "root-member")
	relay = newRelayMatrixNode(t, backend, "relay-member")
	device = newRelayMatrixNode(t, backend, "device-member")
	project, err := root.store.CreateProject(t.Context(), "shared-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	pin = db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, root.store.PinRootAuthority(t.Context(), pin))
	enrollRelayMatrixReplica(t, root, relay, "relay-alias", true)
	syncRelayMatrixNode(t, relay)
	enrollRelayMatrixReplica(t, relay, device, "device-alias", false)
	syncRelayMatrixNode(t, device)
	return root, relay, device, pin
}

func createRootIssue(t *testing.T, root *relayMatrixNode, title string) db.Issue {
	t.Helper()
	issue, _, err := root.store.CreateIssue(db.WithRootAttribution(t.Context(), root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Author: "source-agent", Title: title})
	require.NoError(t, err)
	return issue
}

// requireRootIssueSet checks that a node holds exactly the root's verified
// issues, with creation proof that verifies against the root pin.
func requireRootIssueSet(t *testing.T, node *relayMatrixNode, projectUID string, pin db.RootKeyPin, present []db.Issue, absent []db.Issue) {
	t.Helper()
	for _, issue := range present {
		got, err := node.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err, "%s must hold %q", node.account, issue.Title)
		require.Equal(t, issue.Title, got.Title)
		require.Equal(t, "verified", got.Verification)
		require.Equal(t, "root-member", got.AccountableActor)
		proof, err := node.store.EntityAttribution(t.Context(), projectUID, "issue", issue.UID)
		require.NoError(t, err)
		require.NoError(t, db.VerifyRootReceipt(pin, proof))
	}
	for _, issue := range absent {
		_, err := node.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
		require.ErrorIs(t, err, db.ErrNotFound, "%s must not hold %q", node.account, issue.Title)
	}
}

// A root purge must not force an operator to dismantle a healthy hierarchy:
// the relay installs the root reset while keeping its downstream grant, and
// the device installs the forwarded checkpoint on its next sync.
func TestRelayRootResetKeepsDownstreamHierarchy(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, relay, device, pin := relayResetHierarchy(t, backend)
			kept := createRootIssue(t, root, "Kept issue")
			purged := createRootIssue(t, root, "Purged issue")
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, device)
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept, purged}, nil)

			_, err := root.store.PurgeIssue(t.Context(), purged.ID, "admin", nil)
			require.NoError(t, err)
			syncRelayMatrixNode(t, relay)
			requireRootIssueSet(t, relay, root.project.UID, pin, []db.Issue{kept}, []db.Issue{purged})
			deviceBinding, err := device.store.FederationBindingByProject(t.Context(), device.project.ID)
			require.NoError(t, err)
			bootstrap := relay.store.(db.RelayResetBootstrapStore)
			required, err := bootstrap.RelayEnrollmentNeedsReset(t.Context(), deviceBinding.RelayConfig.BindingUID)
			require.NoError(t, err)
			require.True(t, required, "the relay advertises the forwarded reset to its downstream")
			syncRelayMatrixNode(t, device)
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept}, []db.Issue{purged})
			required, err = bootstrap.RelayEnrollmentNeedsReset(t.Context(), deviceBinding.RelayConfig.BindingUID)
			require.NoError(t, err)
			require.False(t, required, "the downstream grant stays active in its new epoch")

			after := createRootIssue(t, root, "After the reset")
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, device)
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept, after}, []db.Issue{purged})
			quarantines, err := device.store.ActiveFederationQuarantinesByProject(t.Context(), device.project.ID)
			require.NoError(t, err)
			require.Empty(t, quarantines)
		})
	}
}

// A delivery the device never acknowledged is superseded by the forwarded
// checkpoint: the device must not resurrect an issue the root purged.
func TestRelayRootResetSupersedesUnacknowledgedDownstreamDeliveries(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root, relay, device, pin := relayResetHierarchy(t, backend)
			kept := createRootIssue(t, root, "Kept issue")
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, device)

			transient := createRootIssue(t, root, "Transient issue")
			syncRelayMatrixNode(t, relay)
			requireRootIssueSet(t, relay, root.project.UID, pin, []db.Issue{kept, transient}, nil)
			_, err := root.store.PurgeIssue(t.Context(), transient.ID, "admin", nil)
			require.NoError(t, err)
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, device)
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept}, []db.Issue{transient})
			syncRelayMatrixNode(t, device)
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept}, []db.Issue{transient})

			after := createRootIssue(t, root, "After the reset")
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, device)
			requireRootIssueSet(t, device, root.project.UID, pin, []db.Issue{kept, after}, []db.Issue{transient})
			quarantines, err := device.store.ActiveFederationQuarantinesByProject(t.Context(), device.project.ID)
			require.NoError(t, err)
			require.Empty(t, quarantines)
		})
	}
}
