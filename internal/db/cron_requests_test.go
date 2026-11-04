package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

// The UID's millisecond timestamp is the sole persisted admission time; TTL is
// whole seconds in60..86400 and the stored value has no writable read state.
func TestCronRequestValue(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id, err := uid.FromTime(now)
	require.NoError(t, err)
	value := map[string]any{"kind": "run", "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "requested_by": "operator", "requested_at": now, "expires_at": now.Add(600 * time.Second)}
	marshal := func() jsontext.Value { raw, e := json.Marshal(value); require.NoError(t, e); return raw }
	got, err := decodeCronRequest(id, marshal())
	require.NoError(t, err)
	require.Equal(t, id, got.UID)
	require.Equal(t, "run", got.Kind)
	require.Equal(t, "operator", got.RequestedBy)
	require.True(t, now.Equal(got.RequestedAt))
	require.True(t, now.Add(600*time.Second).Equal(got.ExpiresAt))
	for _, change := range []map[string]any{
		{"state": "answered"}, {"uid": id}, {"kind": "other"}, {"kind": "cancel"}, {"job_uid": nil}, {"requested_by": " "}, {"requested_by": strings.Repeat("x", 257)},
		{"requested_at": now.Add(time.Millisecond)}, {"expires_at": now.Add(59 * time.Second)}, {"expires_at": now.Add(86401 * time.Second)}, {"expires_at": now.Add(600*time.Second + time.Nanosecond)},
	} {
		t.Run(string(mustJSONForCronRequestTest(t, change)), func(t *testing.T) {
			merged := make(map[string]any, len(value))
			maps.Copy(merged, value)
			maps.Copy(merged, change)
			raw := mustJSONForCronRequestTest(t, merged)
			_, e := decodeCronRequest(id, raw)
			require.ErrorIs(t, e, cron.ErrInvalid)
		})
	}
	value["kind"] = "cancel"
	value["run_uid"] = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
	got, err = decodeCronRequest(id, marshal())
	require.NoError(t, err)
	require.Equal(t, "cancel", got.Kind)
	require.NotNil(t, got.RunUID)
	value["kind"] = "run"
	_, err = decodeCronRequest(id, marshal())
	require.ErrorIs(t, err, cron.ErrInvalid)
	t.Run("workflow-only cancellation", func(t *testing.T) {
		now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		id, e := uid.FromTime(now)
		require.NoError(t, e)
		raw := mustJSONForCronRequestTest(t, map[string]any{"kind": "cancel", "run_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAX", "requested_by": "operator", "requested_at": now, "expires_at": now.Add(time.Minute)})
		got, e := decodeCronRequest(id, raw)
		require.NoError(t, e)
		require.Empty(t, got.JobUID)
		require.Empty(t, got.DefinitionEventUID)
		require.NotNil(t, got.RunUID)

	})
}

func mustJSONForCronRequestTest(t *testing.T, v any) jsontext.Value {
	t.Helper()
	raw, e := json.Marshal(v)
	require.NoError(t, e)
	return raw
}

func TestCronRequestFreshness(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		delta time.Duration
		want  error
	}{
		{-30 * time.Second, nil}, {-30*time.Second - time.Millisecond, ErrCronRequestExpired}, {5 * time.Second, nil}, {5*time.Second + time.Millisecond, cron.ErrInvalid}, {0, nil},
	} {
		id, e := uid.FromTime(now.Add(tc.delta))
		require.NoError(t, e)
		e = validateCronRequestFreshness(id, now)
		if tc.want == nil {
			require.NoError(t, e)
		} else {
			require.ErrorIs(t, e, tc.want)
		}
	}
	require.ErrorIs(t, validateCronRequestFreshness("bad", now), cron.ErrInvalid)
}

// Construct valid requests independently across the whole supported TTL range;
// decoding must preserve identity, exact strings and timestamp instants.
func TestCronRequestValueProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ttl := hegel.Draw(ht, hegel.Integers(60, 86400))
		seconds := hegel.Draw(ht, hegel.Integers[int64](0, 253402214399))
		now := time.Unix(seconds, int64(hegel.Draw(ht, hegel.Integers(0, 999)))*int64(time.Millisecond)).UTC()
		id, e := uid.FromTime(now)
		require.NoError(ht, e)
		actor := hegel.Draw(ht, hegel.Text())
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{"run", "cancel", "workflow-cancel"}))
		fields := map[string]any{"kind": "run", "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "requested_by": actor, "requested_at": now, "expires_at": now.Add(time.Duration(ttl) * time.Second)}
		if kind != "run" {
			fields["kind"] = "cancel"
			fields["run_uid"] = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
			if kind == "workflow-cancel" {
				delete(fields, "job_uid")
				delete(fields, "definition_event_uid")
			}
		}
		raw, e := json.Marshal(fields)
		require.NoError(ht, e)
		got, e := decodeCronRequest(id, raw)
		valid := strings.TrimSpace(actor) != "" && len(actor) <= 256
		if !valid {
			require.ErrorIs(ht, e, cron.ErrInvalid)
			return
		}
		require.NoError(ht, e)
		require.Equal(ht, id, got.UID)
		require.Equal(ht, fields["kind"], got.Kind)
		if kind == "workflow-cancel" {
			require.Empty(ht, got.JobUID)
			require.Empty(ht, got.DefinitionEventUID)
		} else {
			require.Equal(ht, fields["job_uid"], got.JobUID)
			require.Equal(ht, fields["definition_event_uid"], got.DefinitionEventUID)
		}
		if kind != "run" {
			require.Equal(ht, fields["run_uid"], *got.RunUID)
		}
		require.Equal(ht, actor, got.RequestedBy)
		require.True(ht, got.RequestedAt.Equal(now))
		require.Equal(ht, time.Duration(ttl)*time.Second, got.ExpiresAt.Sub(got.RequestedAt))
		encoded, e := encodeCronRequest(got)
		require.NoError(ht, e)
		roundtrip, e := decodeCronRequest(id, encoded)
		require.NoError(ht, e)
		require.Equal(ht, got, roundtrip, "production encoder preserves independently constructed values")
	}, hegel.WithTestCases(1000))
}

// Only authenticated roots create/delete immutable request bodies. Baselines
// omit from, duplicates may repeat the same body, replacements are rejected.
func TestCronRequestIngestWriteOnce(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id, e := uid.FromTime(now)
	require.NoError(t, e)
	value := mustJSONForCronRequestTest(t, map[string]any{"kind": "run", "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW", "requested_by": "operator", "requested_at": now, "expires_at": now.Add(time.Minute)})
	for _, tc := range []struct {
		from, to          jsontext.Value
		origin, authority string
		ok                bool
	}{
		{nil, value, "root", "root", true}, {jsontext.Value("null"), value, "root", "root", true}, {value, value, "root", "root", true}, {value, jsontext.Value("null"), "root", "root", true},
		{value, jsontext.Value(strings.ReplaceAll(string(value), "operator", "other")), "root", "root", false},
		{nil, value, "root", "", false}, {nil, value, "other", "root", false}, {nil, jsontext.Value("true"), "root", "root", false},
	} {
		change := map[string]jsontext.Value{"to": tc.to}
		if tc.from != nil {
			change["from"] = tc.from
		}
		raw := mustJSONForCronRequestTest(t, map[string]any{"diff": map[string]any{"cron.request." + id: change}})
		e = ValidateCronControlEvent(RemoteEvent{Type: "project.metadata_updated", OriginInstanceUID: tc.origin, Payload: raw}, tc.authority)
		if tc.ok {
			require.NoError(t, e)
		} else {
			require.ErrorIs(t, e, ErrFederationIngestValidation)
		}
	}
}

// Answered has priority at every deadline, and the deadline is inclusive.
// Exercise the full timestamp domain, including sub-millisecond read clocks.
func TestCronRequestStateProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		expiry := time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 253402214399)), int64(hegel.Draw(ht, hegel.Integers(0, 999999999)))).UTC()
		distance := time.Duration(hegel.Draw(ht, hegel.Integers[int64](1, int64(24*time.Hour))))
		for _, read := range []time.Time{expiry.Add(-distance), expiry, expiry.Add(distance)} {
			require.Equal(ht, "answered", cronRequestReadState(true, expiry, read))
		}
		require.Equal(ht, "pending", cronRequestReadState(false, expiry, expiry.Add(-distance)))
		require.Equal(ht, "expired", cronRequestReadState(false, expiry, expiry))
		require.Equal(ht, "expired", cronRequestReadState(false, expiry, expiry.Add(distance)))
	}, hegel.WithTestCases(1000))
}

// Unknown/missing/incorrectly typed fields must reject across both request kinds.
func TestCronRequestRejectShapeProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		at := time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 253402214399)), 0).UTC()
		id, e := uid.FromTime(at)
		require.NoError(ht, e)
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{"run", "cancel"}))
		fields := map[string]any{"kind": kind, "requested_by": "operator", "requested_at": at, "expires_at": at.Add(time.Minute), "job_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW"}
		if kind == "cancel" {
			fields["run_uid"] = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
		}
		switch hegel.Draw(ht, hegel.Integers(0, 5)) {
		case 0:
			fields["extra:"+hegel.Draw(ht, hegel.Text())] = true
		case 1:
			delete(fields, "requested_by")
		case 2:
			fields["requested_at"] = nil
		case 3:
			fields["expires_at"] = at.Add(59 * time.Second)
		case 4:
			delete(fields, "definition_event_uid")
		case 5:
			fields["requested_by"] = hegel.Draw(ht, hegel.Integers[int64](-9223372036854775808, 9223372036854775807))
		}
		raw, e := json.Marshal(fields)
		require.NoError(ht, e)
		_, e = decodeCronRequest(id, raw)
		require.ErrorIs(ht, e, cron.ErrInvalid)
	}, hegel.WithTestCases(1000))
}
