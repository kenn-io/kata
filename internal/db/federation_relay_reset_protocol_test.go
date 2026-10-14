package db_test

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRelayLocalWorkBlocksReset(t *testing.T) {
	awaiting := []db.RelayEventRef{{EventUID: "ev-1", ContentHash: "h1"}, {EventUID: "ev-2", ContentHash: "h2"}}
	require.NoError(t, db.RelayLocalWorkBlocksReset(nil, nil), "nothing awaiting a receipt never blocks")
	require.ErrorIs(t, db.RelayLocalWorkBlocksReset(awaiting, nil), db.ErrFederationResetBlockedByPendingPush, "a lifecycle check installs no checkpoint, so every awaiting event blocks")
	require.NoError(t, db.RelayLocalWorkBlocksReset(awaiting, map[string]string{"ev-1": "h1", "ev-2": "h2"}), "the checkpoint attests both events")
	require.ErrorIs(t, db.RelayLocalWorkBlocksReset(awaiting, map[string]string{"ev-1": "h1"}), db.ErrFederationResetBlockedByPendingPush, "one unattested event blocks")
	require.ErrorIs(t, db.RelayLocalWorkBlocksReset(awaiting, map[string]string{"ev-1": "h1", "ev-2": "other"}), db.ErrFederationResetBlockedByPendingPush, "a receipt for different content does not attest the event")
}

func TestRelayActivationRequested(t *testing.T) {
	activate, err := db.RelayActivationRequested(0, 4)
	require.NoError(t, err)
	require.False(t, activate, "a request without an epoch stays in the grant's namespace")
	activate, err = db.RelayActivationRequested(4, 4)
	require.NoError(t, err)
	require.False(t, activate, "the current epoch is already active")
	activate, err = db.RelayActivationRequested(5, 4)
	require.NoError(t, err)
	require.True(t, activate, "the next epoch activates the prepared checkpoint")
	for _, requested := range []int64{3, 6, -1} {
		activate, err = db.RelayActivationRequested(requested, 4)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation, "epoch %d is neither current nor next", requested)
		require.False(t, activate)
	}
}

func TestRelayPostCheckpointHighWater(t *testing.T) {
	epoch, sequence := db.RelayPostCheckpointHighWater(nil)
	require.Zero(t, epoch)
	require.Zero(t, sequence)
	epoch, sequence = db.RelayPostCheckpointHighWater([]db.AttributionReceipt{{ResetEpoch: 1, Sequence: 5}, {ResetEpoch: 2, Sequence: 1}, {ResetEpoch: 1, Sequence: 9}})
	require.Equal(t, int64(2), epoch, "a later epoch outranks any sequence of an earlier one")
	require.Equal(t, int64(1), sequence)
}

func TestRelayPostCheckpointCandidates(t *testing.T) {
	const projectUID = "00000000000000000000000002"
	receipt := func(eventUID, hash string, epoch, sequence int64) string {
		return `{"version":1,"project_uid":"` + projectUID + `","event_uid":"` + eventUID + `","content_hash":"` + hash + `","authority_uid":"00000000000000000000000003","accountable_actor":"a","source_actor":"a","teammate":"a","ingress_instance_uid":"00000000000000000000000004","accepted_at":"2026-01-01T00:00:00Z","reset_epoch":` + itoa(epoch) + `,"sequence":` + itoa(sequence) + `,"key_id":"k1"}`
	}
	known := map[string]struct{}{"ev-known": {}}
	rows := []db.RelayRetainedReceiptRow{
		{EventID: 10, Receipt: receipt("ev-known", "h", 2, 2), ContentHash: "h", ResetEpoch: 2, Sequence: 2, KeyID: "k1"},
		{EventID: 11, Receipt: receipt("ev-old", "h", 2, 1), ContentHash: "h", ResetEpoch: 2, Sequence: 1, KeyID: "k1"},
		{EventID: 12, Receipt: receipt("ev-new", "h", 2, 3), ContentHash: "h", ResetEpoch: 2, Sequence: 3, KeyID: "k1"},
		{EventID: 13, Receipt: receipt("ev-next", "h", 3, 1), ContentHash: "h", ResetEpoch: 3, Sequence: 1, KeyID: "k1"},
	}
	candidates, err := db.RelayPostCheckpointCandidates(rows, projectUID, known, 2, 1)
	require.NoError(t, err)
	require.Len(t, candidates, 2, "the checkpoint's own receipt and the one at its high-water mark are skipped")
	require.Equal(t, int64(12), candidates[0].EventID)
	require.Equal(t, "ev-new", candidates[0].Receipt.EventUID)
	require.Equal(t, int64(13), candidates[1].EventID)

	for name, row := range map[string]db.RelayRetainedReceiptRow{
		"content hash column": {EventID: 14, Receipt: receipt("ev-x", "h", 2, 4), ContentHash: "other", ResetEpoch: 2, Sequence: 4, KeyID: "k1"},
		"epoch column":        {EventID: 14, Receipt: receipt("ev-x", "h", 2, 4), ContentHash: "h", ResetEpoch: 3, Sequence: 4, KeyID: "k1"},
		"key column":          {EventID: 14, Receipt: receipt("ev-x", "h", 2, 4), ContentHash: "h", ResetEpoch: 2, Sequence: 4, KeyID: "k2"},
		"foreign project":     {EventID: 14, Receipt: `{"version":1,"project_uid":"00000000000000000000000009","event_uid":"ev-x","content_hash":"h","reset_epoch":2,"sequence":4,"key_id":"k1","accepted_at":"2026-01-01T00:00:00Z"}`, ContentHash: "h", ResetEpoch: 2, Sequence: 4, KeyID: "k1"},
	} {
		_, err := db.RelayPostCheckpointCandidates([]db.RelayRetainedReceiptRow{row}, projectUID, nil, 2, 1)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation, name)
	}
	_, err = db.RelayPostCheckpointCandidates([]db.RelayRetainedReceiptRow{{EventID: 15, Receipt: `{"version":1,"unknown":true}`}}, projectUID, nil, 0, 0)
	require.Error(t, err, "a receipt with unknown members is rejected")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestRelayResetForwardTranslation(t *testing.T) {
	pin := db.RootKeyPin{ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003"}
	manifest := db.RootResetManifest{Version: 1, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, ResetEpoch: 9, SnapshotUID: "00000000000000000000000007", SnapshotDigest: strings.Repeat("b", 64), RootBaselines: db.RelayStreamCursors{Events: 50}}
	installed := db.RelayResetCheckpoint{Manifest: manifest, Snapshot: db.RootResetSnapshot{Events: []byte("[]")}, Translation: db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: "00000000000000000000000010", Epoch: 6}, SnapshotUID: manifest.SnapshotUID, SnapshotDigest: manifest.SnapshotDigest, HopBaselines: db.RelayStreamCursors{Events: 12, Receipts: 3, Artifacts: 1}}}
	projectID := int64(1)
	grant := db.FederationEnrollment{ProjectID: &projectID, RelayBindingUID: "00000000000000000000000011", SpokeInstanceUID: "00000000000000000000000012", RelayResetEpoch: 3}

	forwarded := db.RelayResetForwardTranslation(installed, grant, pin, "00000000000000000000000004")

	require.Equal(t, manifest, forwarded.Manifest, "the root manifest is forwarded unchanged")
	require.Equal(t, installed.Snapshot, forwarded.Snapshot)
	want := db.RelayResetTranslation{Authority: db.RelayHopAuthority{BindingUID: grant.RelayBindingUID, ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, SenderInstanceUID: "00000000000000000000000004", ReceiverInstanceUID: grant.SpokeInstanceUID, Epoch: 4}, SnapshotUID: manifest.SnapshotUID, SnapshotDigest: manifest.SnapshotDigest}
	require.Equal(t, want, forwarded.Translation, "the child hop starts at the grant's next epoch with zero hop baselines; upstream baselines never leak downstream")
	require.Equal(t, int64(6), installed.Translation.Authority.Epoch, "the installed checkpoint is not mutated")
}

func TestRelayRetainedDeliveryCheck(t *testing.T) {
	authority := db.RelayHopAuthority{BindingUID: "00000000000000000000000001", ProjectUID: "00000000000000000000000002", AuthorityUID: "00000000000000000000000003", SenderInstanceUID: "00000000000000000000000004", ReceiverInstanceUID: "00000000000000000000000005", Epoch: 2}
	origin := "00000000000000000000000006"
	event := db.RemoteEvent{EventUID: "00000000000000000000000007", OriginInstanceUID: origin, ProjectUID: authority.ProjectUID, ProjectName: "spoke-project", Type: "project.metadata_updated", Actor: "user-a", HLCPhysicalMS: 1_700_000_000_000, Payload: []byte(`{}`), CreatedAt: time.UnixMilli(1_700_000_000_000).UTC()}
	hash, err := db.RemoteEventContentHash(event)
	require.NoError(t, err)
	event.ContentHash = hash
	body, err := db.EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	sealed, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: 1, BindingUID: authority.BindingUID, ProjectUID: authority.ProjectUID, AuthorityUID: authority.AuthorityUID, SenderInstanceUID: authority.SenderInstanceUID, ReceiverInstanceUID: authority.ReceiverInstanceUID, Epoch: authority.Epoch, Sequence: 7, Stream: db.RelayStreamEvent, Path: []string{origin, authority.SenderInstanceUID}, SourceUID: event.EventUID, SourceHash: hash, Body: body})
	require.NoError(t, err)
	raw, err := json.Marshal(sealed)
	require.NoError(t, err)
	row := db.RelayRetainedEnvelopeRow{Sequence: 7, SourceUID: event.EventUID, SourceHash: hash, Envelope: string(raw)}
	check := db.RelayRetainedDeliveryCheck{Authority: authority, Stream: db.RelayStreamEvent, ProjectUID: authority.ProjectUID, Label: "upstream"}

	envelope, err := check.Envelope(row)
	require.NoError(t, err)
	require.Equal(t, []string{origin, authority.SenderInstanceUID}, envelope.Path)
	require.Equal(t, body, envelope.Body)

	strict := check
	strict.OriginFromPath = true
	_, err = strict.Envelope(row)
	require.NoError(t, err, "the event origin is the first hop in the path")

	bad := row
	bad.Sequence = 8
	_, err = check.Envelope(bad)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "row sequence must match the envelope")
	bad = row
	bad.SourceHash = strings.Repeat("c", 64)
	_, err = check.Envelope(bad)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "row hash must match the envelope")
	other := check
	other.Stream = db.RelayStreamReceipt
	_, err = other.Envelope(row)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "an event envelope cannot be reseeded as a receipt")
	other = check
	other.Authority.Epoch = 3
	_, err = other.Envelope(row)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation, "the envelope must authenticate under the retained hop")

	// The same source bytes wrapped by a path whose first hop is not the
	// event's origin: fine as an upstream delivery, refused as outgoing.
	foreign := sealed
	foreign.Path = []string{"00000000000000000000000008", authority.SenderInstanceUID}
	foreign, err = db.SealRelayEnvelope(foreign)
	require.NoError(t, err)
	raw, err = json.Marshal(foreign)
	require.NoError(t, err)
	foreignRow := row
	foreignRow.Envelope = string(raw)
	_, err = check.Envelope(foreignRow)
	require.NoError(t, err)
	_, err = strict.Envelope(foreignRow)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)

	receipt := check
	receipt.Stream = db.RelayStreamReceipt
	receiptSealed, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: 1, BindingUID: authority.BindingUID, ProjectUID: authority.ProjectUID, AuthorityUID: authority.AuthorityUID, SenderInstanceUID: authority.SenderInstanceUID, ReceiverInstanceUID: authority.ReceiverInstanceUID, Epoch: authority.Epoch, Sequence: 9, Stream: db.RelayStreamReceipt, Path: []string{origin, authority.SenderInstanceUID}, SourceUID: event.EventUID, SourceHash: hash, Body: []byte(`{"not":"an event"}`)})
	require.NoError(t, err)
	raw, err = json.Marshal(receiptSealed)
	require.NoError(t, err)
	_, err = receipt.Envelope(db.RelayRetainedEnvelopeRow{Sequence: 9, SourceUID: event.EventUID, SourceHash: hash, Envelope: string(raw)})
	require.NoError(t, err, "receipt bodies are not decoded as source events")
}

func TestRelayDownstreamOwesRootWork(t *testing.T) {
	grant := db.FederationEnrollment{RelayBindingUID: "00000000000000000000000011", RelayResetEpoch: 2}
	rootAccepted := map[string]string{"ev-checkpoint": "h1"}
	harmless := []db.RelayDeliveryRow{
		{Stream: db.RelayStreamArtifact, SourceUID: "artifact-1", SourceHash: "d1"},
		{Stream: db.RelayStreamReceipt, SourceUID: "ev-any", SourceHash: "h9"},
		{Stream: db.RelayStreamEvent, SourceUID: "ev-receipted", SourceHash: "h2", HasReceipt: true},
		{Stream: db.RelayStreamEvent, SourceUID: "ev-checkpoint", SourceHash: "h1"},
	}
	require.NoError(t, db.RelayDownstreamOwesRootWork(grant, nil, nil), "a synchronized downstream never blocks")
	require.NoError(t, db.RelayDownstreamOwesRootWork(grant, harmless, rootAccepted), "artifact offers, receipts and root-accepted events are superseded or stay owed")

	blocked := append(harmless[:3:3], db.RelayDeliveryRow{Stream: db.RelayStreamEvent, SourceUID: "ev-checkpoint", SourceHash: "h-other"})
	err := db.RelayDownstreamOwesRootWork(grant, blocked, rootAccepted)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "a checkpoint receipt for different content does not attest the event")
	require.ErrorContains(t, err, "downstream enrollment 00000000000000000000000011 still holds events deliveries the root has not accepted")
	require.ErrorContains(t, err, "let it synchronize or revoke it, then retry")
	err = db.RelayDownstreamOwesRootWork(grant, []db.RelayDeliveryRow{{Stream: db.RelayStreamEvent, SourceUID: "ev-local", SourceHash: "h3"}}, nil)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
}

func TestRelayResetAcceptedEvents(t *testing.T) {
	const projectUID, authorityUID = "00000000000000000000000002", "00000000000000000000000003"
	events := []db.RemoteEvent{{EventUID: "ev-1", ContentHash: "h1"}, {EventUID: "ev-2", ContentHash: "h2"}}
	receipts := []db.AttributionReceipt{
		{ProjectUID: projectUID, AuthorityUID: authorityUID, EventUID: "ev-1", ContentHash: "h1"},
		{ProjectUID: projectUID, AuthorityUID: authorityUID, EventUID: "ev-2", ContentHash: "stale"},
		{ProjectUID: projectUID, AuthorityUID: "00000000000000000000000009", EventUID: "ev-2", ContentHash: "h2"},
		{ProjectUID: "00000000000000000000000008", AuthorityUID: authorityUID, EventUID: "ev-2", ContentHash: "h2"},
		{ProjectUID: projectUID, AuthorityUID: authorityUID, EventUID: "ev-absent", ContentHash: "h3"},
	}
	accepted := db.RelayResetAcceptedEvents(projectUID, authorityUID, events, receipts)
	require.Equal(t, map[string]string{"ev-1": "h1"}, accepted, "only a receipt from the pinned authority for this project that matches an event in the checkpoint attests it")
}

func TestRelayResetSupersedes(t *testing.T) {
	retained := db.RootResetManifest{Version: 1, ProjectUID: "00000000000000000000000002", ResetEpoch: 4, SnapshotUID: "00000000000000000000000007"}
	require.NoError(t, db.RelayResetSupersedes(retained, retained), "the same checkpoint may be reinstalled")
	newer := retained
	newer.ResetEpoch = 5
	newer.SnapshotUID = "00000000000000000000000008"
	require.NoError(t, db.RelayResetSupersedes(retained, newer), "a later root epoch replaces the retained checkpoint")
	older := retained
	older.ResetEpoch = 3
	require.EqualError(t, db.RelayResetSupersedes(retained, older), "stale or conflicting root reset checkpoint")
	conflicting := retained
	conflicting.SnapshotUID = "00000000000000000000000008"
	require.EqualError(t, db.RelayResetSupersedes(retained, conflicting), "stale or conflicting root reset checkpoint")
}

func TestNextRootResetEpoch(t *testing.T) {
	epoch, err := db.NextRootResetEpoch(nil)
	require.NoError(t, err)
	require.Equal(t, int64(2), epoch, "epoch 1 is the enrollment baseline; the first checkpoint is epoch 2")
	checkpoint := func(epoch int64) string {
		return `{"manifest":{"version":1,"reset_epoch":` + itoa(epoch) + `},"snapshot":{"events":null,"entities":null,"provenance":null,"artifacts":null},"translation":{"authority":{},"hop_baselines":{}}}`
	}
	epoch, err = db.NextRootResetEpoch([]string{checkpoint(3), checkpoint(5), checkpoint(4)})
	require.NoError(t, err)
	require.Equal(t, int64(6), epoch, "a second enrollment never reuses an epoch another retained checkpoint signed")
	_, err = db.NextRootResetEpoch([]string{checkpoint(3), `{"manifest":{"reset_epoch":9,"unknown":1}}`})
	require.Error(t, err, "a retained checkpoint that does not decode is not silently skipped")
}

func TestRelayResetRetryEpoch(t *testing.T) {
	require.NoError(t, db.RelayResetRetryEpoch(4, 4), "a checkpoint the grant already activated is disclosed again")
	require.NoError(t, db.RelayResetRetryEpoch(5, 4), "a prepared checkpoint for the next epoch is disclosed")
	require.ErrorIs(t, db.RelayResetRetryEpoch(3, 4), db.ErrFederationIngestValidation, "a checkpoint from a superseded epoch is never disclosed")
	require.ErrorIs(t, db.RelayResetRetryEpoch(6, 4), db.ErrFederationIngestValidation, "a checkpoint two epochs ahead cannot belong to this grant")
}
