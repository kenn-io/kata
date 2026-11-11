package db

import (
	"context"
	"database/sql"
)

// CheckRelayProjectFeatures gates a specific authenticated hop before relay state changes.
func CheckRelayProjectFeatures(ctx context.Context, tx *sql.Tx, bindingUID string, projectID int64) error {
	features, ok := RelayPeerFeatures(ctx, bindingUID)
	if !ok {
		if ctx.Value(relayPeerFeaturesKey{}) != nil {
			return ErrNotFound
		}
		return nil
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT uid FROM projects WHERE id=$1`, projectID).Scan(&projectUID); err != nil {
		return err
	}
	required, err := ProjectWorkflowRequiredFeatures(ctx, tx, projectUID)
	if err != nil {
		return err
	}
	return RequireEventFeatures(features, required)
}

// CheckRelayBatchFeatures fences workflow formats in the exact source bytes.
// Protocol 1 predates cron negotiation, so only the new argv token is gated.
func CheckRelayBatchFeatures(batch RelayBatch, supported string) error {
	if batch.Stream != RelayStreamEvent {
		return nil
	}
	for _, envelope := range batch.Envelopes {
		source, err := DecodeRelaySourceEvent(envelope.Body)
		if err != nil {
			return err
		}
		required, err := PayloadRequiredFeatures(source.Type, string(source.Payload))
		if err != nil {
			return err
		}
		if RequireEventFeatures(required, CronWorkflowArgvEventFeature) == nil {
			if err := RequireEventFeatures(supported, CronWorkflowArgvEventFeature); err != nil {
				return err
			}
		}
	}
	return nil
}

// RelayWorkflowFeatureReader probes retained workflow history without mutating relay state.
type RelayWorkflowFeatureReader interface {
	RelayWorkflowRequiredFeatures(context.Context, string) (string, error)
}
