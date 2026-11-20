package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayCrossLinkResetHistory checks reset gating, peer-denial rollback, and unlink history across hops.
func RunRelayCrossLinkResetHistory(t *testing.T, root db.Storage, backend Backend) {
	ctx := t.Context()
	hub := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, hub.Close()) })
	leaf := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, leaf.Close()) })
	fixture := NewRelaySharedAccessFixture(t, root)
	projects, issues, signer := fixture.Projects, fixture.Issues, fixture.Signer
	spokeUID := "00000000000000000000000008"
	unlink := newRemoteEvent(t, projects[0], &issues[0].UID, "issue.unlinked", "member", spokeUID, time.Now().Add(time.Second).UnixMilli(), jsontext.Value(`{"issue_uid":"`+issues[0].UID+`","from_uid":"`+issues[0].UID+`","to_uid":"`+issues[1].UID+`","link_from_uid":"`+issues[0].UID+`","link_to_uid":"`+issues[1].UID+`","type":"blocks"}`))
	unlink.RelatedIssueUID = &issues[1].UID
	unlink.ContentHash = remoteEventHash(t, unlink)
	_, err := root.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[0].ID, SpokeInstanceUID: spokeUID, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: unlink}}})
	require.NoError(t, err)
	peerBinding, peerEpoch := setupRelayHop(t, root, hub, projects[1], "reset-peer-hub", true)
	binding, bindingEpoch := setupRelayHop(t, root, hub, projects[0], "reset-hub", true)
	require.NoError(t, root.MaterializeFederatedProject(ctx, projects[0].ID))
	features := db.WithRelayPeerEventFeatures(ctx, db.SupportedEventFeatures)
	denied := errors.New("peer denied")
	rejectPeer := db.WithProjectAccessTransactionCheck(features, func(_ context.Context, _ db.Transaction, uid string) error {
		if uid == projects[1].UID {
			return denied
		}
		return nil
	})
	syncRelayHop(features, t, root, hub, peerBinding, peerEpoch, db.RelayStreamEvent)
	syncRelayHop(features, t, root, hub, binding, bindingEpoch, db.RelayStreamEvent)
	hubIssue, err := hub.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Empty(t, linkTargetUIDs(t, hub, hubIssue.ID), "forwarded events preserve the direct unlink")
	require.Empty(t, linkTargetUIDs(t, root, issues[0].ID))
	_, _, err = root.CreateLinkAndEvent(db.WithRootAttribution(ctx, signer, "member"), db.CreateLinkParams{FromIssueID: issues[0].ID, ToIssueID: issues[1].ID, Type: "parent", Author: "member"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issues[0].ID, Actor: "member", FromUID: issues[0].UID, ToUID: issues[1].UID, FromShortID: issues[0].ShortID, ToShortID: issues[1].ShortID})
	require.NoError(t, err)
	rootResets := root.(db.RelayResetStore)
	relayState := func(store db.Storage) []db.ImportRecord {
		var records []db.ImportRecord
		for record, err := range store.ExportRelayState(ctx) {
			require.NoError(t, err)
			records = append(records, record)
		}
		return records
	}
	projectEvents := func(store db.Storage, projectID int64) []db.EventExport {
		var events []db.EventExport
		for event, err := range store.ExportEvents(ctx, db.ExportFilter{ProjectID: &projectID, IncludeDeleted: true}) {
			require.NoError(t, err)
			events = append(events, event)
		}
		return events
	}
	checkpointMeta := func(store db.Storage, projectUID string) []db.MetaKV {
		var metadata []db.MetaKV
		for record, err := range store.ExportMeta(ctx) {
			require.NoError(t, err)
			if strings.HasPrefix(record.Key, db.RelayResetMetadataPrefix+projectUID) {
				metadata = append(metadata, record)
			}
		}
		return metadata
	}
	type resetState struct {
		relay  []db.ImportRecord
		events []db.EventExport
		meta   []db.MetaKV
		grants []db.FederationEnrollment
	}
	observe := func(store db.Storage, project db.Project) resetState {
		grants, err := store.ListProjectFederationEnrollments(ctx, project.ID)
		require.NoError(t, err)
		return resetState{
			relay: relayState(store), events: projectEvents(store, project.ID),
			meta: checkpointMeta(store, project.UID), grants: grants,
		}
	}
	before := observe(root, projects[0])
	_, err = rootResets.CreateRelayReset(ctx, binding, signer)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Equal(t, before, observe(root, projects[0]))
	blocked, err := rootResets.CreateRelayReset(rejectPeer, binding, signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, before, observe(root, projects[0]))
	checkpoint, err := rootResets.CreateRelayReset(features, binding, signer)
	require.NoError(t, err)
	public := signer.PrivateKey.Public().(ed25519.PublicKey)
	resetEvents, _, err := db.DecodeRootResetPayload(db.RootKeyPin{ProjectUID: projects[0].UID, AuthorityUID: root.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}, checkpoint.Snapshot)
	require.NoError(t, err)
	require.Contains(t, resetEvents, unlink, "the checkpoint retains the direct unlink")
	require.NotEqual(t, before.events, projectEvents(root, projects[0].ID))
	require.NotEqual(t, before.meta, checkpointMeta(root, projects[0].UID))

	retained := observe(root, projects[0])
	blocked, err = rootResets.CreateRelayReset(rejectPeer, binding, signer)
	require.ErrorIs(t, err, denied)
	require.Equal(t, db.RelayResetCheckpoint{}, blocked)
	require.Equal(t, retained, observe(root, projects[0]))
	_, err = rootResets.CreateRelayReset(ctx, binding, signer)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	retry, err := rootResets.CreateRelayReset(features, binding, signer)
	require.NoError(t, err)
	require.Equal(t, checkpoint, retry)
	hubResets := hub.(db.RelayResetStore)
	require.NoError(t, hubResets.InstallRelayReset(ctx, binding, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation))
	peerCheckpoint, err := rootResets.CreateRelayReset(features, peerBinding, signer)
	require.NoError(t, err)
	require.NoError(t, hubResets.InstallRelayReset(ctx, peerBinding, peerCheckpoint.Manifest, peerCheckpoint.Snapshot, peerCheckpoint.Translation))
	hubIssue, err = hub.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	// The signed reset retains the root-authored parent link.
	require.Equal(t, []string{issues[1].UID}, linkTargetUIDs(t, hub, hubIssue.ID))
	hubProject, err := hub.ProjectByUID(ctx, projects[0].UID)
	require.NoError(t, err)
	downstream, downstreamEpoch := setupRelayHop(t, hub, leaf, hubProject, "reset-leaf", false)
	streams := []string{db.RelayStreamEvent, db.RelayStreamReceipt, db.RelayStreamArtifact}
	syncRelayHop(features, t, hub, nil, downstream, downstreamEpoch, streams...)
	beforeHub := observe(hub, hubProject)
	_, err = hubResets.CreateRelayReset(ctx, downstream, db.RootAttributionSigner{})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Equal(t, beforeHub, observe(hub, hubProject))
	_, err = hubResets.CreateRelayReset(rejectPeer, downstream, db.RootAttributionSigner{})
	require.ErrorIs(t, err, denied)
	require.Equal(t, beforeHub, observe(hub, hubProject))
	forwarded, err := hubResets.CreateRelayReset(features, downstream, db.RootAttributionSigner{})
	require.NoError(t, err)
	require.NotEqual(t, beforeHub.meta, checkpointMeta(hub, hubProject.UID))
	require.Equal(t, checkpoint.Manifest, forwarded.Manifest)
	require.Equal(t, checkpoint.Snapshot, forwarded.Snapshot)
	peerProject, err := hub.ProjectByUID(ctx, projects[1].UID)
	require.NoError(t, err)
	peerDownstream, peerDownstreamEpoch := setupRelayHop(t, hub, leaf, peerProject, "reset-peer-leaf", false)
	syncRelayHop(features, t, hub, nil, peerDownstream, peerDownstreamEpoch, streams...)
	peerForwarded, err := hubResets.CreateRelayReset(features, peerDownstream, db.RootAttributionSigner{})
	require.NoError(t, err)
	leafResets := leaf.(db.RelayResetStore)
	require.NoError(t, leafResets.InstallRelayReset(ctx, peerDownstream, peerForwarded.Manifest, peerForwarded.Snapshot, peerForwarded.Translation))
	require.NoError(t, leafResets.InstallRelayReset(ctx, downstream, forwarded.Manifest, forwarded.Snapshot, forwarded.Translation))
	leafIssue, err := leaf.IssueByUID(ctx, issues[0].UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, []string{issues[1].UID}, linkTargetUIDs(t, leaf, leafIssue.ID))
}
