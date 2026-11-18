package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
)

type relayPeerFeaturesKey struct{}

// WithRelayPeerEventFeatures limits a relay transaction to formats the peer reads.
func WithRelayPeerEventFeatures(ctx context.Context, features string) context.Context {
	return context.WithValue(ctx, relayPeerFeaturesKey{}, features)
}

// RequireRelayAckFeatures checks the entire unacknowledged prefix.
func RequireRelayAckFeatures(ctx context.Context, tx *sql.Tx, bindingUID, stream string, epoch, after, through int64) error {
	if stream != RelayStreamEvent || RequireEventFeatures(relayPeerFeatures(ctx), IssueDateZoneEventFeature) == nil {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT envelope FROM federation_relay_outbox WHERE binding_uid=$1 AND stream=$2 AND reset_epoch=$3 AND id>$4 AND id<=$5 AND emitted=1`, bindingUID, stream, epoch, after, through)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		var envelope RelayEnvelope
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return err
		}
		if err := RequireRelayEventFeatures(ctx, []RelayEnvelope{envelope}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func relayPeerFeatures(ctx context.Context) string {
	features, ok := ctx.Value(relayPeerFeaturesKey{}).(string)
	if !ok {
		return SupportedEventFeatures
	}
	return features
}

// RequireRelayEventFeatures checks event bodies before a delivery changes state.
func RequireRelayEventFeatures(ctx context.Context, envelopes []RelayEnvelope) error {
	if RequireEventFeatures(relayPeerFeatures(ctx), IssueDateZoneEventFeature) == nil {
		return nil
	}
	for _, envelope := range envelopes {
		if envelope.Stream != RelayStreamEvent {
			continue
		}
		event, err := DecodeRelaySourceEvent(envelope.Body)
		if err != nil {
			return err
		}
		if err := requireRelaySourceFeatures(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func requireRelaySourceFeatures(ctx context.Context, event RemoteEvent) error {
	zoned, err := payloadHasZonedDate(event.Type, string(event.Payload))
	if err != nil {
		return err
	}
	if zoned {
		return RequireEventFeatures(relayPeerFeatures(ctx), IssueDateZoneEventFeature)
	}
	return nil
}

// RequireRelaySnapshotFeatures checks the event and entity records in a checkpoint.
func RequireRelaySnapshotFeatures(ctx context.Context, snapshot RootResetSnapshot) error {
	if RequireEventFeatures(relayPeerFeatures(ctx), IssueDateZoneEventFeature) == nil {
		return nil
	}
	for _, section := range [][]byte{snapshot.Events, snapshot.Entities} {
		var bodies [][]byte
		if err := json.Unmarshal(section, &bodies); err != nil {
			return err
		}
		for _, body := range bodies {
			event, err := DecodeRelaySourceEvent(body)
			if err != nil {
				return err
			}
			if err := requireRelaySourceFeatures(ctx, event); err != nil {
				return err
			}
		}
	}
	return nil
}
