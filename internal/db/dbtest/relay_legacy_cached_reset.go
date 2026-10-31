package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// RunRelayLegacyCachedResetPrivacy checks that a root rebuilds a retained reset
// checkpoint written before history boundaries existed. Such a checkpoint has
// no history_event_id and can carry a historical event whose endpoint is in an
// unselected project. The checkpoint here is already activated, so no newer
// event forces a rebuild; only the missing boundary can. writeMeta replaces one
// daemon-local metadata value in the backend under test.
func RunRelayLegacyCachedResetPrivacy(t *testing.T, store db.Storage, writeMeta func(context.Context, string, string) error) {
	ctx := t.Context()
	selected, err := store.CreateProject(ctx, "selected-project")
	require.NoError(t, err)
	unselected, err := store.CreateProject(ctx, "unselected-project")
	require.NoError(t, err)
	for _, p := range []db.Project{selected, unselected} {
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleHub, HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true})
		require.NoError(t, err)
	}
	from, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: selected.ID, Author: "member", Title: "Selected link source"})
	require.NoError(t, err)
	private, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: unselected.ID, Author: "member", Title: "Private link endpoint"})
	require.NoError(t, err)
	link, err := store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: from.ID, ToIssueID: private.ID, Type: "blocks", Author: "member"})
	require.NoError(t, err)
	unlinkEvent, err := store.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: from.ID, Actor: "member", FromUID: from.UID, ToUID: private.UID, FromShortID: from.ShortID, ToShortID: private.ShortID})
	require.NoError(t, err)

	public, signingKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pin := db.RootKeyPin{ProjectUID: selected.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
	require.NoError(t, store.PinRootAuthority(ctx, pin))
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		Actor: "member", AdminActor: "admin",
		PlaintextToken: "legacy-reset-parent-test-token", // #nosec G101 -- synthetic API token used only in this test.
	})
	require.NoError(t, err)
	admitted, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: selected.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006",
		ProtocolVersion: db.RelayProtocolVersion, Token: "legacy-reset-test-token", // #nosec G101 -- synthetic relay token used only in this test.
	})
	require.NoError(t, err)
	binding := admitted.Enrollment.RelayBindingUID
	resets, ok := store.(db.RelayResetStore)
	require.True(t, ok)
	signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: signingKey}
	checkpoint, err := resets.CreateRelayReset(ctx, binding, signer)
	require.NoError(t, err)
	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), binding, db.RelayStreamEvent, 1024)
	require.NoError(t, err, "the spoke activates the prepared checkpoint")

	legacy := legacyCheckpointWithEvent(t, checkpoint, pin, signingKey, remoteEventFromStored(unlinkEvent))
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, writeMeta(ctx, db.RelayResetMetadataPrefix+selected.UID+"."+binding, string(raw)))
	served, err := resets.CreateRelayReset(ctx, binding, signer)
	require.NoError(t, err)
	requireResetOmitsIssue(t, served, private)
}

// RunRelayCachedResetLiveBoundary models an activated checkpoint signed before
// current-state boundary validation existed, with a nonzero history cursor.
// No newer event can force a rebuild; the live boundary must prevent disclosure.
func RunRelayCachedResetLiveBoundary(t *testing.T, store db.Storage, writeMeta func(context.Context, string, string) error) {
	ctx := t.Context()
	fixture := newRelayLinkBoundaryFixture(t, store, "cached-live-boundary")
	private, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: fixture.sibling.ID, Title: "Private endpoint", Author: "member"})
	require.NoError(t, err)
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{
		ProjectID: fixture.project.ID, ParentTokenID: fixture.parentTokenID,
		SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion,
	})
	require.NoError(t, err)
	binding := grant.Enrollment.RelayBindingUID
	resets := store.(db.RelayResetStore)
	checkpoint, err := resets.CreateRelayReset(ctx, binding, fixture.signer)
	require.NoError(t, err)
	_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), binding, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	legacyUID, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	event := newRemoteEvent(t, fixture.project, &issueUID, "issue.created", "member", legacyUID, 600, jsontext.Value(`{"uid":"`+issueUID+`","title":"Legacy live boundary","body":"","author":"member","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z","links":[{"type":"blocks","to_issue_uid":"`+private.UID+`","author":"member"}]}`))
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID: fixture.project.ID, SpokeInstanceUID: legacyUID,
		Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}},
	})
	require.NoError(t, err)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, []string{private.UID}, linkTargetUIDs(t, store, issue.ID))
	pin, err := store.RootAuthority(ctx, fixture.project.UID)
	require.NoError(t, err)
	legacy := legacyCheckpointWithEvent(t, checkpoint, pin, fixture.signer.PrivateKey, event)
	for stored, err := range store.ExportEvents(ctx, db.ExportFilter{ProjectID: &fixture.project.ID}) {
		require.NoError(t, err)
		legacy.Manifest.HistoryEventID = max(legacy.Manifest.HistoryEventID, stored.ID)
	}
	require.Positive(t, legacy.Manifest.HistoryEventID)
	legacy.Manifest, err = db.SignRootResetManifest(legacy.Manifest, legacy.Snapshot, fixture.signer.PrivateKey)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(pin, legacy.Manifest, legacy.Snapshot))
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, writeMeta(ctx, db.RelayResetMetadataPrefix+fixture.project.UID+"."+binding, string(raw)))
	served, err := resets.CreateRelayReset(ctx, binding, fixture.signer)
	require.ErrorIs(t, err, db.ErrFederationCrossProjectBoundary)
	require.Empty(t, served.Manifest.SnapshotUID, "never disclose the old signed private checkpoint")
	require.Equal(t, []string{private.UID}, linkTargetUIDs(t, store, issue.ID), "refusal preserves the owner's link")
	links, err := store.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	for _, link := range links {
		unlink, err := store.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{
			EventType: "issue.unlinked", EventIssueID: private.ID, Actor: "member",
			FromUID: issue.UID, ToUID: private.UID, FromShortID: issue.ShortID, ToShortID: private.ShortID,
		})
		require.NoError(t, err)
		require.Equal(t, fixture.sibling.ID, unlink.ProjectID, "unlinking through the private endpoint advances only its history")
	}
	var latest int64
	for stored, err := range store.ExportEvents(ctx, db.ExportFilter{ProjectID: &fixture.project.ID}) {
		require.NoError(t, err)
		latest = max(latest, stored.ID)
	}
	require.Equal(t, legacy.Manifest.HistoryEventID, latest)
	served, err = resets.CreateRelayReset(ctx, binding, fixture.signer)
	require.NoError(t, err)
	requireResetOmitsIssue(t, served, private)
}

// legacyCheckpointWithEvent models an authentic checkpoint from an earlier
// builder: its signed source bytes include event and it has no history boundary.
func legacyCheckpointWithEvent(t *testing.T, checkpoint db.RelayResetCheckpoint, pin db.RootKeyPin, signingKey ed25519.PrivateKey, event db.RemoteEvent) db.RelayResetCheckpoint {
	t.Helper()
	var sources [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Events, &sources))
	historical, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	sources = append(sources, historical)
	checkpoint.Snapshot.Events, err = json.Marshal(sources)
	require.NoError(t, err)
	checkpoint.Translation.SnapshotDigest, err = db.RootResetSnapshotDigest(checkpoint.Snapshot)
	require.NoError(t, err)
	checkpoint.Manifest.HistoryEventID = 0
	checkpoint.Manifest, err = db.SignRootResetManifest(checkpoint.Manifest, checkpoint.Snapshot, signingKey)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	return checkpoint
}

func requireResetOmitsIssue(t *testing.T, checkpoint db.RelayResetCheckpoint, issue db.Issue) {
	t.Helper()
	for _, section := range [][]byte{checkpoint.Snapshot.Events, checkpoint.Snapshot.Entities} {
		var bodies [][]byte
		require.NoError(t, json.Unmarshal(section, &bodies))
		for _, body := range bodies {
			source, err := db.DecodeRelaySourceEvent(body)
			require.NoError(t, err)
			require.NotContains(t, string(source.Payload), issue.UID, "signed reset cannot disclose an excluded historical endpoint")
			require.NotContains(t, string(source.Payload), issue.ShortID, "signed reset cannot disclose an excluded historical short ID")
			if source.RelatedIssueUID != nil {
				require.NotEqual(t, issue.UID, *source.RelatedIssueUID)
			}
		}
	}
}
