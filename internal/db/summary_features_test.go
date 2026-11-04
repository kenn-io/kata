package db_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"hegel.dev/go/hegel"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSummaryPayloadFeatures(t *testing.T) {
	for _, kind := range []string{"cron.run.observed", "cron.run.snapshot"} {
		for _, field := range []string{`"reason":"failed"`, `"usage":{"model":"m"}`, `"steps":[]`, `"log_ref":"urn:example:run"`} {
			payload := `{"summary":{"version":1,` + field + `}}`
			features, err := db.PayloadRequiredFeatures(kind, payload)
			require.NoError(t, err)
			require.Equal(t, "cron_run_summary_v2,cron_v1", features)
			_, wireFeatures, err := db.FederationEventPayloadWireVersion(kind, jsontext.Value(payload))
			require.NoError(t, err)
			require.Equal(t, features, wireFeatures)
		}
		features, err := db.PayloadRequiredFeatures(kind, `{"summary":{"version":1,"message":"reason: failed"}}`)
		require.NoError(t, err)
		require.Equal(t, "cron_v1", features)
	}
	features, err := db.PayloadRequiredFeatures("issue.created", `{"summary":{"version":1,"reason":"failed"}}`)
	require.NoError(t, err)
	require.Empty(t, features)
}

// Transport compatibility follows the retained bytes even when a replacement
// has removed the same v2 evidence from the current project projection.
func TestSummaryRetainedRelayFeatures(t *testing.T) {
	for _, kind := range []string{"cron.run.observed", "cron.run.snapshot"} {
		body, err := json.Marshal(map[string]any{"event": map[string]any{"type": kind}, "payload": []byte(`{"summary":{"version":1,"steps":[]}}`)})
		require.NoError(t, err)
		envelope := db.RelayEnvelope{Stream: db.RelayStreamEvent, Body: body}
		required, err := db.RelayEnvelopeRequiredFeatures(envelope)
		require.NoError(t, err)
		require.Equal(t, "cron_run_summary_v2,cron_v1", required)
		records, err := json.Marshal([][]byte{body})
		require.NoError(t, err)
		for _, snapshot := range []db.RootResetSnapshot{{Events: records, Entities: []byte(`[]`)}, {Events: []byte(`[]`), Entities: records}} {
			require.ErrorIs(t, db.RequireRelayCheckpointFeatures(db.WithFederationEventFeatures(t.Context(), "cron_v1"), snapshot), db.ErrUnsupportedEventFeatures)
			require.NoError(t, db.RequireRelayCheckpointFeatures(db.WithFederationEventFeatures(t.Context(), db.SupportedEventFeatures), snapshot))
			require.NoError(t, db.RequireRelayCheckpointFeatures(t.Context(), snapshot))
		}
	}
}

// Batch requirements form a set regardless of ordering, repeated members or
// grouping. Advertisements may contain harmless duplicate/whitespace entries.
func TestSummaryFeatureUnionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		a := hegel.Draw(ht, hegel.Text())
		b := hegel.Draw(ht, hegel.Text())
		c := hegel.Draw(ht, hegel.Text())
		want := db.MergeEventFeatures(a, b, c)
		require.Equal(ht, want, db.MergeEventFeatures(c, b, a))
		require.Equal(ht, want, db.MergeEventFeatures(db.MergeEventFeatures(a, b), c))
		require.Equal(ht, want, db.MergeEventFeatures(want, want))
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
