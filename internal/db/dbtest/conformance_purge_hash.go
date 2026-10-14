package dbtest

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunPurgePreservesSignedPeerEvent verifies a full backup remains replayable
// after purging an issue referenced by a signed issue.links_changed event.
func RunPurgePreservesSignedPeerEvent(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	subject, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Subject task", Author: "example-actor",
	})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Peer task", Author: "example-actor",
	})
	require.NoError(t, err)
	_, linkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: subject.ID, ToIssueID: peer.ID, Type: "blocks", Author: "example-actor",
	}, db.LinkEventParams{
		EventType: "issue.links_changed", EventIssueID: subject.ID,
		FromShortID: subject.ShortID, FromUID: subject.UID,
		ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "example-actor",
	})
	require.NoError(t, err)
	require.NotNil(t, linkEvent.RelatedIssueUID)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: "00000000000000000000000002",
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "example-actor", Enabled: true,
	})
	require.NoError(t, err)
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{
		Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID,
		KeyID: pin.KeyID, EventUID: linkEvent.UID, ContentHash: linkEvent.ContentHash,
		AccountableActor: "example-actor", SourceActor: linkEvent.Actor,
		IngressInstanceUID: pin.AuthorityUID, AcceptedAt: time.Now().UTC(),
		ResetEpoch: 1, Sequence: 1,
	}, privateKey)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, receipt))
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, peer.ID, "example-actor", nil)
	require.NoError(t, err)

	records, err := CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	attribution, ok := store.(db.AttributionStorage)
	require.True(t, ok)
	for record, err := range attribution.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, record)
	}
	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{}),
		"the signed issue.links_changed envelope and its receipt must remain replayable")
}

// RunProjectPurgePreservesSignedPeerEvent verifies project deletion detaches
// local issue foreign keys without rewriting a surviving signed event.
func RunProjectPurgePreservesSignedPeerEvent(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	sourceProject, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	peerProject, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	subject, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: sourceProject.ID, Title: "Subject task", Author: "example-actor",
	})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: peerProject.ID, Title: "Peer task", Author: "example-actor",
	})
	require.NoError(t, err)
	_, linkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: subject.ID, ToIssueID: peer.ID, Type: "blocks", Author: "example-actor",
	}, db.LinkEventParams{
		EventType: "issue.links_changed", EventIssueID: subject.ID,
		FromShortID: subject.ShortID, FromUID: subject.UID,
		ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "example-actor",
	})
	require.NoError(t, err)
	require.NotNil(t, linkEvent.RelatedIssueUID)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: sourceProject.UID, AuthorityUID: "00000000000000000000000002",
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: sourceProject.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: sourceProject.UID, Actor: "example-actor", Enabled: true,
	})
	require.NoError(t, err)
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{
		Version: 1, ProjectUID: sourceProject.UID, AuthorityUID: pin.AuthorityUID,
		KeyID: pin.KeyID, EventUID: linkEvent.UID, ContentHash: linkEvent.ContentHash,
		AccountableActor: "example-actor", SourceActor: linkEvent.Actor,
		IngressInstanceUID: pin.AuthorityUID, AcceptedAt: time.Now().UTC(),
		ResetEpoch: 1, Sequence: 1,
	}, privateKey)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, receipt))
	_, err = store.LeaveFederationReplica(ctx, sourceProject.ID)
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: peerProject.ID, Actor: "example-actor", Force: true,
	})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{
		ProjectID: peerProject.ID, Actor: "example-actor",
	})
	require.NoError(t, err)

	records, err := CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	attribution, ok := store.(db.AttributionStorage)
	require.True(t, ok)
	for record, err := range attribution.ExportAttribution(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, record)
	}
	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{}),
		"project purge must leave the signed issue.links_changed envelope replayable")
}

// RunLiveOnlyBackupPreservesSignedSoftDeletedPeer verifies that omitting a
// soft-deleted peer clears its local foreign key without rewriting the signed
// UID in a retained issue.links_changed event.
func RunLiveOnlyBackupPreservesSignedSoftDeletedPeer(t *testing.T, store db.Storage, backend Backend) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	subject, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Subject task", Author: "example-actor",
	})
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Peer task", Author: "example-actor",
	})
	require.NoError(t, err)
	_, linkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		FromIssueID: subject.ID, ToIssueID: peer.ID, Type: "blocks", Author: "example-actor",
	}, db.LinkEventParams{
		EventType: "issue.links_changed", EventIssueID: subject.ID,
		FromShortID: subject.ShortID, FromUID: subject.UID,
		ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "example-actor",
	})
	require.NoError(t, err)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{
		ProjectUID: project.UID, AuthorityUID: "00000000000000000000000002",
		KeyID: db.RootPublicKeyID(publicKey), PublicKey: publicKey,
	}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Actor: "example-actor", Enabled: true,
	})
	require.NoError(t, err)
	receipt, err := db.SignRootReceipt(db.AttributionReceipt{
		Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID,
		KeyID: pin.KeyID, EventUID: linkEvent.UID, ContentHash: linkEvent.ContentHash,
		AccountableActor: "example-actor", SourceActor: linkEvent.Actor,
		IngressInstanceUID: pin.AuthorityUID, AcceptedAt: time.Now().UTC(),
		ResetEpoch: 1, Sequence: 1,
	}, privateKey)
	require.NoError(t, err)
	require.NoError(t, store.ApplyUpstreamAttribution(ctx, pin, receipt))
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	_, _, changed, err := store.SoftDeleteIssue(ctx, peer.ID, "example-actor")
	require.NoError(t, err)
	require.True(t, changed)

	filter := db.ExportFilter{IncludeDeleted: false}
	records, err := CollectImportRecords(ctx, store, filter)
	require.NoError(t, err)
	attribution, ok := store.(db.AttributionStorage)
	require.True(t, ok)
	for record, err := range attribution.ExportAttribution(ctx, filter) {
		require.NoError(t, err)
		records = append(records, record)
	}
	target := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, target.ImportReplay(ctx, records, db.ImportOptions{}),
		"live-only backup must preserve the signed event hash when omitting its soft-deleted peer")

	var exported db.EventExport
	found := false
	for event, err := range store.ExportEvents(ctx, filter) {
		require.NoError(t, err)
		if event.UID == linkEvent.UID {
			exported, found = event, true
			break
		}
	}
	require.True(t, found, "the historical link event remains in the live-only export")
	require.Nil(t, exported.RelatedIssueID, "the omitted peer's local foreign key cannot be exported")
	require.NotNil(t, exported.RelatedIssueUID)
	require.Equal(t, peer.UID, *exported.RelatedIssueUID, "signed event content must retain its peer UID")
	require.Equal(t, linkEvent.ContentHash, exported.ContentHash, "the exported event keeps its signed content hash")
}
