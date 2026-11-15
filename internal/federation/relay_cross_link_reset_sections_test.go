package federation_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayCrossLinkResetSections(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		for _, section := range []string{"events", "entities"} {
			t.Run(fmt.Sprintf("%s/%s", backend, section), func(t *testing.T) {
				ctx := t.Context()
				root := newRelayMatrixNode(t, backend, "root-member")
				hub := newRelayMatrixNode(t, backend, "hub-member")
				leaf := newRelayMatrixNode(t, backend, "leaf-member")
				var projects [2]db.Project
				var issues [2]db.Issue
				for i, name := range []string{"reset-source", "reset-peer"} {
					project, err := root.store.CreateProject(ctx, name)
					require.NoError(t, err)
					projects[i] = project
					_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
					require.NoError(t, err)
					public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
					require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
					issues[i], _, err = root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Author: root.account, Title: name})
					require.NoError(t, err)
				}
				params := db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "blocks", Author: root.account}
				if section == "events" {
					link, _, err := root.store.CreateLinkAndEvent(db.WithRootAttribution(ctx, root.signer, root.account), params, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: root.account, FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
					require.NoError(t, err)
					require.NoError(t, root.store.DeleteLinkByID(ctx, link.ID))
				} else {
					_, err := root.store.CreateLink(ctx, params)
					require.NoError(t, err)
				}
				root.project = projects[1]
				peerHub := &relayMatrixNode{store: hub.store, http: hub.http, signer: hub.signer, userToken: hub.userToken, account: hub.account, credentials: hub.credentials}
				enrollRelayMatrixReplica(t, root, peerHub, "reset-peer-hub", true)
				syncRelayMatrixNode(t, peerHub)
				root.project = projects[0]
				enrollRelayMatrixReplica(t, root, hub, "reset-hub", true)
				binding, err := hub.store.FederationBindingByProject(ctx, hub.project.ID)
				require.NoError(t, err)
				rootResets := root.store.(db.RelayResetStore)
				relayState := func(store db.Storage) []db.ImportRecord {
					var records []db.ImportRecord
					for record, err := range store.ExportRelayState(ctx) {
						require.NoError(t, err)
						records = append(records, record)
					}
					return records
				}
				features := db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures)
				denied := errors.New("peer denied")
				rejectPeer := db.WithProjectAccessTransactionCheck(features, func(_ context.Context, _ db.Transaction, uid string) error {
					if uid == projects[1].UID {
						return denied
					}
					return nil
				})
				before := relayState(root.store)
				_, err = rootResets.CreateRelayReset(ctx, binding.RelayConfig.BindingUID, root.signer)
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				_, err = rootResets.CreateRelayReset(rejectPeer, binding.RelayConfig.BindingUID, root.signer)
				require.ErrorIs(t, err, denied)
				require.Equal(t, before, relayState(root.store))
				checkpoint, err := rootResets.CreateRelayReset(features, binding.RelayConfig.BindingUID, root.signer)
				require.NoError(t, err)
				for name, raw := range map[string][]byte{"events": checkpoint.Snapshot.Events, "entities": checkpoint.Snapshot.Entities} {
					var bodies [][]byte
					require.NoError(t, json.Unmarshal(raw, &bodies))
					found := false
					for _, body := range bodies {
						event, err := db.DecodeRelaySourceEvent(body)
						require.NoError(t, err)
						found = found || strings.Contains(string(event.Payload), issues[1].UID)
					}
					require.Equal(t, name == section, found, "crossing belongs only to the selected section")
				}
				_, err = rootResets.CreateRelayReset(ctx, binding.RelayConfig.BindingUID, root.signer)
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				_, err = rootResets.CreateRelayReset(rejectPeer, binding.RelayConfig.BindingUID, root.signer)
				require.ErrorIs(t, err, denied)
				retry, err := rootResets.CreateRelayReset(features, binding.RelayConfig.BindingUID, root.signer)
				require.NoError(t, err)
				require.Equal(t, checkpoint, retry)
				hubResets := hub.store.(db.RelayResetStore)
				require.NoError(t, hubResets.InstallRelayReset(ctx, binding.RelayConfig.BindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation))
				enrollRelayMatrixReplica(t, hub, leaf, "reset-leaf", false)
				syncRelayMatrixNode(t, leaf)
				downstream, err := leaf.store.FederationBindingByProject(ctx, leaf.project.ID)
				require.NoError(t, err)
				_, err = hubResets.CreateRelayReset(ctx, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				forwarded, err := hubResets.CreateRelayReset(features, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
				require.NoError(t, err)
				require.Equal(t, checkpoint.Manifest, forwarded.Manifest)
				require.Equal(t, checkpoint.Snapshot, forwarded.Snapshot)
				before = relayState(hub.store)
				_, err = hubResets.CreateRelayReset(rejectPeer, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
				require.ErrorIs(t, err, denied)
				require.Equal(t, before, relayState(hub.store))
				retry, err = hubResets.CreateRelayReset(features, downstream.RelayConfig.BindingUID, db.RootAttributionSigner{})
				require.NoError(t, err)
				require.Equal(t, forwarded, retry)
			})
		}
	}
}
