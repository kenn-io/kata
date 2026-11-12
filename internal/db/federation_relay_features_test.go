package db

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
)

// Relay compatibility is checked against exact event bytes, including retries.
func TestRelayBatchWorkflowFeatureContract(t *testing.T) {
	argv := `{"definition":{"version":1,"steps":[{"kind":"argv","argv":["echo","hello"]}]}}`
	held := `{"definition":null}`
	cases := []struct {
		kind, payload, supported string
		refused                  bool
	}{
		{"cron.workflow.created", argv, "", true},
		{"cron.workflow.created", argv, CronWorkflowArgvEventFeature + "_other", true},
		{"cron.workflow.created", argv, CronWorkflowArgvEventFeature, false},
		{"cron.workflow.created", `{"definition":{"steps":[{"kind":"command","command":"argv"}]}}`, "", false},
		{"cron.workflow.created", held, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.created", `{"definition":{"steps":null}}`, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.created", `{"definition":{"steps":[null]}}`, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.created", `{"definition":{"steps":[{"kind":null}]}}`, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.created", `{"definition":{"steps":[{"kind":"argv"}]}}`, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.updated", held, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.deleted", held, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.restored", held, CronWorkflowArgvEventFeature, true},
		{"cron.workflow.snapshot", held, CronWorkflowArgvEventFeature, true},
		{"issue.created", held, "", false},
	}
	for _, tc := range cases {
		event := RemoteEvent{Type: tc.kind, Payload: jsontext.Value(tc.payload)}
		var err error
		event.ContentHash, err = RemoteEventContentHash(event)
		require.NoError(t, err)
		body, err := EncodeRelaySourceEvent(event)
		require.NoError(t, err)
		err = CheckRelayBatchFeatures(RelayBatch{Stream: RelayStreamEvent, Envelopes: []RelayEnvelope{{Body: body}}}, tc.supported)
		if tc.refused {
			require.ErrorIs(t, err, ErrUnsupportedEventFeatures, "%s: %s", tc.kind, tc.payload)
		} else {
			require.NoError(t, err, "%s: %s", tc.kind, tc.payload)
		}
	}
}
