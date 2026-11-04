package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

func controlRecoveryEvent(t *testing.T, key string, from, to jsontext.Value, origin string, clock int64) RemoteEvent {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"diff": map[string]cronResetChange{key: {From: from, To: to}}})
	require.NoError(t, err)
	return RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: origin, Payload: payload, HLCPhysicalMS: clock}
}

// An exact later root deletion recovers legacy opaque values without accepting
// unrepaired, forged, regressing or currently malformed signed-reset state.
func TestCronResetDeletedLegacyControls(t *testing.T) {
	for _, key := range []string{"cron.pause", "cron.request." + passoverJobUID, "cron.request.legacy"} {
		for _, mode := range []string{"repaired", "unrepaired", "wrong-origin", "wrong-repair-origin", "mismatched-old", "not-later", "current-malformed"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				legacy := controlRecoveryEvent(t, key, jsontext.Value("null"), jsontext.Value(`{"note":"legacy"}`), "root", 1)
				repair := controlRecoveryEvent(t, key, jsontext.Value(`{"note":"legacy"}`), jsontext.Value("null"), "root", 2)
				history := []RemoteEvent{legacy, repair}
				current := []RemoteEvent{{Type: "project.metadata_updated", OriginInstanceUID: "root", Payload: jsontext.Value(`{"diff":{}}`)}}
				switch mode {
				case "unrepaired":
					history = history[:1]
				case "wrong-origin":
					history[0].OriginInstanceUID = "spoke"
				case "wrong-repair-origin":
					history[1].OriginInstanceUID = "spoke"
				case "mismatched-old":
					history[1] = controlRecoveryEvent(t, key, jsontext.Value(`{"note":"different"}`), jsontext.Value("null"), "root", 2)
				case "not-later":
					history[1].HLCPhysicalMS = 0
				case "current-malformed":
					current = []RemoteEvent{legacy}
				}
				err := validateCronResetHistory(history, current, "root")
				if mode == "repaired" {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, ErrFederationIngestValidation)
				}
			})
		}
	}
}

func TestCronResetRequestMutationNotRepairedByDeletion(t *testing.T) {
	at := uid.MustTime(passoverJobUID)
	first := CronRequest{UID: passoverJobUID, Kind: "run", JobUID: passoverJobUID, DefinitionEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", RequestedBy: "operator", RequestedAt: at, ExpiresAt: at.Add(time.Minute)}
	before, err := encodeCronRequest(first)
	require.NoError(t, err)
	first.RequestedBy = "another-operator"
	after, err := encodeCronRequest(first)
	require.NoError(t, err)
	key := CronRequestMetadataPrefix + passoverJobUID
	for _, value := range []jsontext.Value{after, jsontext.Value(`{"note":"malformed"}`)} {
		history := []RemoteEvent{controlRecoveryEvent(t, key, before, value, "root", 1), controlRecoveryEvent(t, key, value, jsontext.Value("null"), "root", 2)}
		require.ErrorIs(t, validateCronResetHistory(history, nil, "root"), ErrFederationIngestValidation)
	}
}

func TestCronRootDeletesMalformedRequestKey(t *testing.T) {
	key := "cron.request.legacy"
	deletion := controlRecoveryEvent(t, key, jsontext.Value(`{"note":"legacy"}`), jsontext.Value("null"), "root", 2)
	require.NoError(t, ValidateCronControlEvent(deletion, "root"))
	require.ErrorIs(t, ValidateCronControlEvent(deletion, ""), ErrFederationIngestValidation)
	deletion.OriginInstanceUID = "spoke"
	require.ErrorIs(t, ValidateCronControlEvent(deletion, "root"), ErrFederationIngestValidation)
	creation := controlRecoveryEvent(t, key, jsontext.Value("null"), jsontext.Value(`{"note":"legacy"}`), "root", 1)
	require.ErrorIs(t, ValidateCronControlEvent(creation, "root"), ErrFederationIngestValidation)
}

func TestCronPassoverZeroOffsetTimes(t *testing.T) {
	for _, suffix := range []string{"Z", "+00:00"} {
		for _, paused := range []bool{false, true} {
			fields := map[string]any{"version": 1, "through": "2026-01-02T03:04:05" + suffix, "by": "operator"}
			if paused {
				fields["paused_at"] = "2026-01-02T02:04:05" + suffix
			}
			raw, err := json.Marshal(fields)
			require.NoError(t, err)
			_, err = decodeCronPassover(raw)
			require.NoError(t, err, suffix)
			event := controlRecoveryEvent(t, "cron.passover", jsontext.Value("null"), raw, "root", 1)
			require.NoError(t, ValidateCronControlEvent(event, "root"))
			metadata, err := json.Marshal(map[string]jsontext.Value{"cron.passover": raw})
			require.NoError(t, err)
			state := CronPassoverFromMetadata(JSONBlob(metadata), passoverJobUID)
			require.NotNil(t, state)
			require.False(t, state.Invalid)
			anchor := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			_, err = cron.ApplyPassover(cron.RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, state.Through)
			require.NoError(t, err, "zero-offset wire timestamps must work in native planning")
		}
	}
	require.True(t, validCronPassoverTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("zero", 0))))
}
