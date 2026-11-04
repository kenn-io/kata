package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: any present malformed pause fails closed; absent means unpaused.
func TestCronProjectPauseDecode(t *testing.T) {
	require.Nil(t, CronProjectPauseFromMetadata(`{}`))
	require.Nil(t, CronProjectPauseFromMetadata(``))
	valid := `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"maintenance"}`
	p := CronProjectPauseFromMetadata(JSONBlob(`{"cron.pause":` + valid + `}`))
	require.NotNil(t, p)
	require.False(t, p.Invalid)
	require.Equal(t, "operator", p.PausedBy)
	require.Equal(t, "maintenance", p.Reason)
	require.Equal(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), p.PausedAt)
	for _, raw := range []string{`true`, `[]`, `{}`, `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":null}`, `{"paused_by":"operator"}`, `{"paused_by":" ","paused_at":"2026-01-02T03:04:05Z","reason":""}`, `{"paused_by":"operator","paused_at":"bad","reason":""}`, `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"","extra":true}`, `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"` + strings.Repeat("x", 257) + `"}`} {
		p := CronProjectPauseFromMetadata(JSONBlob(`{"cron.pause":` + raw + `}`))
		require.NotNil(t, p, raw)
		require.True(t, p.Invalid, raw)
	}
}

// Reasons vary with a fixed valid actor so actor rejection cannot mask them.
func TestCronProjectPauseReasonProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		reason := hegel.Draw(ht, hegel.Text())
		wantValid := len(reason) <= 256
		raw, err := json.Marshal(map[string]any{"cron.pause": map[string]any{"paused_by": "operator", "paused_at": "2026-01-02T03:04:05Z", "reason": reason}})
		if err != nil {
			ht.Fatal(err)
		}
		pause := CronProjectPauseFromMetadata(JSONBlob(raw))
		if pause == nil || pause.Invalid == wantValid {
			ht.Fatalf("invalid classification for %d bytes: %#v", len(reason), pause)
		}
		if wantValid && pause.Reason != reason {
			ht.Fatalf("reason changed: %q -> %q", reason, pause.Reason)
		}
	})
}

// Contract: only a pinned root may change the reserved key; malformed old state can be deleted.
func TestCronProjectPauseIngestValidation(t *testing.T) {
	valid := `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":""}`
	for _, tc := range []struct {
		from, to, origin, authority string
		ok                          bool
	}{
		{`null`, valid, "root", "root", true}, {valid, `null`, "root", "root", true}, {`{"legacy":true}`, `null`, "root", "root", true},
		{`null`, valid, "spoke", "root", false}, {`null`, valid, "root", "", false}, {`null`, `true`, "root", "root", false},
		{`null`, `{"paused_by":"operator","paused_at":"2026-01-02T03:04:05Z","reason":"","extra":1}`, "root", "root", false},
	} {
		ev := RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: tc.origin, Payload: jsontext.Value(`{"diff":{"cron.pause":{"from":` + tc.from + `,"to":` + tc.to + `}}}`)}
		err := ValidateCronControlEvent(ev, tc.authority)
		if tc.ok {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrFederationIngestValidation)
		}
	}
	require.NoError(t, ValidateCronControlEvent(RemoteEvent{Type: "project.metadata_updated", Payload: jsontext.Value(`{"diff":{"ordinary":{"to":true}}}`)}, ""))
}

// Contract: valid actor/reason/timestamp values survive byte-for-byte strings and
// the same timestamp instant; invalid actor/reason values fail closed.
func TestCronProjectPauseValueProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actor := hegel.Draw(ht, hegel.Text())
		reason := hegel.Draw(ht, hegel.Text())
		instant := time.Unix(hegel.Draw(ht, hegel.Integers[int64](-62135596799, 253402300799)), int64(hegel.Draw(ht, hegel.Integers(0, 999999999)))).UTC()
		wantValid := strings.TrimSpace(actor) != "" && len(actor) <= 256 && len(reason) <= 256
		raw, err := json.Marshal(map[string]any{"cron.pause": map[string]any{"paused_by": actor, "paused_at": instant.Format(time.RFC3339Nano), "reason": reason}})
		require.NoError(ht, err)
		pause := CronProjectPauseFromMetadata(JSONBlob(raw))
		require.NotNil(ht, pause)
		require.Equal(ht, !wantValid, pause.Invalid)
		if wantValid {
			require.Equal(ht, actor, pause.PausedBy)
			require.Equal(ht, reason, pause.Reason)
			require.True(ht, instant.Equal(pause.PausedAt))
		}
	}, hegel.WithTestCases(1000))
}

// Contract: ingest depends on authenticated provenance and the replacement,
// never the old value or the receiver clock. Only explicit null deletes.
func TestCronProjectPauseReplacementProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		actor := hegel.Draw(ht, hegel.Text())
		reason := hegel.Draw(ht, hegel.Text())
		instant := time.Unix(hegel.Draw(ht, hegel.Integers[int64](-62135596799, 253402300799)), 0).UTC()
		valid := strings.TrimSpace(actor) != "" && len(actor) <= 256 && len(reason) <= 256
		next := map[string]any{"paused_by": actor, "paused_at": instant.Format(time.RFC3339), "reason": reason}
		change := map[string]any{"from": hegel.Draw(ht, hegel.Text()), "to": next}
		switch hegel.Draw(ht, hegel.Integers(0, 4)) {
		case 1:
			change["to"] = nil
			valid = true
		case 2:
			delete(change, "to")
			valid = false
		case 3:
			next["unknown"] = true
			valid = false
		case 4:
			next["reason"] = nil
			valid = false
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			delete(change, "from")
		}
		origin := hegel.Draw(ht, hegel.SampledFrom([]string{"root", "other", ""}))
		authority := hegel.Draw(ht, hegel.SampledFrom([]string{"root", "other", ""}))
		raw, err := json.Marshal(map[string]any{"diff": map[string]any{"cron.pause": change}})
		require.NoError(ht, err)
		err = ValidateCronControlEvent(RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: origin, Payload: raw}, authority)
		if valid && authority != "" && origin == authority {
			require.NoError(ht, err)
		} else {
			require.ErrorIs(ht, err, ErrFederationIngestValidation)
		}
	}, hegel.WithTestCases(1000))
}

// Contract: only the exact pause key and request-dot prefix are reserved.
func TestCronProjectPauseReservationProperty(t *testing.T) {
	a := jsontext.Value(`{"version":1,"through":"2026-01-02T03:04:05Z","by":"operator"}`)
	for _, key := range []string{"cron.passover", "cron.passover." + passoverJobUID, "cron.passover.bad", "cron.passover."} {
		require.Error(t, CheckCronProjectMetadataWrite(map[string]jsontext.Value{key: a}))
	}
	require.NoError(t, CheckCronProjectMetadataWrite(map[string]jsontext.Value{"cron.passovers": a}))

	hegel.Test(t, func(ht *hegel.T) {
		suffix := hegel.Draw(ht, hegel.Text())
		key := hegel.Draw(ht, hegel.SampledFrom([]string{suffix, "cron.pause", "cron.pause" + suffix, "cron.request." + suffix, "cron.request" + suffix, "cron.passover", "cron.passover." + suffix, "cron.passover" + suffix}))
		err := CheckCronProjectMetadataWrite(map[string]jsontext.Value{key: jsontext.Value("null")})
		require.Equal(ht, key == "cron.pause" || strings.HasPrefix(key, "cron.request.") || key == "cron.passover" || strings.HasPrefix(key, "cron.passover."), err != nil)
	}, hegel.WithTestCases(1000))
}
