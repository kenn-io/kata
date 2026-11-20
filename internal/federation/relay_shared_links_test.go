package federation_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Use the existing real HTTP matrix nodes and transport, including both peer
// arrival orders at each hop; no special reference or projection fixture.
func TestRelaySharedLinksAcrossHops(t *testing.T) {
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
				_, _, err := root.store.CreateLinkAndEvent(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: root.account},
					db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
				require.NoError(t, err)
				rootCtx := db.WithRootAttribution(ctx, root.signer, root.account)
				newParent, _, err := root.store.CreateIssue(rootCtx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "New parent", Author: root.account})
				require.NoError(t, err)
				linkEvent := func(eventType string, parent db.Issue) db.LinkEventParams {
					return db.LinkEventParams{EventType: eventType, EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: parent.UID, FromShortID: issues[0].ShortID, ToShortID: parent.ShortID}
				}
				oldLink, _, err := root.store.CreateLinkAndEvent(rootCtx, db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "parent", Author: root.account}, linkEvent("issue.linked", issues[1]))
				require.NoError(t, err)
				order := []int{0, 1}
				if reverse {
					order = []int{1, 0}
				}
				for _, nodes := range [][]*relayMatrixNode{hubs, leaves} {
					for _, i := range order {
						syncRelayMatrixNode(t, nodes[i])
					}
					links, err := nodes[0].store.LinksByIssue(ctx, issueIDForRelayMatrix(t, nodes[0], issues[0].UID))
					require.NoError(t, err)
					require.Len(t, links, 2)
					for _, link := range links {
						require.Equal(t, issues[1].UID, link.ToIssueUID)
					}
				}
				_, err = root.store.(db.ParentLinkReplacementStorage).ReplaceParentAndEvents(rootCtx, db.ReplaceParentAndEventsParams{
					ExpectedParentLinkID: oldLink.ID, ExpectedParentIssueID: issues[1].ID,
					Link:        db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: newParent.ID, Type: "parent", Author: root.account},
					UnlinkEvent: linkEvent("issue.unlinked", issues[1]), LinkEvent: linkEvent("issue.linked", newParent),
				})
				require.NoError(t, err)
				for _, nodes := range [][]*relayMatrixNode{hubs, leaves} {
					for _, i := range order {
						syncRelayMatrixNode(t, nodes[i])
					}
					childID := issueIDForRelayMatrix(t, nodes[0], issues[0].UID)
					parent, err := nodes[0].store.ParentOf(ctx, childID)
					require.NoError(t, err)
					require.Equal(t, newParent.UID, parent.ToIssueUID)
					links, err := nodes[0].store.LinksByIssue(ctx, childID)
					require.NoError(t, err)
					require.Len(t, links, 2)
				}
			})
		}
	}
}
