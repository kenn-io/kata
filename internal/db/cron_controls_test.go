package db

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/uid"
)

// Contract: any present malformed pause fails closed; absent or null means unpaused.
func TestCronProjectPauseDecode(t *testing.T) {
	for _, blob := range []JSONBlob{``, `{}`, `{"cron.pause":null}`} {
		require.Nil(t, CronProjectPauseFromMetadata(blob), blob)
	}
	const at = `"paused_at":"2026-01-02T03:04:05Z"`
	p := CronProjectPauseFromMetadata(JSONBlob(`{"cron.pause":{"paused_by":"operator",` + at + `,"reason":"maintenance"}}`))
	require.Equal(t, &CronProjectPause{PausedBy: "operator", PausedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Reason: "maintenance"}, p)
	long := strings.Repeat("é", 128)
	p = CronProjectPauseFromMetadata(JSONBlob(`{"cron.pause":{"paused_by":"` + long + `",` + at + `,"reason":"` + long + `"}}`))
	require.Equal(t, &CronProjectPause{PausedBy: long, PausedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Reason: long}, p, "256-byte actor and reason are retained")

	for _, raw := range []string{
		`true`, `[]`, `{}`, `"paused"`,
		`{"paused_by":"operator",` + at + `}`,
		`{"paused_by":"operator",` + at + `,"reason":null}`,
		`{"paused_by":"operator",` + at + `,"reason":1}`,
		`{"paused_by":"operator",` + at + `,"reason":"","reason":""}`,
		`{"paused_by":"operator",` + at + `,"reason":"","extra":true}`,
		`{"paused_by":"operator",` + at + `,"reason":"` + long + `x"}`,
		`{` + at + `,"reason":""}`,
		`{"paused_by":null,` + at + `,"reason":""}`,
		`{"paused_by":" ",` + at + `,"reason":""}`,
		`{"paused_by":"` + long + `x",` + at + `,"reason":""}`,
		`{"paused_by":"operator","reason":""}`,
		`{"paused_by":"operator","paused_at":null,"reason":""}`,
		`{"paused_by":"operator","paused_at":"bad","reason":""}`,
		`{"paused_by":"operator","paused_at":"0001-01-01T00:00:00Z","reason":""}`,
	} {
		p := CronProjectPauseFromMetadata(JSONBlob(`{"cron.pause":` + raw + `}`))
		require.Equal(t, &CronProjectPause{Invalid: true}, p, raw)
	}
	require.Equal(t, &CronProjectPause{Invalid: true}, CronProjectPauseFromMetadata(`not json`))
}

// Contract: only the authenticated root may write controls. Ingest checks the
// new value, never the receiver clock; any root deletion is accepted so old
// malformed state can be repaired, but a request body is write-once.
func TestCronControlEventValidation(t *testing.T) {
	const (
		pause    = `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":""}`
		passover = `{"version":1,"through":"2026-01-02T03:04:05Z","by":"operator"}` // #nosec G101 -- Cutoff fixture, not a credential.
		later    = `{"version":1,"through":"2026-01-02T04:04:05Z","by":"operator"}`
		request  = `{"kind":"run","job_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","requested_by":"operator","requested_at":"2026-01-02T03:04:05Z","expires_at":"2026-01-02T03:05:05Z"}`
	)
	id, err := uid.FromTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	require.NoError(t, err)
	requestKey := CronRequestMetadataPrefix + id
	for _, tc := range []struct {
		name, key, change string
		origin, authority string
		ok                bool
	}{
		{"pause baseline", "cron.pause", `{"to":` + pause + `}`, "root", "root", true},
		{"pause create", "cron.pause", `{"from":null,"to":` + pause + `}`, "root", "root", true},
		{"pause delete", "cron.pause", `{"from":` + pause + `,"to":null}`, "root", "root", true},
		{"pause legacy delete", "cron.pause", `{"from":{"legacy":true},"to":null}`, "root", "root", true},
		{"pause spoke origin", "cron.pause", `{"from":null,"to":` + pause + `}`, "spoke", "root", false},
		{"pause untrusted ingress", "cron.pause", `{"from":null,"to":` + pause + `}`, "root", "", false},
		{"pause untrusted delete", "cron.pause", `{"from":` + pause + `,"to":null}`, "root", "", false},
		{"pause malformed", "cron.pause", `{"from":null,"to":true}`, "root", "root", false},
		{"pause missing to", "cron.pause", `{"from":null}`, "root", "root", false},
		{"pause unknown member", "cron.pause", `{"from":null,"to":{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"","extra":1}}`, "root", "root", false},
		{"request create", requestKey, `{"to":` + request + `}`, "root", "root", true},
		{"request identical repeat", requestKey, `{"from":` + request + `,"to":` + request + `}`, "root", "root", true},
		{"request delete", requestKey, `{"from":` + request + `,"to":null}`, "root", "root", true},
		{"request replace", requestKey, `{"from":` + request + `,"to":` + strings.Replace(request, `"operator"`, `"other"`, 1) + `}`, "root", "root", false},
		{"request spoke origin", requestKey, `{"to":` + request + `}`, "other", "root", false},
		{"request malformed", requestKey, `{"to":true}`, "root", "root", false},
		{"legacy request key delete", "cron.request.legacy", `{"from":{"note":"legacy"},"to":null}`, "root", "root", true},
		{"legacy request key spoke delete", "cron.request.legacy", `{"from":{"note":"legacy"},"to":null}`, "spoke", "root", false},
		{"legacy request key create", "cron.request.legacy", `{"from":null,"to":{"note":"legacy"}}`, "root", "root", false},
		{"passover create", "cron.passover", `{"from":null,"to":` + passover + `}`, "root", "root", true},
		{"passover repair", "cron.passover", `{"from":true,"to":` + passover + `}`, "root", "root", true},
		{"passover advance", "cron.passover." + passoverJobUID, `{"from":` + passover + `,"to":` + later + `}`, "root", "root", true},
		{"passover decrease", "cron.passover", `{"from":` + later + `,"to":` + passover + `}`, "root", "root", false},
		{"passover delete", "cron.passover", `{"from":` + passover + `,"to":null}`, "root", "root", false},
		{"passover invalid job key", "cron.passover.bad", `{"from":null,"to":` + passover + `}`, "root", "root", false},
		{"passover untrusted ingress", "cron.passover", `{"from":null,"to":` + passover + `}`, "root", "", false},
		{"ordinary metadata", "cron.passovers", `{"to":true}`, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: tc.origin, Payload: jsontext.Value(`{"diff":{"` + tc.key + `":` + tc.change + `}}`)}
			err := ValidateCronControlEvent(event, tc.authority)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrFederationIngestValidation)
			}
		})
	}
}
