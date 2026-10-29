package dbtest

import (
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
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
	policy, err := store.ProjectAccessPolicy(ctx, unselected.UID)
	require.NoError(t, err)
	policy.Visibility, policy.TeamUIDs = "teams", nil
	policy, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	served, err := resets.CreateRelayReset(ctx, binding, signer)
	if err != nil {
		require.ErrorIs(t, err, db.ErrNotFound, "selected inaccessible peer refuses the whole reset")
		require.Equal(t, db.RelayResetCheckpoint{}, served)
	} else {
		require.NoError(t, db.VerifyRootResetManifest(pin, served.Manifest, served.Snapshot))
		requireResetOmitsIssue(t, served, private)
	}
	policy.Visibility = "all"
	_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
	require.NoError(t, err)
	served, err = resets.CreateRelayReset(ctx, binding, signer)
	require.NoError(t, err)
	require.Positive(t, served.Manifest.HistoryEventID, "legacy checkpoint must be rebuilt with a history boundary")
	require.NoError(t, db.VerifyRootResetManifest(pin, served.Manifest, served.Snapshot))
	events, _, err := db.DecodeRootResetPayload(pin, served.Snapshot)
	require.NoError(t, err)
	found := 0
	for _, event := range events {
		if event.EventUID == unlinkEvent.UID {
			require.Equal(t, remoteEventFromStored(unlinkEvent), event, "authorized original unlink payload and hash remain unchanged")
			found++
		}
	}
	require.Equal(t, 1, found)
	retry, err := resets.CreateRelayReset(ctx, binding, signer)
	require.NoError(t, err)
	require.Equal(t, served, retry)
}

// legacyCheckpointWithEvent models an authentic checkpoint from an earlier
// builder: its signed source bytes include event and it has no history boundary.
func legacyCheckpointWithEvent(t *testing.T, checkpoint db.RelayResetCheckpoint, pin db.RootKeyPin, signingKey ed25519.PrivateKey, event db.RemoteEvent) db.RelayResetCheckpoint {
	t.Helper()
	var sources [][]byte
	require.NoError(t, json.Unmarshal(checkpoint.Snapshot.Events, &sources))
	historical, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	found := 0
	for _, body := range sources {
		source, err := db.DecodeRelaySourceEvent(body)
		require.NoError(t, err)
		if source.EventUID == event.EventUID {
			require.Equal(t, event, source)
			found++
		}
	}
	if found == 0 {
		sources = append(sources, historical)
		found++
	}
	require.Equal(t, 1, found, "legacy source identities must remain unique")
	checkpoint.Snapshot.Events, err = json.Marshal(sources)
	require.NoError(t, err)
	checkpoint.Translation.SnapshotDigest, err = db.RootResetSnapshotDigest(checkpoint.Snapshot)
	require.NoError(t, err)
	checkpoint.Manifest.HistoryEventID = 0
	checkpoint.Manifest, err = db.SignRootResetManifest(checkpoint.Manifest, checkpoint.Snapshot, signingKey)
	require.NoError(t, err)
	require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
	_, _, err = db.DecodeRootResetPayload(pin, checkpoint.Snapshot)
	require.NoError(t, err)
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
