package federation_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayUnresolvedPeerDeliveryRetry(t *testing.T) {
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
			_, event, err := root.store.CreateLinkAndEvent(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: root.account}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
			require.NoError(t, err)
			syncRelayMatrixNode(t, hub)
			binding, err := leaf.store.FederationBindingByProject(ctx, leaf.project.ID)
			require.NoError(t, err)
			bindingUID := binding.RelayConfig.BindingUID
			var retained db.RelayOutboxExport
			for record, err := range hub.store.ExportRelayState(ctx) {
				require.NoError(t, err)
				if row, ok := record.(*db.RelayOutboxExport); ok && row.BindingUID == bindingUID && row.Stream == db.RelayStreamEvent && row.SourceUID == event.UID {
					retained = *row
				}
			}
			require.NotEmpty(t, retained.Envelope)
			require.False(t, retained.Emitted)
			features := db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures)
			batch, err := hub.store.PendingRelayDeliveries(features, bindingUID, db.RelayStreamEvent, 100)
			require.ErrorIs(t, err, db.ErrRelayReferenceUnprovable)
			require.Empty(t, batch)
			peerHub := &relayMatrixNode{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials}
			root.project = projects[1]
			enrollRelayMatrixReplica(t, root, peerHub, "hub-peer", true)
			syncRelayMatrixNode(t, peerHub)
			source, err := hub.store.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			links, err := hub.store.LinksByIssue(ctx, source.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, issues[1].UID, links[0].ToIssueUID)
			batch, err = hub.store.PendingRelayDeliveries(features, bindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			var expected db.RelayEnvelope
			require.NoError(t, json.Unmarshal([]byte(retained.Envelope), &expected))
			require.Contains(t, batch, expected)
			require.Equal(t, event.ContentHash, expected.SourceHash)
		})
	}
}
