package federation_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Use the existing real HTTP matrix nodes and transport, including both peer
// arrival orders at each hop; no special reference or projection fixture.
func TestRelaySharedAccessAcrossHops(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reverse=%v", backend, reverse), func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member")
				hub := newRelayMatrixNode(t, backend, "hub-member")
				leaf := newRelayMatrixNode(t, backend, "leaf-member")
				roots := []*relayMatrixNode{root, root}
				hubs := []*relayMatrixNode{{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials},
					{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials}}
				leaves := []*relayMatrixNode{{store: leaf.store, http: leaf.http, signer: leaf.signer, userToken: leaf.userToken, account: leaf.account, credentials: leaf.credentials},
					{store: leaf.store, http: leaf.http, signer: leaf.signer, userToken: leaf.userToken, account: leaf.account, credentials: leaf.credentials}}
				var projects [2]db.Project
				var issues [2]db.Issue
				for i, name := range []string{"first-shared", "second-shared"} {
					project, err := root.store.CreateProject(ctx, name)
					require.NoError(t, err)
					projects[i] = project
					_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
					require.NoError(t, err)
					root.project = project
					enrollRelayMatrixReplica(t, roots[i], hubs[i], name+"-hub", true)
					syncRelayMatrixNode(t, hubs[i])
					enrollRelayMatrixReplica(t, hubs[i], leaves[i], name+"-leaf", false)
					syncRelayMatrixNode(t, leaves[i])
					issues[i], _, err = root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: "assistant", Title: "Shared endpoint"})
					require.NoError(t, err)
				}
				linked, _, err := root.store.CreateLinkAndEvent(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: root.account},
					db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
				require.NoError(t, err)
				order := []int{0, 1}
				if reverse {
					order = []int{1, 0}
				}
				for _, nodes := range [][]*relayMatrixNode{hubs, leaves} {

					if nodes[0] == leaves[0] {
						upstream, err := hubs[0].store.FederationBindingByProject(ctx, hubs[0].project.ID)
						require.NoError(t, err)
						checkpoint, err := root.store.(db.RelayResetStore).CreateRelayReset(ctx, upstream.RelayConfig.BindingUID, root.signer)
						require.NoError(t, err)
						syncRelayMatrixNode(t, hubs[0])
						// Sync may refresh the root baseline before downloading a reset;
						// compare the forwarded bytes with the checkpoint it actually installed.
						checkpoint, err = root.store.(db.RelayResetStore).CreateRelayReset(ctx, upstream.RelayConfig.BindingUID, root.signer)
						require.NoError(t, err)
						downstream, err := leaves[0].store.FederationBindingByProject(ctx, leaves[0].project.ID)
						require.NoError(t, err)
						policy, err := hub.store.ProjectAccessPolicy(ctx, projects[1].UID)
						require.NoError(t, err)
						policy.Visibility = "teams"
						policy.TeamUIDs = nil
						_, _, err = hub.store.SetProjectAccessPolicy(ctx, policy, "admin")
						require.NoError(t, err)
						_, err = hub.store.(db.RelayResetStore).CreateRelayReset(ctx, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
						require.Error(t, err, "prepared forwarded checkpoint rechecks the child account's peer access")
						policy, err = hub.store.ProjectAccessPolicy(ctx, projects[1].UID)
						require.NoError(t, err)
						policy.Visibility = "all"
						_, _, err = hub.store.SetProjectAccessPolicy(ctx, policy, "admin")
						require.NoError(t, err)
						forwarded, err := hub.store.(db.RelayResetStore).CreateRelayReset(ctx, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
						require.NoError(t, err)
						require.Equal(t, checkpoint.Manifest, forwarded.Manifest)
						require.Equal(t, checkpoint.Snapshot, forwarded.Snapshot, "forwarding preserves exact signed bodies")
					}
					for _, i := range order {
						syncRelayMatrixNode(t, nodes[i])
					}
					local, err := nodes[0].store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					links, err := nodes[0].store.LinksByIssue(ctx, local.ID)
					require.NoError(t, err)
					require.Len(t, links, 1)
					require.Equal(t, issues[1].UID, links[0].ToIssueUID)
					peer, err := nodes[1].store.IssueByUID(ctx, issues[1].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					_, err = nodes[0].store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: local.ID, ToIssueID: peer.ID, Type: "related", Author: nodes[0].account})
					require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
					title := "Replica edit remains local-first"
					_, err = nodes[0].store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: local.ID, Actor: nodes[0].account, Title: &title, AddRelated: []int64{peer.ID}})
					require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
					unchanged, err := nodes[0].store.IssueByID(ctx, local.ID)
					require.NoError(t, err)
					require.Equal(t, local.Title, unchanged.Title)
					_, err = nodes[0].store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: local.ID, Actor: nodes[0].account, Title: &title})
					require.NoError(t, err)
					require.ErrorIs(t, nodes[0].store.DeleteLinkByID(ctx, links[0].ID), db.ErrFederationCrossProjectBoundary)
				}
				_, err = root.store.DeleteLinkAndEvent(ctx, linked, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
				require.NoError(t, err)
				for _, nodes := range [][]*relayMatrixNode{hubs, leaves} {
					for _, i := range order {
						syncRelayMatrixNode(t, nodes[i])
					}
					local, err := nodes[0].store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					links, err := nodes[0].store.LinksByIssue(ctx, local.ID)
					require.NoError(t, err)
					require.Empty(t, links)
				}
			})
		}
	}
}
