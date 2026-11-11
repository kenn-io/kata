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

// RunRelayOutboxAtomicity exercises relay outbox atomicity on the supplied native store.
// R4: queue intent commits with source data, survives retries, and acknowledges
// only an actually emitted prefix in its binding/stream/reset namespace.
func RunRelayOutboxAtomicity(t *testing.T, store db.Storage, backend Backend) {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "shared-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	parent, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{PlaintextToken: "outbox-parent-test-token", Actor: "member", AdminActor: "admin"})
	require.NoError(t, err)
	//nolint:gosec // This is a deterministic fixture credential, never an operational secret.
	grant, err := store.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: project.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000006", ProtocolVersion: db.RelayProtocolVersion, Token: "outbox-relay-test-token"})
	require.NoError(t, err)
	writeCtx := db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}, "member")
	issue, creation, err := store.CreateIssue(writeCtx, db.CreateIssueParams{ProjectID: project.ID, Title: "Queued with source", Author: "assistant"})
	require.NoError(t, err)
	comment, commentEvent, err := store.CreateComment(writeCtx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-assistant", Body: "Queued comment", Teammate: "reviewer"})
	require.NoError(t, err)
	_ = comment
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	featureEvent := argvCompatRemoteWorkflow(t, project, store.InstanceUID(), `{"version":1,"steps":[{"key":"run","kind":"argv","argv":["true"]}]}`)
	insertFeature := func() {
		_, err := q.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, featureEvent.EventUID, store.InstanceUID(), project.ID, project.Name, featureEvent.Type, "worker", string(featureEvent.Payload), featureEvent.HLCPhysicalMS, featureEvent.HLCCounter, featureEvent.ContentHash, featureEvent.CreatedAt.UTC().Format(db.EventTimestampFormat))
		require.NoError(t, err)
	}
	removeFeature := func() {
		_, err := q.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, featureEvent.EventUID)
		require.NoError(t, err)
	}
	peerCtx := db.WithRelayPeerFeatures(ctx, grant.Enrollment.RelayBindingUID, "")
	insertFeature()
	_, err = store.PendingRelayDeliveries(peerCtx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	_, err = store.AcceptRelayDeliveries(peerCtx, grant.Enrollment.RelayBindingUID, db.RelayBatch{Stream: db.RelayStreamEvent})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	var emittedCursor, acceptedCursor int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COALESCE(MAX(emitted_through),0),COALESCE(MAX(accepted_through),0) FROM federation_relay_cursors WHERE binding_uid=$1 AND stream=$2`, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent).Scan(&emittedCursor, &acceptedCursor))
	require.Zero(t, emittedCursor)
	require.Zero(t, acceptedCursor)
	var emitted int
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_relay_outbox WHERE binding_uid=$1 AND emitted=1`, grant.Enrollment.RelayBindingUID).Scan(&emitted))
	require.Zero(t, emitted)
	removeFeature()
	first, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, creation.UID, first[0].SourceUID)
	require.Equal(t, creation.ContentHash, first[0].SourceHash)
	source, err := db.DecodeRelaySourceEvent(first[0].Body)
	require.NoError(t, err)
	require.Equal(t, db.RemoteEventFromStored(creation), source)
	retry, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.NoError(t, err)
	require.Equal(t, first, retry)
	require.Error(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence+100, "arbitrary"), "cannot acknowledge an unoffered maximum")
	require.Error(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 2, db.RelayStreamEvent, first[0].Sequence, first[0].Digest), "stale/wrong epoch cannot advance")
	insertFeature()
	require.ErrorIs(t, store.AckRelayDeliveries(peerCtx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence, first[0].Digest), db.ErrUnsupportedEventFeatures)
	var acknowledged int
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_relay_outbox WHERE binding_uid=$1 AND acknowledged=1`, grant.Enrollment.RelayBindingUID).Scan(&acknowledged))
	require.Zero(t, acknowledged)
	var acknowledgedCursor int64
	require.NoError(t, q.QueryRowContext(ctx, `SELECT COALESCE(MAX(acknowledged_through),0) FROM federation_relay_cursors WHERE binding_uid=$1 AND stream=$2`, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent).Scan(&acknowledgedCursor))
	require.Zero(t, acknowledgedCursor)
	removeFeature()
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence, first[0].Digest))
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, first[0].Sequence, first[0].Digest), "lost ack reply is idempotent")
	second, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 1)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, commentEvent.UID, second[0].SourceUID)
	receipts, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
	require.NoError(t, err)
	require.Len(t, receipts, 2, "receipt stream remains independent of event acknowledgements")
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, second[0].Sequence, second[0].Digest))
	empty, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Empty(t, empty)
	// A legacy direct-spoke baseline still carries portable snapshots, while its
	// project.federation_enabled control record stays local to that transport.
	_, changed, err := store.RefreshProjectFederationBaseline(ctx, project.ID, "federation")
	require.NoError(t, err)
	require.True(t, changed)
	baseline, err := store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 100)
	require.NoError(t, err)
	require.Equal(t, 1, len(baseline), "legacy control event must not enter relay delivery")
	baselineSource, err := db.DecodeRelaySourceEvent(baseline[0].Body)
	require.NoError(t, err)
	require.Equal(t, "issue.snapshot", baselineSource.Type)
	require.NoError(t, store.AckRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, 1, db.RelayStreamEvent, baseline[0].Sequence, baseline[0].Digest))
	// A source/proof failure must not leave either a projection or queued intent.
	_, wrongPrivate, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	_, _, err = store.CreateIssue(db.WithRootAttribution(ctx, db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: wrongPrivate}, "member"), db.CreateIssueParams{ProjectID: project.ID, Title: "Must roll back", Author: "assistant"})
	require.Error(t, err)
	empty, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamEvent, 10)
	require.NoError(t, err)
	require.Empty(t, empty)

	// A full owner backup may retain an older delivery after its source event
	// has been compacted, independently of the current readable workflow.
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "first", Kind: "command", Command: "true"}}}
	old, oldEvent, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Earlier", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	current, currentEvent, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: old.UID, ExpectedEventUID: old.DefinitionEventUID, Name: "Current", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	filter := db.ExportFilter{IncludeDeleted: true}
	records, err := CollectImportRecords(ctx, store, filter)
	require.NoError(t, err)
	for record, exportErr := range store.ExportAttribution(ctx, filter) {
		require.NoError(t, exportErr)
		records = append(records, record)
	}
	var held *db.RelayOutboxExport
	for record, exportErr := range store.ExportRelayState(ctx) {
		require.NoError(t, exportErr)
		if outbox, ok := record.(*db.RelayOutboxExport); ok && outbox.BindingUID == grant.Enrollment.RelayBindingUID && outbox.SourceUID == oldEvent.UID {
			held = outbox
		}
		records = append(records, record)
	}
	require.NotNil(t, held)
	require.Equal(t, grant.Enrollment.RelayResetEpoch, held.ResetEpoch)
	require.Equal(t, db.RelayStreamEvent, held.Stream)
	require.False(t, held.Emitted)
	require.False(t, held.Acknowledged)
	var envelope db.RelayEnvelope
	require.NoError(t, json.Unmarshal([]byte(held.Envelope), &envelope))
	require.Equal(t, project.UID, envelope.ProjectUID)
	require.Equal(t, store.InstanceUID(), envelope.AuthorityUID)
	require.Equal(t, store.InstanceUID(), envelope.SenderInstanceUID)
	require.Equal(t, grant.Enrollment.SpokeInstanceUID, envelope.ReceiverInstanceUID)
	oldSource, err := db.DecodeRelaySourceEvent(envelope.Body)
	require.NoError(t, err)
	var payload db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal(oldSource.Payload, &payload))
	payload.Definition = []byte(`{"version":1,"steps":null}`)
	oldSource.Payload, err = json.Marshal(payload)
	require.NoError(t, err)
	oldSource.ContentHash, err = db.RemoteEventContentHash(oldSource)
	require.NoError(t, err)
	envelope.SourceHash = oldSource.ContentHash
	envelope.Body, err = db.EncodeRelaySourceEvent(oldSource)
	require.NoError(t, err)
	envelope, err = db.SealRelayEnvelope(envelope)
	require.NoError(t, err)
	held.SourceHash = envelope.SourceHash
	rawEnvelope, err := json.Marshal(envelope)
	require.NoError(t, err)
	held.Envelope = string(rawEnvelope)
	require.NoError(t, db.ValidateRelayDeliveryRecord(held))
	filtered := make([]db.ImportRecord, 0, len(records))
	for _, record := range records {
		if event, ok := record.(*db.EventExport); ok && event.UID == oldEvent.UID {
			continue
		}
		filtered = append(filtered, record)
	}
	require.NoError(t, db.ValidateImportReplay(filtered, db.ImportOptions{}))
	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, filtered, db.ImportOptions{}))
	restoredProject, err := restored.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	restoredWorkflow, err := restored.CronWorkflow(ctx, restoredProject.ID, current.UID)
	require.NoError(t, err)
	require.Equal(t, currentEvent.UID, restoredWorkflow.DefinitionEventUID)
	currentEvents, err := restored.EventsByUIDs(ctx, restoredProject.ID, []string{currentEvent.UID})
	require.NoError(t, err)
	require.Len(t, currentEvents, 1)
	requiredFeatures, err := restored.(db.RelayWorkflowFeatureReader).RelayWorkflowRequiredFeatures(ctx, project.UID)
	require.NoError(t, err)
	require.Empty(t, requiredFeatures, "current project evidence is readable by the peer")
	_, err = restored.EventsByUIDs(ctx, restoredProject.ID, []string{oldEvent.UID})
	require.ErrorIs(t, err, db.ErrNotFound)
	readState := func() ([]db.RelayOutboxExport, []db.RelayCursorExport) {
		var outbox []db.RelayOutboxExport
		var cursors []db.RelayCursorExport
		for record, exportErr := range restored.ExportRelayState(ctx) {
			require.NoError(t, exportErr)
			switch value := record.(type) {
			case *db.RelayOutboxExport:
				if value.BindingUID == held.BindingUID && value.Stream == held.Stream && value.ResetEpoch == held.ResetEpoch {
					outbox = append(outbox, *value)
				}
			case *db.RelayCursorExport:
				if value.BindingUID == held.BindingUID && value.Stream == held.Stream && value.ResetEpoch == held.ResetEpoch {
					cursors = append(cursors, *value)
				}
			}
		}
		return outbox, cursors
	}
	beforeOutbox, beforeCursors := readState()
	assert.Contains(t, beforeOutbox, *held)
	_, err = restored.PendingRelayDeliveries(db.WithRelayPeerFeatures(ctx, held.BindingUID, ""), held.BindingUID, db.RelayStreamEvent, 100)
	assert.ErrorIs(t, err, db.ErrHeldWorkflowEvidence)
	afterOutbox, afterCursors := readState()
	assert.Equal(t, beforeOutbox, afterOutbox, "refusal must not mark deliveries emitted")
	assert.Equal(t, beforeCursors, afterCursors, "refusal must not advance the emission cursor")

	_, _, err = store.RevokeAPIToken(ctx, parent.ID, "admin")
	require.NoError(t, err)
	_, err = store.PendingRelayDeliveries(ctx, grant.Enrollment.RelayBindingUID, db.RelayStreamReceipt, 10)
	require.Error(t, err, "revoke suppresses future queued proof disclosure")
}
