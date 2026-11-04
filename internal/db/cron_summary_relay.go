package db

import (
	"context"
	"encoding/json/v2"
)

// RelayEnvelopeRequiredFeatures classifies structured run evidence in the
// original source bytes. Other bodies remain opaque to this compatibility gate;
// the receiver owns their full validation.
func RelayEnvelopeRequiredFeatures(envelope RelayEnvelope) (string, error) {
	if envelope.Stream != RelayStreamEvent {
		return "", nil
	}
	var source struct {
		Event struct {
			Type string `json:"type"`
		} `json:"event"`
		Payload []byte `json:"payload"`
	}
	if json.Unmarshal(envelope.Body, &source) != nil {
		return "", nil
	}
	if source.Event.Type != "cron.run.observed" && source.Event.Type != "cron.run.snapshot" {
		return "", nil
	}
	return PayloadRequiredFeatures(source.Event.Type, string(source.Payload))
}

// RequireRelayEnvelopeFeatures checks retained delivery bytes before emission.
func RequireRelayEnvelopeFeatures(ctx context.Context, envelope RelayEnvelope) error {
	supported, peer := ctx.Value(federationEventFeaturesKey{}).(string)
	if !peer {
		return nil
	}
	required, err := RelayEnvelopeRequiredFeatures(envelope)
	if err != nil {
		return err
	}
	return RequireEventFeatures(supported, required)
}

// RequireRelayCheckpointFeatures checks the exact snapshot being disclosed,
// including an older retained checkpoint after current state was replaced.
func RequireRelayCheckpointFeatures(ctx context.Context, snapshot RootResetSnapshot) error {
	if _, peer := ctx.Value(federationEventFeaturesKey{}).(string); !peer {
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
