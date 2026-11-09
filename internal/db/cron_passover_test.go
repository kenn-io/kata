package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"pgregory.net/rapid"
)

// #nosec G101 -- Portable job UID fixture, not a credential.
const passoverJobUID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func passoverValue(t interface {
	require.TestingT
	Helper()
}, at time.Time, by string, paused *time.Time) jsontext.Value {
	t.Helper()
	v := map[string]any{"version": 1, "through": at, "by": by}
	if paused != nil {
		v["paused_at"] = *paused
	}
	raw, e := json.Marshal(v)
	require.NoError(t, e)
	return raw
}

// The later applicable cutoff wins, a job cutoff wins an equal instant, null is
// absent, and any malformed applicable value blocks scheduling.
func TestCronPassoverFromMetadata(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a := passoverValue(t, at, "operator", nil)
	b := passoverValue(t, at.Add(time.Hour), "worker", &at)
	null := jsontext.Value("null")
	metadata := func(project, job jsontext.Value) JSONBlob {
		m := map[string]jsontext.Value{"cron.passover.01ARZ3NDEKTSV4RRFFQ69G5FAW": jsontext.Value("false")}
		if project != nil {
			m["cron.passover"] = project
		}
		if job != nil {
			m["cron.passover."+passoverJobUID] = job
		}
		raw, e := json.Marshal(m)
		require.NoError(t, e)
		return JSONBlob(raw)
	}
	for _, blob := range []JSONBlob{``, `{}`, metadata(nil, nil), metadata(null, null)} {
		require.Nil(t, CronPassoverFromMetadata(blob, passoverJobUID), blob)
	}
	for _, tc := range []struct {
		p, j  jsontext.Value
		scope string
		at    time.Time
	}{{a, nil, "project", at}, {a, null, "project", at}, {null, a, "job", at}, {a, b, "job", at.Add(time.Hour)}, {b, a, "project", at.Add(time.Hour)}, {a, a, "job", at}} {
		require.Equal(t, &CronPassoverState{Through: tc.at, Scope: tc.scope}, CronPassoverFromMetadata(metadata(tc.p, tc.j), passoverJobUID))
	}
	// "+00:00" names UTC; reads normalize it so native planning accepts it.
	state := CronPassoverFromMetadata(`{"cron.passover":{"version":1,"through":"2026-01-02T03:04:05+00:00","by":"operator"}}`, passoverJobUID)
	require.Equal(t, &CronPassoverState{Through: at, Scope: "project"}, state)
	_, err := cron.ApplyPassover(cron.RecurringProgress{Version: 1, Anchor: at, Through: at}, state.Through)
	require.NoError(t, err)

	bad := jsontext.Value(`{"version":1}`)
	for _, values := range [][2]jsontext.Value{{bad, nil}, {bad, b}, {a, bad}, {nil, bad}} {
		require.Equal(t, &CronPassoverState{Invalid: true}, CronPassoverFromMetadata(metadata(values[0], values[1]), passoverJobUID))
	}
	require.Equal(t, &CronPassoverState{Invalid: true}, CronPassoverFromMetadata(`not json`, passoverJobUID))
}

// The complete-value codec accepts the portable UTC domain and rejects nearby
// malformed representations.
func TestCronPassoverDecode(t *testing.T) {
	long := strings.Repeat("é", 128)
	for _, raw := range []string{
		`{"version":1,"through":"2026-01-02T03:04:05Z","by":"operator"}`,
		`{"version":1,"through":"2026-01-02T03:04:05+00:00","by":"operator","paused_at":"2026-01-02T02:04:05+00:00"}`,
		`{"version":1,"through":"0001-01-01T00:00:01Z","by":"` + long + `"}`,
		`{"version":1,"through":"9999-12-31T23:59:59.999999999Z","by":"operator"}`,
	} {
		_, err := decodeCronPassover(jsontext.Value(raw))
		require.NoError(t, err, raw)
	}
	for _, raw := range []string{
		`true`, `null`, `{}`,
		`{"version":2,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1.0,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1e0,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":"1","through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":null,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1,"version":1,"through":"2026-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":"operator","extra":true}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z"}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":null}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":1}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":" \t"}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":"` + long + `x"}`,
		`{"version":1,"by":"operator"}`,
		`{"version":1,"through":null,"by":"operator"}`,
		`{"version":1,"through":1,"by":"operator"}`,
		`{"version":1,"through":"0001-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1,"through":"0000-12-31T23:59:59Z","by":"operator"}`,
		`{"version":1,"through":"10000-01-01T00:00:00Z","by":"operator"}`,
		`{"version":1,"through":"2026-01-02T04:04:05+01:00","by":"operator"}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":"operator","paused_at":null}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":"operator","paused_at":1}`,
		`{"version":1,"through":"2026-01-01T00:00:00Z","by":"operator","paused_at":"2026-01-01T01:00:00+01:00"}`,
	} {
		_, err := decodeCronPassover(jsontext.Value(raw))
		require.ErrorIs(t, err, cron.ErrInvalid, raw)
	}
}

// Whole-value maximum is associative/commutative/idempotent and preserves the
// winner's attribution, including optional paused_at, across equal timestamps.
func TestCronPassoverFoldProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(ht *rapid.T) {
		base := time.Unix(rapid.Int64Range(-62135596799, 253402214399).Draw(ht, "base"), 0).UTC()
		values := make([]jsontext.Value, 3)
		for i := range values {
			at := base.Add(time.Duration(rapid.IntRange(0, 5).Draw(ht, "at")) * time.Second)
			actor := rapid.SampledFrom([]string{"operator", "worker", "zz"}).Draw(ht, "actor")
			var paused *time.Time
			if rapid.Bool().Draw(ht, "paused") {
				paused = &base
			}
			values[i] = passoverValue(ht, at, actor, paused)
		}
		fold := func(a, b jsontext.Value) jsontext.Value {
			v, e := FoldCronPassover(a, b)
			require.NoError(ht, e)
			return v
		}
		a, b, c := values[0], values[1], values[2]
		require.Equal(ht, fold(a, b), fold(b, a))
		require.Equal(ht, fold(fold(a, b), c), fold(a, fold(b, c)))
		require.Equal(ht, fold(a, a), fold(nil, a))
		// Independent ordered-maximum oracle, including complete attribution.
		expected := canonicalJSON(values[0])
		var first CronPassover
		require.NoError(ht, json.Unmarshal(values[0], &first))
		expectedTime := first.Through
		for _, raw := range values[1:] {
			var candidate CronPassover
			require.NoError(ht, json.Unmarshal(raw, &candidate))
			canonical := canonicalJSON(raw)
			if candidate.Through.After(expectedTime) || (candidate.Through.Equal(expectedTime) && bytes.Compare(canonical, expected) > 0) {
				expected, expectedTime = canonical, candidate.Through
			}
		}
		require.Equal(ht, expected, fold(fold(a, b), c))
	})
}

// Incremental and baseline folds keep the maximum cutoff in any arrival order;
// deleting an absent cutoff leaves it absent.
func TestCronPassoverProjection(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	high := passoverValue(t, at.Add(time.Hour), "first", &at)
	low := passoverValue(t, at, "later", nil)
	events := []FoldEvent{{UID: "event1", ProjectUID: "project", Type: "project.metadata_updated", HLCPhysicalMS: 1, Payload: []byte(`{"diff":{"cron.passover":{"from":null,"to":` + string(high) + `},"ordinary":{"from":null,"to":true}}}`)}, {UID: "event2", ProjectUID: "project", Type: "project.metadata_updated", HLCPhysicalMS: 2, Payload: []byte(`{"diff":{"cron.passover":{"from":null,"to":` + string(low) + `}}}`)}}
	for _, order := range [][]int{{0, 1}, {1, 0}, {0, 1, 0, 1}, {1, 1, 0, 0}} {
		incremental := FoldEvents(nil)
		for _, i := range order {
			incremental.apply(events[i])
		}
		var got map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(incremental.ProjectMetadata["project"], &got))
		require.JSONEq(t, string(high), string(got["cron.passover"]), "incremental arrival order must not regress cutoff")
		require.Equal(t, "true", string(got["ordinary"]))
	}
	events = append(events, FoldEvent{UID: "baseline", ProjectUID: "project", Type: "project.federation_enabled", HLCPhysicalMS: 3, Payload: []byte(`{"metadata":{"cron.passover":` + string(low) + `,"other":"value"}}`)})
	var values map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(FoldEvents(events).ProjectMetadata["project"], &values))
	require.JSONEq(t, string(high), string(values["cron.passover"]), "baseline metadata uses the same maximum")
	require.Equal(t, `"value"`, string(values["other"]))

	got := applyCronProjectMetadataDiff(jsontext.Value(`{}`), jsontext.Value(`{"cron.passover":{"from":null,"to":null}}`))
	require.JSONEq(t, `{}`, string(got))
}

// A signed reset may retain malformed history only with the exact later root
// repair and valid current state; changing any witness fails closed.
func TestCronPassoverResetRepair(t *testing.T) {
	good := passoverValue(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "operator", nil)
	for _, key := range []string{CronPassoverMetadataKey, CronPassoverMetadataPrefix + passoverJobUID} {
		for _, mode := range []string{"repaired", "unrepaired", "inexact", "not-later", "spoke-legacy", "no-current"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				legacy := controlRecoveryEvent(t, key, jsontext.Value("null"), jsontext.Value(`"legacy"`), "root", 1)
				repair := controlRecoveryEvent(t, key, jsontext.Value(`"legacy"`), good, "root", 2)
				current := []RemoteEvent{controlRecoveryEvent(t, key, jsontext.Value("null"), good, "root", 3)}
				switch mode {
				case "inexact":
					repair = controlRecoveryEvent(t, key, jsontext.Value("false"), good, "root", 2)
				case "not-later":
					repair.HLCPhysicalMS = 0
				case "spoke-legacy":
					legacy.OriginInstanceUID = "spoke"
				case "no-current":
					current = nil
				}
				history := []RemoteEvent{legacy, repair}
				if mode == "unrepaired" {
					history = history[:1]
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
