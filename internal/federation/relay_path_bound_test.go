package federation_test

import (
	"crypto/ed25519"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayCrossBranchDeliveryAtMaximumDepth(t *testing.T) {
	root := newRelayMatrixNode(t, "sqlite", "hub-member-root")
	project, err := root.store.CreateProject(t.Context(), "cross-branch-project")
	require.NoError(t, err)
	root.project = project
	_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
	require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))

	makeBranch := func(side string) ([]*relayMatrixNode, *relayMatrixNode) {
		hubs := []*relayMatrixNode{root}
		parent := root
		for depth := 2; depth <= db.MaxRelayHubs; depth++ {
			child := newRelayMatrixNode(t, "sqlite", fmt.Sprintf("hub-member-%s-%d", side, depth))
			enrollRelayMatrixReplica(t, parent, child, fmt.Sprintf("%s-hub-%d", side, depth), true)
			hubs = append(hubs, child)
			parent = child
		}
		leaf := newRelayMatrixNode(t, "sqlite", "leaf-member-"+side)
		enrollRelayMatrixReplica(t, parent, leaf, side+"-leaf", false)
		return hubs, leaf
	}

	sourceHubs, sourceLeaf := makeBranch("source")
	destinationHubs, destinationLeaf := makeBranch("destination")
	issue, sourceEvent, err := sourceLeaf.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sourceLeaf.project.ID, Author: "source-assistant", Title: "Cross-branch delivery"})
	require.NoError(t, err)

	// Send the source up the deepest valid branch, through the shared root, and
	// down a second deepest branch. The maximum emitted path is the source leaf
	// plus both eight-hub paths with their shared root counted once.
	syncRelayMatrixNode(t, sourceLeaf)
	for depth := len(sourceHubs) - 1; depth > 0; depth-- {
		syncRelayMatrixNode(t, sourceHubs[depth])
	}
	for depth := 1; depth < len(destinationHubs); depth++ {
		syncRelayMatrixNode(t, destinationHubs[depth])
	}
	leafBinding, err := destinationLeaf.store.FederationBindingByProject(t.Context(), destinationLeaf.project.ID)
	require.NoError(t, err)
	pending, err := destinationHubs[len(destinationHubs)-1].store.PendingRelayDeliveries(t.Context(), leafBinding.RelayConfig.BindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	var issueDelivery *db.RelayEnvelope
	for i := range pending {
		if pending[i].SourceUID == sourceEvent.UID {
			issueDelivery = &pending[i]
			break
		}
	}
	require.NotNil(t, issueDelivery, "the source issue is pending for the final leaf")
	require.Equal(t, 2*db.MaxRelayHubs, len(issueDelivery.Path), "the deepest cross-branch delivery uses the full visited-path bound")
	syncRelayMatrixNode(t, destinationLeaf)

	mirrored, err := destinationLeaf.store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, issue.UID, mirrored.UID)
}
