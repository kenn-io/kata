package dbtest

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
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
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: selected.ID, Name: "Checks", Actor: "member", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "check", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	checkpoint, err := resets.CreateRelayReset(ctx, binding, signer)
	require.NoError(t, err)
	metaReader := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	key := db.RelayResetMetadataPrefix + selected.UID + "." + binding
	for _, section := range []string{"source", "entity"} {
		for _, tc := range []struct {
			definition string
			held       bool
		}{
			{`null`, true},
			{`{"version":1,"steps":null}`, true},
			{`{"version":1,"steps":[{"key":"run","kind":"argv"}]}`, true},
			{`{"version":1,"steps":[{"key":"run","kind":"argv","argv":["true"]}]}`, false},
			{`{"version":2,"steps":[]}`, false},
			{`{"version":1,"steps":[{"key":"run","kind":"future"}]}`, false},
			{`"malformed"`, false},
			{``, false}, // Missing definition, distinct from explicit null.
		} {
			bad := checkpointWithWorkflowDefinition(t, checkpoint, selected, pin, signingKey, section, tc.definition)
			raw, err := json.Marshal(bad)
			require.NoError(t, err)
			require.NoError(t, writeMeta(ctx, key, string(raw)))
			_, _, err = db.DecodeRootResetPayload(pin, bad.Snapshot)
			require.NoError(t, err)
			_, err = resets.CreateRelayReset(ctx, binding, signer)
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures, "cached content must be checked even when current rows are supported")
			if tc.held {
				assert.ErrorIs(t, err, db.ErrHeldWorkflowEvidence, "definition %q in %s", tc.definition, section)
			} else {
				require.NotErrorIs(t, err, db.ErrHeldWorkflowEvidence)
			}
			_, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), binding, db.RelayStreamEvent, 1024)
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures, "held checkpoint cannot activate the next epoch")
			if tc.held {
				assert.ErrorIs(t, err, db.ErrHeldWorkflowEvidence, "definition %q in %s", tc.definition, section)
			} else {
				require.NotErrorIs(t, err, db.ErrHeldWorkflowEvidence)
			}
			var retained string
			require.NoError(t, metaReader.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&retained))
			require.Equal(t, string(raw), retained, "refusal preserves exact signed checkpoint")
		}
	}
	// Restore the exact same owner backup before each operation: activation on
	// the unfixed gate changes the epoch and must not mask the download check.
	filter := db.ExportFilter{IncludeDeleted: true}
	records := relayOwnerRecords(t, store, filter)
	require.NotZero(t, checkpoint.Manifest.HistoryEventID)
	for _, section := range []string{"source", "entity"} {
		bad := checkpointWithWorkflowDefinition(t, checkpoint, selected, pin, signingKey, section, `{"version":1,"steps":[{"key":"check","kind":"command","command":"true"}]}`, true)
		require.NoError(t, db.VerifyRootResetManifest(pin, bad.Manifest, bad.Snapshot))
		require.NoError(t, db.ValidateRelayResetTranslation(bad.Translation.Authority, bad.Manifest, bad.Translation))
		_, _, err = db.DecodeRootResetPayload(pin, bad.Snapshot)
		require.NoError(t, err)
		raw, err := json.Marshal(bad)
		require.NoError(t, err)
		restored := replaceRelayOwnerMeta(t, records, key, string(raw))
		for _, operation := range []string{"download", "activation"} {
			t.Run(section+"/"+operation, func(t *testing.T) {
				require.NoError(t, db.ValidateImportReplay(restored, db.ImportOptions{}))
				require.NoError(t, store.ImportReplay(ctx, restored, db.ImportOptions{}))
				live, err := store.CronWorkflow(ctx, selected.ID, workflow.UID)
				require.NoError(t, err)
				require.Equal(t, workflow.Definition, live.Definition)
				before := relayOwnerRecords(t, store, filter)
				maximum, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				var retained string
				require.NoError(t, metaReader.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, key).Scan(&retained))
				require.Equal(t, string(raw), retained)
				if operation == "download" {
					_, err = resets.CreateRelayReset(ctx, binding, signer)
				} else {
					var envelopes []db.RelayEnvelope
					envelopes, err = store.PendingRelayDeliveries(db.WithRelayRequestedEpoch(ctx, checkpoint.Translation.Authority.Epoch), binding, db.RelayStreamEvent, 1024)
					assert.Empty(t, envelopes)
				}
				assert.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				assert.NotErrorIs(t, err, db.ErrHeldWorkflowEvidence)
				afterMaximum, err := store.MaxEventID(ctx)
				require.NoError(t, err)
				assert.Equal(t, maximum, afterMaximum)
				assert.Equal(t, before, relayOwnerRecords(t, store, filter), "refusal preserves metadata, grants, cursors, deliveries and events")
			})
		}
	}
	// Historical, strictly decodable workflows remain readable even when they
	// would not satisfy today's authoring validation.
	historical := `{"version":1,"steps":[]}`
	_, err = cron.ParseWorkflow([]byte(historical))
	require.Error(t, err)
	positive := checkpointWithWorkflowDefinition(t, checkpoint, selected, pin, signingKey, "source", historical)
	positiveRaw, err := json.Marshal(positive)
	require.NoError(t, err)
	positiveRecords := replaceRelayOwnerMeta(t, records, key, string(positiveRaw))
	require.NoError(t, db.ValidateImportReplay(positiveRecords, db.ImportOptions{}))
	require.NoError(t, store.ImportReplay(ctx, positiveRecords, db.ImportOptions{}))
	_, err = resets.CreateRelayReset(ctx, binding, signer)
	require.NoError(t, err)
	require.NoError(t, db.ValidateImportReplay(records, db.ImportOptions{}))
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))
	rawCheckpoint, err := json.Marshal(checkpoint)
	require.NoError(t, err)
	require.NoError(t, writeMeta(ctx, db.RelayResetMetadataPrefix+selected.UID+"."+binding, string(rawCheckpoint)))
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

// relayOwnerRecords collects the complete ordinary owner replay, including
// attribution and relay state which are exported separately from project data.
func relayOwnerRecords(t *testing.T, store db.Storage, filter db.ExportFilter) []db.ImportRecord {
	t.Helper()
	ctx := t.Context()
	records, err := CollectImportRecords(ctx, store, filter)
	require.NoError(t, err)
	for record, err := range store.ExportAttribution(ctx, filter) {
		require.NoError(t, err)
		records = append(records, record)
	}
	for record, err := range store.ExportRelayState(ctx) {
		require.NoError(t, err)
		records = append(records, record)
	}
	return records
}

func replaceRelayOwnerMeta(t *testing.T, records []db.ImportRecord, key, value string) []db.ImportRecord {
	t.Helper()
	result := append([]db.ImportRecord(nil), records...)
	found := false
	for i, record := range result {
		if meta, ok := record.(*db.MetaKV); ok && meta.Key == key {
			require.False(t, found)
			result[i] = &db.MetaKV{Key: key, Value: value}
			found = true
		}
	}
	require.True(t, found)
	return result
}
