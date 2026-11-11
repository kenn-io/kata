package db

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	return RequireEventFeatures(features, IssueScheduleEventFeature)
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
	required, err := issueScheduleRequiredFeature(event.Type, string(event.Payload))
	if err != nil {
		return err
	}
	if required != "" {
		return incompatible
	}
	return nil
}

// ValidateRelaySchedule checks that saved rules can be read before committing
// a source event or activating its projection from a checkpoint.
func ValidateRelaySchedule(event RemoteEvent) error {
	_, err := issueScheduleRequiredFeature(event.Type, string(event.Payload))
	if err != nil && !errors.Is(err, ErrUnsupportedEventFeatures) {
		return fmt.Errorf("%w: %w", ErrFederationIngestValidation, err)
	}
	return err
}
