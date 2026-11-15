package federation_test

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayDeferredLinkDifferentRoot(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member")
			hub := newRelayMatrixNode(t, backend, "hub-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			var projects [2]db.Project
			var issues [2]db.Issue
			for i, name := range []string{"source", "peer"} {
				project, err := root.store.CreateProject(ctx, name)
				require.NoError(t, err)
				projects[i] = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				issues[i], _, err = root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: root.account, Title: name})
				require.NoError(t, err)
			}
			root.project = projects[0]
			enrollRelayMatrixReplica(t, root, hub, "hub-source", true)
			syncRelayMatrixNode(t, hub)
			enrollRelayMatrixReplica(t, hub, leaf, "leaf-source", false)
			syncRelayMatrixNode(t, leaf)
			_, _, err := root.store.CreateLinkAndEvent(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: root.account}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
			require.NoError(t, err)
			for _, node := range []*relayMatrixNode{hub, leaf} {
				syncRelayMatrixNode(t, node)
			}
			other := newRelayMatrixNode(t, backend, "other-member")
			public := other.signer.PrivateKey.Public().(ed25519.PublicKey)
			for _, node := range []*relayMatrixNode{hub, leaf} {
				peerProject, err := node.store.CreateProjectWithUID(ctx, "other-peer", projects[1].UID)
				require.NoError(t, err)
				require.NoError(t, node.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: peerProject.UID, AuthorityUID: other.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				sourceBinding, err := node.store.FederationBindingByProject(ctx, node.project.ID)
				require.NoError(t, err)
				_, _, err = node.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peerProject.ID, UID: issues[1].UID, Author: node.account, Title: "Different root"})
				require.NoError(t, err)
				_, err = node.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: peerProject.ID, Role: db.FederationRoleSpoke, HubURL: sourceBinding.HubURL, HubProjectID: projects[1].ID, HubProjectUID: peerProject.UID, Actor: node.account, Enabled: true})
				require.NoError(t, err)
				require.NoError(t, node.store.MaterializeFederatedProject(ctx, node.project.ID))
				peer, err := node.store.IssueByUID(ctx, issues[1].UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				require.Equal(t, peerProject.ID, peer.ProjectID)
				source, err := node.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				links, err := node.store.LinksByIssue(ctx, source.ID)
				require.NoError(t, err)
				require.Empty(t, links)
			}
		})
	}
}
