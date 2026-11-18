package db

import (
	"context"
	"encoding/json/v2"
)

type relayPeerFeaturesKey struct{}

// WithRelayPeerFeatures records the advertised features for an authenticated
// relay exchange. An empty header is an advertisement of no event features.
func WithRelayPeerFeatures(ctx context.Context, features string) context.Context {
	return context.WithValue(ctx, relayPeerFeaturesKey{}, features)
}

func relayScheduleFeatureError(ctx context.Context) error {
	features, negotiated := ctx.Value(relayPeerFeaturesKey{}).(string)
	if !negotiated {
		return nil
	}
	return RequireEventFeatures(features, IssueDateZoneEventFeature)
}

// RequireRelayWriteFeatures rejects schedule writes from peers lacking date zones.
func RequireRelayWriteFeatures(ctx context.Context, event RemoteEvent) error {
	features, negotiated := ctx.Value(relayPeerFeaturesKey{}).(string)
	if !negotiated {
		return nil
	}
	required, err := FederationWriteRequiredFeatures(event.Type, string(event.Payload))
	if err != nil {
		return err
	}
	return RequireEventFeatures(features, required)
}

// RequireRelayDeliveryFeatures checks an event prefix before it is emitted.
func RequireRelayDeliveryFeatures(ctx context.Context, envelopes []RelayEnvelope) error {
	incompatible := relayScheduleFeatureError(ctx)
	if incompatible == nil {
		return nil
	}
	for _, envelope := range envelopes {
		if err := requireRelayBodyFeatures(envelope.Body, incompatible); err != nil {
			return err
		}
	}
	return nil
}

func requireRelayCheckpointFeatures(ctx context.Context, snapshot RootResetSnapshot) error {
	incompatible := relayScheduleFeatureError(ctx)
	if incompatible == nil {
		return nil
	}
	for _, section := range [][]byte{snapshot.Events, snapshot.Entities} {
		var bodies [][]byte
		if err := json.Unmarshal(section, &bodies); err != nil {
			return err
		}
		for _, body := range bodies {
			if err := requireRelayBodyFeatures(body, incompatible); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireRelayBodyFeatures(body []byte, incompatible error) error {
	event, err := DecodeRelaySourceEvent(body)
	if err != nil {
		return err
	}
	required, err := PayloadRequiredFeatures(event.Type, string(event.Payload))
	if err != nil {
		return err
	}
	if RequireEventFeatures(required, IssueDateZoneEventFeature) == nil {
		return incompatible
	}
	return nil
}
