package federation

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/kata/internal/db"
)

// Only canonical event batches rejected as poisoned data become quarantine.
// Authentication, transient transport, schema skew, receipts and artifacts keep
// their existing retry/revocation behavior. No rejected prefix is acknowledged.
func recordRelayEventQuarantine(ctx context.Context, store db.Storage, binding db.FederationBinding, direction db.FederationQuarantineDirection, batch db.RelayBatch, syncErr error, validateLease func(context.Context) error) error {
	if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
		return err
	}
	if batch.Stream != db.RelayStreamEvent || len(batch.Envelopes) == 0 || len(batch.Envelopes) > 1024 {
		return syncErr
	}
	poisoned := errors.Is(syncErr, db.ErrFederationIngestValidation) || errors.Is(syncErr, db.ErrRemoteEventConflict) || errors.Is(syncErr, db.ErrRemoteEventHashMismatch)
	if status, ok := errors.AsType[*HubStatusError](syncErr); ok {
		body, parsed := federationQuarantineHubError(status.Body)
		poisoned = parsed && ((status.StatusCode == http.StatusBadRequest && body.Code == "relay_invalid") || (status.StatusCode == http.StatusConflict && body.Code == "relay_conflict"))
	}
	if !poisoned {
		return syncErr
	}
	c := binding.RelayConfig
	hop := db.RelayHopAuthority{BindingUID: c.BindingUID, ProjectUID: binding.HubProjectUID, AuthorityUID: c.AuthorityUID, SenderInstanceUID: store.InstanceUID(), ReceiverInstanceUID: c.UpstreamInstanceUID, Epoch: c.ResetEpoch}
	if direction == db.FederationQuarantineDirectionPull {
		hop.SenderInstanceUID, hop.ReceiverInstanceUID = hop.ReceiverInstanceUID, hop.SenderInstanceUID
	}
	uids := make([]string, 0, len(batch.Envelopes))
	var previous int64
	for _, envelope := range batch.Envelopes {
		if envelope.Stream != batch.Stream || envelope.Sequence <= previous || db.ValidateRelayEnvelope(hop, envelope) != nil {
			return syncErr
		}
		event, err := db.DecodeRelaySourceEvent(envelope.Body)
		if err != nil || event.ProjectUID != hop.ProjectUID || event.EventUID != envelope.SourceUID || event.ContentHash != envelope.SourceHash {
			return syncErr
		}
		previous = envelope.Sequence
		uids = append(uids, envelope.SourceUID)
	}
	_, err := store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: binding.ProjectID, Direction: direction,
		FirstEventID: batch.Envelopes[0].Sequence, LastEventID: previous,
		EventUIDs: uids, Error: syncErr.Error(), CreatedAt: time.Now().UTC(),
		RelayBindingUID: c.BindingUID, RelayResetEpoch: c.ResetEpoch,
	})
	return errors.Join(syncErr, err)
}
