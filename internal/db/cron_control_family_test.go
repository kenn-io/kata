package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// Main's metadata contract treats a null value as absent, including controls.
func TestCronControlNullReadsAsAbsent(t *testing.T) {
	require.Nil(t, CronProjectPauseFromMetadata(`{"cron.pause":null}`))
	require.Nil(t, CronPassoverFromMetadata(`{"cron.passover":null,"cron.passover.01ARZ3NDEKTSV4RRFFQ69G5FAV":null}`, "01ARZ3NDEKTSV4RRFFQ69G5FAV"))
	values, err := cronRequestMetadataValues(`{"cron.request.01ARZ3NDEKTSV4RRFFQ69G5FAV":null,"ordinary":null}`)
	require.NoError(t, err)
	require.NotContains(t, values, "cron.request.01ARZ3NDEKTSV4RRFFQ69G5FAV")
	require.Contains(t, values, "ordinary")
}

func TestCronControlAdoptionBaseline(t *testing.T) {
	for _, key := range []string{"cron.pause", "cron.request.01ARZ3NDEKTSV4RRFFQ69G5FAV", "cron.passover", "cron.passover.01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		t.Run(key, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{key: nil, "ordinary": nil})
			require.NoError(t, err)
			require.NoError(t, CheckCronControlAdoption(Project{Metadata: JSONBlob(raw)}))
			baseline, err := ProjectMetadataAdoptionPayload(JSONBlob(raw))
			require.NoError(t, err)
			require.JSONEq(t, `{"diff":{"ordinary":{"from":null,"to":null}}}`, baseline)
			require.NoError(t, ValidateCronControlEvent(RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: "spoke", Payload: jsontext.Value(baseline)}, "root"))
		})
	}
	for _, raw := range []JSONBlob{`{}`, `{"ordinary":null}`} {
		payload, err := ProjectMetadataAdoptionPayload(raw)
		require.NoError(t, err)
		want := `{"diff":{}}`
		if raw != `{}` {
			want = `{"diff":{"ordinary":{"from":null,"to":null}}}`
		}
		require.Equal(t, want, payload, "ordinary baseline bytes stay unchanged")
	}
}

func TestCronControlResetHistoryRejectsMalformedNonPassover(t *testing.T) {
	for _, key := range []string{"cron.pause", "cron.request.01ARZ3NDEKTSV4RRFFQ69G5FAV"} {
		t.Run(key, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"diff": map[string]any{key: map[string]any{"from": nil, "to": false}}})
			require.NoError(t, err)
			event := RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: "root", Payload: payload}
			require.ErrorIs(t, validateCronResetHistory([]RemoteEvent{event}, nil, "root"), ErrFederationIngestValidation)
		})
	}
}

func TestCronPassoverAbsentDeletionStaysAbsent(t *testing.T) {
	got := applyCronProjectMetadataDiff(jsontext.Value(`{}`), jsontext.Value(`{"cron.passover":{"from":null,"to":null}}`))
	require.JSONEq(t, `{}`, string(got))
}

// All controls in one event remain independently validated, including when a
// repaired historical cutoff shares a diff with malformed non-passover state.
func TestCronControlMixedHistory(t *testing.T) {
	good := jsontext.Value(`{"version":1,"through":"2026-01-02T03:04:05Z","by":"operator"}`)
	for _, key := range []string{"cron.pause", "cron.request." + passoverJobUID} {
		changes := map[string]cronResetChange{
			"cron.passover": {From: jsontext.Value("null"), To: jsontext.Value("false")},
			key:             {From: jsontext.Value("null"), To: jsontext.Value("false")},
		}
		payload, err := json.Marshal(map[string]any{"diff": changes})
		require.NoError(t, err)
		history := []RemoteEvent{{Type: "project.metadata_updated", OriginInstanceUID: "root", Payload: payload, HLCPhysicalMS: 1}}
		repairPayload, err := json.Marshal(map[string]any{"diff": map[string]cronResetChange{"cron.passover": {From: jsontext.Value("false"), To: good}}})
		require.NoError(t, err)
		repair := RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: "root", Payload: repairPayload, HLCPhysicalMS: 2}
		require.ErrorIs(t, validateCronResetHistory(append(history, repair), []RemoteEvent{repair}, "root"), ErrFederationIngestValidation, key)
	}
}
