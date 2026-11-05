package db

import (
	"context"
	"encoding/json/v2"
)

// RelayEnvelopeRequiredFeatures classifies structured run evidence in the
// original source bytes, verified by the owning relay source codec.
func RelayEnvelopeRequiredFeatures(envelope RelayEnvelope) (string, error) {
	if envelope.Stream != RelayStreamEvent {
		return "", nil
	}
	source, err := DecodeRelaySourceEvent(envelope.Body)
	if err != nil {
		return "", err
	}
	if source.Type != "cron.run.observed" && source.Type != "cron.run.snapshot" {
		return "", nil
	}
	return PayloadRequiredFeatures(source.Type, string(source.Payload))
}

// RequireRelayEnvelopeFeatures checks retained delivery bytes before emission.
func RequireRelayEnvelopeFeatures(ctx context.Context, envelope RelayEnvelope) error {
	if !relayNeedsSummaryGate(ctx) {
		return nil
	}
	required, err := RelayEnvelopeRequiredFeatures(envelope)
	if err != nil {
		return err
	}
	if RequireEventFeatures(required, CronRunSummaryV2Feature) == nil {
		return RequireEventFeatures("", CronRunSummaryV2Feature)
	}
	return nil
}

// RequireRelayCheckpointFeatures checks the exact snapshot being disclosed,
// including an older retained checkpoint after current state was replaced.
func RequireRelayCheckpointFeatures(ctx context.Context, snapshot RootResetSnapshot) error {
	if !relayNeedsSummaryGate(ctx) {
		return nil
	}
	for _, section := range [][]byte{snapshot.Events, snapshot.Entities} {
		var records [][]byte
		if err := json.Unmarshal(section, &records); err != nil {
			return err
		}
		for _, record := range records {
			if err := RequireRelayEnvelopeFeatures(ctx, RelayEnvelope{Stream: RelayStreamEvent, Body: record}); err != nil {
				return err
			}
		}
	}
	return nil
}
