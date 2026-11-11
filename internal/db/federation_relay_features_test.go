package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

// Relay compatibility is checked against exact event bytes, including retries.
func TestRelayBatchWorkflowFeatureContract(t *testing.T) {
	event := RemoteEvent{Type: "cron.workflow.created", Payload: jsontext.Value(`{"definition":{"steps":[{"kind":"argv","argv":["echo","hello"]}]}}`)}
	hash, err := RemoteEventContentHash(event)
	require.NoError(t, err)
	event.ContentHash = hash
	body, err := EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	batch := RelayBatch{Stream: RelayStreamEvent, Envelopes: []RelayEnvelope{{Body: body}}}
	require.ErrorIs(t, CheckRelayBatchFeatures(batch, ""), ErrUnsupportedEventFeatures)
	require.ErrorIs(t, CheckRelayBatchFeatures(batch, CronWorkflowArgvEventFeature+"_other"), ErrUnsupportedEventFeatures)
	require.NoError(t, CheckRelayBatchFeatures(batch, CronWorkflowArgvEventFeature))

	event.Payload = jsontext.Value(`{"definition":{"steps":[{"kind":"command","command":"argv"}]}}`)
	event.ContentHash, err = RemoteEventContentHash(event)
	require.NoError(t, err)
	batch.Envelopes[0].Body, err = EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	require.NoError(t, CheckRelayBatchFeatures(batch, ""), "ordinary protocol-1 cron remains compatible")
}
