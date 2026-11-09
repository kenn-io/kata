package db

import (
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/uid"
)

// A stored request is exact: the UID's millisecond is the admission time, TTL
// is whole seconds in 60..86400, and run and cancel bodies have fixed members.
func TestCronRequestValue(t *testing.T) {
	id, err := uid.FromTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	require.NoError(t, err)
	const (
		job    = `"job_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW"`
		run    = `"kind":"run",` + job
		cancel = `"kind":"cancel","run_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAX"`
		by     = `"requested_by":"operator"`
		at     = `"requested_at":"2026-01-02T03:04:05Z"`
		ttl    = `"expires_at":"2026-01-02T03:14:05Z"`
		times  = by + "," + at + "," + ttl
	)
	obj := func(members ...string) string { return "{" + strings.Join(members, ",") + "}" }
	valid := []string{
		obj(run, times),
		obj(cancel, times),
		obj(cancel, job, times),
		obj(run, by, at, `"expires_at":"2026-01-02T03:05:05Z"`),
		obj(run, by, at, `"expires_at":"2026-01-03T03:04:05Z"`),
		obj(run, `"requested_by":"`+strings.Repeat("x", 256)+`"`, at, ttl),
	}
	for _, raw := range valid {
		got, err := decodeCronRequest(id, jsontext.Value(raw))
		require.NoError(t, err, raw)
		require.Equal(t, id, got.UID)
		require.True(t, got.RequestedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), raw)
	}
	got, err := decodeCronRequest(id, jsontext.Value(obj(cancel, times)))
	require.NoError(t, err)
	require.Equal(t, CronRequest{UID: id, Kind: "cancel", RunUID: new("01ARZ3NDEKTSV4RRFFQ69G5FAX"), RequestedBy: "operator", RequestedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ExpiresAt: time.Date(2026, 1, 2, 3, 14, 5, 0, time.UTC)}, got)

	invalid := []string{
		`true`, `null`, `[]`, `{}`,
		obj(run, times, `"state":"answered"`),
		obj(run, times, `"uid":"`+id+`"`),
		obj(`"kind":"run"`, `"kind":"run"`, job, times),
		obj(`"kind":"other"`, job, times),
		obj(`"kind":null`, job, times),
		obj(job, times),
		obj(`"kind":"run","job_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"`, times),
		obj(`"kind":"run","job_uid":null,"definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW"`, times),
		obj(`"kind":"run","job_uid":"","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW"`, times),
		obj(run, `"run_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAX"`, times),
		obj(run, `"run_uid":null`, times),
		obj(`"kind":"cancel"`, times),
		obj(`"kind":"cancel"`, job, times),
		obj(`"kind":"cancel","run_uid":null`, times),
		obj(`"kind":"cancel","run_uid":1`, times),
		obj(`"kind":"cancel","run_uid":"01arz3ndektsv4rrffq69g5fax"`, times),
		obj(cancel, `"job_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV"`, times),
		obj(cancel, `"job_uid":null,"definition_event_uid":null`, times),
		obj(cancel, `"job_uid":"","definition_event_uid":""`, times),
		obj(run, `"requested_by":" "`, at, ttl),
		obj(run, `"requested_by":"`+strings.Repeat("x", 257)+`"`, at, ttl),
		obj(run, `"requested_by":1`, at, ttl),
		obj(run, `"requested_by":null`, at, ttl),
		obj(run, at, ttl),
		obj(run, by, `"requested_at":"2026-01-02T03:04:05.001Z"`, ttl),
		obj(run, by, `"requested_at":null`, ttl),
		obj(run, by, `"requested_at":1`, ttl),
		obj(run, by, ttl),
		obj(run, by, at, `"expires_at":"2026-01-02T03:05:04Z"`),
		obj(run, by, at, `"expires_at":"2026-01-03T03:04:06Z"`),
		obj(run, by, at, `"expires_at":"2026-01-02T03:14:05.000000001Z"`),
		obj(run, by, at, `"expires_at":null`),
		obj(run, by, at),
	}
	for _, raw := range invalid {
		_, err := decodeCronRequest(id, jsontext.Value(raw))
		require.ErrorIs(t, err, cron.ErrInvalid, raw)
	}
	_, err = decodeCronRequest(strings.ToLower(id), jsontext.Value(obj(run, times)))
	require.ErrorIs(t, err, cron.ErrInvalid, "the key suffix must be a canonical UID")
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
}
