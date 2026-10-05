package federation

import (
	"context"

	"go.kenn.io/kata/internal/db"
)

// Artifact exchange follows both directions of ordinary content delivery.
// Staging can leave this independent stream pending until a later content sync.
func syncRelayArtifacts(ctx context.Context, store db.Storage, binding db.FederationBinding, remoteProjectID int64, client *Client, validateLease func(context.Context) error) error {
	downloader, ok := store.(db.RelayArtifactStorage)
	if !ok {
		return db.ErrTransactionFinalizationFailed
	}
	c := binding.RelayConfig
	for {
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		pending, err := store.PendingRelayDeliveries(ctx, c.BindingUID, db.RelayStreamArtifact, 32)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			break
		}
		batch := db.RelayBatch{Stream: db.RelayStreamArtifact, Envelopes: pending}
		accepted, err := client.AcceptRelayDeliveries(ctx, remoteProjectID, batch)
		if err != nil {
			return err
		}
		if len(accepted.MissingDigests) > 0 {
			if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
				return err
			}
			batch.Artifacts, err = downloader.RelayArtifactPayloads(ctx, c.BindingUID, c.ResetEpoch, accepted.MissingDigests)
			if err != nil {
				return err
			}
			accepted, err = client.AcceptRelayDeliveries(ctx, remoteProjectID, batch)
			if err != nil {
				return err
			}
		}
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		if accepted.Through > 0 {
			if err := store.AckRelayDeliveries(ctx, c.BindingUID, c.ResetEpoch, db.RelayStreamArtifact, accepted.Through, accepted.Digest); err != nil {
				return err
			}
		}
		if accepted.Through < pending[len(pending)-1].Sequence {
			break
		}
	}
	for {
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		batch, err := client.OfferRelayDeliveries(ctx, remoteProjectID, db.RelayStreamArtifact, 32, c.ResetEpoch)
		if err != nil {
			return err
		}
		if batch.Stream != db.RelayStreamArtifact || len(batch.Artifacts) != 0 {
			return db.ErrFederationIngestValidation
		}
		if len(batch.Envelopes) == 0 {
			break
		}
		accepted, err := store.AcceptRelayDeliveries(ctx, c.BindingUID, batch)
		if err != nil {
			return err
		}
		if len(accepted.MissingDigests) > 0 {
			if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
				return err
			}
			payloads, err := client.DownloadRelayArtifacts(ctx, remoteProjectID, c.ResetEpoch, accepted.MissingDigests)
			if err != nil {
				return err
			}
			batch.Artifacts = payloads.Artifacts
			if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
				return err
			}
			accepted, err = store.AcceptRelayDeliveries(ctx, c.BindingUID, batch)
			if err != nil {
				return err
			}
		}
		if err := validateFederationRunnerLease(ctx, validateLease); err != nil {
			return err
		}
		if accepted.Through > 0 {
			if err := client.AckRelayDeliveries(ctx, remoteProjectID, c.ResetEpoch, db.RelayStreamArtifact, accepted); err != nil {
				return err
			}
		}
		if accepted.Through < batch.Envelopes[len(batch.Envelopes)-1].Sequence {
			break
		}
	}
	return nil
}
