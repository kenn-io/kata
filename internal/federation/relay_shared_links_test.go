package federation_test

import (
	"fmt"
	"io"
	"net/http"
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
				linked, linkEvent, err := root.store.CreateLinkAndEvent(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: root.account},
					db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
				require.NoError(t, err)
				for _, path := range []string{"/federation/relay?stream=events", "/federation/relay/reset"} {
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d%s", root.http.URL, projects[0].ID, path), nil)
					require.NoError(t, err)
					req.Header.Set("Authorization", "Bearer "+hubs[0].credential.Token)
					response, err := root.http.Client().Do(req)
					require.NoError(t, err)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					require.Equal(t, http.StatusConflict, response.StatusCode, string(body))
					require.Contains(t, string(body), "unsupported_event_features")
					require.Contains(t, string(body), db.RelayCrossLinksEventFeature)
				}
				order := []int{0, 1}
				if reverse {
					order = []int{1, 0}
				}
				for _, nodes := range [][]*relayMatrixNode{hubs, leaves} {

					for position, i := range order {
						syncRelayMatrixNode(t, nodes[i])
						if position == 0 && i == 0 {
							local, err := nodes[0].store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
							require.NoError(t, err)
							links, err := nodes[0].store.LinksByIssue(ctx, local.ID)
							require.NoError(t, err)
							require.Empty(t, links)
						}
					}
					_, err := nodes[1].store.IssueByUID(ctx, issues[1].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					stored, err := nodes[0].store.EventsByUIDs(ctx, nodes[0].project.ID, []string{linkEvent.UID})
					require.NoError(t, err)
					require.Len(t, stored, 1)
					require.Equal(t, linkEvent.ContentHash, stored[0].ContentHash)
					require.Equal(t, linkEvent.Payload, stored[0].Payload)
					local, err := nodes[0].store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
					require.NoError(t, err)
					links, err := nodes[0].store.LinksByIssue(ctx, local.ID)
					require.NoError(t, err)
					require.Len(t, links, 1)
					require.Equal(t, issues[1].UID, links[0].ToIssueUID)
					if !reverse && nodes[0] == hubs[0] {
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
						_, err = nodes[0].store.DeleteLinkAndEvent(ctx, links[0], db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: local.ID, Actor: nodes[0].account, FromUID: local.UID, ToUID: peer.UID, FromShortID: local.ShortID, ToShortID: peer.ShortID})
						require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
					}
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
