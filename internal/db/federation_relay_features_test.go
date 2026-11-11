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

func TestRelayBatchHeldWorkflowEvidence(t *testing.T) {
	for _, payload := range []string{`{"definition":null}`, `{"definition":{"steps":null}}`, `{"definition":{"steps":[null]}}`, `{"definition":{"steps":[{"kind":null}]}}`, `{"definition":{"steps":[{"kind":"argv"}]}}`} {
		for _, kind := range []string{"cron.workflow.created", "cron.workflow.updated", "cron.workflow.deleted", "cron.workflow.restored", "cron.workflow.snapshot"} {
			event := RemoteEvent{Type: kind, Payload: jsontext.Value(payload)}
			var err error
			event.ContentHash, err = RemoteEventContentHash(event)
			require.NoError(t, err)
			body, err := EncodeRelaySourceEvent(event)
			require.NoError(t, err)
			batch := RelayBatch{Stream: RelayStreamEvent, Envelopes: []RelayEnvelope{{Body: body}}}
			require.ErrorIs(t, CheckRelayBatchFeatures(batch, CronWorkflowArgvEventFeature), ErrUnsupportedEventFeatures, "%s: %s", kind, payload)
		}
	}
	event := RemoteEvent{Type: "issue.created", Payload: jsontext.Value(`{"definition":null}`)}
	hash, err := RemoteEventContentHash(event)
	require.NoError(t, err)
	event.ContentHash = hash
	body, err := EncodeRelaySourceEvent(event)
	require.NoError(t, err)
	require.NoError(t, CheckRelayBatchFeatures(RelayBatch{Stream: RelayStreamEvent, Envelopes: []RelayEnvelope{{Body: body}}}, ""))
}
